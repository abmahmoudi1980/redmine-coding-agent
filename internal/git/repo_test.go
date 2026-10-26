package git_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentgit "github.com/systemgroup/redmine-coding-agent/internal/git"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newOrigin(t *testing.T) (origin, initialRevision, base string) {
	t.Helper()
	base = t.TempDir()
	origin = filepath.Join(base, "origin.git")
	gitRun(t, base, "init", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(base, "seed")
	gitRun(t, base, "init", "--initial-branch=main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	gitRun(t, seed, "remote", "add", "origin", origin)
	gitRun(t, seed, "push", "origin", "main")
	initialRevision = gitRun(t, seed, "rev-parse", "HEAD")
	return origin, initialRevision, base
}

func newManager(t *testing.T, base string) (*agentgit.Manager, string) {
	t.Helper()
	workspaceRoot := filepath.Join(base, "workspaces")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := agentgit.NewManager(filepath.Join(base, "repos"), workspaceRoot)
	return manager, workspaceRoot
}

func TestPlanAttemptBranchReusesOnlySafeBranches(t *testing.T) {
	origin, initialRevision, base := newOrigin(t)
	manager, workspaceRoot := newManager(t, base)
	ctx := context.Background()

	if _, err := manager.EnsureMirror(ctx, 1, origin); err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}

	plan, err := manager.PlanAttemptBranch(ctx, 1, "main", "agent/issue-9", 1, false, "")
	if err != nil {
		t.Fatalf("plan attempt 1: %v", err)
	}
	if plan.Branch != "agent/issue-9" || !plan.Reused {
		t.Fatalf("attempt 1 plan = %+v", plan)
	}

	// A prior attempt that was never pushed, left no local branch, and used the
	// same base may reuse its name.
	plan, err = manager.PlanAttemptBranch(ctx, 1, "main", "agent/issue-9", 2, false, initialRevision)
	if err != nil {
		t.Fatalf("plan reusable retry: %v", err)
	}
	if plan.Branch != "agent/issue-9" || !plan.Reused || plan.Superseded {
		t.Fatalf("reusable retry plan = %+v", plan)
	}

	// A local branch still holding the previous attempt forces a suffix.
	gitRun(t, filepath.Join(base, "repos", "1.git"), "branch", "agent/issue-9", "refs/remotes/origin/main")
	plan, err = manager.PlanAttemptBranch(ctx, 1, "main", "agent/issue-9", 2, false, initialRevision)
	if err != nil {
		t.Fatalf("plan superseding retry: %v", err)
	}
	if plan.Branch != "agent/issue-9-r2" || !plan.Superseded || plan.Reused {
		t.Fatalf("superseding retry plan = %+v", plan)
	}

	// A pushed previous attempt always gets a new branch.
	workspace := filepath.Join(workspaceRoot, "attempt-one")
	if err := manager.AddWorktree(ctx, 1, workspace, "agent/issue-9-r2", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	plan, err = manager.PlanAttemptBranch(ctx, 1, "main", "agent/issue-9", 3, true, initialRevision)
	if err != nil {
		t.Fatalf("plan after push: %v", err)
	}
	if plan.Branch == "agent/issue-9" || !plan.Superseded {
		t.Fatalf("a pushed attempt must never be reused: %+v", plan)
	}
	if plan.Branch == "agent/issue-9-r2" {
		t.Fatalf("an existing attempt branch must be skipped: %+v", plan)
	}
	if plan.Branch != "agent/issue-9-r3" {
		t.Fatalf("branch = %q, want agent/issue-9-r3", plan.Branch)
	}
}

func TestMirrorAndConcurrentWorktrees(t *testing.T) {
	origin, initialRevision, base := newOrigin(t)
	manager, workspaceRoot := newManager(t, base)
	ctx := context.Background()

	mirror, err := manager.EnsureMirror(ctx, 1, origin)
	if err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("mirror is not bare: %v", err)
	}

	workspaces := []string{
		filepath.Join(workspaceRoot, "run-1"),
		filepath.Join(workspaceRoot, "run-2"),
	}
	branches := []string{"agent/issue-1", "agent/issue-2"}
	var wg sync.WaitGroup
	errs := make([]error, len(workspaces))
	for index := range workspaces {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = manager.AddWorktree(ctx, 1, workspaces[i], branches[i], "main")
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("AddWorktree %d: %v", index, err)
		}
	}

	if got := gitRun(t, workspaces[0], "rev-parse", "--abbrev-ref", "HEAD"); got != branches[0] {
		t.Fatalf("worktree 1 branch = %s", got)
	}
	if got := gitRun(t, workspaces[1], "rev-parse", "--abbrev-ref", "HEAD"); got != branches[1] {
		t.Fatalf("worktree 2 branch = %s", got)
	}
	if _, err := os.Stat(filepath.Join(workspaces[0], "README.md")); err != nil {
		t.Fatalf("worktree is empty: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workspaces[0], "change.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddAll(ctx, workspaces[0]); err != nil {
		t.Fatalf("AddAll: %v", err)
	}
	commit, err := manager.Commit(ctx, workspaces[0], "Implement issue 1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commit == initialRevision {
		t.Fatal("commit did not advance HEAD")
	}

	if got := gitRun(t, workspaces[1], "rev-parse", "HEAD"); got != initialRevision {
		t.Fatalf("worktree 2 moved to %s", got)
	}
	status, err := manager.Status(ctx, workspaces[1])
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != "" {
		t.Fatalf("worktree 2 is dirty: %s", status)
	}

	if tip := gitRun(t, origin, "rev-parse", "main"); tip != initialRevision {
		t.Fatalf("origin main moved to %s", tip)
	}

	if err := manager.CleanupWorktree(ctx, 1, workspaces[0], initialRevision); !errors.Is(err, agentgit.ErrUnpushedChanges) {
		t.Fatalf("cleanup of unpushed commit: got %v, want ErrUnpushedChanges", err)
	}
	if err := manager.CleanupWorktree(ctx, 1, workspaces[1], initialRevision); err != nil {
		t.Fatalf("cleanup of clean worktree: %v", err)
	}
	if _, err := os.Stat(workspaces[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree 2 still exists: %v", err)
	}
}

func TestDirtyWorktreeRefusesCleanup(t *testing.T) {
	origin, initialRevision, base := newOrigin(t)
	manager, workspaceRoot := newManager(t, base)
	ctx := context.Background()
	if _, err := manager.EnsureMirror(ctx, 2, origin); err != nil {
		t.Fatal(err)
	}

	workspace := filepath.Join(workspaceRoot, "run-dirty")
	if err := manager.AddWorktree(ctx, 2, workspace, "agent/issue-9", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "dirty.txt"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.CleanupWorktree(ctx, 2, workspace, initialRevision); !errors.Is(err, agentgit.ErrDirtyWorkspace) {
		t.Fatalf("got %v, want ErrDirtyWorkspace", err)
	}
	if err := os.Remove(filepath.Join(workspace, "dirty.txt")); err != nil {
		t.Fatal(err)
	}
	if err := manager.CleanupWorktree(ctx, 2, workspace, initialRevision); err != nil {
		t.Fatalf("cleanup after removing the change: %v", err)
	}
}

func TestEnsureMirrorRefreshes(t *testing.T) {
	origin, _, base := newOrigin(t)
	manager, _ := newManager(t, base)
	ctx := context.Background()
	mirror, err := manager.EnsureMirror(ctx, 3, origin)
	if err != nil {
		t.Fatal(err)
	}

	seed := filepath.Join(base, "seed")
	if err := os.WriteFile(filepath.Join(seed, "second.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "second")
	gitRun(t, seed, "push", "origin", "main")
	newTip := gitRun(t, seed, "rev-parse", "HEAD")

	if _, err := manager.EnsureMirror(ctx, 3, origin); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// The cache tracks branches under refs/remotes/origin so a live worktree can
	// never block a refresh; that tracking ref is what worktrees are based on.
	if got := gitRun(t, mirror, "rev-parse", "refs/remotes/origin/main"); got != newTip {
		t.Fatalf("mirror origin/main = %s, want %s", got, newTip)
	}
}

func TestPushedBranchReachesOriginAndSurvivesRefresh(t *testing.T) {
	origin, initialRevision, base := newOrigin(t)
	manager, workspaceRoot := newManager(t, base)
	ctx := context.Background()
	if _, err := manager.EnsureMirror(ctx, 4, origin); err != nil {
		t.Fatal(err)
	}

	workspace := filepath.Join(workspaceRoot, "run-push")
	if err := manager.AddWorktree(ctx, 4, workspace, "agent/issue-42", "main"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "change.txt"), []byte("pushed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddAll(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(ctx, workspace, "Issue #42: change")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Push(ctx, workspace, "agent/issue-42"); err != nil {
		t.Fatalf("Push from a mirror worktree: %v", err)
	}

	if tip := gitRun(t, origin, "rev-parse", "agent/issue-42"); tip != commit {
		t.Fatalf("origin branch = %s, want %s", tip, commit)
	}
	if tip := gitRun(t, origin, "rev-parse", "main"); tip != initialRevision {
		t.Fatalf("the base branch moved to %s", tip)
	}

	// A refresh while the branch is checked out in a worktree must still work.
	if _, err := manager.EnsureMirror(ctx, 4, origin); err != nil {
		t.Fatalf("refresh after push: %v", err)
	}
}

func TestParsePorcelainPaths(t *testing.T) {
	status := " M internal/api/router.go\n?? new dir/file.txt\nR  old.txt -> renamed.txt\n"
	paths := agentgit.ParsePorcelainPaths(status)
	want := []string{"internal/api/router.go", "new dir/file.txt", "renamed.txt"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v", paths)
	}
	for index := range want {
		if paths[index] != want[index] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
}

func TestResolveWorkspacePathGuardsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	path, err := agentgit.ResolveWorkspacePath(root, "018f2e4a-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatalf("ResolveWorkspacePath: %v", err)
	}
	if !strings.HasPrefix(path, root) {
		t.Fatalf("path %q is not under %q", path, root)
	}
	for _, bad := range []string{"", ".", "..", "../escape", `..\escape`, "a/b", `a\b`} {
		if _, err := agentgit.ResolveWorkspacePath(root, bad); !errors.Is(err, agentgit.ErrInvalidRunID) {
			t.Errorf("run id %q: got %v, want ErrInvalidRunID", bad, err)
		}
	}
	if err := agentgit.EnsureWithin(root, filepath.Join(root, "..", "elsewhere")); !errors.Is(err, agentgit.ErrOutsideWorkspaceRoot) {
		t.Errorf("outside path: got %v", err)
	}
}
