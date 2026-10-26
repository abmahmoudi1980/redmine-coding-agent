package runner

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// maxRepairExcerptBytes bounds the failure text handed back to the agent.
const maxRepairExcerptBytes = 8 << 10

// validateAndCommit is the independent-verification step: the worker inspects
// the diff, runs every configured check itself, repairs within the configured
// limit, and only then creates a commit (FR-031 – FR-038).
func (r *Runner) validateAndCommit(ctx context.Context, run *domain.Run, snapshot domain.IssueSnapshot, mapping domain.RepositoryMapping, workspace *domain.Workspace, invocation *domain.AgentInvocation, attempt int, log *logging.RunLog) outcome {
	publishEnabled := r.publishEnabled(mapping)

	checks := mapping.Checks
	if len(checks) == 0 && publishEnabled {
		return failureOutcome(domain.FailNoChecksConfigured,
			"This repository has no configured checks, so the work cannot be verified before publishing.",
			domain.StageChecksRunning)
	}

	for {
		paths, err := r.git.ChangedPaths(ctx, workspace.Path)
		if err != nil {
			return failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageChecksRunning)
		}
		if len(paths) == 0 {
			return r.noChanges(ctx, run, workspace, log)
		}
		r.recordChanges(ctx, run, workspace, paths, log)

		results := r.runChecks(ctx, run, checks, workspace, invocation, attempt, log)
		if requiredPassed(results) {
			break
		}

		failing := firstFailure(results)
		if attempt > r.agent.MaxRepairs {
			return r.validationFailed(ctx, run, attempt, results, log)
		}

		if out := r.advance(ctx, run, domain.StateRepairing, domain.StageChecksFinished, domain.Conditions{}, nil, log,
			fmt.Sprintf("attempt %d failed validation; repairing within the configured limit", attempt)); out != nil {
			return *out
		}

		repaired, out := r.invoke(ctx, run, snapshot, mapping, workspace, attempt+1, "repair", repairExcerpt(failing), log)
		if out != nil {
			return *out
		}
		invocation = repaired
		attempt++

		if out := r.advance(ctx, run, domain.StateValidating, domain.AgentInvokedStage(attempt), domain.Conditions{}, nil, log,
			"the worker is re-running the configured checks"); out != nil {
			return *out
		}
	}

	return r.commit(ctx, run, mapping, workspace, log, publishEnabled)
}

func (r *Runner) runChecks(ctx context.Context, run *domain.Run, checks []domain.CheckDefinition, workspace *domain.Workspace, invocation *domain.AgentInvocation, attempt int, log *logging.RunLog) []domain.ValidationResult {
	results := make([]domain.ValidationResult, 0, len(checks))
	for _, check := range checks {
		stage := domain.CheckRunningStage(check.Name)
		_ = r.stage(ctx, run, stage, domain.RunEvent{
			Type:    domain.EventCheck,
			Message: fmt.Sprintf("running %s", check.Name),
		}, log)

		result := r.runOneCheck(ctx, check, workspace.Path)
		result.RunID = run.ID
		if invocation != nil {
			id := invocation.ID
			result.InvocationID = &id
		}

		severity := domain.SeverityInfo
		if result.Outcome != domain.OutcomePassed {
			severity = domain.SeverityWarn
		}
		details := fmt.Sprintf("%s: %s", check.Name, result.Outcome)
		if result.ExitCode != nil {
			details += fmt.Sprintf(" (exit %d, %dms)", *result.ExitCode, result.DurationMS)
		}
		if result.Reason != "" {
			details += " — " + result.Reason
		}
		r.event(ctx, run, domain.RunEvent{Type: domain.EventCheck, Severity: severity, Message: details})

		if _, err := r.store.InsertValidationResult(ctx, &result); err != nil {
			r.baseLogger().Error("could not persist a validation result",
				"run_id", run.PublicID, "check", check.Name, "error", err)
		}
		results = append(results, result)
	}

	_ = r.stage(ctx, run, domain.StageChecksFinished, domain.RunEvent{
		Type:    domain.EventCheck,
		Message: fmt.Sprintf("attempt %d finished %d checks", attempt, len(results)),
	}, log)
	return results
}

func (r *Runner) runOneCheck(ctx context.Context, check domain.CheckDefinition, workspacePath string) domain.ValidationResult {
	if r.validator == nil {
		return domain.ValidationResult{
			CheckName: check.Name,
			Command:   check.CommandFor(runtime.GOOS),
			Required:  check.Required,
			Outcome:   domain.OutcomeNotExecuted,
			Reason:    "no_validator_configured",
		}
	}
	return r.validator.RunCheck(ctx, check, workspacePath)
}

func (r *Runner) noChanges(ctx context.Context, run *domain.Run, workspace *domain.Workspace, log *logging.RunLog) outcome {
	if err := r.store.UpdateWorkspaceChanges(ctx, run.ID, nil, false); err != nil && log != nil {
		log.Warn("could not record the empty change set", "error", err)
	}
	return failureOutcome(domain.FailNoChanges,
		"The agent finished without changing any file, so no commit was created.",
		domain.StageChecksFinished)
}

func (r *Runner) recordChanges(ctx context.Context, run *domain.Run, workspace *domain.Workspace, paths []string, log *logging.RunLog) {
	if strings.Join(workspace.ChangedPaths, "\x00") != strings.Join(paths, "\x00") {
		if err := r.store.UpdateWorkspaceChanges(ctx, run.ID, paths, true); err != nil && log != nil {
			log.Warn("could not record the changed paths", "error", err)
		}
		workspace.ChangedPaths = paths
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventWorkspace,
			Message:  fmt.Sprintf("%d file(s) changed: %s", len(paths), strings.Join(paths, ", ")),
			Severity: domain.SeverityInfo,
		})
	}
	summary := fmt.Sprintf("%d file(s) changed", len(paths))
	if err := r.store.UpdateRun(ctx, run.ID, store.RunPatch{Summary: &summary}); err != nil && log != nil {
		log.Warn("could not record the change summary", "error", err)
	}
}

func (r *Runner) validationFailed(ctx context.Context, run *domain.Run, attempt int, results []domain.ValidationResult, log *logging.RunLog) outcome {
	failing := firstFailure(results)
	message := fmt.Sprintf("Required validation still fails after %d attempt(s).", attempt)
	if failing != nil {
		message = fmt.Sprintf("%s failed with exit code %d after %d attempt(s).", failing.CheckName, valueOr(failing.ExitCode, -1), attempt)
		if failing.Reason != "" {
			message = fmt.Sprintf("%s did not pass (%s) after %d attempt(s).", failing.CheckName, failing.Reason, attempt)
		}
	}
	if log != nil {
		log.Warn("validation failed at the repair limit", "attempts", attempt)
	}
	return failureOutcome(domain.FailValidationFailed, message, domain.StageChecksFinished)
}

func (r *Runner) commit(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, workspace *domain.Workspace, log *logging.RunLog, publishEnabled bool) outcome {
	if err := r.git.AddAll(ctx, workspace.Path); err != nil {
		return failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageChecksFinished)
	}
	message := fmt.Sprintf("Issue #%d: %s\n\nImplemented by RedmineCodingAgent.", run.IssueID, run.IssueSubject)
	if run.IssueSubject == "" {
		message = fmt.Sprintf("Issue #%d\n\nImplemented by RedmineCodingAgent.", run.IssueID)
	}
	sha, err := r.git.Commit(ctx, workspace.Path, message)
	if err != nil {
		return failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageChecksFinished)
	}
	run.CommitSHA = sha
	if err := r.store.UpdateRun(ctx, run.ID, store.RunPatch{CommitSHA: &sha}); err != nil {
		return failureOutcome(domain.FailWorkspaceFailed, "the commit could not be recorded: "+err.Error(), domain.StageCommitCreated)
	}
	if err := r.store.UpdateWorkspaceChanges(ctx, run.ID, workspace.ChangedPaths, !publishEnabled); err != nil && log != nil {
		log.Warn("could not record the unpushed state", "error", err)
	}

	if err := r.stage(ctx, run, domain.StageCommitCreated, domain.RunEvent{
		Type:    domain.EventPublish,
		Message: "commit " + sha + " created for issue " + fmt.Sprintf("%d", run.IssueID),
	}, log); err != nil {
		return failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageCommitCreated)
	}

	if !publishEnabled {
		return outcome{State: domain.StateCompleted, Stage: domain.StageCommitCreated, Conditions: domain.Conditions{PublishingEnabled: false}}
	}

	if out := r.advance(ctx, run, domain.StatePublishing, domain.StageCommitCreated, domain.Conditions{PublishingEnabled: true}, nil, log,
		"validation passed and the commit exists; publishing begins"); out != nil {
		return *out
	}
	return outcome{}
}

// publishEnabled resolves the global and per-repository policy (FR-042).
func (r *Runner) publishEnabled(mapping domain.RepositoryMapping) bool {
	if r.config == nil {
		return false
	}
	return mapping.PublishEnabled && r.config.GitLab.PushEnabled
}

func requiredPassed(results []domain.ValidationResult) bool {
	required := 0
	for _, result := range results {
		if !result.Required {
			continue
		}
		required++
		if result.Outcome != domain.OutcomePassed {
			return false
		}
	}
	return required > 0
}

func firstFailure(results []domain.ValidationResult) *domain.ValidationResult {
	for index := range results {
		if results[index].Required && results[index].Outcome != domain.OutcomePassed {
			return &results[index]
		}
	}
	return nil
}

func repairExcerpt(failing *domain.ValidationResult) string {
	if failing == nil {
		return "Required checks did not pass."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "check: %s\n", failing.CheckName)
	fmt.Fprintf(&b, "command: %s\n", failing.Command)
	fmt.Fprintf(&b, "outcome: %s\n", failing.Outcome)
	if failing.ExitCode != nil {
		fmt.Fprintf(&b, "exit_code: %d\n", *failing.ExitCode)
	}
	if failing.Reason != "" {
		fmt.Fprintf(&b, "reason: %s\n", failing.Reason)
	}
	if failing.OutputExcerpt != "" {
		excerpt := failing.OutputExcerpt
		if len(excerpt) > maxRepairExcerptBytes {
			excerpt = excerpt[len(excerpt)-maxRepairExcerptBytes:]
		}
		b.WriteString("\n")
		b.WriteString(excerpt)
	}
	return b.String()
}

func valueOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
