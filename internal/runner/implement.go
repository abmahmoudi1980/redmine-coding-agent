package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// maxActivityEventsPerAttempt bounds how much agent chatter reaches the
// timeline; the transcript remains the full record.
const maxActivityEventsPerAttempt = 50

// invoke runs one implementation or repair attempt and records everything the
// contract requires: prompt, hash, transcript, exit facts, and claims (FR-023,
// FR-027).
func (r *Runner) invoke(ctx context.Context, run *domain.Run, snapshot domain.IssueSnapshot, mapping domain.RepositoryMapping, workspace *domain.Workspace, attempt int, kind, priorFailure string, log *logging.RunLog) (*domain.AgentInvocation, *outcome) {
	runDir, err := r.prepareRunDir(run.PublicID)
	if err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), run.Stage)
		return nil, &out
	}
	sandboxDir := filepath.Join(runDir, "sandbox")
	attachmentsDir := filepath.Join(runDir, "attachments")
	if info, statErr := os.Stat(attachmentsDir); statErr != nil || !info.IsDir() {
		attachmentsDir = ""
	}
	transcriptPath := filepath.Join(runDir, fmt.Sprintf("transcript-attempt-%d.jsonl", attempt))
	promptPath := filepath.Join(runDir, fmt.Sprintf("prompt-attempt-%d.txt", attempt))

	text := agent.BuildPrompt(agent.PromptInput{
		IssueID:            run.IssueID,
		IssueSubject:       snapshot.Subject,
		Description:        snapshot.Description,
		AcceptanceCriteria: snapshot.AcceptanceCriteria,
		CriteriaSource:     snapshot.CriteriaSource,
		ProjectIdentifier:  snapshot.ProjectIdentifier,
		ProjectName:        snapshot.ProjectName,
		Status:             snapshot.StatusName,
		Tracker:            snapshot.TrackerName,
		Priority:           snapshot.PriorityName,
		Assignee:           snapshot.AssigneeName,
		Comments:           snapshot.SelectedComments,
		Attachments:        snapshot.Attachments,
		AttachmentsDir:     attachmentsDir,
		RepositoryPath:     mapping.RepositoryPath,
		BaseBranch:         run.BaseBranch,
		Branch:             run.Branch,
		WorkspaceDir:       workspace.Path,
		Checks:             mapping.Checks,
		Goos:               runtime.GOOS,
		Instructions:       agent.DiscoverInstructions(workspace.Path),
		Attempt:            attempt,
		PriorFailure:       priorFailure,
		Redact:             r.redact,
	})
	if err := os.WriteFile(promptPath, []byte(text), 0o600); err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), run.Stage)
		return nil, &out
	}

	startedAt := time.Now().UTC()
	invocation := &domain.AgentInvocation{
		RunID:          run.ID,
		AttemptNo:      attempt,
		Kind:           kind,
		Adapter:        r.agent.Adapter,
		Model:          r.agent.Model,
		ConfigID:       r.agent.ConfigID,
		PromptPath:     promptPath,
		PromptSHA256:   agent.HashPrompt(text),
		TranscriptPath: transcriptPath,
		StartedAt:      &startedAt,
		Status:         "running",
	}
	if _, err := r.store.CreateInvocation(ctx, invocation); err != nil {
		out := failureOutcome(domain.FailAgentError, err.Error(), run.Stage)
		return nil, &out
	}

	if err := r.stage(ctx, run, domain.AgentInvokedStage(attempt), domain.RunEvent{
		Type:    domain.EventAgent,
		Message: fmt.Sprintf("%s attempt %d started with %s", kind, attempt, r.agent.Adapter),
	}, log); err != nil {
		out := failureOutcome(domain.FailAgentError, err.Error(), run.Stage)
		return invocation, &out
	}

	adapter, err := r.adapters.Lookup(r.agent.Adapter)
	if err != nil {
		out := failureOutcome(domain.FailAgentUnavailable, err.Error(), domain.AgentInvokedStage(attempt))
		r.finishInvocation(ctx, invocation, "failed", nil, false, "", "")
		return invocation, &out
	}

	layout, err := agent.PrepareSandbox(sandboxDir)
	if err != nil {
		r.finishInvocation(ctx, invocation, "failed", nil, false, "", "")
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.AgentInvokedStage(attempt))
		return invocation, &out
	}
	policyPath, err := agent.WritePolicy(layout, agent.DefaultPolicy(workspace.Path, sandboxDir, filepath.Dir(r.config.Data.Database)))
	if err != nil {
		r.finishInvocation(ctx, invocation, "failed", nil, false, "", "")
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.AgentInvokedStage(attempt))
		return invocation, &out
	}
	environment, err := agent.EnvironmentAllowlist(runtime.GOOS)
	if err != nil {
		r.finishInvocation(ctx, invocation, "failed", nil, false, "", "")
		out := failureOutcome(domain.FailAgentError, err.Error(), domain.AgentInvokedStage(attempt))
		return invocation, &out
	}

	callCtx, cancel := context.WithTimeout(ctx, r.agent.Timeout)
	defer cancel()

	result, runErr := adapter.Run(callCtx, agent.Request{
		RunID:        run.PublicID,
		Attempt:      attempt,
		IssueID:      run.IssueID,
		WorkspaceDir: workspace.Path,
		SandboxDir:   sandboxDir,
		Prompt:       agent.NewPrompt(text, transcriptPath),
		Model:        r.agent.Model,
		AgentName:    r.agent.Profile,
		Timeout:      r.agent.Timeout,
		EnvAllowlist: environment,
		PolicyFile:   policyPath,
	})

	finish := invocationFinish(result, runErr)
	r.finishInvocation(ctx, invocation, finish.Status, finish.ExitCode, finish.TimedOut, finish.ReportedStatus, finish.ResultSummary)
	r.recordInvocationEvents(ctx, run, attempt, result, runErr, log)

	// A cancel request wins over whatever the agent reported: the operator's
	// stop is the truthful cause, and no later stage may run (US4-AC4).
	if out := r.stopRequested(ctx, run); out != nil {
		return invocation, out
	}
	if out := r.outcomeForInvocation(result, runErr, attempt); out != nil {
		return invocation, out
	}
	return invocation, nil
}

// finishInvocation is a thin wrapper that keeps the write in one place.
func (r *Runner) finishInvocation(ctx context.Context, invocation *domain.AgentInvocation, status string, exitCode *int, timedOut bool, reportedStatus, resultSummary string) {
	if invocation == nil {
		return
	}
	writeCtx, cancel := durableContext(ctx)
	defer cancel()

	ended := time.Now().UTC()
	err := r.store.FinishInvocation(writeCtx, invocation.ID, store.InvocationFinish{
		Status:         status,
		ExitCode:       exitCode,
		TimedOut:       timedOut,
		ReportedStatus: reportedStatus,
		ResultSummary:  resultSummary,
		EndedAt:        &ended,
	})
	if err != nil {
		r.baseLogger().Warn("could not finish the invocation record",
			"run_id", invocation.RunID, "attempt", invocation.AttemptNo, "error", err)
	}
}

func invocationFinish(result agent.Result, runErr error) store.InvocationFinish {
	status := string(result.Status)
	if status == "" {
		status = "failed"
	}
	summary := summariseClaim(result.Summary)
	finish := store.InvocationFinish{
		Status:         status,
		TimedOut:       result.TimedOut,
		ReportedStatus: result.ReportedStatus,
		ResultSummary:  summary,
		EndedAt:        &result.EndedAt,
	}
	if result.ExitCode >= 0 && (result.ExitCode != 0 || runErr == nil) {
		code := result.ExitCode
		finish.ExitCode = &code
	}
	return finish
}

func summariseClaim(summary agent.Summary) string {
	parts := make([]string, 0, 3)
	if len(summary.Files) > 0 {
		parts = append(parts, "claimed files: "+strings.Join(summary.Files, ", "))
	}
	if summary.ChecksRun != "" {
		parts = append(parts, "claimed checks: "+summary.ChecksRun)
	}
	if summary.Blockers != "" {
		parts = append(parts, "blockers: "+summary.Blockers)
	}
	return strings.Join(parts, "; ")
}

func (r *Runner) recordInvocationEvents(ctx context.Context, run *domain.Run, attempt int, result agent.Result, runErr error, log *logging.RunLog) {
	severity := domain.SeverityInfo
	if result.Status != agent.StatusSucceeded {
		severity = domain.SeverityWarn
	}
	r.event(ctx, run, domain.RunEvent{
		Type:     domain.EventAgent,
		Severity: severity,
		Message: fmt.Sprintf("attempt %d finished: %s (exit %d, %dms)",
			attempt, result.Status, result.ExitCode, result.EndedAt.Sub(result.StartedAt).Milliseconds()),
	})

	if result.ReportedStatus == "" {
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventWarning,
			Severity: domain.SeverityWarn,
			Message:  fmt.Sprintf("attempt %d did not produce a parsable RESULT block; the claim is unavailable and validation decides", attempt),
		})
	}
	for _, violation := range result.PolicyViolations {
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventError,
			Severity: domain.SeverityError,
			Message:  "policy violation: " + violation,
		})
	}
	if runErr != nil {
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventError,
			Severity: domain.SeverityError,
			Message:  "agent invocation error: " + runErr.Error(),
		})
	}

	emitted := 0
	for _, line := range summariseActivity(result.Activity, maxActivityEventsPerAttempt) {
		if emitted >= maxActivityEventsPerAttempt {
			break
		}
		r.event(ctx, run, domain.RunEvent{
			Type:    domain.EventActivity,
			Message: line,
		})
		emitted++
	}
	if log != nil {
		log.Info("agent attempt recorded", "attempt", attempt, "status", string(result.Status))
	}
}

func (r *Runner) outcomeForInvocation(result agent.Result, runErr error, attempt int) *outcome {
	stage := domain.AgentInvokedStage(attempt)

	if len(result.PolicyViolations) > 0 {
		out := failureOutcome(domain.FailPolicyViolation,
			"The agent attempted an operation outside its workspace: "+result.PolicyViolations[0], stage)
		return &out
	}
	if result.TimedOut {
		out := failureOutcome(domain.FailAgentTimeout,
			fmt.Sprintf("The agent did not finish within %s.", r.agent.Timeout), stage)
		return &out
	}
	switch result.Status {
	case agent.StatusBlocked:
		message := "The agent stopped because the issue needs an answer."
		if strings.TrimSpace(result.Summary.Blockers) != "" {
			message = "The agent is blocked: " + result.Summary.Blockers
		}
		out := failureOutcome(domain.FailAgentBlocked, message, stage)
		return &out
	case agent.StatusCancelled:
		out := failureOutcome(domain.FailAgentError, "The agent was stopped before it finished.", stage)
		return &out
	case agent.StatusFailed:
		message := fmt.Sprintf("The agent exited with code %d.", result.ExitCode)
		if runErr != nil {
			message = runErr.Error()
		}
		out := failureOutcome(domain.FailAgentError, message, stage)
		return &out
	}
	if runErr != nil {
		out := failureOutcome(domain.FailAgentError, runErr.Error(), stage)
		return &out
	}
	return nil
}
