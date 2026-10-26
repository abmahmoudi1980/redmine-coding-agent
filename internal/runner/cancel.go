package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// ErrRunNotCancellable reports that a cancel arrived after the run finished in
// a terminal state other than cancelled.
var ErrRunNotCancellable = errors.New("runner: the run has already finished")

// durableWriteTimeout bounds audit writes that must survive cancellation.
const durableWriteTimeout = 30 * time.Second

// durableContext detaches a store write from the run's context so the audit
// trail is still recorded after the operator stops the run (Principle III).
func durableContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), durableWriteTimeout)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), durableWriteTimeout)
}

// trackCancel registers the cancel function of a live pipeline and returns the
// deregistration hook.
func (r *Runner) trackCancel(publicID string, cancel context.CancelFunc) func() {
	if r == nil || strings.TrimSpace(publicID) == "" || cancel == nil {
		return func() {}
	}
	r.cancelMu.Lock()
	if r.cancels == nil {
		r.cancels = make(map[string]context.CancelFunc)
	}
	r.cancels[publicID] = cancel
	r.cancelMu.Unlock()

	return func() {
		r.cancelMu.Lock()
		defer r.cancelMu.Unlock()
		delete(r.cancels, publicID)
	}
}

func (r *Runner) cancelFor(publicID string) context.CancelFunc {
	if r == nil {
		return nil
	}
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	return r.cancels[publicID]
}

// Cancel records the operator's request, cancels the live pipeline context, and
// reports whether anything changed. Cancelling an already-cancelled run is
// idempotent; cancelling any other terminal run is refused (FR-012, FR-013).
func (r *Runner) Cancel(ctx context.Context, publicID string) (*domain.Run, bool, error) {
	if r == nil || r.store == nil {
		return nil, false, errors.New("runner: the store is not configured")
	}
	publicID = strings.TrimSpace(publicID)
	if publicID == "" {
		return nil, false, store.ErrNotFound
	}

	run, err := r.store.GetRunByPublicID(ctx, publicID)
	if err != nil {
		return nil, false, err
	}
	if domain.IsTerminal(run.State) {
		if run.State == domain.StateCancelled {
			return run, false, nil
		}
		return run, false, fmt.Errorf("%w: state %s", ErrRunNotCancellable, run.State)
	}

	changed, err := r.store.RequestCancel(ctx, run.ID)
	if err != nil {
		if errors.Is(err, store.ErrRunTerminal) {
			return r.settleCancelRace(ctx, publicID)
		}
		return nil, false, err
	}

	if cancel := r.cancelFor(publicID); cancel != nil {
		cancel()
	}

	latest, err := r.store.GetRunByPublicID(ctx, publicID)
	if err != nil {
		return run, changed, nil
	}
	return latest, changed, nil
}

// settleCancelRace resolves the window where the run finished between the read
// and the write: a cancelled run is still an idempotent success.
func (r *Runner) settleCancelRace(ctx context.Context, publicID string) (*domain.Run, bool, error) {
	latest, err := r.store.GetRunByPublicID(ctx, publicID)
	if err != nil {
		return nil, false, ErrRunNotCancellable
	}
	if latest.State == domain.StateCancelled {
		return latest, false, nil
	}
	return latest, false, fmt.Errorf("%w: state %s", ErrRunNotCancellable, latest.State)
}

// stopRequested is the cancellation gate every stage boundary calls. A non-nil
// result means the pipeline must stop now, with the workspace's unpushed work
// recorded first (US4-AC4, FR-053).
func (r *Runner) stopRequested(ctx context.Context, run *domain.Run) *outcome {
	if run == nil || !r.cancelRequested(ctx, run) {
		return nil
	}
	r.noteCancelledWork(ctx, run)
	cancelled := cancelledOutcome(run.Stage)
	return &cancelled
}

// cancelledOutcome builds the terminal cancelled result for the current stage.
func cancelledOutcome(stage domain.Stage) outcome {
	failure := domain.NewFailure(domain.FailCancelled,
		"The operator cancelled this run; nothing further was published.", stage)
	return outcome{
		State:      domain.StateCancelled,
		Stage:      stage,
		Failure:    &failure,
		Conditions: domain.Conditions{CancelRequested: true},
	}
}

// noteCancelledWork records the changed files a stopped run left behind, so
// cleanup refuses to delete work that was never published (FR-053).
func (r *Runner) noteCancelledWork(ctx context.Context, run *domain.Run) {
	if r.git == nil || r.store == nil || run == nil {
		return
	}
	writeCtx, cancel := durableContext(ctx)
	defer cancel()

	workspace, err := r.store.GetWorkspaceByRun(writeCtx, run.ID)
	if err != nil || workspace == nil || strings.TrimSpace(workspace.Path) == "" {
		return
	}
	paths, err := r.git.ChangedPaths(writeCtx, workspace.Path)
	if err != nil || len(paths) == 0 {
		return
	}
	if err := r.store.UpdateWorkspaceChanges(writeCtx, run.ID, paths, true); err != nil {
		return
	}
	r.event(writeCtx, run, domain.RunEvent{
		Type:     domain.EventWorkspace,
		Severity: domain.SeverityWarn,
		Message: fmt.Sprintf(
			"the cancelled run left %d changed file(s) in the workspace; cleanup will refuse until they are handled",
			len(paths)),
	})
}
