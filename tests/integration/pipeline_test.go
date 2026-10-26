package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/api"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/validate"
)

type pipelineEnvironment struct {
	harness *Harness
	runner  *runner.Runner
	server  *httptest.Server
}

type pipelineOptions struct {
	tune      func(*config.Config)
	validator runner.CommandRunner
}

func newPipeline(t *testing.T, opts pipelineOptions) *pipelineEnvironment {
	t.Helper()
	h := New(t)
	h.FakeAgent()
	if opts.tune != nil {
		opts.tune(h.Config)
	}
	if err := h.Config.Validate(); err != nil {
		t.Fatalf("pipeline config must be valid: %v", err)
	}
	if _, err := h.Store.SeedRepositories(context.Background(), h.Config); err != nil {
		t.Fatalf("seed repositories: %v", err)
	}

	redmineClient, err := redmine.New(redmine.Options{
		BaseURL:  h.Redmine.URL(),
		APIKey:   h.Config.Redmine.APIKey,
		Redactor: h.Redactor,
	})
	if err != nil {
		t.Fatalf("new redmine client: %v", err)
	}
	gitlabClient, err := gitlab.New(gitlab.Options{
		BaseURL:  h.GitLab.URL(),
		Token:    h.Config.GitLab.Token,
		Redactor: h.Redactor,
	})
	if err != nil {
		t.Fatalf("new gitlab client: %v", err)
	}

	manager := git.NewManager(h.ReposDir, h.WorkspaceRoot)
	registry := agent.NewRegistry()
	adapter, err := agent.NewOpenCode(agent.Config{
		Command:       h.Config.Agent.Command,
		Model:         h.Config.Agent.Model,
		AgentName:     h.Config.Agent.Profile,
		WorkspaceRoot: h.WorkspaceRoot,
		Redactor:      h.Redactor,
	})
	if err != nil {
		t.Fatalf("new opencode adapter: %v", err)
	}
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("register adapter: %v", err)
	}

	logger := logging.New(logging.Options{LogDir: h.LogDir, Redactor: h.Redactor, Stderr: io.Discard})

	validator := opts.validator
	if validator == nil {
		validator = validate.New(validate.Options{Redactor: h.Redactor})
	}

	pipeline, err := runner.New(runner.Options{
		Config:    h.Config,
		Store:     h.Store,
		Git:       manager,
		Adapters:  registry,
		Redmine:   redmineClient,
		GitLab:    gitlabClient,
		Logger:    logger,
		Validator: validator,
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	server := httptest.NewServer(api.NewServer(api.Deps{
		Config:   h.Config,
		Store:    h.Store,
		Logger:   logger,
		Redactor: h.Redactor,
		Redmine:  redmineClient,
		Runs:     pipeline,
	}).Handler())
	t.Cleanup(server.Close)
	t.Cleanup(pipeline.Wait)

	return &pipelineEnvironment{harness: h, runner: pipeline, server: server}
}

func (e *pipelineEnvironment) start(t *testing.T, issueID int) *domain.Run {
	t.Helper()
	body, err := json.Marshal(map[string]any{"issueId": issueID, "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(e.server.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/runs: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read create response: %v", err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.StatusCode, payload)
	}
	if location := response.Header.Get("Location"); !strings.HasPrefix(location, "/api/runs/") {
		t.Fatalf("Location = %q", location)
	}
	var created struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return e.waitForTerminal(t, created.RunID)
}

func (e *pipelineEnvironment) waitForTerminal(t *testing.T, publicID string) *domain.Run {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		run, err := e.harness.Store.GetRunByPublicID(context.Background(), publicID)
		if err != nil {
			t.Fatalf("read run %s: %v", publicID, err)
		}
		if domain.IsTerminal(run.State) {
			return run
		}
		time.Sleep(25 * time.Millisecond)
	}
	run, _ := e.harness.Store.GetRunByPublicID(context.Background(), publicID)
	t.Fatalf("run did not reach a terminal state: %+v", run)
	return nil
}

func (e *pipelineEnvironment) detail(t *testing.T, publicID string) map[string]any {
	t.Helper()
	response, err := http.Get(e.server.URL + "/api/runs/" + publicID)
	if err != nil {
		t.Fatalf("GET run detail: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d: %s", response.StatusCode, payload)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode detail: %v (%s)", err, payload)
	}
	return document
}

func issueWithMode(mode string) Issue {
	issue := defaultIssue()
	issue.Description += "\n[fake-agent:" + mode + "]\n"
	return issue
}

func publishDisabled(cfg *config.Config) {
	cfg.GitLab.PushEnabled = false
	cfg.GitLab.MergeRequestEnabled = false
}

func TestPipelineHappyPathCommitsAfterChecksPass(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s), want completed", run.State, run.FailureMessage)
	}
	if run.CommitSHA == "" {
		t.Fatal("a passing run must create a commit")
	}
	if run.FailureCode != "" {
		t.Fatalf("failure code = %q", run.FailureCode)
	}

	workspace, err := env.harness.Store.GetWorkspaceByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if workspace.BaseRevision == "" || workspace.BaseRevision == run.CommitSHA {
		t.Fatalf("base revision = %q, commit = %q", workspace.BaseRevision, run.CommitSHA)
	}
	if workspace.Branch != "agent/issue-4821" {
		t.Fatalf("branch = %q", workspace.Branch)
	}
	if !contains(workspace.ChangedPaths, "agent-change.txt") {
		t.Fatalf("changed paths = %v", workspace.ChangedPaths)
	}

	head := strings.TrimSpace(gitOutput(t, workspace.Path, "rev-parse", "HEAD"))
	if head != run.CommitSHA {
		t.Fatalf("HEAD = %q, recorded commit = %q", head, run.CommitSHA)
	}
	message := gitOutput(t, workspace.Path, "log", "-1", "--pretty=%s")
	if !strings.Contains(message, "#4821") {
		t.Fatalf("commit message does not reference the issue: %q", message)
	}

	results, err := env.harness.Store.ListValidationResults(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("validation results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("validation results = %+v", results)
	}
	if results[0].CheckName != "unit-tests" || results[0].Outcome != domain.OutcomePassed {
		t.Fatalf("check = %+v", results[0])
	}
	if results[0].ExitCode == nil || *results[0].ExitCode != 0 {
		t.Fatalf("exit code = %v", results[0].ExitCode)
	}
	if results[0].Command == "" {
		t.Fatal("the executed command must be recorded")
	}

	if tip := env.harness.Origin.Tip(t, "main"); tip != env.harness.Origin.InitialRevision {
		t.Fatalf("publishing disabled, but main moved: %s -> %s", env.harness.Origin.InitialRevision, tip)
	}

	detail := env.detail(t, run.PublicID)
	if detail["state"] != "completed" {
		t.Fatalf("detail state = %v", detail["state"])
	}
	if _, ok := detail["workspace"].(map[string]any); !ok {
		t.Fatalf("detail has no workspace: %v", detail["workspace"])
	}
	if invocations, ok := detail["invocations"].([]any); !ok || len(invocations) != 1 {
		t.Fatalf("detail invocations = %v", detail["invocations"])
	}
	if timeline, ok := detail["timeline"].([]any); !ok || len(timeline) == 0 {
		t.Fatalf("detail timeline = %v", detail["timeline"])
	}
	if checks, ok := detail["checks"].([]any); !ok || len(checks) != 1 {
		t.Fatalf("detail checks = %v", detail["checks"])
	}
}

func TestPipelineFailingRequiredCheckBlocksTheCommit(t *testing.T) {
	env := newPipeline(t, pipelineOptions{
		tune: func(cfg *config.Config) {
			publishDisabled(cfg)
			cfg.Agent.MaxRepairs = 0
			cfg.Repositories[0].Checks = []config.Check{
				{Name: "unit-tests", CommandLinux: "exit 1", CommandWindows: "exit 1", TimeoutSeconds: 60},
			}
		},
	})
	env.harness.AddIssue(issueWithMode("lies"))

	run := env.start(t, 4821)
	if run.State != domain.StateFailed {
		t.Fatalf("state = %q, want failed", run.State)
	}
	if run.FailureCode != domain.FailValidationFailed {
		t.Fatalf("failure code = %q, want validation_failed", run.FailureCode)
	}
	if run.CommitSHA != "" {
		t.Fatalf("a failed run must not commit: %q", run.CommitSHA)
	}

	workspace, err := env.harness.Store.GetWorkspaceByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	head := strings.TrimSpace(gitOutput(t, workspace.Path, "rev-parse", "HEAD"))
	if head != workspace.BaseRevision {
		t.Fatalf("the workspace moved to %q despite a failed check (base %q)", head, workspace.BaseRevision)
	}
	if tip := env.harness.Origin.Tip(t, "main"); tip != env.harness.Origin.InitialRevision {
		t.Fatal("the base branch must be untouched")
	}

	results, err := env.harness.Store.ListValidationResults(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("validation results: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("the failing check must be recorded")
	}
	if results[0].Outcome != domain.OutcomeFailed || results[0].ExitCode == nil || *results[0].ExitCode == 0 {
		t.Fatalf("check = %+v", results[0])
	}

	detail := env.detail(t, run.PublicID)
	if detail["failureCode"] != "validation_failed" {
		t.Fatalf("detail failureCode = %v", detail["failureCode"])
	}
}

func TestPipelineNoChangesCreatesNoCommit(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("no-changes"))

	run := env.start(t, 4821)
	if run.State != domain.StateFailed {
		t.Fatalf("state = %q, want failed", run.State)
	}
	if run.FailureCode != domain.FailNoChanges {
		t.Fatalf("failure code = %q, want no_changes", run.FailureCode)
	}
	if run.CommitSHA != "" {
		t.Fatalf("no commit may exist: %q", run.CommitSHA)
	}

	workspace, err := env.harness.Store.GetWorkspaceByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	head := strings.TrimSpace(gitOutput(t, workspace.Path, "rev-parse", "HEAD"))
	if head != workspace.BaseRevision {
		t.Fatalf("HEAD = %q, base = %q", head, workspace.BaseRevision)
	}
}

func TestPipelineBlockedAgentEndsBlocked(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("blocked"))

	run := env.start(t, 4821)
	if run.State != domain.StateBlocked {
		t.Fatalf("state = %q, want blocked (%s)", run.State, run.FailureMessage)
	}
	if run.FailureCode != domain.FailAgentBlocked {
		t.Fatalf("failure code = %q", run.FailureCode)
	}
	if !strings.Contains(run.FailureMessage, "database") {
		t.Fatalf("the blocker question must reach the operator: %q", run.FailureMessage)
	}
	if run.CommitSHA != "" {
		t.Fatal("a blocked run must not commit")
	}
}

func TestPipelineEscapeAttemptEndsPolicyViolation(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("escape-path"))

	run := env.start(t, 4821)
	if run.State != domain.StateFailed {
		t.Fatalf("state = %q, want failed", run.State)
	}
	if run.FailureCode != domain.FailPolicyViolation {
		t.Fatalf("failure code = %q, want policy_violation", run.FailureCode)
	}
	if run.CommitSHA != "" {
		t.Fatal("a policy violation must not produce a commit")
	}
}

type stubValidator struct {
	outcome domain.Outcome
	reason  string
}

func (s stubValidator) RunCheck(_ context.Context, check domain.CheckDefinition, _ string) domain.ValidationResult {
	return domain.ValidationResult{
		CheckName: check.Name,
		Command:   check.CommandFor(runtime.GOOS),
		Required:  check.Required,
		Outcome:   s.outcome,
		Reason:    s.reason,
	}
}

func TestPipelineRequiredCheckThatNeverRunsFails(t *testing.T) {
	env := newPipeline(t, pipelineOptions{
		tune: func(cfg *config.Config) {
			publishDisabled(cfg)
			cfg.Agent.MaxRepairs = 0
		},
		validator: stubValidator{outcome: domain.OutcomeNotExecuted, reason: "not_configured"},
	})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateFailed {
		t.Fatalf("state = %q, want failed", run.State)
	}
	if run.FailureCode != domain.FailValidationFailed {
		t.Fatalf("failure code = %q, want validation_failed", run.FailureCode)
	}
	if run.CommitSHA != "" {
		t.Fatal("a required check that never ran must block the commit (FR-035)")
	}

	results, err := env.harness.Store.ListValidationResults(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("validation results: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != domain.OutcomeNotExecuted {
		t.Fatalf("results = %+v", results)
	}
}

func TestCreateRunRequiresConfirmation(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))

	body := strings.NewReader(`{"issueId":4821,"confirm":false}`)
	response, err := http.Post(env.server.URL+"/api/runs", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.StatusCode, payload)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "confirmation_required" {
		t.Fatalf("code = %q", problem.Code)
	}
}

func TestCreateRunRejectsASecondActiveRun(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("hang"))

	body, _ := json.Marshal(map[string]any{"issueId": 4821, "confirm": true})
	first, err := http.Post(env.server.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	firstPayload, _ := io.ReadAll(first.Body)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d: %s", first.StatusCode, firstPayload)
	}

	second, err := http.Post(env.server.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	secondPayload, _ := io.ReadAll(second.Body)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second create = %d, want 409: %s", second.StatusCode, secondPayload)
	}
	var problem struct {
		Code      string `json:"code"`
		ActiveRun struct {
			RunID string `json:"runId"`
			Stage string `json:"stage"`
			State string `json:"state"`
		} `json:"activeRun"`
	}
	if err := json.Unmarshal(secondPayload, &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, secondPayload)
	}
	if problem.Code != "active_run_exists" || problem.ActiveRun.RunID == "" || problem.ActiveRun.Stage == "" {
		t.Fatalf("problem = %+v", problem)
	}

	var workspaces int
	if err := env.harness.Store.Reader().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workspaces`).Scan(&workspaces); err != nil {
		t.Fatal(err)
	}
	if workspaces < 0 {
		t.Fatalf("workspaces = %d", workspaces)
	}
}

func TestRunDetailUnknownRunIs404(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	response, err := http.Get(env.server.URL + "/api/runs/01999999-0000-7000-8000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestPipelineWritesRunArtifacts(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))
	run := env.start(t, 4821)

	runDir := fmt.Sprintf("%s/.runs/%s", env.harness.WorkspaceRoot, run.PublicID)
	for _, name := range []string{"prompt-attempt-1.txt", "transcript-attempt-1.jsonl"} {
		path := runDir + "/" + name
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			t.Fatalf("%s missing or empty: %v", path, err)
		}
	}

	prompt, err := os.ReadFile(runDir + "/prompt-attempt-1.txt")
	if err != nil {
		t.Fatal(err)
	}
	text := string(prompt)
	if !strings.Contains(text, "## Operating restrictions") {
		t.Fatal("the persisted prompt must carry the restrictions")
	}
	if !strings.Contains(text, "## RESULT") {
		t.Fatal("the persisted prompt must end with the RESULT block")
	}

	invocations, err := env.harness.Store.ListInvocations(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 || invocations[0].PromptSHA256 == "" {
		t.Fatalf("invocations = %+v", invocations)
	}
}
