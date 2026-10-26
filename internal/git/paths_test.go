package git_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentgit "github.com/systemgroup/redmine-coding-agent/internal/git"
)

// TestWorkspacePathsSurviveSpacesAndUnicode is the cross-platform regression
// guard for R9: an operator may install under "C:\Program Files\…" or a home
// directory with a non-ASCII name, and neither may break containment.
func TestWorkspacePathsSurviveSpacesAndUnicode(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace root", "کارگاه پرداخت")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("create root: %v", err)
	}

	path, err := agentgit.ResolveWorkspacePath(root, "018f2e4a-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatalf("ResolveWorkspacePath: %v", err)
	}
	if !strings.Contains(path, "کارگاه پرداخت") {
		t.Fatalf("path %q lost the non-ASCII segment", path)
	}
	if err := agentgit.EnsureWithin(root, path); err != nil {
		t.Fatalf("EnsureWithin rejected a workspace inside the root: %v", err)
	}

	// A sibling whose name only shares a prefix must still be rejected.
	outside := filepath.Join(base, "workspace root", "کارگاه پرداخت-escape")
	if err := agentgit.EnsureWithin(root, outside); !errors.Is(err, agentgit.ErrOutsideWorkspaceRoot) {
		t.Fatalf("EnsureWithin(%q) = %v, want ErrOutsideWorkspaceRoot", outside, err)
	}

	// The default mirror directory is a sibling of the root, whatever the name.
	mirrors := agentgit.DefaultMirrorsDir(root)
	if filepath.Base(mirrors) != "repos" {
		t.Fatalf("DefaultMirrorsDir = %q", mirrors)
	}
	if !filepath.IsAbs(mirrors) {
		t.Fatalf("DefaultMirrorsDir is relative: %q", mirrors)
	}
}

func TestEnsureWithinAcceptsTheRootItself(t *testing.T) {
	root := filepath.Join(t.TempDir(), "space root", "پوشه")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("create root: %v", err)
	}
	if err := agentgit.EnsureWithin(root, root); err != nil {
		t.Fatalf("EnsureWithin(root, root) = %v", err)
	}
}
