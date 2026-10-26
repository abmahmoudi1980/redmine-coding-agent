package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

// reportMarkerPrefix begins the HTML comment that makes a posted report
// idempotent: the same outcome is never commented twice (FR-045).
const reportMarkerPrefix = "<!-- redmine-agent:report:"

// reportInput is everything the final report is rendered from. It carries the
// worker's own facts only; agent claims arrive through invocations.
type reportInput struct {
	Run             *domain.Run
	Snapshot        *domain.IssueSnapshot
	Workspace       *domain.Workspace
	Checks          []domain.ValidationResult
	Invocations     []domain.AgentInvocation
	Terminal        domain.RunState
	Failure         *domain.Failure
	MergeRequestURL string
	// MergeRequestSkipped distinguishes "disabled by policy" from a plain
	// absence, so the report never implies a failure (FR-042).
	MergeRequestSkipped bool
	Duration            time.Duration
	RenderedAt          time.Time
}

// dedupeKey is stable for one outcome and changes when the commit or merge
// request changes (data-model §8).
func dedupeKey(runPublicID string, terminal domain.RunState, commitSHA, mergeRequestURL string) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{runPublicID, string(terminal), commitSHA, mergeRequestURL}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func reportMarker(dedupeKey string) string {
	return reportMarkerPrefix + dedupeKey + " -->"
}

// renderReport produces the markdown report required by FR-044: terminal
// status, change summary, branch, commit, merge request, validation results,
// blockers or failure details, and duration.
func renderReport(input reportInput) string {
	var b strings.Builder

	run := input.Run
	issueID := 0
	subject := ""
	baseBranch := ""
	branch := ""
	commit := ""
	baseRevision := ""
	summary := ""
	if run != nil {
		issueID = run.IssueID
		subject = run.IssueSubject
		baseBranch = run.BaseBranch
		branch = run.Branch
		commit = run.CommitSHA
		baseRevision = run.BaseRevision
		summary = run.Summary
	}
	if input.Snapshot != nil {
		if issueID == 0 {
			issueID = input.Snapshot.RedmineIssueID
		}
		if subject == "" {
			subject = input.Snapshot.Subject
		}
	}
	if input.Workspace != nil {
		if branch == "" {
			branch = input.Workspace.Branch
		}
		if baseRevision == "" {
			baseRevision = input.Workspace.BaseRevision
		}
	}

	fmt.Fprintf(&b, "## RedmineCodingAgent report — issue #%d\n\n", issueID)
	if subject != "" {
		fmt.Fprintf(&b, "**%s**\n\n", subject)
	}

	fmt.Fprintf(&b, "- **Status**: `%s`\n", input.Terminal)
	if input.Failure != nil {
		fmt.Fprintf(&b, "- **Failure**: `%s`", input.Failure.Code)
		if input.Failure.Message != "" {
			fmt.Fprintf(&b, " — %s", input.Failure.Message)
		}
		b.WriteString("\n")
	}
	if baseBranch == "" {
		baseBranch = "the configured base branch"
	}
	fmt.Fprintf(&b, "- **Branch**: `%s` → `%s`\n", valueOrLabel(branch, "none"), baseBranch)
	fmt.Fprintf(&b, "- **Commit**: `%s`\n", valueOrLabel(commit, "none"))
	if baseRevision != "" {
		fmt.Fprintf(&b, "- **Base revision**: `%s`\n", baseRevision)
	}
	fmt.Fprintf(&b, "- **Merge request**: %s\n", mergeRequestLabel(input))
	fmt.Fprintf(&b, "- **Duration**: %s\n", formatDuration(input.Duration))
	if !input.RenderedAt.IsZero() {
		fmt.Fprintf(&b, "- **Reported at**: %s\n", domain.FormatTime(input.RenderedAt))
	}

	b.WriteString("\n### Change summary\n\n")
	changed := changedPaths(input)
	if len(changed) == 0 {
		b.WriteString("No file was changed.\n")
	} else {
		fmt.Fprintf(&b, "%d file(s) changed:\n\n", len(changed))
		limit := len(changed)
		if limit > maxReportFiles {
			limit = maxReportFiles
		}
		for _, path := range changed[:limit] {
			fmt.Fprintf(&b, "- `%s`\n", path)
		}
		if len(changed) > limit {
			fmt.Fprintf(&b, "- … and %d more\n", len(changed)-limit)
		}
	}
	if summary != "" {
		fmt.Fprintf(&b, "\n%s\n", summary)
	}

	b.WriteString("\n### Validation\n\n")
	if len(input.Checks) == 0 {
		b.WriteString("No check result was recorded for this run.\n")
	} else {
		b.WriteString("| Check | Required | Command | Outcome | Exit | Duration |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, check := range input.Checks {
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
			fmt.Fprintf(&b, "| %s | %s | `%s` | %s | %s | %s |\n",
				check.CheckName, required, check.Command, outcome, exit, formatMilliseconds(check.DurationMS))
		}
	}

	b.WriteString("\n### Agent claims\n\n")
	if len(input.Invocations) == 0 {
		b.WriteString("The agent was not invoked.\n")
	} else {
		for _, invocation := range input.Invocations {
			claim := strings.TrimSpace(invocation.ResultSummary)
			if claim == "" {
				claim = "no claim recorded"
			}
			status := strings.TrimSpace(invocation.ReportedStatus)
			if status == "" {
				status = "not reported"
			}
			fmt.Fprintf(&b, "- attempt %d (%s): exited with %s, reported `%s`, claimed %s\n",
				invocation.AttemptNo, invocation.Kind, exitLabel(invocation.ExitCode), status, claim)
		}
	}

	b.WriteString("\n### Blockers\n\n")
	blockers := blockerText(input)
	fmt.Fprintf(&b, "%s\n", blockers)

	b.WriteString("\n---\n\n")
	b.WriteString("Every check above was executed by the worker, not by the agent. ")
	b.WriteString("Nothing was merged and the base branch was not written by this tool.\n")
	return b.String()
}

const maxReportFiles = 25

func changedPaths(input reportInput) []string {
	if input.Workspace != nil && len(input.Workspace.ChangedPaths) > 0 {
		return input.Workspace.ChangedPaths
	}
	return nil
}

func blockerText(input reportInput) string {
	if input.Failure != nil {
		switch input.Failure.Code {
		case domain.FailAgentBlocked, domain.FailNoRepositoryMapping, domain.FailRepositoryAmbiguous:
			if input.Failure.Message != "" {
				return input.Failure.Message
			}
		}
	}
	for _, invocation := range input.Invocations {
		claim := strings.TrimSpace(invocation.ResultSummary)
		if index := strings.Index(claim, "blockers:"); index >= 0 {
			return strings.TrimSpace(claim[index+len("blockers:"):])
		}
	}
	return "None recorded."
}

func exitLabel(code *int) string {
	if code == nil {
		return "no exit code"
	}
	return fmt.Sprintf("exit %d", *code)
}

func valueOrLabel(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func mergeRequestLabel(input reportInput) string {
	if strings.TrimSpace(input.MergeRequestURL) != "" {
		return input.MergeRequestURL
	}
	if input.MergeRequestSkipped {
		return "not created (merge requests are disabled for this repository)"
	}
	return "not created"
}

func formatMilliseconds(milliseconds int64) string {
	if milliseconds <= 0 {
		return "—"
	}
	return formatDuration(time.Duration(milliseconds) * time.Millisecond)
}

// formatDuration renders a duration compactly (2m5s, 8s, 1h2m).
func formatDuration(duration time.Duration) string {
	if duration <= 0 {
		return "unknown"
	}
	seconds := int(duration.Round(time.Second) / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		if remainder := seconds % 60; remainder != 0 {
			return fmt.Sprintf("%dm%ds", minutes, remainder)
		}
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	if remainder := minutes % 60; remainder != 0 {
		return fmt.Sprintf("%dh%dm", hours, remainder)
	}
	return fmt.Sprintf("%dh", hours)
}

// persistReport renders, stores, and when enabled delivers the report. It is
// safe to call for any terminal outcome: a delivery failure never changes the
// run's own result (FR-047).
func (r *Runner) persistReport(ctx context.Context, run *domain.Run, snapshot domain.IssueSnapshot, workspace *domain.Workspace, result publishResult, terminal domain.RunState, failure *domain.Failure, log *logging.RunLog) *domain.Report {
	checks, err := r.store.ListValidationResults(ctx, run.ID)
	if err != nil {
		r.baseLogger().Warn("could not read the check results for the report", "run_id", run.PublicID, "error", err)
		checks = nil
	}
	invocations, err := r.store.ListInvocations(ctx, run.ID)
	if err != nil {
		r.baseLogger().Warn("could not read the agent attempts for the report", "run_id", run.PublicID, "error", err)
		invocations = nil
	}

	renderedAt := time.Now().UTC()
	var duration time.Duration
	if latest, err := r.store.GetRun(ctx, run.ID); err == nil && latest != nil && latest.StartedAt != nil {
		run.StartedAt = latest.StartedAt
	}
	if run.StartedAt != nil {
		duration = renderedAt.Sub(*run.StartedAt)
	}

	body := renderReport(reportInput{
		Run:                 run,
		Snapshot:            &snapshot,
		Workspace:           workspace,
		Checks:              checks,
		Invocations:         invocations,
		Terminal:            terminal,
		Failure:             failure,
		MergeRequestURL:     result.MergeRequestURL,
		MergeRequestSkipped: result.MergeRequestSkipped,
		Duration:            duration,
		RenderedAt:          renderedAt,
	})

	report := &domain.Report{
		RunID:           run.ID,
		BodyMD:          body,
		CommitSHA:       run.CommitSHA,
		MergeRequestURL: result.MergeRequestURL,
		DeliveryStatus:  "pending",
		DedupeKey:       dedupeKey(run.PublicID, terminal, run.CommitSHA, result.MergeRequestURL),
		CreatedAt:       renderedAt,
	}
	if _, err := r.store.UpsertReport(ctx, report); err != nil {
		r.baseLogger().Error("could not store the run report", "run_id", run.PublicID, "error", err)
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventError,
			Severity: domain.SeverityError,
			Message:  "the run report could not be stored: " + err.Error(),
		})
	}

	status, deliveryError, deliveredAt := r.deliverReport(ctx, run, report, log)
	report.DeliveryStatus = status
	report.DeliveryError = deliveryError
	report.DeliveredAt = deliveredAt
	if _, err := r.store.UpsertReport(ctx, report); err != nil {
		r.baseLogger().Error("could not record the report delivery state", "run_id", run.PublicID, "error", err)
	}
	return report
}

// deliverReport posts the report to the Redmine issue exactly once per outcome.
func (r *Runner) deliverReport(ctx context.Context, run *domain.Run, report *domain.Report, log *logging.RunLog) (string, string, *time.Time) {
	if r.redmine == nil || r.config == nil || !r.config.Redmine.CommentOnFinish {
		r.event(ctx, run, domain.RunEvent{
			Type:    domain.EventReport,
			Message: "Redmine commenting is disabled; the report is stored but not posted",
		})
		return "skipped", "", nil
	}

	marker := reportMarker(report.DedupeKey)
	alreadyPosted, err := r.redmine.NoteContains(ctx, run.IssueID, marker)
	if err != nil {
		message := "could not check issue #" + fmt.Sprintf("%d", run.IssueID) + " for an existing report: " + err.Error()
		r.event(ctx, run, domain.RunEvent{Type: domain.EventWarning, Severity: domain.SeverityWarn, Message: message})
		return "failed", err.Error(), nil
	}
	if alreadyPosted {
		r.event(ctx, run, domain.RunEvent{
			Type:    domain.EventReport,
			Message: fmt.Sprintf("issue #%d already carries this outcome; the comment was not duplicated", run.IssueID),
		})
		now := time.Now().UTC()
		return "delivered", "", &now
	}

	body := r.redact(report.BodyMD + "\n\n" + marker + "\n")
	if err := r.redmine.PostNote(ctx, run.IssueID, body); err != nil {
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventError,
			Severity: domain.SeverityError,
			Message:  "the report could not be posted to Redmine: " + err.Error(),
		})
		return "failed", err.Error(), nil
	}

	_ = r.stage(ctx, run, domain.StageCommentPosted, domain.RunEvent{
		Type:    domain.EventReport,
		Message: fmt.Sprintf("the completion report was posted to issue #%d", run.IssueID),
	}, log)
	now := time.Now().UTC()
	return "delivered", "", &now
}

// persistFallbackReport records a report for a run that never reached the
// reporting stage, so FR-044 holds for every terminal run.
func (r *Runner) persistFallbackReport(ctx context.Context, run *domain.Run, out outcome) {
	if _, err := r.store.GetReport(ctx, run.ID); err == nil {
		return
	}
	snapshot := domain.IssueSnapshot{RedmineIssueID: run.IssueID, Subject: run.IssueSubject}
	if stored, err := r.store.GetIssueSnapshot(ctx, run.IssueSnapshotID); err == nil && stored != nil {
		snapshot = *stored
	}
	var workspace *domain.Workspace
	if stored, err := r.store.GetWorkspaceByRun(ctx, run.ID); err == nil {
		workspace = stored
	}
	result := publishResult{Pushed: run.PushedAt != nil}
	_ = r.persistReport(ctx, run, snapshot, workspace, result, out.State, out.Failure, nil)
}
