package runner

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// cancellableRunner is the smallest wiring that can own a run record: the
// agent is never invoked, so no redmine, gitlab, or adapter is needed.
type cancellableRunner struct {
	runner *Runner
	store  *store.Store
	run    *domain.Run
}

func newCancellableRunner(t *testing.T, state domain.RunState, stage domain.Stage) *cancellableRunner {
	t.Helper()

	root := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(root, "cancel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		SchemaVersion: 1,
		Data: config.Data{
			Database:      filepath.Join(root, "cancel.db"),
			WorkspaceRoot: filepath.Join(root, "workspaces"),
			LogDir:        filepath.Join(root, "logs"),
		},
		Agent: config.Agent{
			Adapter:        "opencode",
			Command:        "git",
			Model:          "fake/model",
			Profile:        "restricted",
			TimeoutMinutes: 1,
			MaxRepairs:     0,
		},
		Concurrency: config.Concurrency{MaxActiveRuns: 1},
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

	run := &domain.Run{
		PublicID: "018f2e4a-0000-7000-8000-000000000001",
		IssueID:  4821,
		State:    domain.StateQueued,
		Stage:    domain.StageCreated,
	}
	snapshot := &domain.IssueSnapshot{
		RedmineIssueID:    4821,
		Subject:           "Invoice totals round incorrectly",
		ProjectIdentifier: "billing",
		ContentHash:       "fixture-hash",
	}
	if err := st.ReserveRun(context.Background(), snapshot, run, 1); err != nil {
		t.Fatalf("reserve run: %v", err)
	}
	for _, step := range pathTo(state) {
		if err := st.TransitionRun(context.Background(), run.ID, store.Transition{To: step, Stage: stage}); err != nil {
			t.Fatalf("transition to %s: %v", step, err)
		}
	}
	run.State = state
	run.Stage = stage

	return &cancellableRunner{runner: pipeline, store: st, run: run}
}

func pathTo(state domain.RunState) []domain.RunState {
	switch state {
	case domain.StateImplementing:
		return []domain.RunState{domain.StatePreparing, domain.StateImplementing}
	case domain.StateValidating:
		return []domain.RunState{domain.StatePreparing, domain.StateImplementing, domain.StateValidating}
	case domain.StateQueued:
		return nil
	default:
		return []domain.RunState{state}
	}
}

func TestCancelRequestsCancellationAndCancelsTheAgentContext(t *testing.T) {
	env := newCancellableRunner(t, domain.StateImplementing, domain.AgentInvokedStage(1))

	signalled := make(chan struct{}, 1)
	env.runner.trackCancel(env.run.PublicID, func() {
		select {
		case signalled <- struct{}{}:
		default:
		}
	})

	cancelled, changed, err := env.runner.Cancel(context.Background(), env.run.PublicID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !changed {
		t.Fatal("the first cancel must report a change")
	}
	if cancelled == nil || cancelled.CancelRequestedAt == nil {
		t.Fatalf("cancel_requested_at was not recorded: %+v", cancelled)
	}
	select {
	case <-signalled:
	default:
		t.Fatal("the running pipeline context was not cancelled")
	}

	// The machine must accept the truthful terminal state once the request is
	// durable (FR-013).
	if err := env.store.TransitionRun(context.Background(), env.run.ID, store.Transition{
		To:    domain.StateCancelled,
		Stage: domain.AgentInvokedStage(1),
	}); err != nil {
		t.Fatalf("implementing -> cancelled: %v", err)
	}
}

func TestShutdownCancelsActiveRunsAndWaits(t *testing.T) {
	env := newCancellableRunner(t, domain.StateImplementing, domain.AgentInvokedStage(1))

	signalled := make(chan struct{}, 1)
	env.runner.trackCancel(env.run.PublicID, func() {
		select {
		case signalled <- struct{}{}:
		default:
		}
	})

	if err := env.runner.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-signalled:
	default:
		t.Fatal("shutdown did not cancel the live pipeline context")
	}

	latest, err := env.store.GetRun(context.Background(), env.run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if latest.CancelRequestedAt == nil {
		t.Fatal("shutdown did not persist a cancel request")
	}
}

func TestCancelIsIdempotentAndRefusesTerminalRuns(t *testing.T) {
	env := newCancellableRunner(t, domain.StateImplementing, domain.AgentInvokedStage(1))

	if _, _, err := env.runner.Cancel(context.Background(), env.run.PublicID); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	if err := env.store.TransitionRun(context.Background(), env.run.ID, store.Transition{To: domain.StateCancelled}); err != nil {
		t.Fatalf("transition to cancelled: %v", err)
	}

	run, changed, err := env.runner.Cancel(context.Background(), env.run.PublicID)
	if err != nil {
		t.Fatalf("cancelling an already cancelled run must succeed: %v", err)
	}
	if changed {
		t.Fatal("the second cancel must not report a change")
	}
	if run.State != domain.StateCancelled {
		t.Fatalf("state = %q, want cancelled", run.State)
	}

	failed := newCancellableRunner(t, domain.StateImplementing, domain.AgentInvokedStage(1))
	if err := failed.store.TransitionRun(context.Background(), failed.run.ID, store.Transition{
		To:    domain.StateFailed,
		Stage: domain.StageAgentInvoked,
	}); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}
	if _, _, err := failed.runner.Cancel(context.Background(), failed.run.PublicID); !errors.Is(err, ErrRunNotCancellable) {
		t.Fatalf("error = %v, want ErrRunNotCancellable", err)
	}
}

func TestCancelOfAnUnknownRunReportsNotFound(t *testing.T) {
	env := newCancellableRunner(t, domain.StateImplementing, domain.AgentInvokedStage(1))
	if _, _, err := env.runner.Cancel(context.Background(), "01999999-0000-7000-8000-000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want store.ErrNotFound", err)
	}
}

func TestStopRequestedYieldsTheCancelledOutcomeBeforePublishing(t *testing.T) {
	env := newCancellableRunner(t, domain.StateValidating, domain.StageChecksFinished)

	if out := env.runner.stopRequested(context.Background(), env.run); out != nil {
		t.Fatalf("a run without a cancel request must keep going: %+v", out)
	}

	if _, err := env.store.RequestCancel(context.Background(), env.run.ID); err != nil {
		t.Fatalf("request cancel: %v", err)
	}

	out := env.runner.stopRequested(context.Background(), env.run)
	if out == nil {
		t.Fatal("a cancelled run must stop at the next stage boundary")
	}
	if out.State != domain.StateCancelled {
		t.Fatalf("state = %q, want cancelled", out.State)
	}
	if out.Failure == nil || out.Failure.Code != domain.FailCancelled {
		t.Fatalf("failure = %+v, want the cancelled code", out.Failure)
	}
	if !out.Conditions.CancelRequested {
		t.Fatal("the outcome must satisfy the cancel condition for the state machine")
	}
	if out.Failure.Stage != domain.StageChecksFinished {
		t.Fatalf("stage = %q, want the current stage", out.Failure.Stage)
	}
}
