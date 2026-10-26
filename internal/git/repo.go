package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrBranchExists reports that the branch for this attempt is already taken.
var ErrBranchExists = errors.New("git: branch already exists")

// Manager owns mirror caches and worktrees for one worker process.
type Manager struct {
	gitBin        string
	mirrorsDir    string
	workspaceRoot string
	locks         *Locks
}

// NewManager builds a manager. mirrorsDir is normally DefaultMirrorsDir(root).
func NewManager(mirrorsDir, workspaceRoot string) *Manager {
	if strings.TrimSpace(mirrorsDir) == "" {
		mirrorsDir = DefaultMirrorsDir(workspaceRoot)
	}
	return &Manager{
		gitBin:        "git",
		mirrorsDir:    mirrorsDir,
		workspaceRoot: workspaceRoot,
		locks:         NewLocks(),
	}
}

// WorkspaceRoot is the configured root every worktree must stay inside.
func (m *Manager) WorkspaceRoot() string { return m.workspaceRoot }

// MirrorPath is the bare mirror cache path for a repository.
func (m *Manager) MirrorPath(repositoryID int64) (string, error) {
	if repositoryID <= 0 {
		return "", fmt.Errorf("git: invalid repository id %d", repositoryID)
	}
	return filepath.Join(m.mirrorsDir, strconv.FormatInt(repositoryID, 10)+".git"), nil
}

// EnsureMirror clones a bare mirror the first time and refreshes it afterwards.
// The repository lock is held across the fetch so two runs cannot race.
func (m *Manager) EnsureMirror(ctx context.Context, repositoryID int64, remoteURL string) (string, error) {
	if strings.TrimSpace(remoteURL) == "" {
		return "", errors.New("git: remote URL is required")
	}
	mirror, err := m.MirrorPath(repositoryID)
	if err != nil {
		return "", err
	}
	unlock := m.locks.Lock(repositoryID)
	defer unlock()

	if _, statErr := os.Stat(filepath.Join(mirror, "HEAD")); statErr == nil {
		if _, err := m.git(ctx, mirror, "fetch", "--prune", "origin"); err != nil {
			return "", fmt.Errorf("git: refresh mirror %d: %w", repositoryID, err)
		}
		return mirror, nil
	}
	if err := os.MkdirAll(m.mirrorsDir, 0o750); err != nil {
		return "", fmt.Errorf("git: create mirror dir: %w", err)
	}
	// A partial directory is disposable; the origin is the source of truth.
	if _, statErr := os.Stat(mirror); statErr == nil {
		if err := os.RemoveAll(mirror); err != nil {
			return "", fmt.Errorf("git: remove partial mirror: %w", err)
		}
	}
	if _, err := m.git(ctx, "", "clone", "--mirror", remoteURL, mirror); err != nil {
		return "", fmt.Errorf("git: clone mirror %d: %w", repositoryID, err)
	}
	return mirror, nil
}

// AddWorktree creates a dedicated branch and checkout for one run.
func (m *Manager) AddWorktree(ctx context.Context, repositoryID int64, workspace, branch, baseBranch string) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("git: branch is required")
	}
	if strings.TrimSpace(baseBranch) == "" {
		return errors.New("git: base branch is required")
	}
	if err := EnsureWithin(m.workspaceRoot, workspace); err != nil {
		return err
	}
	mirror, err := m.MirrorPath(repositoryID)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(mirror, "HEAD")); statErr != nil {
		return fmt.Errorf("git: mirror %d is not prepared: %w", repositoryID, statErr)
	}

	unlock := m.locks.Lock(repositoryID)
	defer unlock()

	if _, statErr := os.Stat(workspace); statErr == nil {
		return fmt.Errorf("%w: %s", ErrWorkspaceExists, workspace)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("git: inspect workspace: %w", statErr)
	}

	if _, err := m.git(ctx, mirror, "worktree", "add", "-b", branch, workspace, baseBranch); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("%w: %s", ErrBranchExists, branch)
		}
		return fmt.Errorf("git: add worktree: %w", err)
	}
	return nil
}

// Head is the fully resolved commit at the workspace HEAD.
func (m *Manager) Head(ctx context.Context, workspace string) (string, error) {
	out, err := m.git(ctx, workspace, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", errors.New("git: HEAD is empty")
	}
	return out, nil
}

// Status returns `git status --porcelain` output ("" means clean).
func (m *Manager) Status(ctx context.Context, workspace string) (string, error) {
	return m.git(ctx, workspace, "-c", "core.quotepath=false", "status", "--porcelain")
}

// ChangedPaths is the parsed, de-duplicated set of paths touched by the run.
func (m *Manager) ChangedPaths(ctx context.Context, workspace string) ([]string, error) {
	status, err := m.Status(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return ParsePorcelainPaths(status), nil
}

// DiffNameStatus returns `git diff --name-status <from>` lines. An empty from
// compares against HEAD.
func (m *Manager) DiffNameStatus(ctx context.Context, workspace, from string) ([]string, error) {
	if from == "" {
		from = "HEAD"
	}
	out, err := m.git(ctx, workspace, "-c", "core.quotepath=false", "diff", "--name-status", from)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	for index := range lines {
		lines[index] = strings.TrimSuffix(lines[index], "\r")
	}
	return lines, nil
}

// AddAll stages every change in the worktree.
func (m *Manager) AddAll(ctx context.Context, workspace string) error {
	_, err := m.git(ctx, workspace, "add", "-A")
	return err
}

// Commit creates a commit with the worker's identity and returns its SHA.
func (m *Manager) Commit(ctx context.Context, workspace, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", errors.New("git: commit message is required")
	}
	_, err := m.git(ctx, workspace,
		"-c", "user.name=RedmineCodingAgent",
		"-c", "user.email=redmine-agent@localhost",
		"commit", "-m", message,
	)
	if err != nil {
		return "", err
	}
	return m.Head(ctx, workspace)
}

// Push sends the branch to origin. There is no force or force-with-lease path.
func (m *Manager) Push(ctx context.Context, workspace, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("git: branch is required")
	}
	_, err := m.git(ctx, workspace, "push", "origin", branch)
	if err != nil {
		return fmt.Errorf("git: push %s: %w", branch, err)
	}
	return nil
}

// BranchExists reports local and remote existence of a branch.
func (m *Manager) BranchExists(ctx context.Context, workspace, branch string) (bool, bool, error) {
	if strings.TrimSpace(branch) == "" {
		return false, false, errors.New("git: branch is required")
	}
	local := true
	if _, err := m.git(ctx, workspace, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		local = false
	}
	out, err := m.git(ctx, workspace, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return local, false, fmt.Errorf("git: list remote branches: %w", err)
	}
	remote := strings.Contains(out, "refs/heads/"+branch)
	return local, remote, nil
}

// HasUnpushedCommits reports commits on the current branch that no remote
// branch contains. baseRevision is the commit the worktree was created from,
// so an untouched branch is correctly reported as nothing-to-lose even though
// a mirror clone has no remote-tracking refs.
func (m *Manager) HasUnpushedCommits(ctx context.Context, workspace, baseRevision string) (int, error) {
	head, err := m.Head(ctx, workspace)
	if err != nil {
		return 0, err
	}
	if baseRevision != "" && head == baseRevision {
		return 0, nil
	}

	branch, err := m.currentBranch(ctx, workspace)
	if err != nil {
		return 0, err
	}
	remoteTip := ""
	if branch != "" {
		out, err := m.git(ctx, workspace, "ls-remote", "--heads", "origin", branch)
		if err != nil {
			return 0, fmt.Errorf("git: list remote branches: %w", err)
		}
		fields := strings.Fields(out)
		if len(fields) >= 1 {
			remoteTip = fields[0]
		}
	}
	if remoteTip != "" && remoteTip == head {
		return 0, nil
	}

	from := remoteTip
	if from == "" {
		from = baseRevision
	}
	if from == "" {
		from = "HEAD"
	}
	out, err := m.git(ctx, workspace, "rev-list", "--count", from+"..HEAD")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("git: parse unpushed count %q: %w", out, err)
	}
	return count, nil
}

func (m *Manager) currentBranch(ctx context.Context, workspace string) (string, error) {
	out, err := m.git(ctx, workspace, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "HEAD" {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// CleanupWorktree removes a worktree only when no work would be lost.
func (m *Manager) CleanupWorktree(ctx context.Context, repositoryID int64, workspace, baseRevision string) error {
	if err := EnsureWithin(m.workspaceRoot, workspace); err != nil {
		return err
	}
	mirror, err := m.MirrorPath(repositoryID)
	if err != nil {
		return err
	}

	if _, statErr := os.Stat(workspace); errors.Is(statErr, os.ErrNotExist) {
		_, _ = m.git(ctx, mirror, "worktree", "prune")
		return nil
	}

	status, err := m.Status(ctx, workspace)
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%w: %s", ErrDirtyWorkspace, workspace)
	}
	unpushed, err := m.HasUnpushedCommits(ctx, workspace, baseRevision)
	if err != nil {
		return err
	}
	if unpushed > 0 {
		return fmt.Errorf("%w: %d commit(s) in %s", ErrUnpushedChanges, unpushed, workspace)
	}

	unlock := m.locks.Lock(repositoryID)
	defer unlock()
	if _, err := m.git(ctx, mirror, "worktree", "remove", workspace); err != nil {
		return fmt.Errorf("git: remove worktree: %w", err)
	}
	return nil
}

// ParsePorcelainPaths extracts paths from `git status --porcelain` output.
func ParsePorcelainPaths(status string) []string {
	if strings.TrimSpace(status) == "" {
		return nil
	}
	var paths []string
	seen := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimRight(status, "\r\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if arrow := strings.Index(path, " -> "); arrow >= 0 {
			path = path[arrow+4:]
		}
		path = unquotePath(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

func unquotePath(path string) string {
	if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
		if unquoted, err := strconv.Unquote(path); err == nil {
			return unquoted
		}
	}
	return path
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	// #nosec G204 -- the binary is fixed and args are built by this package, not callers' text.
	command := exec.CommandContext(ctx, m.gitBin, args...)
	if dir != "" {
		command.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}
