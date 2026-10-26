package runner

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

type recoveryFixture struct {
	runner *Runner
	store  *store.Store
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(root, "recovery.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		SchemaVersion: 1,
		Data: config.Data{
			Database:      filepath.Join(root, "recovery.db"),
			WorkspaceRoot: filepath.Join(root, "workspaces"),
			LogDir:        filepath.Join(root, "logs"),
		},
		Agent: config.Agent{
			Adapter:        "opencode",
			Command:        "git",
			Model:          "fake/model",
			Profile:        "restricted",
			TimeoutMinutes: 1,
		},
		Concurrency: config.Concurrency{MaxActiveRuns: 2},
	}
	pipeline, err := New(Options{
		Config:   cfg,
		Store:    st,
		Git:      git.NewManager(filepath.Join(root, "repos"), cfg.Data.WorkspaceRoot),
		Adapters: agent.NewRegistry(),
		Logger:   logging.New(logging.Options{LogDir: cfg.Data.LogDir, Stderr: io.Discard}),
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return &recoveryFixture{runner: pipeline, store: st}
}

func (f *recoveryFixture) activeRun(t *testing.T, issueID int, stage domain.Stage) *domain.Run {
	t.Helper()
	ctx := context.Background()
	snapshot := &domain.IssueSnapshot{
		RedmineIssueID:    issueID,
		Subject:           "Recovered run",
		ProjectIdentifier: "billing",
		ContentHash:       "recovery-hash-" + stage.String(),
	}
	run := &domain.Run{
		PublicID: "recovery-run-" + strconv.Itoa(issueID),
		IssueID:  issueID,
		State:    domain.StateQueued,
		Stage:    domain.StageCreated,
	}
	if err := f.store.ReserveRun(ctx, snapshot, run, 5); err != nil {
		t.Fatalf("reserve run: %v", err)
	}
	for _, to := range []domain.RunState{domain.StatePreparing, domain.StateImplementing} {
		if err := f.store.TransitionRun(ctx, run.ID, store.Transition{To: to, Stage: stage}); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	run.State = domain.StateImplementing
	run.Stage = stage
	return run
}

func TestRecoverInterruptedFreezesEveryActiveRunAtItsStage(t *testing.T) {
	fixture := newRecoveryFixture(t)
	ctx := context.Background()

	implementing := fixture.activeRun(t, 4821, domain.AgentInvokedStage(1))
	validating := fixture.activeRun(t, 4822, domain.StageChecksRunning)

	terminalRun := fixture.activeRun(t, 4823, domain.AgentInvokedStage(1))
	failure := domain.NewFailure(domain.FailValidationFailed, "the unit tests failed", domain.StageChecksFinished)
	if err := fixture.store.TransitionRun(ctx, terminalRun.ID, store.Transition{
		To:      domain.StateFailed,
		Stage:   domain.StageChecksFinished,
		Failure: &failure,
	}); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}

	report, err := fixture.runner.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(report.Interrupted) != 2 {
		t.Fatalf("interrupted %d runs, want 2", len(report.Interrupted))
	}

	for _, expected := range []struct {
		run   *domain.Run
		stage domain.Stage
	}{
		{implementing, domain.AgentInvokedStage(1)},
		{validating, domain.StageChecksRunning},
	} {
		stored, err := fixture.store.GetRun(ctx, expected.run.ID)
		if err != nil {
			t.Fatalf("read recovered run: %v", err)
		}
		if stored.State != domain.StateInterrupted {
			t.Fatalf("state = %q, want interrupted", stored.State)
		}
		if stored.InterruptedStage != expected.stage {
			t.Fatalf("interrupted_stage = %q, want %q", stored.InterruptedStage, expected.stage)
		}
		if stored.EndedAt == nil {
			t.Fatal("an interrupted run must have an ended_at")
		}
		if stored.FailureCode != domain.FailInterrupted {
			t.Fatalf("failure code = %q, want interrupted", stored.FailureCode)
		}

		events, err := fixture.store.LatestEvents(ctx, stored.ID, 50)
		if err != nil {
			t.Fatalf("read events: %v", err)
		}
		found := false
		for _, event := range events {
			if strings.Contains(event.Message, "startup recovery") {
				found = true
			}
		}
		if !found {
			t.Fatal("the recovery sweep must leave a timeline event")
		}
	}

	// Terminal runs are untouched: same state, failure, and no interrupted stage.
	untouched, err := fixture.store.GetRun(ctx, terminalRun.ID)
	if err != nil {
		t.Fatalf("read terminal run: %v", err)
	}
	if untouched.State != domain.StateFailed {
		t.Fatalf("terminal state changed to %q", untouched.State)
	}
	if untouched.FailureCode != domain.FailValidationFailed {
		t.Fatalf("terminal failure changed to %q", untouched.FailureCode)
	}
	if untouched.InterruptedStage != "" {
		t.Fatalf("terminal run gained interrupted_stage %q", untouched.InterruptedStage)
	}

	// Recovery never publishes or reports: no invocation, workspace, or report
	// was fabricated for the interrupted runs.
	for _, run := range []*domain.Run{implementing, validating} {
		invocations, err := fixture.store.ListInvocations(ctx, run.ID)
		if err != nil {
			t.Fatalf("list invocations: %v", err)
		}
		if len(invocations) != 0 {
			t.Fatalf("recovery created %d invocation(s)", len(invocations))
		}
		if _, err := fixture.store.GetReport(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("recovery created a report: %v", err)
		}
	}

	// A second sweep is a no-op: interrupted is terminal.
	second, err := fixture.runner.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatalf("second recover: %v", err)
	}
	if len(second.Interrupted) != 0 {
		t.Fatalf("second sweep interrupted %d runs, want 0", len(second.Interrupted))
	}
}

func TestRecoverInterruptedOnAnEmptyDatabaseIsClean(t *testing.T) {
	fixture := newRecoveryFixture(t)
	report, err := fixture.runner.RecoverInterrupted(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(report.Interrupted) != 0 {
		t.Fatalf("interrupted = %d, want 0", len(report.Interrupted))
	}
}

func TestRecoverInterruptedRequiresAStore(t *testing.T) {
	var pipeline *Runner
	if _, err := pipeline.RecoverInterrupted(context.Background()); err == nil {
		t.Fatal("a nil runner must report an error")
	}
	if _, err := (&Runner{}).RecoverInterrupted(context.Background()); err == nil {
		t.Fatal("a runner without a store must report an error")
	}
}
