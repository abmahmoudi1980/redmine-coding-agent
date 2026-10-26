package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// CleanupOptions selects what manual retention removes.
type CleanupOptions struct {
	// OlderThan is the age window measured from a run's created_at.
	OlderThan time.Duration
	// Now is injectable for tests; zero means time.Now().UTC().
	Now time.Time
	// DryRun reports what would be deleted without touching disk or the database.
	DryRun bool
	// Limit bounds one invocation.
	Limit int
}

// CleanupSkip explains why one eligible run was kept.
type CleanupSkip struct {
	RunID  string `json:"runId"`
	Reason string `json:"reason"`
}

// CleanupReport is the machine-readable result of `prune`.
type CleanupReport struct {
	OlderThan  string        `json:"olderThan"`
	Candidates int           `json:"candidates"`
	Deleted    int           `json:"deleted"`
	Skipped    []CleanupSkip `json:"skipped"`
}

// Prune implements manual retention (FR-053, FR-054). It never deletes a
// workspace with unpushed changes, and it never deletes a branch or merge
// request: those belong to the review a human still has to do.
func (r *Runner) Prune(ctx context.Context, opts CleanupOptions) (CleanupReport, error) {
	report := CleanupReport{
		OlderThan: opts.OlderThan.String(),
		Skipped:   []CleanupSkip{},
	}
	if r == nil || r.store == nil {
		return report, errors.New("runner: the store is not configured")
	}
	if opts.OlderThan <= 0 {
		return report, errors.New("runner: retention window must be positive")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 500
	}

	runs, err := r.store.ListPrunableRuns(ctx, now.Add(-opts.OlderThan), limit)
	if err != nil {
		return report, err
	}
	report.Candidates = len(runs)

	for index := range runs {
		run := runs[index]
		if opts.DryRun {
			continue
		}
		reason, err := r.pruneRun(ctx, &run)
		if err != nil {
			return report, err
		}
		if reason != "" {
			report.Skipped = append(report.Skipped, CleanupSkip{RunID: run.PublicID, Reason: reason})
			r.baseLogger().Warn("retention skipped a run", "run_id", run.PublicID, "reason", reason)
			continue
		}
		report.Deleted++
		r.baseLogger().Info("retention deleted a run", "run_id", run.PublicID, "issue_id", run.IssueID)
	}
	return report, nil
}

// pruneRun removes one run's workspace, artifacts, log, and row. A non-empty
// reason means the run was kept on purpose.
func (r *Runner) pruneRun(ctx context.Context, run *domain.Run) (string, error) {
	if run == nil {
		return "", nil
	}
	workspace, err := r.store.GetWorkspaceByRun(ctx, run.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("runner: read workspace for %s: %w", run.PublicID, err)
	}
	if workspace != nil {
		if workspace.HasUnpushedChanges {
			return "has_unpushed_changes", nil
		}
		if run.RepositoryID == nil {
			if err := removeWithin(r.config.Data.WorkspaceRoot, workspace.Path); err != nil {
				return "workspace_cleanup_failed: " + err.Error(), nil
			}
		} else if err := r.git.CleanupWorktree(ctx, *run.RepositoryID, workspace.Path, workspace.BaseRevision); err != nil {
			switch {
			case errors.Is(err, git.ErrDirtyWorkspace):
				return "dirty_workspace", nil
			case errors.Is(err, git.ErrUnpushedChanges):
				return "has_unpushed_changes", nil
			default:
				return "workspace_cleanup_failed: " + err.Error(), nil
			}
		}
	}

	artifacts := filepath.Join(r.config.Data.WorkspaceRoot, ".runs", run.PublicID)
	if err := removeWithin(r.config.Data.WorkspaceRoot, artifacts); err != nil {
		return "artifact_cleanup_failed: " + err.Error(), nil
	}

	if r.logger != nil {
		if path := r.logger.RunLogPath(run.PublicID); path != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "log_cleanup_failed: " + err.Error(), nil
			}
		}
	}

	if err := r.store.DeleteRun(ctx, run.ID); err != nil {
		return "", err
	}
	return "", nil
}

// removeWithin deletes a path only when it is inside the configured root. It
// restores write permission first: an older run may still carry the read-only
// sandbox policy directory, which blocks removal even for its owner.
func removeWithin(root, path string) error {
	if path == "" {
		return nil
	}
	if err := git.EnsureWithin(root, path); err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_ = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			// #nosec G302 G122 -- retention must restore owner write before removal; the walk is scoped to the root.
			_ = os.Chmod(current, 0o700)
		} else {
			// #nosec G122 -- the walk is scoped inside the configured workspace root.
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
