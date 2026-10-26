package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// postStart attempts to start a run and returns the raw status and body.
func postStart(t *testing.T, baseURL string, issueID int) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"issueId": issueID, "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/runs: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read start response: %v", err)
	}
	return response.StatusCode, payload
}

func countRows(t *testing.T, env *pipelineEnvironment, query string, args ...any) int {
	t.Helper()
	var count int
	if err := env.harness.Store.Reader().QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return count
}

func TestSecondStartForAnActiveIssueIsRejectedAndCreatesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real agent process")
	}

	env := newPipeline(t, pipelineOptions{
		tune: func(cfg *config.Config) {
			cfg.Agent.TimeoutMinutes = 10
		},
	})
	env.harness.AddIssue(issueWithMode("grandchild"))

	first := env.createOnly(t, 4821)
	env.waitForRunningInvocation(t, first)

	status, payload := postStart(t, env.server.URL, 4821)
	if status != http.StatusConflict {
		t.Fatalf("second start status = %d, want 409: %s", status, payload)
	}
	var problem struct {
		Code      string `json:"code"`
		Stage     string `json:"stage"`
		ActiveRun *struct {
			RunID string `json:"runId"`
			Stage string `json:"stage"`
			State string `json:"state"`
		} `json:"activeRun"`
	}
	if err := json.Unmarshal(payload, &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, payload)
	}
	if problem.Code != "active_run_exists" {
		t.Fatalf("code = %q, want active_run_exists", problem.Code)
	}
	if problem.ActiveRun == nil {
		t.Fatal("the problem must carry the blocking run so the UI can link to it")
	}
	if problem.ActiveRun.RunID != first {
		t.Fatalf("activeRun.runId = %q, want %q", problem.ActiveRun.RunID, first)
	}
	if problem.ActiveRun.Stage == "" || problem.ActiveRun.State == "" {
		t.Fatalf("activeRun is incomplete: %+v", problem.ActiveRun)
	}

	// Exactly one active run, one workspace, and one live agent process exist
	// for the issue (SC-008).
	if active := countRows(t, env,
		`SELECT COUNT(*) FROM runs WHERE issue_id = ? AND state IN ('queued','preparing','implementing','repairing','validating','publishing','reporting')`,
		4821); active != 1 {
		t.Fatalf("active runs = %d, want 1", active)
	}
	if workspaces := countRows(t, env,
		`SELECT COUNT(*) FROM workspaces w JOIN runs r ON r.id = w.run_id WHERE r.issue_id = ?`,
		4821); workspaces != 1 {
		t.Fatalf("workspaces = %d, want 1", workspaces)
	}
	if invocations := countRows(t, env,
		`SELECT COUNT(*) FROM agent_invocations i JOIN runs r ON r.id = i.run_id WHERE r.issue_id = ? AND i.status = 'running'`,
		4821); invocations != 1 {
		t.Fatalf("running agent invocations = %d, want 1", invocations)
	}

	// Nothing was pushed by either attempt.
	for _, branch := range env.harness.Origin.Branches(t) {
		if branch != "main" {
			t.Fatalf("unexpected branch on the origin: %q", branch)
		}
	}

	if status, payload := postCancel(t, env.server.URL, first); status != http.StatusAccepted {
		t.Fatalf("cancel status = %d: %s", status, payload)
	}
	run := env.waitForTerminal(t, first)
	if run.State != domain.StateCancelled {
		t.Fatalf("state = %q, want cancelled", run.State)
	}

	// Once the blocking run is terminal a new attempt is accepted again.
	env.harness.AddIssue(issueWithMode("ok"))
	status, payload = postStart(t, env.server.URL, 4821)
	if status != http.StatusCreated {
		t.Fatalf("start after terminal status = %d: %s", status, payload)
	}
}
