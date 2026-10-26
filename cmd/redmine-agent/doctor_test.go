package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/health"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

func TestDoctorReportsAConfigFailureWithExitCode3(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nunknown_key: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errOut bytes.Buffer
	err := doctor(doctorOptions{configPath: path, out: &out, errOut: &errOut})
	var code *exitError
	if !errors.As(err, &code) {
		t.Fatalf("error = %v, want an exitError", err)
	}
	if code.code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code.code, exitConfig)
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "configuration") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDoctorJSONIsOneDocumentPerLine(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 9\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errOut bytes.Buffer
	err := doctor(doctorOptions{configPath: path, json: true, out: &out, errOut: &errOut})
	var code *exitError
	if !errors.As(err, &code) || code.code != exitConfig {
		t.Fatalf("error = %v, want exit 3", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("json output lines = %d, want 1: %q", len(lines), out.String())
	}
	var report doctorReport
	if decodeErr := json.Unmarshal([]byte(lines[0]), &report); decodeErr != nil {
		t.Fatalf("decode json: %v", decodeErr)
	}
	if report.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", report.Status)
	}
	if len(report.Components) == 0 || report.Components[0].Name != "configuration" {
		t.Fatalf("components = %+v", report.Components)
	}
}

func TestDoctorPathAndGitChecks(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		SchemaVersion: 1,
		Data: config.Data{
			Database:      filepath.Join(root, "data", "agent.db"),
			WorkspaceRoot: filepath.Join(root, "data", "workspaces"),
			LogDir:        filepath.Join(root, "data", "logs"),
		},
	}

	paths := checkWritablePaths(cfg)
	if paths.Status != "ok" {
		t.Fatalf("paths = %+v", paths)
	}

	gitCheck := checkGitVersion()
	if gitCheck.Status != "ok" {
		t.Fatalf("git check = %+v (git is required by this repository's tests)", gitCheck)
	}
	if !strings.Contains(gitCheck.Detail, "git version") {
		t.Fatalf("git detail = %q", gitCheck.Detail)
	}
}

func TestParseGitVersion(t *testing.T) {
	cases := []struct {
		input     string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{"git version 2.43.0.windows.1", 2, 43, true},
		{"git version 2.30.1", 2, 30, true},
		{"git version 2.29.0", 2, 29, true},
		{"not a version", 0, 0, false},
	}
	for _, testCase := range cases {
		major, minor, ok := parseGitVersion(testCase.input)
		if ok != testCase.wantOK || major != testCase.wantMajor || minor != testCase.wantMinor {
			t.Errorf("parseGitVersion(%q) = %d, %d, %v", testCase.input, major, minor, ok)
		}
	}
}

func TestRequiredCheckWarningDoesNotFailTheRun(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(root, "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		SchemaVersion: 1,
		Repositories: []config.Repository{{
			RedmineProject: "billing",
			RepositoryPath: "group/billing-api",
			BaseBranch:     "main",
			Checks: []config.Check{{
				Name:           "optional-lint",
				CommandLinux:   "true",
				CommandWindows: "cmd /c exit 0",
				Required:       boolPointer(false),
			}},
		}},
	}
	if _, err := st.SeedConfiguration(context.Background(), cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}

	component := checkRequiredChecks(context.Background(), st)
	if component.Status != "unknown" {
		t.Fatalf("status = %q, want unknown (a warning, not a failure)", component.Status)
	}
	if !strings.Contains(component.Detail, "group/billing-api") {
		t.Fatalf("detail = %q", component.Detail)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestSecretsCheckAcceptsAnAllLocalDeployment(t *testing.T) {
	t.Setenv("RCA_DOCTOR_REDMINE_KEY", "redmine-key")
	cfg := &config.Config{
		Redmine: config.Redmine{APIKey: config.NewSecretRef("env:RCA_DOCTOR_REDMINE_KEY")},
		GitLab:  config.GitLab{}, // no server, no token: every repository is local
	}

	component := checkSecretReferences(cfg)
	if component.Status != health.StatusOK {
		t.Fatalf("secrets status = %q (%s), want ok", component.Status, component.Detail)
	}
	if !strings.Contains(component.Detail, "RCA_DOCTOR_REDMINE_KEY") {
		t.Fatalf("detail = %q", component.Detail)
	}
}
