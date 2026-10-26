package main

import (
	"context"
	"encoding/json"

	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/health"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// Exit codes mirror contracts/cli.md. They are a public interface, so a
// scheduler can branch without parsing output.
const (
	exitOK         = 0
	exitInternal   = 1
	exitUsage      = 2
	exitConfig     = 3
	exitDependency = 4
)

// exitError carries the process exit status through the normal error path.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func exitWith(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// doctorReport is the machine-readable document from `doctor --json`.
type doctorReport struct {
	Status     string             `json:"status"`
	Components []health.Component `json:"components"`
}

type doctorOptions struct {
	configPath string
	deep       bool
	all        bool
	json       bool
	out        io.Writer
	errOut     io.Writer
}

// runDoctor is pre-flight verification (FR-058). It never merges, pushes, or
// writes to a repository.
func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to config.yaml (defaults to $"+config.EnvConfigPath+" or config.yaml)")
	deep := flags.Bool("deep", false, "also fetch each mapped repository")
	all := flags.Bool("all", false, "run every check instead of stopping at the first failure")
	jsonOut := flags.Bool("json", false, "machine-readable output on stdout")
	if err := flags.Parse(args); err != nil {
		return exitWith(exitUsage, "doctor: %v", err)
	}
	return doctor(doctorOptions{
		configPath: *configPath,
		deep:       *deep,
		all:        *all,
		json:       *jsonOut,
		out:        os.Stdout,
		errOut:     os.Stderr,
	})
}

func doctor(opts doctorOptions) error {
	if opts.out == nil {
		opts.out = io.Discard
	}
	if opts.errOut == nil {
		opts.errOut = io.Discard
	}

	report := doctorReport{Status: "ok", Components: make([]health.Component, 0, 10)}
	failures := 0
	configFailed := false

	record := func(component health.Component) bool {
		report.Components = append(report.Components, component)
		if component.Status == health.StatusFailing {
			failures++
			return true
		}
		return false
	}

	// 1. Configuration parses, schema_version is known, required sections exist.
	cfg, err := config.Load(opts.configPath)
	started := time.Now()
	configComponent := health.Component{
		Name:        "configuration",
		Status:      health.StatusOK,
		Remediation: "Fix the YAML key named in the detail, then run `redmine-agent doctor` again.",
	}
	if err != nil {
		configComponent.Status = health.StatusFailing
		configComponent.Detail = err.Error()
		configFailed = true
	} else {
		configComponent.Detail = fmt.Sprintf("%s is valid at schema_version %d", cfg.SourcePath, cfg.SchemaVersion)
	}
	configComponent.LatencyMS = time.Since(started).Milliseconds()
	record(configComponent)
	if configFailed {
		return finishDoctor(opts, report, exitConfig)
	}

	// 2. Every secret reference resolves; only the reference is ever reported.
	secretComponent := checkSecretReferences(cfg)
	record(secretComponent)
	if secretComponent.Status == health.StatusFailing {
		configFailed = true
		if !opts.all {
			return finishDoctor(opts, report, exitConfig)
		}
	}

	redactor := logging.NewRedactor()
	registerDoctorSecrets(cfg, redactor)

	// 3. The database file is writable and migrations are current.
	ctx := context.Background()
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	st, storeErr := store.Open(openCtx, cfg.Data.Database, store.WithRedactor(redactor))
	cancel()
	checker := health.NewChecker(health.Options{Redactor: redactor})
	probes := health.DefaultProbes(health.Deps{Store: st, Config: cfg, Redactor: redactor})
	databaseComponent := health.Component{
		Name:        "database",
		Status:      health.StatusFailing,
		Remediation: "Check data.database and that its directory is writable.",
	}
	if storeErr != nil {
		databaseComponent.Detail = storeErr.Error()
	} else {
		databaseComponent = checker.RunOne(ctx, mustProbe(probes, "database"))
	}
	if st != nil {
		defer st.Close()
	}
	if record(databaseComponent) && !opts.all {
		return finishDoctor(opts, report, exitDependency)
	}

	// 4. git is on PATH and new enough to understand worktrees.
	gitComponent := checkGitVersion()
	if record(gitComponent) && !opts.all {
		return finishDoctor(opts, report, exitDependency)
	}

	// 5. The data directories are writable.
	pathsComponent := checkWritablePaths(cfg)
	if record(pathsComponent) && !opts.all {
		return finishDoctor(opts, report, exitDependency)
	}

	// 6-9 need clients; build them from the configuration.
	redmineClient, err := redmine.New(redmine.Options{
		BaseURL: cfg.Redmine.BaseURL, APIKey: cfg.Redmine.APIKey, Redactor: redactor,
	})
	if err != nil {
		record(health.Component{Name: "redmine", Status: health.StatusFailing, Detail: err.Error(),
			Remediation: "Fix redmine.base_url."})
		return finishDoctor(opts, report, exitDependency)
	}
	gitlabClient, err := gitlab.New(gitlab.Options{
		BaseURL: cfg.GitLab.BaseURL, Token: cfg.GitLab.Token, Redactor: redactor,
	})
	if err != nil {
		record(health.Component{Name: "gitlab", Status: health.StatusFailing, Detail: err.Error(),
			Remediation: "Fix gitlab.base_url."})
		return finishDoctor(opts, report, exitDependency)
	}

	manager := git.NewManager("", cfg.Data.WorkspaceRoot)
	if token, tokenErr := cfg.GitLab.Token.Resolve(); tokenErr == nil && token != "" {
		manager.SetHTTPAuth("oauth2", token)
	}
	registry := agent.NewRegistry()
	adapter, err := agent.NewOpenCode(agent.Config{
		Command:       cfg.Agent.Command,
		Model:         cfg.Agent.Model,
		AgentName:     cfg.Agent.Profile,
		WorkspaceRoot: cfg.Data.WorkspaceRoot,
		Redactor:      redactor,
	})
	if err == nil {
		_ = registry.Register(adapter)
	}

	probes = health.DefaultProbes(health.Deps{
		Store: st, Config: cfg, Redmine: redmineClient, GitLab: gitlabClient,
		Git: manager, Adapters: registry, Redactor: redactor,
	})

	// 6. Redmine answers and the credential can read.
	redmineComponent := checker.RunOne(ctx, mustProbe(probes, "redmine"))
	if record(redmineComponent) && !opts.all {
		return finishDoctor(opts, report, exitDependency)
	}

	// 7. GitLab answers and the credential can see a mapped project.
	gitlabComponent := checker.RunOne(ctx, mustProbe(probes, "gitlab"))
	if record(gitlabComponent) && !opts.all {
		return finishDoctor(opts, report, exitDependency)
	}

	// 8. Each mapped repository fetches; deep only, because it touches the network.
	repositoryComponent := health.Component{
		Name:        "repository",
		Status:      health.StatusUnknown,
		Detail:      "not probed: pass --deep to fetch each mapped repository",
		Remediation: "Run `redmine-agent doctor --deep`.",
	}
	if opts.deep {
		repositoryComponent = checker.RunOne(ctx, mustProbe(probes, "repository"))
	}
	repositoryComponent.Name = "repository"
	if record(repositoryComponent) && !opts.all {
		return finishDoctor(opts, report, exitDependency)
	}

	// 9. The configured agent executable answers --version.
	agentComponent := checker.RunOne(ctx, mustProbe(probes, "agent"))
	if record(agentComponent) && !opts.all {
		return finishDoctor(opts, report, exitDependency)
	}

	// 10. Every mapping has at least one required check: a warning, not a failure.
	checksComponent := checkRequiredChecks(ctx, st)
	record(checksComponent)

	code := exitOK
	if failures > 0 {
		code = exitDependency
	}
	if configFailed {
		code = exitConfig
	}
	return finishDoctor(opts, report, code)
}

func mustProbe(probes []health.Probe, name string) health.Probe {
	probe, ok := health.ByName(probes, name)
	if !ok {
		return health.Probe{
			Name: name,
			Check: func(context.Context) (string, error) {
				return "", fmt.Errorf("%w: the %s probe is not available", health.ErrNotConfigured, name)
			},
		}
	}
	return probe
}

func checkSecretReferences(cfg *config.Config) health.Component {
	started := time.Now()
	component := health.Component{
		Name:        "secrets",
		Status:      health.StatusOK,
		Remediation: "Export the environment variable or create the file named by the reference.",
	}
	references := []struct {
		name      string
		reference config.SecretRef
	}{
		{"redmine.api_key", cfg.Redmine.APIKey},
		{"gitlab.token", cfg.GitLab.Token},
	}
	var resolved []string
	for _, item := range references {
		if !item.reference.IsSet() {
			component.Status = health.StatusFailing
			component.Detail = item.name + " has no reference"
			component.LatencyMS = time.Since(started).Milliseconds()
			return component
		}
		if _, err := item.reference.Resolve(); err != nil {
			component.Status = health.StatusFailing
			component.Detail = fmt.Sprintf("%s: %v", item.name, err)
			component.LatencyMS = time.Since(started).Milliseconds()
			return component
		}
		resolved = append(resolved, item.reference.Reference())
	}
	component.Detail = "resolved " + strings.Join(resolved, ", ") + " (values never printed)"
	component.LatencyMS = time.Since(started).Milliseconds()
	return component
}

func registerDoctorSecrets(cfg *config.Config, redactor *logging.Redactor) {
	if redactor == nil {
		return
	}
	for _, reference := range []config.SecretRef{cfg.Redmine.APIKey, cfg.GitLab.Token} {
		if !reference.IsSet() {
			continue
		}
		if value, err := reference.Resolve(); err == nil {
			redactor.Register(value)
		}
	}
}

func checkGitVersion() health.Component {
	started := time.Now()
	component := health.Component{
		Name:        "git",
		Status:      health.StatusOK,
		Remediation: "Install git 2.30 or newer and put it on PATH.",
	}
	path, err := exec.LookPath("git")
	if err != nil {
		component.Status = health.StatusFailing
		component.Detail = "git is not on PATH"
		component.LatencyMS = time.Since(started).Milliseconds()
		return component
	}
	// #nosec G204 -- the binary came from LookPath and the arguments are literals.
	command := exec.Command(path, "version")
	output, err := command.CombinedOutput()
	if err != nil {
		component.Status = health.StatusFailing
		component.Detail = "git did not answer --version: " + strings.TrimSpace(string(output))
		component.LatencyMS = time.Since(started).Milliseconds()
		return component
	}
	version := strings.TrimSpace(string(output))
	if major, minor, ok := parseGitVersion(version); !ok || major < 2 || (major == 2 && minor < 30) {
		component.Status = health.StatusFailing
		component.Detail = version + " is older than 2.30"
		component.LatencyMS = time.Since(started).Milliseconds()
		return component
	}
	component.Detail = version
	component.LatencyMS = time.Since(started).Milliseconds()
	return component
}

func parseGitVersion(output string) (int, int, bool) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return 0, 0, false
	}
	token := fields[len(fields)-1]
	parts := strings.SplitN(token, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func checkWritablePaths(cfg *config.Config) health.Component {
	started := time.Now()
	component := health.Component{
		Name:        "paths",
		Status:      health.StatusOK,
		Remediation: "Create the directories or grant the service account write access.",
	}
	directories := []string{cfg.Data.WorkspaceRoot, cfg.Data.LogDir, filepath.Dir(cfg.Data.Database)}
	for _, directory := range directories {
		if strings.TrimSpace(directory) == "" {
			component.Status = health.StatusFailing
			component.Detail = "a data directory is empty in config.yaml"
			component.LatencyMS = time.Since(started).Milliseconds()
			return component
		}
		if err := os.MkdirAll(directory, 0o750); err != nil {
			component.Status = health.StatusFailing
			component.Detail = fmt.Sprintf("%s is not writable: %v", directory, err)
			component.LatencyMS = time.Since(started).Milliseconds()
			return component
		}
		probe, err := os.CreateTemp(directory, ".doctor-*")
		if err != nil {
			component.Status = health.StatusFailing
			component.Detail = fmt.Sprintf("%s is not writable: %v", directory, err)
			component.LatencyMS = time.Since(started).Milliseconds()
			return component
		}
		name := probe.Name()
		_ = probe.Close()
		_ = os.Remove(name)
	}
	component.Detail = "workspace root, log dir, and data dir are writable"
	component.LatencyMS = time.Since(started).Milliseconds()
	return component
}

func checkRequiredChecks(ctx context.Context, st *store.Store) health.Component {
	started := time.Now()
	component := health.Component{
		Name:        "checks",
		Status:      health.StatusOK,
		Remediation: "Add at least one required check for each mapping in config.yaml.",
	}
	if st == nil {
		component.Status = health.StatusUnknown
		component.Detail = "not checked: no database is open"
		component.LatencyMS = time.Since(started).Milliseconds()
		return component
	}
	mappings, err := st.ListRepositories(ctx)
	if err != nil {
		component.Status = health.StatusUnknown
		component.Detail = "not checked: " + err.Error()
		component.LatencyMS = time.Since(started).Milliseconds()
		return component
	}
	var missing []string
	for _, mapping := range mappings {
		if !mapping.Enabled {
			continue
		}
		required := 0
		for _, check := range mapping.Checks {
			if check.Required {
				required++
			}
		}
		if required == 0 {
			missing = append(missing, mapping.RepositoryPath)
		}
	}
	if len(missing) > 0 {
		component.Status = health.StatusUnknown
		component.Detail = "warning: no required check for " + strings.Join(missing, ", ")
	} else {
		component.Detail = "every mapped repository has a required check"
	}
	component.LatencyMS = time.Since(started).Milliseconds()
	return component
}

// finishDoctor renders the report and converts it to an exit code.
func finishDoctor(opts doctorOptions, report doctorReport, code int) error {
	degraded := false
	for _, component := range report.Components {
		if component.Status == health.StatusFailing {
			degraded = true
		}
	}
	if degraded {
		report.Status = "degraded"
	} else {
		report.Status = "ok"
	}

	if opts.json {
		encoded, err := json.Marshal(report)
		if err != nil {
			return exitWith(exitInternal, "doctor: encode report: %v", err)
		}
		fmt.Fprintln(opts.out, string(encoded))
		fmt.Fprintf(opts.errOut, "doctor: %s (%d checks)\n", report.Status, len(report.Components))
	} else {
		renderDoctorReport(opts.out, report)
	}

	if code == exitOK {
		return nil
	}
	degradedComponents := 0
	for _, component := range report.Components {
		if component.Status == health.StatusFailing {
			degradedComponents++
		}
	}
	if degradedComponents == 0 {
		return nil
	}
	return exitWith(code, "doctor: %d check(s) failed", degradedComponents)
}

func renderDoctorReport(out io.Writer, report doctorReport) {
	for index, component := range report.Components {
		marker := "ok"
		switch component.Status {
		case health.StatusFailing:
			marker = "FAIL"
		case health.StatusUnknown:
			marker = "warn"
		}
		fmt.Fprintf(out, "%-4s %2d. %-14s %5dms  %s\n",
			marker, index+1, component.Name, component.LatencyMS, component.Detail)
		if component.Status == health.StatusFailing && component.Remediation != "" {
			fmt.Fprintf(out, "      fix: %s\n", component.Remediation)
		}
	}
	fmt.Fprintf(out, "status: %s\n", report.Status)
}
