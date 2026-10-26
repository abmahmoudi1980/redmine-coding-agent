// Package git wraps the git CLI for mirror caches and per-run worktrees.
// It deliberately exposes no merge, rebase, or force-push operation (FR-041).
package git

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

var (
	// ErrOutsideWorkspaceRoot guards every path that becomes a worktree.
	ErrOutsideWorkspaceRoot = errors.New("git: path is outside the workspace root")
	// ErrInvalidRunID rejects a run id that could traverse directories.
	ErrInvalidRunID = errors.New("git: invalid run id")
	// ErrWorkspaceExists refuses to reuse an existing directory.
	ErrWorkspaceExists = errors.New("git: workspace path already exists")
	// ErrDirtyWorkspace refuses cleanup while uncommitted work exists.
	ErrDirtyWorkspace = errors.New("git: workspace has uncommitted changes")
	// ErrUnpushedChanges refuses cleanup while local commits were never pushed.
	ErrUnpushedChanges = errors.New("git: workspace has unpushed commits")
)

// DefaultMirrorsDir is the mirror cache next to the workspace root.
func DefaultMirrorsDir(workspaceRoot string) string {
	cleaned := filepath.Clean(workspaceRoot)
	return filepath.Join(filepath.Dir(cleaned), "repos")
}

// ResolveWorkspacePath builds <workspace_root>/<run_id>/ and proves it stays
// inside the configured root.
func ResolveWorkspacePath(workspaceRoot, runID string) (string, error) {
	if workspaceRoot == "" {
		return "", errors.New("git: workspace root is required")
	}
	if err := validateRunID(runID); err != nil {
		return "", err
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("git: resolve workspace root: %w", err)
	}
	path := filepath.Join(root, runID)
	if err := EnsureWithin(root, path); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureWithin reports ErrOutsideWorkspaceRoot when path escapes root.
func EnsureWithin(root, path string) error {
	if root == "" || path == "" {
		return ErrOutsideWorkspaceRoot
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("git: resolve root: %w", err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("git: resolve path: %w", err)
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		return ErrOutsideWorkspaceRoot
	}
	if relative == "." {
		return nil
	}
	if filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrOutsideWorkspaceRoot
	}
	return nil
}

func validateRunID(runID string) error {
	trimmed := strings.TrimSpace(runID)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return ErrInvalidRunID
	}
	if filepath.Base(trimmed) != trimmed {
		return ErrInvalidRunID
	}
	if strings.ContainsAny(trimmed, `/\:`) {
		return ErrInvalidRunID
	}
	return nil
}
