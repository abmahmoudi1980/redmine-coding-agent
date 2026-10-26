package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// publishResult records exactly what the publish phase did, so the report can
// state skipped steps honestly instead of implying they happened (FR-042).
type publishResult struct {
	Pushed              bool
	MergeRequestURL     string
	MergeRequestSkipped bool
}

// publishAndReport is the tail of every validated run: push the dedicated
// branch, open or reuse the merge request, then write the completion report.
// Nothing here can merge or write a base branch (FR-039 – FR-044).
func (r *Runner) publishAndReport(ctx context.Context, run *domain.Run, snapshot domain.IssueSnapshot, mapping domain.RepositoryMapping, workspace *domain.Workspace, log *logging.RunLog) outcome {
	result, failure, cancelled := r.publishSteps(ctx, run, mapping, workspace, log)

	terminal := domain.StateCompleted
	switch {
	case cancelled:
		terminal = domain.StateCancelled
	case failure != nil:
		terminal = failure.Code.TerminalState()
	}

	if !cancelled {
		stage := domain.StageCommitCreated
		if result.Pushed {
			stage = domain.StageBranchPushed
		}
		if out := r.advance(ctx, run, domain.StateReporting, stage, domain.Conditions{}, nil, log,
			"the completion report is being written"); out != nil {
			r.persistReport(ctx, run, snapshot, workspace, result, out.State, out.Failure, log)
			return *out
		}
	}

	r.persistReport(ctx, run, snapshot, workspace, result, terminal, failure, log)

	if cancelled {
		return cancelledOutcome(run.Stage)
	}
	if failure != nil {
		return outcome{State: terminal, Stage: domain.StageReportWritten, Failure: failure}
	}
	return outcome{State: domain.StateCompleted, Stage: domain.StageReportWritten}
}

// publishSteps performs the irreversible actions in order. A returned failure
// never undoes an action that already completed.
func (r *Runner) publishSteps(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, workspace *domain.Workspace, log *logging.RunLog) (publishResult, *domain.Failure, bool) {
	result := publishResult{}

	if r.cancelRequested(ctx, run) {
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventPublish,
			Severity: domain.SeverityWarn,
			Message:  "the run was cancelled before publishing; nothing was pushed",
		})
		return result, nil, true
	}

	if err := r.pushBranch(ctx, run, workspace); err != nil {
		failure := domain.PushRejectedFailure(err)
		failure.Stage = domain.StageCommitCreated
		// The commit stays local and the workspace records that it is unpushed,
		// so cleanup can never discard the work (FR-039, FR-053).
		_ = r.store.UpdateWorkspaceChanges(ctx, run.ID, workspace.ChangedPaths, true)
		workspace.HasUnpushedChanges = true
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventPublish,
			Severity: domain.SeverityError,
			Message:  "the branch was not pushed: " + failure.Message,
		})
		return result, &failure, false
	}

	now := time.Now().UTC()
	result.Pushed = true
	run.PushedAt = &now
	if err := r.store.UpdateRun(ctx, run.ID, store.RunPatch{PushedAt: &now}); err != nil {
		if log != nil {
			log.Warn("could not record pushed_at", "error", err)
		}
	}
	_ = r.store.UpdateWorkspaceChanges(ctx, run.ID, workspace.ChangedPaths, false)
	workspace.HasUnpushedChanges = false
	_ = r.stage(ctx, run, domain.StageBranchPushed, domain.RunEvent{
		Type:    domain.EventPublish,
		Message: fmt.Sprintf("branch %s pushed to origin", run.Branch),
	}, log)

	// US4-AC4: once the operator cancels, no further publishing step starts.
	if r.cancelRequested(ctx, run) {
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventPublish,
			Severity: domain.SeverityWarn,
			Message:  "the run was cancelled after the push; no merge request was opened",
		})
		return result, nil, true
	}

	if !r.mergeRequestEnabled(mapping) {
		result.MergeRequestSkipped = true
		r.event(ctx, run, domain.RunEvent{
			Type:    domain.EventPublish,
			Message: "merge request creation is disabled for this repository; the branch is pushed and the run continues",
		})
		return result, nil, false
	}

	url, err := r.ensureMergeRequest(ctx, run, mapping, workspace)
	if err != nil {
		failure := domain.MergeRequestFailure(err)
		failure.Stage = domain.StageBranchPushed
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventPublish,
			Severity: domain.SeverityError,
			Message:  "the merge request could not be opened: " + failure.Message,
		})
		return result, &failure, false
	}
	result.MergeRequestURL = url
	return result, nil, false
}

func (r *Runner) pushBranch(ctx context.Context, run *domain.Run, workspace *domain.Workspace) error {
	if workspace == nil || strings.TrimSpace(workspace.Path) == "" {
		return errors.New("the run has no workspace to push from")
	}
	if strings.TrimSpace(run.CommitSHA) == "" {
		return errors.New("no commit exists, so there is nothing to push")
	}
	if strings.TrimSpace(run.Branch) == "" {
		return errors.New("the run has no branch to push")
	}
	r.configureGitAuth()
	return r.git.Push(ctx, workspace.Path, run.Branch)
}

// mergeRequestEnabled resolves the global and per-repository policy (FR-042).
func (r *Runner) mergeRequestEnabled(mapping domain.RepositoryMapping) bool {
	if r.config == nil || r.gitlab == nil {
		return false
	}
	return mapping.MergeRequestEnabled && r.config.GitLab.MergeRequestEnabled
}

// cancelRequested reads the operator's cancel request from the durable record,
// so a cancel that arrived between stages is never missed (FR-012). The read is
// detached from the run context: after a cancel the context is already dead.
func (r *Runner) cancelRequested(ctx context.Context, run *domain.Run) bool {
	if run.CancelRequestedAt != nil {
		return true
	}
	if r.store == nil {
		return false
	}
	readCtx, cancel := durableContext(ctx)
	defer cancel()
	latest, err := r.store.GetRun(readCtx, run.ID)
	if err != nil || latest == nil || latest.CancelRequestedAt == nil {
		return false
	}
	run.CancelRequestedAt = latest.CancelRequestedAt
	return true
}

// ensureMergeRequest creates the merge request or refreshes the existing one
// for the same branch pair, so repeated publishing never duplicates it
// (FR-043).
func (r *Runner) ensureMergeRequest(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, workspace *domain.Workspace) (string, error) {
	title := mergeRequestTitle(run)
	description := r.mergeRequestDescription(ctx, run, mapping, workspace)

	existing, err := r.gitlab.FindOpenMergeRequest(ctx, mapping.RepositoryPath, run.Branch, mapping.BaseBranch)
	if err != nil {
		return "", err
	}
	if existing != nil {
		updated, err := r.gitlab.UpdateMergeRequest(ctx, mapping.RepositoryPath, existing.IID, mergeRequestInput(run, mapping, title, description))
		if err != nil {
			return "", err
		}
		url := updated.WebURL
		if url == "" {
			url = existing.WebURL
		}
		_ = r.stage(ctx, run, domain.StageMergeRequestReused, domain.RunEvent{
			Type:    domain.EventPublish,
			Message: fmt.Sprintf("merge request !%d already existed and was refreshed", existing.IID),
		}, nil)
		return url, nil
	}

	created, err := r.gitlab.CreateMergeRequest(ctx, mapping.RepositoryPath, mergeRequestInput(run, mapping, title, description))
	if err != nil {
		return "", err
	}
	_ = r.stage(ctx, run, domain.StageMergeRequestCreated, domain.RunEvent{
		Type:    domain.EventPublish,
		Message: "merge request " + created.WebURL + " opened against " + mapping.BaseBranch,
	}, nil)
	return created.WebURL, nil
}

func mergeRequestInput(run *domain.Run, mapping domain.RepositoryMapping, title, description string) gitlab.MergeRequestInput {
	return gitlab.MergeRequestInput{
		SourceBranch: run.Branch,
		TargetBranch: mapping.BaseBranch,
		Title:        title,
		Description:  description,
	}
}

func mergeRequestTitle(run *domain.Run) string {
	subject := strings.TrimSpace(run.IssueSubject)
	if subject == "" {
		return fmt.Sprintf("Issue #%d", run.IssueID)
	}
	return fmt.Sprintf("Issue #%d: %s", run.IssueID, subject)
}

// mergeRequestDescription renders the fixed section order required by FR-040.
func (r *Runner) mergeRequestDescription(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, workspace *domain.Workspace) string {
	checks, err := r.store.ListValidationResults(ctx, run.ID)
	if err != nil {
		checks = nil
	}
	invocations, err := r.store.ListInvocations(ctx, run.ID)
	if err != nil {
		invocations = nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Issue\n\n#%d — %s\n\n", run.IssueID, valueOrLabel(run.IssueSubject, "(no subject)"))

	b.WriteString("## Implementation summary\n\n")
	var changed []string
	if workspace != nil {
		changed = workspace.ChangedPaths
	}
	switch {
	case len(changed) > 0:
		fmt.Fprintf(&b, "The agent changed %d file(s) on `%s`. ", len(changed), run.Branch)
	case run.Summary != "":
		fmt.Fprintf(&b, "%s. ", run.Summary)
	default:
		fmt.Fprintf(&b, "The agent produced a change on `%s`. ", run.Branch)
	}
	b.WriteString("The worker itself executed every validation command listed below; the agent's own claims are recorded and never treated as results.\n\n")

	b.WriteString("## Notable changed files\n\n")
	if len(changed) == 0 {
		b.WriteString("- (the change set was not recorded)\n")
	} else {
		limit := len(changed)
		if limit > maxMergeRequestFiles {
			limit = maxMergeRequestFiles
		}
		for _, path := range changed[:limit] {
			fmt.Fprintf(&b, "- `%s`\n", path)
		}
		if len(changed) > limit {
			fmt.Fprintf(&b, "- … and %d more\n", len(changed)-limit)
		}
	}

	b.WriteString("\n## Validation\n\n")
	if len(checks) == 0 {
		b.WriteString("No check result was recorded.\n")
	} else {
		b.WriteString("| Check | Required | Command | Outcome | Exit |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, check := range checks {
			required := "no"
			if check.Required {
				required = "yes"
			}
			exit := "—"
			if check.ExitCode != nil {
				exit = fmt.Sprintf("%d", *check.ExitCode)
			}
			outcome := string(check.Outcome)
			if check.Reason != "" {
				outcome += " (" + check.Reason + ")"
			}
			fmt.Fprintf(&b, "| %s | %s | `%s` | %s | %s |\n", check.CheckName, required, check.Command, outcome, exit)
		}
	}

	b.WriteString("\n## Known limitations\n\n")
	optionalFailures := optionalFailures(checks)
	if len(optionalFailures) > 0 {
		fmt.Fprintf(&b, "- Optional check(s) did not pass: %s.\n", strings.Join(optionalFailures, ", "))
	}
	if len(invocations) > 1 {
		fmt.Fprintf(&b, "- The change needed %d agent attempts; the first attempt's output is in the run transcript.\n", len(invocations))
	}
	b.WriteString("- No human has reviewed the generated diff yet.\n")

	b.WriteString("\n## Manual follow-up\n\n")
	fmt.Fprintf(&b, "- Review the diff, then approve and merge this merge request into `%s` when satisfied.\n", mapping.BaseBranch)
	b.WriteString("- RedmineCodingAgent never merges and never writes to the base branch; approval and merging stay with a human.\n")
	b.WriteString("- Open the run in the worker UI for the full check output, transcript, and timeline.\n")
	return b.String()
}

const maxMergeRequestFiles = 20

func optionalFailures(checks []domain.ValidationResult) []string {
	var failed []string
	for _, check := range checks {
		if check.Required || check.Outcome == domain.OutcomePassed {
			continue
		}
		failed = append(failed, check.CheckName)
	}
	return failed
}
