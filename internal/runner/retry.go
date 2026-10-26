package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// RetryRequest starts a new attempt for the issue of a finished run.
type RetryRequest struct {
	PublicID    string
	RequestedBy string
}

// Retry creates a new, linked run for the issue of a finished attempt. The
// previous run is never resumed, and its branch is never force-pushed: a
// suffixed branch is chosen whenever the earlier work still exists (FR-052).
func (r *Runner) Retry(ctx context.Context, req RetryRequest) (*domain.Run, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("runner: the store is not configured")
	}
	publicID := strings.TrimSpace(req.PublicID)
	if publicID == "" {
		return nil, store.ErrNotFound
	}

	previous, err := r.store.GetRunByPublicID(ctx, publicID)
	if err != nil {
		return nil, err
	}
	if !domain.IsTerminal(previous.State) {
		return nil, &ActiveRunError{Run: previous}
	}
	if r.redmine == nil {
		return nil, errors.New("runner: the Redmine client is not configured")
	}

	active, err := r.store.ActiveRunByIssue(ctx, previous.IssueID)
	if err != nil {
		return nil, fmt.Errorf("runner: active run lookup: %w", err)
	}
	if active != nil {
		return nil, &ActiveRunError{Run: active}
	}

	attempt := r.attemptNumber(ctx, previous) + 1
	branch := r.retryBranch(ctx, previous, attempt)

	var repositoryID int64
	if previous.RepositoryID != nil {
		repositoryID = *previous.RepositoryID
	}

	return r.create(ctx, CreateRequest{
		IssueID:      previous.IssueID,
		RepositoryID: repositoryID,
		Confirm:      true,
		RequestedBy:  req.RequestedBy,
	}, createOptions{previous: previous, branch: branch})
}

// retryBranch picks a branch that cannot destroy the previous attempt's work.
// A planning error falls back to the conservative suffixed name when the prior
// attempt was published.
func (r *Runner) retryBranch(ctx context.Context, previous *domain.Run, attempt int) string {
	issueBranch := strings.TrimSpace(previous.Branch)
	if issueBranch == "" {
		issueBranch = BranchName(previous.IssueID)
	}
	if previous.RepositoryID == nil || r.git == nil {
		return git.AttemptBranch(issueBranch, attempt)
	}

	baseBranch := strings.TrimSpace(previous.BaseBranch)
	if baseBranch == "" {
		if mapping, err := r.store.GetRepositoryByID(ctx, *previous.RepositoryID); err == nil {
			baseBranch = mapping.BaseBranch
		}
	}
	plan, err := r.git.PlanAttemptBranch(ctx, *previous.RepositoryID, baseBranch, issueBranch,
		attempt, previous.PushedAt != nil, previous.BaseRevision)
	if err != nil {
		// Never risk the earlier attempt when the cache cannot be inspected.
		return git.AttemptBranch(issueBranch, attempt)
	}
	return plan.Branch
}

// attemptNumber is 1 for an original run and grows along the retry chain.
func (r *Runner) attemptNumber(ctx context.Context, run *domain.Run) int {
	if r == nil || r.store == nil || run == nil {
		return 1
	}
	number := 1
	current := run
	for steps := 0; steps < 50 && current.PreviousRunID != nil; steps++ {
		previous, err := r.store.GetRun(ctx, *current.PreviousRunID)
		if err != nil {
			break
		}
		number++
		current = previous
	}
	return number
}
