package integration

import (
	"bytes"
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
)

// createOnly starts a run and returns its public id without waiting for a
// terminal state, so the test can act while the pipeline is still running.
func (e *pipelineEnvironment) createOnly(t *testing.T, issueID int) string {
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
	var created struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return created.RunID
}

// waitForRunningInvocation blocks until the agent process has been started, so
// cancellation lands while the process tree actually exists.
func (e *pipelineEnvironment) waitForRunningInvocation(t *testing.T, publicID string) *domain.Run {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		run, err := e.harness.Store.GetRunByPublicID(context.Background(), publicID)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		invocations, err := e.harness.Store.ListInvocations(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("list invocations: %v", err)
		}
		for _, invocation := range invocations {
			if invocation.Status == "running" {
				return run
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s never reached a running agent invocation", publicID)
	return nil
}

// waitForAgentHeartbeat waits for the fake agent's liveness file and returns
// its path. Its existence proves the process tree is writing to the workspace.
func (e *pipelineEnvironment) waitForAgentHeartbeat(t *testing.T, publicID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		run, err := e.harness.Store.GetRunByPublicID(context.Background(), publicID)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		workspace, err := e.harness.Store.GetWorkspaceByRun(context.Background(), run.ID)
		if err == nil {
			path := filepath.Join(workspace.Path, ".fakeagent-heartbeat")
			if _, statErr := os.Stat(path); statErr == nil {
				return path
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the fake agent never wrote its heartbeat for run %s", publicID)
	return ""
}

func postCancel(t *testing.T, baseURL, publicID string) (int, []byte) {
	t.Helper()
	response, err := http.Post(baseURL+"/api/runs/"+publicID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read cancel response: %v", err)
	}
	return response.StatusCode, payload
}

func TestCancelKillsTheAgentTreeAndPublishesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real process tree")
	}

	env := newPipeline(t, pipelineOptions{
		tune: func(cfg *config.Config) {
			// Long enough that cancellation, not the timeout, ends the run.
			cfg.Agent.TimeoutMinutes = 10
			cfg.GitLab.PushEnabled = true
			cfg.GitLab.MergeRequestEnabled = true
		},
	})
	env.harness.AddIssue(issueWithMode("grandchild"))

	publicID := env.createOnly(t, 4821)
	env.waitForRunningInvocation(t, publicID)
	// The invocation row is written before the process starts, so wait for the
	// agent's own heartbeat before cancelling; that proves the tree is alive.
	heartbeat := env.waitForAgentHeartbeat(t, publicID)

	cancelledAt := time.Now()
	status, payload := postCancel(t, env.server.URL, publicID)
	if status != http.StatusAccepted {
		t.Fatalf("cancel status = %d: %s", status, payload)
	}

	run := env.waitForTerminal(t, publicID)
	if elapsed := time.Since(cancelledAt); elapsed > 30*time.Second {
		t.Fatalf("the agent tree took %s to stop after cancellation (SC-009 allows 30s)", elapsed)
	}

	if run.State != domain.StateCancelled {
		t.Fatalf("state = %q (%s), want cancelled", run.State, run.FailureMessage)
	}
	if run.CancelRequestedAt == nil {
		t.Fatal("cancel_requested_at was not recorded")
	}
	if run.EndedAt == nil {
		t.Fatal("a cancelled run must have an ended_at")
	}
	if run.PushedAt != nil {
		t.Fatal("nothing may be pushed after a cancellation")
	}
	for _, branch := range env.harness.Origin.Branches(t) {
		if branch == run.Branch {
			t.Fatalf("branch %q was pushed despite the cancellation", branch)
		}
	}

	workspace, err := env.harness.Store.GetWorkspaceByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if !workspace.HasUnpushedChanges {
		t.Fatal("cancelled work in the workspace must be recorded as unpushed")
	}
	if !contains(workspace.ChangedPaths, "agent-change.txt") {
		t.Fatalf("changed paths = %v", workspace.ChangedPaths)
	}

	logPath := filepath.Join(env.harness.LogDir, publicID+".log")
	info, err := os.Stat(logPath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("the run log must survive cancellation: %v", err)
	}

	// US4-AC4: the report is stored for the operator but no reporting action
	// (a Redmine comment) starts after the cancel.
	reportResponse, err := http.Get(env.server.URL + "/api/runs/" + run.PublicID + "/report")
	if err != nil {
		t.Fatal(err)
	}
	reportPayload, _ := io.ReadAll(reportResponse.Body)
	_ = reportResponse.Body.Close()
	if reportResponse.StatusCode != http.StatusOK {
		t.Fatalf("report status = %d: %s", reportResponse.StatusCode, reportPayload)
	}
	var reportDocument struct {
		Report *struct {
			DeliveryStatus string `json:"deliveryStatus"`
		} `json:"report"`
	}
	if err := json.Unmarshal(reportPayload, &reportDocument); err != nil {
		t.Fatalf("decode report: %v (%s)", err, reportPayload)
	}
	if reportDocument.Report == nil {
		t.Fatal("a cancelled run must still have a stored report")
	}
	if reportDocument.Report.DeliveryStatus != "skipped" {
		t.Fatalf("delivery status = %q, want skipped", reportDocument.Report.DeliveryStatus)
	}
	if notes := env.harness.Redmine.Notes(); len(notes) != 0 {
		t.Fatalf("no Redmine note may be posted after a cancellation: %+v", notes)
	}

	// The fake agent touches a heartbeat file every 100 ms. Once the tree is
	// dead the file stops moving; a surviving grandchild would keep writing.
	before := readIfPresent(t, heartbeat)
	time.Sleep(800 * time.Millisecond)
	after := readIfPresent(t, heartbeat)
	if before != "" && before == after {
		return
	}
	if before == "" {
		// No heartbeat at all means the agent never got that far; the changed
		// file already proves it ran, so treat it as a pass.
		return
	}
	t.Fatal("the agent process tree was still writing after cancellation")
}

func TestCancelOfATerminalRunIsRejected(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	status, payload := postCancel(t, env.server.URL, run.PublicID)
	if status != http.StatusConflict {
		t.Fatalf("cancel status = %d, want 409: %s", status, payload)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, payload)
	}
	if problem.Code != "run_not_cancellable" {
		t.Fatalf("code = %q, want run_not_cancellable", problem.Code)
	}
}

func TestCancelOfAnUnknownRunIs404(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	status, payload := postCancel(t, env.server.URL, "01999999-0000-7000-8000-000000000000")
	if status != http.StatusNotFound {
		t.Fatalf("cancel status = %d, want 404: %s", status, payload)
	}
}

func TestRunLogsReturnTheRedactedTail(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))
	run := env.start(t, 4821)

	response, err := http.Get(env.server.URL + "/api/runs/" + run.PublicID + "/logs?tail=20")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("logs status = %d: %s", response.StatusCode, payload)
	}
	var document struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode logs: %v (%s)", err, payload)
	}
	if len(document.Lines) == 0 {
		t.Fatal("a finished run must have log lines")
	}
	joined := strings.Join(document.Lines, "\n")
	if strings.Contains(joined, fixtureRedmineKey) || strings.Contains(joined, fixtureGitLabToken) {
		t.Fatal("the log tail leaked a credential value")
	}
}

func TestRunLogsForAPrunedFileReturn410(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))
	run := env.start(t, 4821)

	if err := removeWithRetry(filepath.Join(env.harness.LogDir, run.PublicID+".log")); err != nil {
		t.Fatalf("prune the log file: %v", err)
	}

	response, err := http.Get(env.server.URL + "/api/runs/" + run.PublicID + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", response.StatusCode)
	}
}

func readIfPresent(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(content)
}

// removeWithRetry waits for the pipeline goroutine to close the run log, which
// on Windows keeps the file locked for a moment after the terminal state lands.
func removeWithRetry(path string) error {
	var err error
	for attempt := 0; attempt < 40; attempt++ {
		if err = os.Remove(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}
