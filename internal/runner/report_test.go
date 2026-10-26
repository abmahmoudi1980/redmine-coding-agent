package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

func intPointer(value int) *int { return &value }

func sampleReportInput() reportInput {
	started := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	return reportInput{
		Run: &domain.Run{
			PublicID:     "018f2e4a-0000-7000-8000-000000000001",
			IssueID:      4821,
			IssueSubject: "Invoice totals round incorrectly",
			State:        domain.StateCompleted,
			Stage:        domain.StageReportWritten,
			BaseBranch:   "main",
			BaseRevision: "base-revision-sha",
			Branch:       "agent/issue-4821",
			CommitSHA:    "0123456789abcdef0123456789abcdef01234567",
			Summary:      "1 file(s) changed",
			StartedAt:    &started,
		},
		Snapshot: &domain.IssueSnapshot{
			RedmineIssueID: 4821,
			Subject:        "Invoice totals round incorrectly",
			ProjectName:    "Billing",
		},
		Workspace: &domain.Workspace{
			Branch:       "agent/issue-4821",
			BaseRevision: "base-revision-sha",
			ChangedPaths: []string{"agent-change.txt", "internal/billing/round.go"},
		},
		Checks: []domain.ValidationResult{
			{
				CheckName:  "unit-tests",
				Command:    "go test ./...",
				Required:   true,
				Outcome:    domain.OutcomePassed,
				ExitCode:   intPointer(0),
				DurationMS: 1234,
			},
			{
				CheckName:  "lint",
				Command:    "golangci-lint run",
				Required:   false,
				Outcome:    domain.OutcomeNotExecuted,
				Reason:     "not_configured",
				DurationMS: 0,
			},
		},
		Invocations: []domain.AgentInvocation{
			{
				AttemptNo:      1,
				Kind:           "implement",
				Status:         "succeeded",
				ReportedStatus: "done",
				ResultSummary:  "claimed files: agent-change.txt",
			},
		},
		Terminal:        domain.StateCompleted,
		MergeRequestURL: "https://gitlab.internal/group/billing-api/-/merge_requests/7",
		Duration:        2*time.Minute + 5*time.Second,
		RenderedAt:      started.Add(2*time.Minute + 5*time.Second),
	}
}

func TestRenderReportContainsEveryRequiredFact(t *testing.T) {
	input := sampleReportInput()
	body := renderReport(input)

	required := map[string]string{
		"terminal status":     "completed",
		"change summary":      "agent-change.txt",
		"branch":              "agent/issue-4821",
		"commit":              "0123456789abcdef0123456789abcdef01234567",
		"merge request link":  "https://gitlab.internal/group/billing-api/-/merge_requests/7",
		"validation check":    "unit-tests",
		"validation command":  "go test ./...",
		"validation outcome":  "passed",
		"not executed reason": "not_configured",
		"agent claim":         "claimed files: agent-change.txt",
		"blockers section":    "Blockers",
		"duration":            "2m5s",
		"base branch":         "main",
		"issue reference":     "#4821",
		"issue subject":       "Invoice totals round incorrectly",
	}
	for label, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("report is missing %s (%q):\n%s", label, want, body)
		}
	}
}

func TestRenderReportRecordsFailureAndBlockers(t *testing.T) {
	input := sampleReportInput()
	input.Terminal = domain.StateFailed
	input.MergeRequestURL = ""
	failure := domain.NewFailure(domain.FailPushRejected, "the remote rejected the branch", domain.StageCommitCreated)
	input.Failure = &failure

	body := renderReport(input)
	if !strings.Contains(body, "failed") {
		t.Errorf("report does not state the terminal status:\n%s", body)
	}
	if !strings.Contains(body, "push_rejected") {
		t.Errorf("report does not name the failure code:\n%s", body)
	}
	if !strings.Contains(body, "the remote rejected the branch") {
		t.Errorf("report does not carry the failure detail:\n%s", body)
	}
	if !strings.Contains(body, "0123456789abcdef0123456789abcdef01234567") {
		t.Errorf("the local commit must survive a failed publish:\n%s", body)
	}
}

func TestDedupeKeyIsStableForTheSameOutcome(t *testing.T) {
	first := dedupeKey("018f2e4a-0000-7000-8000-000000000001", domain.StateCompleted, "commit-a", "https://gitlab.internal/mr/1")
	second := dedupeKey("018f2e4a-0000-7000-8000-000000000001", domain.StateCompleted, "commit-a", "https://gitlab.internal/mr/1")
	if first != second {
		t.Fatalf("dedupe key is not stable: %q != %q", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("dedupe key is not a sha256 hex digest: %q", first)
	}

	changedCommit := dedupeKey("018f2e4a-0000-7000-8000-000000000001", domain.StateCompleted, "commit-b", "https://gitlab.internal/mr/1")
	if changedCommit == first {
		t.Fatal("a new commit must produce a new dedupe key")
	}
	changedState := dedupeKey("018f2e4a-0000-7000-8000-000000000001", domain.StateFailed, "commit-a", "https://gitlab.internal/mr/1")
	if changedState == first {
		t.Fatal("a different terminal state must produce a new dedupe key")
	}
	changedMR := dedupeKey("018f2e4a-0000-7000-8000-000000000001", domain.StateCompleted, "commit-a", "https://gitlab.internal/mr/2")
	if changedMR == first {
		t.Fatal("a new merge request must produce a new dedupe key")
	}
}
