package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
)

// crashedAttempt seeds the durable record a hard-killed worker leaves behind:
// an active run frozen mid-implementation, a workspace, and a local branch.
func (e *pipelineEnvironment) crashedAttempt(t *testing.T, issueID int) (*domain.Run, string) {
	t.Helper()
	h := e.harness
	ctx := context.Background()

	mappings, err := h.Store.ListRepositories(ctx)
	if err != nil || len(mappings) == 0 {
		t.Fatalf("list repositories: %v", err)
	}
	repositoryID := mappings[0].ID

	manager := git.NewManager(h.ReposDir, h.WorkspaceRoot)
	if _, err := manager.EnsureMirror(ctx, repositoryID, h.Origin.Path); err != nil {
		t.Fatalf("prepare mirror: %v", err)
	}
	mirror, err := manager.MirrorPath(repositoryID)
	if err != nil {
		t.Fatalf("mirror path: %v", err)
	}
	// The crashed run had already created its branch before the process died.
	gitCommand(t, mirror, "branch", "agent/issue-4821", "refs/remotes/origin/main")

	snapshot := &domain.IssueSnapshot{
		RedmineIssueID:    issueID,
		Subject:           defaultIssue().Subject,
		Description:       defaultIssue().Description,
		CriteriaSource:    "heading",
		ProjectID:         "7",
		ProjectIdentifier: "billing",
		ProjectName:       "Billing",
		ContentHash:       "crashed-snapshot",
		RawJSON:           "{}",
		FetchedAt:         time.Now().UTC(),
	}
	if _, err := h.Store.CreateIssueSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	run := &domain.Run{
		PublicID:        "crashed-run-0001",
		IssueID:         issueID,
		IssueSnapshotID: snapshot.ID,
		RepositoryID:    &repositoryID,
		State:           domain.StateImplementing,
		Stage:           domain.AgentInvokedStage(1),
		BaseBranch:      "main",
		BaseRevision:    h.Origin.InitialRevision,
		Branch:          "agent/issue-4821",
		CreatedAt:       time.Now().UTC().Add(-2 * time.Minute),
	}
	if _, err := h.Store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create crashed run: %v", err)
	}

	workspacePath := filepath.Join(h.WorkspaceRoot, "crashed-workspace")
	if err := os.MkdirAll(workspacePath, 0o750); err != nil {
		t.Fatalf("create crashed workspace dir: %v", err)
	}
	if _, err := h.Store.CreateWorkspace(ctx, &domain.Workspace{
		RunID:        run.ID,
		Path:         workspacePath,
		GitDir:       filepath.Join(workspacePath, ".git"),
		Branch:       run.Branch,
		BaseRevision: run.BaseRevision,
		CreatedAt:    run.CreatedAt,
	}); err != nil {
		t.Fatalf("create crashed workspace: %v", err)
	}
	return run, workspacePath
}

func TestRestartInterruptsTheRunAndRetryIsANewLinkedAttempt(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	ctx := context.Background()

	crashed, crashedWorkspace := env.crashedAttempt(t, 4821)

	report, err := env.runner.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	if len(report.Interrupted) != 1 {
		t.Fatalf("interrupted %d runs, want 1", len(report.Interrupted))
	}

	recovered, err := env.harness.Store.GetRun(ctx, crashed.ID)
	if err != nil {
		t.Fatalf("read recovered run: %v", err)
	}
	if recovered.State != domain.StateInterrupted {
		t.Fatalf("state = %q, want interrupted", recovered.State)
	}
	if recovered.InterruptedStage != domain.AgentInvokedStage(1) {
		t.Fatalf("interrupted_stage = %q", recovered.InterruptedStage)
	}
	if recovered.EndedAt == nil {
		t.Fatal("an interrupted run must have an ended_at")
	}
	if recovered.FailureCode != domain.FailInterrupted {
		t.Fatalf("failure code = %q, want interrupted", recovered.FailureCode)
	}

	// Recovery never pushed: the origin still has only main.
	for _, branch := range env.harness.Origin.Branches(t) {
		if branch != "main" {
			t.Fatalf("recovery pushed branch %q", branch)
		}
	}

	// The retry runs the real pipeline against the fake agent.
	env.harness.AddIssue(issueWithMode("ok"))
	response, err := http.Post(env.server.URL+"/api/runs/"+crashed.PublicID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("POST retry: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("retry status = %d: %s", response.StatusCode, payload)
	}
	var created struct {
		RunID         string `json:"runId"`
		PreviousRunID string `json:"previousRunId"`
		Branch        string `json:"branch"`
		AttemptNo     int    `json:"attemptNo"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode retry response: %v (%s)", err, payload)
	}
	if created.PreviousRunID != crashed.PublicID {
		t.Fatalf("previousRunId = %q, want %q", created.PreviousRunID, crashed.PublicID)
	}
	if created.Branch != "agent/issue-4821-r2" {
		t.Fatalf("branch = %q, want agent/issue-4821-r2", created.Branch)
	}
	if created.AttemptNo != 2 {
		t.Fatalf("attemptNo = %d, want 2", created.AttemptNo)
	}

	retried := env.waitForTerminal(t, created.RunID)
	if retried.PreviousRunID == nil || *retried.PreviousRunID != crashed.ID {
		t.Fatalf("stored previous_run_id = %v, want %d", retried.PreviousRunID, crashed.ID)
	}
	if retried.State != domain.StateCompleted {
		t.Fatalf("retried state = %q (%s)", retried.State, retried.FailureMessage)
	}
	if retried.Branch != created.Branch {
		t.Fatalf("stored branch = %q, want %q", retried.Branch, created.Branch)
	}

	workspace, err := env.harness.Store.GetWorkspaceByRun(ctx, retried.ID)
	if err != nil {
		t.Fatalf("retry workspace: %v", err)
	}
	if workspace.Path == crashedWorkspace {
		t.Fatal("the retry reused the crashed workspace")
	}
	if _, err := os.Stat(workspace.Path); err != nil {
		t.Fatalf("the retry workspace does not exist: %v", err)
	}
	if _, err := os.Stat(crashedWorkspace); err != nil {
		t.Fatalf("the crashed workspace must be preserved: %v", err)
	}
	if workspace.Branch != created.Branch {
		t.Fatalf("workspace branch = %q, want %q", workspace.Branch, created.Branch)
	}

	// The interrupted record is unchanged by the retry and still shows its stage.
	detail := getRunDocument(t, env, crashed.PublicID)
	if detail.State != string(domain.StateInterrupted) {
		t.Fatalf("recovered state after retry = %q", detail.State)
	}
	if detail.InterruptedStage != string(domain.AgentInvokedStage(1)) {
		t.Fatalf("interruptedStage = %q", detail.InterruptedStage)
	}
	if detail.AttemptNo != 1 {
		t.Fatalf("attemptNo of the original = %d, want 1", detail.AttemptNo)
	}

	// The history endpoint returns both attempts for the issue, newest first.
	response, err = http.Get(env.server.URL + "/api/runs?issueId=4821")
	if err != nil {
		t.Fatalf("GET run history: %v", err)
	}
	defer response.Body.Close()
	historyPayload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d: %s", response.StatusCode, historyPayload)
	}
	var page struct {
		Items []struct {
			RunID         string `json:"runId"`
			PreviousRunID string `json:"previousRunId"`
		} `json:"items"`
	}
	if err := json.Unmarshal(historyPayload, &page); err != nil {
		t.Fatalf("decode history: %v (%s)", err, historyPayload)
	}
	if len(page.Items) != 2 {
		t.Fatalf("history has %d items, want 2", len(page.Items))
	}
	if page.Items[0].RunID != created.RunID || page.Items[1].RunID != crashed.PublicID {
		t.Fatalf("history order = %s, %s", page.Items[0].RunID, page.Items[1].RunID)
	}
	if page.Items[0].PreviousRunID != crashed.PublicID {
		t.Fatalf("history lineage = %q", page.Items[0].PreviousRunID)
	}
}

func getRunDocument(t *testing.T, env *pipelineEnvironment, publicID string) struct {
	State            string `json:"state"`
	Stage            string `json:"stage"`
	AttemptNo        int    `json:"attemptNo"`
	InterruptedStage string `json:"interruptedStage"`
} {
	t.Helper()
	response, err := http.Get(env.server.URL + "/api/runs/" + publicID)
	if err != nil {
		t.Fatalf("GET run: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("run detail status = %d: %s", response.StatusCode, payload)
	}
	var document struct {
		State            string `json:"state"`
		Stage            string `json:"stage"`
		AttemptNo        int    `json:"attemptNo"`
		InterruptedStage string `json:"interruptedStage"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode run detail: %v (%s)", err, payload)
	}
	return document
}

func TestRetryOfAnActiveRunIsRejectedWithTheBlocker(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real agent process")
	}
	env := newPipeline(t, pipelineOptions{
		tune: func(cfg *config.Config) {
			cfg.Agent.TimeoutMinutes = 10
		},
	})
	env.harness.AddIssue(issueWithMode("grandchild"))
	active := env.createOnly(t, 4821)
	env.waitForRunningInvocation(t, active)

	response, err := http.Post(env.server.URL+"/api/runs/"+active+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("POST retry: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("retry of an active run status = %d, want 409: %s", response.StatusCode, payload)
	}
	var problem struct {
		Code      string `json:"code"`
		ActiveRun *struct {
			RunID string `json:"runId"`
		} `json:"activeRun"`
	}
	if err := json.Unmarshal(payload, &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, payload)
	}
	if problem.Code != "active_run_exists" {
		t.Fatalf("code = %q", problem.Code)
	}
	if problem.ActiveRun == nil || problem.ActiveRun.RunID != active {
		t.Fatalf("activeRun = %+v, want %s", problem.ActiveRun, active)
	}
	if status, cancelPayload := postCancel(t, env.server.URL, active); status != http.StatusAccepted {
		t.Fatalf("cancel status = %d: %s", status, cancelPayload)
	}
	env.waitForTerminal(t, active)
}

func TestRetryOfAnUnknownRunIs404(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	response, err := http.Post(env.server.URL+"/api/runs/01999999-0000-7000-8000-000000000000/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 404: %s", response.StatusCode, payload)
	}
}

func TestRunHistoryRejectsAnUnknownFilter(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	for _, path := range []string{
		"/api/runs?state=exploded",
		"/api/runs?issueId=0",
		"/api/runs?limit=1000",
		"/api/runs?cursor=not-a-cursor",
	} {
		response, err := http.Get(env.server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		payload, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", path, response.StatusCode, payload)
		}
		if !strings.Contains(string(payload), `"code"`) {
			t.Fatalf("%s did not return a problem document: %s", path, payload)
		}
	}
}
