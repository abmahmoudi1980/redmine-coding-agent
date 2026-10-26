package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PolicyVersion is bumped whenever the containment policy changes, so the
// agent configuration identifier recorded on a run stays meaningful.
const PolicyVersion = 1

// Policy is the worker-authored permission document handed to the agent. It is
// written into a read-only sandbox subdirectory before each invocation and is
// never derived from issue content (R9 layer 2).
type Policy struct {
	Version    int       `json:"version"`
	Workspace  string    `json:"workspace"`
	SandboxDir string    `json:"sandboxDir"`
	Deny       DenyList  `json:"deny"`
	Allow      AllowList `json:"allow"`
}

// DenyList names the operations the agent must not perform.
type DenyList struct {
	ShellOutsideWorkspace bool     `json:"shellOutsideWorkspace"`
	NetworkFetch          bool     `json:"networkFetch"`
	AbsolutePathWrites    bool     `json:"absolutePathWrites"`
	ReadPaths             []string `json:"readPaths"`
	WritePaths            []string `json:"writePaths"`
}

// AllowList narrows the agent to the workspace and its sandbox.
type AllowList struct {
	Shell []string `json:"shell"`
	Write []string `json:"write"`
	Read  []string `json:"read"`
}

// DefaultPolicy denies everything the worker cannot verify is contained.
func DefaultPolicy(workspaceDir, sandboxDir, dataDir string) Policy {
	denied := make([]string, 0, 3)
	if strings.TrimSpace(dataDir) != "" {
		denied = append(denied, filepath.Clean(dataDir))
	}
	return Policy{
		Version:    PolicyVersion,
		Workspace:  filepath.Clean(workspaceDir),
		SandboxDir: filepath.Clean(sandboxDir),
		Deny: DenyList{
			ShellOutsideWorkspace: true,
			NetworkFetch:          true,
			AbsolutePathWrites:    true,
			ReadPaths:             denied,
			WritePaths:            denied,
		},
		Allow: AllowList{
			Shell: []string{filepath.Clean(workspaceDir)},
			Write: []string{filepath.Clean(workspaceDir), filepath.Clean(sandboxDir)},
			Read:  []string{filepath.Clean(workspaceDir), filepath.Clean(sandboxDir)},
		},
	}
}

// WritePolicy renders the policy into the sandbox's read-only policy
// directory and returns its absolute path (contract §7).
func WritePolicy(layout SandboxLayout, policy Policy) (string, error) {
	if strings.TrimSpace(layout.Policy) == "" {
		return "", errors.New("agent: sandbox policy directory is required")
	}
	if err := os.MkdirAll(layout.Policy, 0o700); err != nil {
		return "", fmt.Errorf("agent: create policy directory: %w", err)
	}
	// #nosec G302 -- a directory needs its execute bit to be traversable.
	if err := os.Chmod(layout.Policy, 0o700); err != nil {
		return "", fmt.Errorf("agent: reopen policy directory: %w", err)
	}
	encoded, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return "", fmt.Errorf("agent: encode policy: %w", err)
	}
	path := filepath.Join(layout.Policy, "policy.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o400); err != nil {
		return "", fmt.Errorf("agent: write policy: %w", err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		return "", fmt.Errorf("agent: protect policy: %w", err)
	}
	// #nosec G302 -- the read-only policy directory still has to be traversable.
	if err := os.Chmod(layout.Policy, 0o500); err != nil {
		return "", fmt.Errorf("agent: protect policy directory: %w", err)
	}
	return path, nil
}

// UnprotectSandbox restores write permission on the sandbox tree after the
// agent invocation has ended, so retention and run-directory cleanup can remove
// it. The policy is read-only only while the agent is running.
func UnprotectSandbox(sandboxDir string) error {
	if strings.TrimSpace(sandboxDir) == "" {
		return nil
	}
	root, err := filepath.Abs(sandboxDir)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(root); errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			// #nosec G302 G122 -- a directory needs execute (and, for cleanup, write) for its owner.
			return os.Chmod(path, 0o700)
		}
		// #nosec G122 -- the walk starts at a worker-created sandbox; this only restores ownership.
		return os.Chmod(path, 0o600)
	})
}

// baseEnvironment names the only inherited variables that are safe to pass
// through. Everything else, including credentials, is dropped (A3, FR-056).
var baseEnvironment = []string{
	"PATH",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TZ",
	"TERM",
}

// windowsEnvironment adds the variables Windows process startup needs; without
// SystemRoot a child process cannot resolve DLLs or start a shell. User-profile
// variables are deliberately absent: the sandbox redirects HOME instead.
var windowsEnvironment = []string{
	"SystemRoot",
	"SystemDrive",
	"windir",
	"ComSpec",
	"PATHEXT",
	"PROCESSOR_ARCHITECTURE",
	"NUMBER_OF_PROCESSORS",
}

// EnvironmentAllowlist builds the KEY=VALUE allowlist for one invocation. The
// sandbox redirects in BuildEnvironment take precedence over these values.
func EnvironmentAllowlist(goos string) ([]string, error) {
	names := append([]string{}, baseEnvironment...)
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "windows" {
		names = append(names, windowsEnvironment...)
	}

	allowlist := make([]string, 0, len(names))
	for _, name := range names {
		if credentialLike(name) {
			continue
		}
		value, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		allowlist = append(allowlist, name+"="+value)
	}
	if len(allowlist) == 0 {
		return nil, errors.New("agent: no safe environment variables are available")
	}
	return allowlist, nil
}
