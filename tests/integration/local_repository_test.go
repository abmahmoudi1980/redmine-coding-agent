package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// TestLocalRepositoryRunsWithoutGitLab proves the pipeline needs no GitLab
// server when the mapping is an absolute local path: it clones from the path,
// validates, commits, and pushes the dedicated branch back into that local
// repository. The branch name is the only thing written; the base branch and
// the working tree are untouched.
func TestLocalRepositoryRunsWithoutGitLab(t *testing.T) {
	env := newPipeline(t, pipelineOptions{
		tuneHarness: func(h *Harness) {
			h.Config.GitLab = config.GitLab{PushEnabled: true, MergeRequestEnabled: true}
			h.Config.Repositories[0].RepositoryPath = h.Origin.Path
		},
	})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s), want completed", run.State, run.FailureMessage)
	}
	if run.CommitSHA == "" {
		t.Fatal("a passing run must create a commit")
	}
	if run.PushedAt == nil {
		t.Fatal("the dedicated branch should be pushed into the local repository")
	}

	branches := env.harness.Origin.Branches(t)
	if !contains(branches, "agent/issue-4821") {
		t.Fatalf("origin branches = %v, want agent/issue-4821", branches)
	}
	if tip := env.harness.Origin.Tip(t, "main"); tip != env.harness.Origin.InitialRevision {
		t.Fatalf("the base branch moved: %s -> %s", env.harness.Origin.InitialRevision, tip)
	}
	if pushed := env.harness.Origin.Tip(t, "agent/issue-4821"); pushed != run.CommitSHA {
		t.Fatalf("pushed tip = %s, recorded commit = %s", pushed, run.CommitSHA)
	}

	if merges := env.harness.GitLab.MergeRequests(); len(merges) != 0 {
		t.Fatalf("a local repository must not create merge requests: %+v", merges)
	}

	report, err := env.harness.Store.GetReport(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !strings.Contains(report.BodyMD, "the repository is a local path") {
		t.Fatalf("the report must say why no merge request exists:\n%s", report.BodyMD)
	}

	// No GitLab connectivity stage may be recorded for a local repository.
	events, err := env.harness.Store.EventsAfter(context.Background(), run.ID, 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, event := range events {
		if strings.Contains(event.Data, `"stage":"connectivity.gitlab"`) {
			t.Fatalf("a local run must not check GitLab connectivity: %+v", event)
		}
	}
}

// TestLocalRepositoryWithoutPublishingKeepsTheCommitLocal is the offline
// variant: with push_enabled false the commit stays in the workspace and the
// run still completes honestly.
func TestLocalRepositoryWithoutPublishingKeepsTheCommitLocal(t *testing.T) {
	env := newPipeline(t, pipelineOptions{
		tuneHarness: func(h *Harness) {
			h.Config.GitLab = config.GitLab{}
			h.Config.Repositories[0].RepositoryPath = h.Origin.Path
		},
	})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s), want completed", run.State, run.FailureMessage)
	}
	if run.CommitSHA == "" {
		t.Fatal("a passing run must create a commit")
	}
	if run.PushedAt != nil {
		t.Fatal("nothing may be pushed when publishing is disabled")
	}
	if branches := env.harness.Origin.Branches(t); contains(branches, "agent/issue-4821") {
		t.Fatalf("origin branches = %v, want no pushed branch", branches)
	}
}

// TestSecondRunForTheSameIssueUsesAFreeBranch is the recovery path a user hits
// after a failed attempt: the first run's branch still exists, so a fresh run
// must not collide with it and must not overwrite it.
func TestSecondRunForTheSameIssueUsesAFreeBranch(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))

	first := env.start(t, 4821)
	if first.State != domain.StateCompleted {
		t.Fatalf("first run state = %q (%s)", first.State, first.FailureMessage)
	}
	if first.Branch != "agent/issue-4821" {
		t.Fatalf("first branch = %q", first.Branch)
	}

	second := env.start(t, 4821)
	if second.State != domain.StateCompleted {
		t.Fatalf("second run state = %q (%s)", second.State, second.FailureMessage)
	}
	if second.Branch != "agent/issue-4821-r2" {
		t.Fatalf("second branch = %q, want agent/issue-4821-r2", second.Branch)
	}
	if second.CommitSHA == "" {
		t.Fatal("the second run must create its own commit")
	}
}
