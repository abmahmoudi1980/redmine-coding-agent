package store_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

var historyIDCounter atomic.Int64

// createHistoryRun inserts one run with a caller-controlled timestamp and
// drives it to the requested state through the legal state machine.
func createHistoryRun(t *testing.T, s *store.Store, issueID int, state domain.RunState, createdAt time.Time) *domain.Run {
	t.Helper()
	snapshot := createSnapshot(t, s, issueID)
	run := &domain.Run{
		PublicID:        fmt.Sprintf("history-run-%d", historyIDCounter.Add(1)),
		IssueID:         issueID,
		IssueSnapshotID: snapshot.ID,
		State:           domain.StateQueued,
		Stage:           domain.StageCreated,
		CreatedAt:       createdAt,
	}
	if _, err := s.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	ctx := context.Background()
	advance := func(to domain.RunState, stage domain.Stage) {
		t.Helper()
		if err := s.TransitionRun(ctx, run.ID, store.Transition{To: to, Stage: stage}); err != nil {
			t.Fatalf("%s -> %s: %v", run.State, to, err)
		}
		run.State = to
		run.Stage = stage
	}

	switch state {
	case domain.StateQueued:
	case domain.StateFailed:
		advance(domain.StatePreparing, domain.StageConnectivityRedmine)
		advance(domain.StateImplementing, domain.StageWorkspaceCreated)
		advance(domain.StateFailed, domain.StageAgentInvoked)
	case domain.StateBlocked:
		advance(domain.StatePreparing, domain.StageConnectivityGitlab)
		advance(domain.StateBlocked, domain.StageConnectivityGitlab)
	case domain.StateCompleted:
		advance(domain.StatePreparing, domain.StageConnectivityRedmine)
		advance(domain.StateImplementing, domain.StageWorkspaceCreated)
		advance(domain.StateValidating, domain.StageChecksFinished)
		advance(domain.StatePublishing, domain.StageBranchPushed)
		advance(domain.StateReporting, domain.StageMergeRequestCreated)
		advance(domain.StateCompleted, domain.StageReportWritten)
	case domain.StateCancelled:
		if _, err := s.RequestCancel(ctx, run.ID); err != nil {
			t.Fatalf("request cancel: %v", err)
		}
		advance(domain.StateCancelled, domain.StageCreated)
	case domain.StateInterrupted:
		if err := s.MarkRunInterrupted(ctx, run.ID); err != nil {
			t.Fatalf("mark interrupted: %v", err)
		}
		run.State = domain.StateInterrupted
	default:
		t.Fatalf("unsupported history state %q", state)
	}
	return run
}

func TestListRunsPaginatesNewestFirstWithoutGaps(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	states := []domain.RunState{
		domain.StateFailed,
		domain.StateCompleted,
		domain.StateBlocked,
		domain.StateCancelled,
		domain.StateInterrupted,
		domain.StateFailed,
	}
	created := make([]*domain.Run, 0, len(states))
	for index, state := range states {
		created = append(created, createHistoryRun(t, s, 1001+index, state, base.Add(time.Duration(index)*time.Minute)))
	}

	// Two pages of two must cover everything exactly once, newest first.
	var collected []domain.Run
	cursor := ""
	pages := 0
	for {
		page, next, err := s.ListRuns(ctx, store.RunFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		pages++
		collected = append(collected, page...)
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(collected) != len(created) {
		t.Fatalf("collected %d runs, want %d", len(collected), len(created))
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}

	seen := make(map[string]bool, len(collected))
	for index := range collected {
		if seen[collected[index].PublicID] {
			t.Fatalf("run %s appeared twice", collected[index].PublicID)
		}
		seen[collected[index].PublicID] = true
		if index > 0 && collected[index].CreatedAt.After(collected[index-1].CreatedAt) {
			t.Fatalf("runs are not newest-first at %d: %s after %s",
				index, collected[index].CreatedAt, collected[index-1].CreatedAt)
		}
	}
	for _, run := range created {
		if !seen[run.PublicID] {
			t.Fatalf("run %s was skipped by pagination", run.PublicID)
		}
	}

	// A run inserted after the first page was read must not perturb the cursor:
	// no duplicate and no gap in the snapshot being paged.
	first, cursorAfterFirst, err := s.ListRuns(ctx, store.RunFilter{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if cursorAfterFirst == "" {
		t.Fatal("expected a next cursor")
	}
	createHistoryRun(t, s, 2000, domain.StateFailed, base.Add(time.Hour))
	second, _, err := s.ListRuns(ctx, store.RunFilter{Limit: 2, Cursor: cursorAfterFirst})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	for _, item := range second {
		if item.IssueID == 2000 {
			t.Fatal("the cursor leaked a run created after the first page was read")
		}
		for _, earlier := range first {
			if item.PublicID == earlier.PublicID {
				t.Fatalf("run %s appeared on both pages", item.PublicID)
			}
		}
	}
}

func TestListRunsFiltersByStateAndIssue(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	failed := createHistoryRun(t, s, 3001, domain.StateFailed, base)
	blocked := createHistoryRun(t, s, 3002, domain.StateBlocked, base.Add(time.Minute))
	completed := createHistoryRun(t, s, 3003, domain.StateCompleted, base.Add(2*time.Minute))
	createHistoryRun(t, s, 3004, domain.StateFailed, base.Add(3*time.Minute))

	failedPage, _, err := s.ListRuns(ctx, store.RunFilter{State: domain.StateFailed})
	if err != nil {
		t.Fatalf("filter by state: %v", err)
	}
	if len(failedPage) != 2 {
		t.Fatalf("failed page has %d runs, want 2", len(failedPage))
	}
	for _, run := range failedPage {
		if run.State != domain.StateFailed {
			t.Fatalf("state filter returned %q", run.State)
		}
	}
	if failedPage[0].IssueID != 3004 || failedPage[1].IssueID != 3001 {
		t.Fatalf("failed page order = %d, %d; want 3004, 3001",
			failedPage[0].IssueID, failedPage[1].IssueID)
	}

	issuePage, _, err := s.ListRuns(ctx, store.RunFilter{IssueID: 3002})
	if err != nil {
		t.Fatalf("filter by issue: %v", err)
	}
	if len(issuePage) != 1 || issuePage[0].PublicID != blocked.PublicID {
		t.Fatalf("issue filter = %+v", issuePage)
	}

	bothPage, _, err := s.ListRuns(ctx, store.RunFilter{IssueID: 3003, State: domain.StateCompleted})
	if err != nil {
		t.Fatalf("filter by issue and state: %v", err)
	}
	if len(bothPage) != 1 || bothPage[0].PublicID != completed.PublicID {
		t.Fatalf("combined filter = %+v", bothPage)
	}

	// The failed run must still be independently retrievable by id.
	stored, err := s.GetRun(ctx, failed.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.PublicID != failed.PublicID || stored.State != domain.StateFailed {
		t.Fatalf("stored run = %+v", stored)
	}
}

func TestListRunsSurvivesReopen(t *testing.T) {
	path := t.TempDir() + "/history.db"
	ctx := context.Background()

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	run := createHistoryRun(t, first, 4001, domain.StateFailed, base)
	if err := first.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	page, _, err := reopened.ListRuns(ctx, store.RunFilter{IssueID: 4001})
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("reopened history has %d runs, want 1", len(page))
	}
	if page[0].PublicID != run.PublicID || page[0].State != domain.StateFailed {
		t.Fatalf("reopened run = %+v", page[0])
	}
	if page[0].EndedAt == nil {
		t.Fatal("a terminal run must keep its ended_at across a restart")
	}
}
