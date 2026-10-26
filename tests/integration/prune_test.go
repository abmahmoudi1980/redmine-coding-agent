package integration

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

func TestPruneDeletesOldRunsButNeverTheirBranches(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	env.harness.AddIssue(issueWithMode("ok"))
	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %s (%s)", run.State, run.FailureMessage)
	}
	workspace, err := env.harness.Store.GetWorkspaceByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	report, err := env.runner.Prune(context.Background(), runner.CleanupOptions{
		OlderThan: time.Minute,
		Now:       time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.Deleted != 1 || len(report.Skipped) != 0 {
		t.Fatalf("report = %+v, want one deleted and no skips", report)
	}
	if _, err := env.harness.Store.GetRunByPublicID(context.Background(), run.PublicID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run row still present after prune: %v", err)
	}
	if _, err := os.Stat(workspace.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still present after prune: %v", err)
	}
	// FR-054: retention never deletes a pushed branch or the merge request.
	if !contains(env.harness.Origin.Branches(t), run.Branch) {
		t.Fatalf("branch %q was deleted from the origin", run.Branch)
	}
	if len(env.harness.GitLab.MergeRequests()) != 1 {
		t.Fatal("retention deleted the merge request")
	}
}

func TestPruneKeepsRunsWithUnpushedChanges(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	env.harness.AddIssue(issueWithMode("ok"))
	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %s (%s)", run.State, run.FailureMessage)
	}
	if err := env.harness.Store.UpdateWorkspaceChanges(context.Background(), run.ID, []string{"agent-change.txt"}, true); err != nil {
		t.Fatalf("mark unpushed: %v", err)
	}

	report, err := env.runner.Prune(context.Background(), runner.CleanupOptions{
		OlderThan: time.Minute,
		Now:       time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.Deleted != 0 || len(report.Skipped) != 1 {
		t.Fatalf("report = %+v, want one skip", report)
	}
	if report.Skipped[0].Reason != "has_unpushed_changes" {
		t.Fatalf("skip reason = %q", report.Skipped[0].Reason)
	}
	if _, err := env.harness.Store.GetRunByPublicID(context.Background(), run.PublicID); err != nil {
		t.Fatalf("a skipped run must keep its row: %v", err)
	}
}
