package runner

import (
	"context"
	"errors"
	"fmt"
)

// Shutdown cancels every in-flight run this process owns and waits for the
// pipelines to settle so terminal state (and the workspace's unpushed facts)
// is durable before the process exits (T136, SC-009).
func (r *Runner) Shutdown(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}

	runs, err := r.store.ListActiveRuns(ctx)
	if err != nil {
		return fmt.Errorf("runner: list active runs: %w", err)
	}

	var cancelErrs []error
	for index := range runs {
		run := runs[index]
		if _, _, err := r.Cancel(ctx, run.PublicID); err != nil && !errors.Is(err, ErrRunNotCancellable) {
			cancelErrs = append(cancelErrs, fmt.Errorf("%s: %w", run.PublicID, err))
		}
	}

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		cancelErrs = append(cancelErrs, ctx.Err())
	}
	if len(cancelErrs) > 0 {
		return errors.Join(cancelErrs...)
	}
	// A cancel that arrived before a run reached its first stage boundary is
	// settled by the wait above; the state itself is written by the pipeline.
	return nil
}
