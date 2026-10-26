package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

var publicIDCounter atomic.Int64

func openTestStore(t *testing.T) (*store.Store, *logging.Redactor) {
	t.Helper()
	redactor := logging.NewRedactor()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "agent.db"), store.WithRedactor(redactor))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, redactor
}

func createSnapshot(t *testing.T, s *store.Store, issueID int) *domain.IssueSnapshot {
	t.Helper()
	snapshot := &domain.IssueSnapshot{
		RedmineIssueID:    issueID,
		Subject:           fmt.Sprintf("Issue %d", issueID),
		Description:       "desc",
		CriteriaSource:    "none",
		ProjectID:         "1",
		ProjectIdentifier: "billing",
		ProjectName:       "Billing",
		ContentHash:       fmt.Sprintf("hash-%d-%d", issueID, publicIDCounter.Add(1)),
		RawJSON:           "{}",
		FetchedAt:         time.Now().UTC(),
	}
	if _, err := s.CreateIssueSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("CreateIssueSnapshot: %v", err)
	}
	return snapshot
}

func createRun(t *testing.T, s *store.Store, issueID int, state domain.RunState) *domain.Run {
	t.Helper()
	snapshot := createSnapshot(t, s, issueID)
	run := &domain.Run{
		PublicID:        fmt.Sprintf("run-%d", publicIDCounter.Add(1)),
		IssueID:         issueID,
		IssueSnapshotID: snapshot.ID,
		State:           state,
		Stage:           domain.StageCreated,
		CreatedAt:       time.Now().UTC(),
	}
	if _, err := s.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

func TestPartialUniqueIndexRejectsSecondActiveRun(t *testing.T) {
	s, _ := openTestStore(t)
	createRun(t, s, 4821, domain.StateQueued)

	second := &domain.Run{
		PublicID:        "run-second",
		IssueID:         4821,
		IssueSnapshotID: createSnapshot(t, s, 4821).ID,
		State:           domain.StateQueued,
	}
	if _, err := s.CreateRun(context.Background(), second); err == nil {
		t.Fatal("a second active run for the same issue must be rejected")
	}

	// The index is partial: a terminal run for the same issue is allowed.
	if err := s.InTx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`UPDATE runs SET state = 'failed', ended_at = ? WHERE id = ?`,
			domain.FormatTime(time.Now()), second.ID)
		return err
	}); err != nil {
		t.Fatalf("terminal update: %v", err)
	}
}

func TestTerminalRunsCannotBeUpdated(t *testing.T) {
	s, _ := openTestStore(t)
	run := createRun(t, s, 7, domain.StateQueued)
	ctx := context.Background()

	if err := s.TransitionRun(ctx, run.ID, store.Transition{
		To:    domain.StatePreparing,
		Stage: domain.StageConnectivityRedmine,
		Event: domain.RunEvent{Message: "checking Redmine"},
	}); err != nil {
		t.Fatalf("queued -> preparing: %v", err)
	}

	failure := domain.NewFailure(domain.FailConnectivityRedmine, "redmine refused the key", domain.StageConnectivityRedmine)
	if err := s.TransitionRun(ctx, run.ID, store.Transition{
		To:      domain.StateFailed,
		Stage:   domain.StageConnectivityRedmine,
		Failure: &failure,
	}); err != nil {
		t.Fatalf("preparing -> failed: %v", err)
	}

	stored, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.State != domain.StateFailed {
		t.Fatalf("state = %s", stored.State)
	}
	if stored.EndedAt == nil {
		t.Fatal("terminal run has no ended_at")
	}
	if stored.FailureCode != domain.FailConnectivityRedmine {
		t.Fatalf("failure code = %s", stored.FailureCode)
	}

	if err := s.TransitionRun(ctx, run.ID, store.Transition{To: domain.StateReporting}); !errors.Is(err, store.ErrRunTerminal) {
		t.Fatalf("transition from terminal: got %v, want ErrRunTerminal", err)
	}
	if err := s.UpdateRun(ctx, run.ID, store.RunPatch{Summary: stringPtr("changed")}); !errors.Is(err, store.ErrRunTerminal) {
		t.Fatalf("update of terminal run: got %v, want ErrRunTerminal", err)
	}
}

func TestValidationResultChecks(t *testing.T) {
	s, _ := openTestStore(t)
	run := createRun(t, s, 9, domain.StateQueued)
	ctx := context.Background()

	one := 1
	zero := 0
	cases := []struct {
		name    string
		result  domain.ValidationResult
		wantErr bool
	}{
		{
			name: "passed with non-zero exit",
			result: domain.ValidationResult{
				RunID: run.ID, CheckName: "a", Command: "x", Required: true,
				Outcome: domain.OutcomePassed, ExitCode: &one,
			},
			wantErr: true,
		},
		{
			name: "passed without exit code",
			result: domain.ValidationResult{
				RunID: run.ID, CheckName: "b", Command: "x", Required: true,
				Outcome: domain.OutcomePassed,
			},
			wantErr: true,
		},
		{
			name: "failed with non-zero exit",
			result: domain.ValidationResult{
				RunID: run.ID, CheckName: "c", Command: "x", Required: true,
				Outcome: domain.OutcomeFailed, ExitCode: &one,
			},
		},
		{
			name: "not executed without reason",
			result: domain.ValidationResult{
				RunID: run.ID, CheckName: "d", Command: "x", Required: true,
				Outcome: domain.OutcomeNotExecuted,
			},
			wantErr: true,
		},
		{
			name: "passed with exit zero",
			result: domain.ValidationResult{
				RunID: run.ID, CheckName: "e", Command: "x", Required: true,
				Outcome: domain.OutcomePassed, ExitCode: &zero,
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := testCase.result
			_, err := s.InsertValidationResult(ctx, &result)
			if testCase.wantErr && err == nil {
				t.Fatal("expected the database CHECK to reject this row")
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("InsertValidationResult: %v", err)
			}
		})
	}
}

func TestEventSeqIsMonotonicUnderConcurrentAppends(t *testing.T) {
	s, _ := openTestStore(t)
	run := createRun(t, s, 11, domain.StateQueued)
	ctx := context.Background()

	const writers = 32
	seqs := make([]int64, writers)
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(index int) {
			defer wg.Done()
			seq, err := s.AppendEvent(ctx, run.ID, domain.RunEvent{
				Type:    domain.EventActivity,
				Message: fmt.Sprintf("event %d", index),
			})
			if err != nil {
				t.Errorf("AppendEvent: %v", err)
				return
			}
			seqs[index] = seq
		}(i)
	}
	wg.Wait()

	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for index, seq := range seqs {
		if seq != int64(index+1) {
			t.Fatalf("seq at %d = %d, want %d (seqs=%v)", index, seq, index+1, seqs)
		}
	}

	events, err := s.LatestEvents(ctx, run.ID, 100)
	if err != nil {
		t.Fatalf("LatestEvents: %v", err)
	}
	if len(events) != writers {
		t.Fatalf("got %d events", len(events))
	}
}

func TestReopenPreservesHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.db")
	ctx := context.Background()

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	run := createRun(t, first, 21, domain.StateQueued)
	if err := first.TransitionRun(ctx, run.ID, store.Transition{
		To: domain.StatePreparing, Stage: domain.StageConnectivityGitlab,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}
	invocation := &domain.AgentInvocation{
		RunID: run.ID, AttemptNo: 1, Kind: "implement", Adapter: "fake",
		Model: "p/m", ConfigID: "cfg", PromptPath: "/tmp/prompt", PromptSHA256: "sha",
		TranscriptPath: "/tmp/transcript",
	}
	if _, err := first.CreateInvocation(ctx, invocation); err != nil {
		t.Fatalf("CreateInvocation: %v", err)
	}
	zero := 0
	if err := first.FinishInvocation(ctx, invocation.ID, store.InvocationFinish{
		Status: "succeeded", ExitCode: &zero,
	}); err != nil {
		t.Fatalf("FinishInvocation: %v", err)
	}
	check := &domain.ValidationResult{
		RunID: run.ID, InvocationID: &invocation.ID, CheckName: "unit", Command: "go test ./...",
		Required: true, Outcome: domain.OutcomePassed, ExitCode: &zero, DurationMS: 12,
	}
	if _, err := first.InsertValidationResult(ctx, check); err != nil {
		t.Fatalf("InsertValidationResult: %v", err)
	}
	if _, err := first.UpsertReport(ctx, &domain.Report{
		RunID: run.ID, BodyMD: "# Report", DeliveryStatus: "pending", DedupeKey: "dedupe-1",
	}); err != nil {
		t.Fatalf("UpsertReport: %v", err)
	}
	profile := &domain.ConfigurationProfile{
		ContentHash: "profile-hash", SchemaVersion: 1, SourcePath: "/etc/config.yaml", Content: "{}",
	}
	if _, err := first.SaveConfigurationProfile(ctx, profile); err != nil {
		t.Fatalf("SaveConfigurationProfile: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	stored, err := second.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after reopen: %v", err)
	}
	if stored.Stage != domain.StageConnectivityGitlab {
		t.Fatalf("stage = %s", stored.Stage)
	}
	events, err := second.LatestEvents(ctx, run.ID, 50)
	if err != nil {
		t.Fatalf("LatestEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events after reopen", len(events))
	}
	attempts, err := second.CheckAttempts(ctx, run.ID)
	if err != nil {
		t.Fatalf("CheckAttempts: %v", err)
	}
	if len(attempts) != 1 || !attempts[0].AllRequiredPassed {
		t.Fatalf("attempts = %+v", attempts)
	}
	report, err := second.GetReport(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if report.BodyMD != "# Report" || report.DedupeKey != "dedupe-1" {
		t.Fatalf("report = %+v", report)
	}
	if _, err := second.GetConfigurationProfileByHash(ctx, "profile-hash"); err != nil {
		t.Fatalf("GetConfigurationProfileByHash: %v", err)
	}
}

func TestListRunsCursorPagination(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 5; i++ {
		snapshot := createSnapshot(t, s, i)
		run := &domain.Run{
			PublicID:        fmt.Sprintf("page-run-%d", i),
			IssueID:         i,
			IssueSnapshotID: snapshot.ID,
			State:           domain.StateQueued,
			Stage:           domain.StageCreated,
			CreatedAt:       base.Add(time.Duration(i) * time.Second),
		}
		if _, err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun %d: %v", i, err)
		}
	}

	seen := map[int]bool{}
	cursor := ""
	for page := 0; page < 3; page++ {
		runs, next, err := s.ListRuns(ctx, store.RunFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, run := range runs {
			if seen[run.IssueID] {
				t.Fatalf("issue %d appeared twice", run.IssueID)
			}
			seen[run.IssueID] = true
		}
		cursor = next
		if cursor == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d runs, want 5", len(seen))
	}

	firstPage, _, err := s.ListRuns(ctx, store.RunFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage) != 1 || firstPage[0].IssueID != 5 {
		t.Fatalf("newest-first order broken: %+v", firstPage)
	}
}

func TestActiveRunLookupAndCount(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	first := createRun(t, s, 31, domain.StateQueued)
	createRun(t, s, 32, domain.StateQueued)

	active, err := s.ActiveRunByIssue(ctx, 31)
	if err != nil {
		t.Fatalf("ActiveRunByIssue: %v", err)
	}
	if active == nil || active.ID != first.ID {
		t.Fatalf("active run = %+v", active)
	}
	count, err := s.ActiveRunCount(ctx)
	if err != nil {
		t.Fatalf("ActiveRunCount: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d", count)
	}

	if err := s.TransitionRun(ctx, first.ID, store.Transition{To: domain.StateFailed}); err != nil {
		t.Fatalf("transition: %v", err)
	}
	active, err = s.ActiveRunByIssue(ctx, 31)
	if err != nil {
		t.Fatal(err)
	}
	if active != nil {
		t.Fatalf("terminal run still reported active: %+v", active)
	}
}

func TestCancelRequestAndTransition(t *testing.T) {
	s, _ := openTestStore(t)
	run := createRun(t, s, 41, domain.StateImplementing)
	ctx := context.Background()

	changed, err := s.RequestCancel(ctx, run.ID)
	if err != nil || !changed {
		t.Fatalf("RequestCancel = %v, %v", changed, err)
	}
	changed, err = s.RequestCancel(ctx, run.ID)
	if err != nil || changed {
		t.Fatalf("second RequestCancel = %v, %v", changed, err)
	}
	if err := s.TransitionRun(ctx, run.ID, store.Transition{To: domain.StateCancelled}); err != nil {
		t.Fatalf("implementing -> cancelled: %v", err)
	}
	stored, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.StateCancelled || stored.CancelRequestedAt == nil {
		t.Fatalf("run = %+v", stored)
	}
}

func TestMarkRunInterrupted(t *testing.T) {
	s, _ := openTestStore(t)
	run := createRun(t, s, 51, domain.StateImplementing)
	ctx := context.Background()

	if err := s.MarkRunInterrupted(ctx, run.ID); err != nil {
		t.Fatalf("MarkRunInterrupted: %v", err)
	}
	stored, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.StateInterrupted {
		t.Fatalf("state = %s", stored.State)
	}
	if stored.InterruptedStage != domain.StageCreated {
		t.Fatalf("interrupted stage = %q", stored.InterruptedStage)
	}
	if err := s.MarkRunInterrupted(ctx, run.ID); !errors.Is(err, store.ErrRunTerminal) {
		t.Fatalf("second sweep: %v", err)
	}
}

func TestEventRedactionOnWrite(t *testing.T) {
	s, redactor := openTestStore(t)
	redactor.Register("hunter2-secret")
	run := createRun(t, s, 61, domain.StateQueued)

	if _, err := s.AppendEvent(context.Background(), run.ID, domain.RunEvent{
		Type:    domain.EventActivity,
		Message: "using token hunter2-secret",
		Data:    `{"token":"hunter2-secret"}`,
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	events, err := s.LatestEvents(context.Background(), run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	if strings.Contains(events[0].Message, "hunter2-secret") || strings.Contains(events[0].Data, "hunter2-secret") {
		t.Fatalf("event leaked the secret: %+v", events[0])
	}
}

func TestConfigurationProfileDedupe(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	first := &domain.ConfigurationProfile{ContentHash: "same", SchemaVersion: 1, SourcePath: "a", Content: "{}"}
	firstID, err := s.SaveConfigurationProfile(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := &domain.ConfigurationProfile{ContentHash: "same", SchemaVersion: 1, SourcePath: "b", Content: "{}"}
	secondID, err := s.SaveConfigurationProfile(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID {
		t.Fatalf("hash reuse produced %d then %d", firstID, secondID)
	}
}

func stringPtr(value string) *string { return &value }

func TestReserveRunEnforcesConcurrencyAndIssueUniqueness(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)

	first := createRun(t, s, 5001, domain.StateQueued)

	snapshot := createSnapshot(t, s, 5001)
	duplicate := &domain.Run{
		PublicID:        fmt.Sprintf("reserve-%d", publicIDCounter.Add(1)),
		IssueID:         5001,
		IssueSnapshotID: snapshot.ID,
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.ReserveRun(ctx, snapshot, duplicate, 2); !errors.Is(err, store.ErrActiveRunExists) {
		t.Fatalf("second run for issue 5001: %v, want ErrActiveRunExists", err)
	}

	otherSnapshot := createSnapshot(t, s, 5002)
	other := &domain.Run{
		PublicID:        fmt.Sprintf("reserve-%d", publicIDCounter.Add(1)),
		IssueID:         5002,
		IssueSnapshotID: otherSnapshot.ID,
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.ReserveRun(ctx, otherSnapshot, other, 1); !errors.Is(err, store.ErrConcurrencyLimit) {
		t.Fatalf("concurrency: %v, want ErrConcurrencyLimit", err)
	}
	if err := s.ReserveRun(ctx, otherSnapshot, other, 2); err != nil {
		t.Fatalf("ReserveRun with a free slot: %v", err)
	}
	if other.ID == 0 || other.IssueSnapshotID != otherSnapshot.ID {
		t.Fatalf("reserved run = %+v", other)
	}

	var count int
	if err := s.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE issue_id = 5002`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("issue 5002 has %d runs, want 1", count)
	}

	if err := s.TransitionRun(ctx, other.ID, store.Transition{To: domain.StateFailed, Stage: domain.StageCreated}); err != nil {
		t.Fatalf("TransitionRun: %v", err)
	}
	if err := s.SetStage(ctx, first.ID, domain.StageConnectivityRedmine, domain.RunEvent{Message: "probing"}); err != nil {
		t.Fatalf("SetStage: %v", err)
	}
	updated, err := s.GetRun(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Stage != domain.StageConnectivityRedmine {
		t.Fatalf("stage = %q", updated.Stage)
	}
	if err := s.SetStage(ctx, other.ID, domain.StageChecksRunning, domain.RunEvent{Message: "too late"}); !errors.Is(err, store.ErrRunTerminal) {
		t.Fatalf("SetStage on a terminal run: %v, want ErrRunTerminal", err)
	}
}
