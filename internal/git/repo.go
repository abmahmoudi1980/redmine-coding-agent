package git

import (
	"bytes"
	"context"
	"encoding/base64"
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
	authHeader    string
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

// SetHTTPAuth configures HTTP basic credentials for network operations. The
// header is passed through the child environment, never argv or a repository
// config file, and callers must register the secret with the redactor (FR-056).
func (m *Manager) SetHTTPAuth(username, password string) {
	if username == "" && password == "" {
		m.authHeader = ""
		return
	}
	credentials := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	m.authHeader = "Authorization: Basic " + credentials
}

// WorktreeGitDir is the worktree's git directory, recorded so the working copy
// can be audited after the fact.
func (m *Manager) WorktreeGitDir(ctx context.Context, workspace string) (string, error) {
	out, err := m.git(ctx, workspace, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", errors.New("git: worktree git directory is empty")
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("git: resolve worktree git directory: %w", err)
	}
	return absolute, nil
}

// MirrorPath is the bare mirror cache path for a repository.
func (m *Manager) MirrorPath(repositoryID int64) (string, error) {
	if repositoryID <= 0 {
		return "", fmt.Errorf("git: invalid repository id %d", repositoryID)
	}
	return filepath.Join(m.mirrorsDir, strconv.FormatInt(repositoryID, 10)+".git"), nil
}

// EnsureMirror clones a bare mirror the first time and refreshes it afterwards.
// The repository lock is held across the fetch so two runs cannot race.
//
// The cache is a mirror for fetching but a normal remote for pushing: a
// `--mirror` clone cannot push an explicit refspec, and mirroring into
// refs/heads/* fails as soon as a branch is checked out in a live worktree. The
// fetch refspec therefore targets refs/remotes/origin/* only.
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
		if err := m.normalizeMirrorConfig(ctx, mirror); err != nil {
			return "", fmt.Errorf("git: prepare mirror %d: %w", repositoryID, err)
		}
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
	if err := m.normalizeMirrorConfig(ctx, mirror); err != nil {
		return "", fmt.Errorf("git: prepare mirror %d: %w", repositoryID, err)
	}
	if _, err := m.git(ctx, mirror, "fetch", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("git: prime mirror %d: %w", repositoryID, err)
	}
	return mirror, nil
}

// normalizeMirrorConfig turns the clone from a pushing mirror into a cache that
// tracks branches under refs/remotes/origin, which is what AddWorktree and Push
// expect. It is idempotent, so an older cache is upgraded on the next refresh.
func (m *Manager) normalizeMirrorConfig(ctx context.Context, mirror string) error {
	// The key is absent once normalized; a failing unset is expected.
	_, _ = m.git(ctx, mirror, "config", "--unset-all", "remote.origin.mirror")
	if _, err := m.git(ctx, mirror, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return err
	}
	return nil
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

	base := m.resolveBaseRef(ctx, mirror, baseBranch)
	if _, err := m.git(ctx, mirror, "worktree", "add", "-b", branch, workspace, base); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("%w: %s", ErrBranchExists, branch)
		}
		return fmt.Errorf("git: add worktree: %w", err)
	}
	return nil
}

// resolveBaseRef prefers the freshly fetched remote-tracking branch, falling
// back to a local branch when the mirror has no tracking ref for it.
func (m *Manager) resolveBaseRef(ctx context.Context, mirror, baseBranch string) string {
	tracking := "refs/remotes/origin/" + baseBranch
	if _, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet", tracking); err == nil {
		return tracking
	}
	return baseBranch
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

// CheckRemote proves a remote is readable without changing local state. The
// doctor and health checks use it; nothing is cloned, fetched, or written.
func (m *Manager) CheckRemote(ctx context.Context, remoteURL string) (int, error) {
	if strings.TrimSpace(remoteURL) == "" {
		return 0, errors.New("git: remote URL is required")
	}
	out, err := m.git(ctx, "", "ls-remote", "--heads", remoteURL)
	if err != nil {
		return 0, err
	}
	heads := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			heads++
		}
	}
	return heads, nil
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

// AttemptBranch is the branch name for one attempt. Attempt 1 uses the issue
// branch unchanged; a retry that must not overwrite an earlier attempt gets a
// stable attempt suffix (FR-052, R18).
func AttemptBranch(issueBranch string, attempt int) string {
	if attempt <= 1 || strings.TrimSpace(issueBranch) == "" {
		return issueBranch
	}
	return fmt.Sprintf("%s-r%d", issueBranch, attempt)
}

// BranchPlan is the outcome of retry branch planning. Reused is true only when
// the previous attempt left nothing a retry could overwrite.
type BranchPlan struct {
	Branch       string
	Attempt      int
	Reused       bool
	Superseded   bool
	LocalExists  bool
	RemoteExists bool
	BaseRevision string
}

// PlanAttemptBranch decides which branch a retry may use. The prior attempt's
// branch is reused only when it was never pushed, no local branch still holds
// it, and the base revision has not moved; otherwise a suffixed branch is
// chosen. There is no force-push path anywhere in this package (FR-041).
func (m *Manager) PlanAttemptBranch(ctx context.Context, repositoryID int64, baseBranch, issueBranch string, attempt int, priorPushed bool, priorBaseRevision string) (BranchPlan, error) {
	if strings.TrimSpace(issueBranch) == "" {
		return BranchPlan{}, errors.New("git: branch is required")
	}
	if attempt < 1 {
		attempt = 1
	}
	plan := BranchPlan{Branch: issueBranch, Attempt: attempt}

	mirror, err := m.MirrorPath(repositoryID)
	if err != nil {
		return plan, err
	}
	unlock := m.locks.Lock(repositoryID)
	defer unlock()

	if _, statErr := os.Stat(filepath.Join(mirror, "HEAD")); statErr != nil {
		// No cache to inspect: never risk the previous attempt's work.
		if attempt > 1 && priorPushed {
			plan.Branch = AttemptBranch(issueBranch, attempt)
			plan.Superseded = true
		}
		return plan, nil
	}

	plan.BaseRevision = m.resolveRevision(ctx, mirror, m.resolveBaseRef(ctx, mirror, baseBranch))
	plan.LocalExists = m.mirrorRefExists(ctx, mirror, "refs/heads/"+issueBranch)
	plan.RemoteExists = m.mirrorRefExists(ctx, mirror, "refs/remotes/origin/"+issueBranch)

	sameBase := plan.BaseRevision != "" && plan.BaseRevision == priorBaseRevision
	if attempt == 1 || (!priorPushed && !plan.LocalExists && !plan.RemoteExists && sameBase) {
		plan.Reused = true
		return plan, nil
	}

	for next := attempt; next < attempt+50; next++ {
		candidate := AttemptBranch(issueBranch, next)
		if m.mirrorRefExists(ctx, mirror, "refs/heads/"+candidate) ||
			m.mirrorRefExists(ctx, mirror, "refs/remotes/origin/"+candidate) {
			continue
		}
		plan.Branch = candidate
		plan.Superseded = true
		return plan, nil
	}
	return plan, fmt.Errorf("git: no free attempt branch for %s after attempt %d", issueBranch, attempt)
}

func (m *Manager) mirrorRefExists(ctx context.Context, mirror, ref string) bool {
	_, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func (m *Manager) resolveRevision(ctx context.Context, mirror, ref string) string {
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	out, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
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
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if m.authHeader != "" {
		command.Env = append(command.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+m.authHeader,
		)
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
