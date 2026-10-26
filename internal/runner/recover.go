package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// RecoveryReport is the outcome of the startup sweep.
type RecoveryReport struct {
	Interrupted []domain.Run
}

// RecoverInterrupted freezes every run that a previous process left active. It
// runs before the listener accepts traffic, writes only the interrupted
// terminal state, and never starts a pipeline, a push, a merge request, or a
// report (FR-051).
func (r *Runner) RecoverInterrupted(ctx context.Context) (RecoveryReport, error) {
	if r == nil || r.store == nil {
		return RecoveryReport{}, errors.New("runner: the store is not configured")
	}

	writeCtx, cancel := durableContext(ctx)
	defer cancel()

	interrupted, err := r.store.InterruptActiveRuns(writeCtx)
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("runner: startup recovery: %w", err)
	}

	if r.logger != nil {
		log := r.logger.Base()
		for _, run := range interrupted {
			log.Warn("run interrupted by startup recovery",
				"run_id", run.PublicID, "issue_id", run.IssueID, "stage", run.InterruptedStage)
		}
		log.Info("startup recovery complete", "interrupted_runs", len(interrupted))
	}
	return RecoveryReport{Interrupted: interrupted}, nil
}
