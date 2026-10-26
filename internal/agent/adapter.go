// Package agent isolates coding-agent implementations from the orchestrator.
// Everything an adapter returns about the agent's work is untrusted input.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/git"
)

// Status is the observed outcome of one invocation.
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusTimedOut  Status = "timed_out"
	StatusCancelled Status = "cancelled"
	StatusBlocked   Status = "blocked"
)

// Config is the non-secret adapter configuration used for probing.
type Config struct {
	Command       string
	Model         string
	AgentName     string
	WorkspaceRoot string
}

// ProbeResult reports whether the agent executable is usable.
type ProbeResult struct {
	Available bool
	Version   string
	Detail    string
}

// Adapter performs exactly one non-interactive invocation in one workspace.
type Adapter interface {
	Name() string
	Probe(ctx context.Context, cfg Config) (ProbeResult, error)
	Run(ctx context.Context, req Request) (Result, error)
}

// Request is one invocation. WorkspaceDir must be the process working directory.
type Request struct {
	RunID        string
	Attempt      int
	IssueID      int
	WorkspaceDir string
	SandboxDir   string
	Prompt       Prompt

	Model        string
	AgentName    string
	Timeout      time.Duration
	EnvAllowlist []string
	PolicyFile   string
}

// Prompt is the exact text sent to the agent, hashed before the call.
type Prompt struct {
	Text       string
	SHA256     string
	Transcript string
}

// Summary is the parsed `## RESULT` block. Every field is a claim.
type Summary struct {
	Status    string
	Files     []string
	ChecksRun string
	Blockers  string
}

// ActivityEvent is a best-effort progress line for the UI.
type ActivityEvent struct {
	At      time.Time
	Kind    string
	Message string
	Data    map[string]any
}

// Result is the only channel through which agent claims reach the worker.
type Result struct {
	Status     Status
	ExitCode   int
	TimedOut   bool
	StartedAt  time.Time
	EndedAt    time.Time
	Transcript string

	ReportedStatus   string
	Summary          Summary
	Activity         []ActivityEvent
	PolicyViolations []string
}

// Registry maps the configured adapter name to its implementation.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// ErrUnknownAdapter names a configuration mistake rather than a run failure.
var ErrUnknownAdapter = errors.New("agent: unknown adapter")

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register adds an adapter by Name.
func (r *Registry) Register(adapter Adapter) error {
	if adapter == nil {
		return errors.New("agent: nil adapter")
	}
	name := strings.TrimSpace(adapter.Name())
	if name == "" {
		return errors.New("agent: adapter name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adapters == nil {
		r.adapters = make(map[string]Adapter)
	}
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("agent: adapter %q is already registered", name)
	}
	r.adapters[name] = adapter
	return nil
}

// Lookup resolves an adapter or returns ErrUnknownAdapter.
func (r *Registry) Lookup(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAdapter, name)
	}
	return adapter, nil
}

// Names lists the registered adapters in stable order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var (
	// ErrWorkspaceMissing is A1: the workspace must already exist.
	ErrWorkspaceMissing = errors.New("agent: workspace directory does not exist")
	// ErrWorkspaceOutsideRoot is A1: the workspace must be inside the root.
	ErrWorkspaceOutsideRoot = errors.New("agent: workspace is outside the workspace root")
	// ErrSandboxOutsideRoot is A2: the sandbox must also live inside the root.
	ErrSandboxOutsideRoot = errors.New("agent: sandbox is outside the workspace root")
	// ErrCredentialInEnv is A3: credentials never reach the child.
	ErrCredentialInEnv = errors.New("agent: environment allowlist contains a credential-like variable")
)

// SandboxLayout is the redirected HOME/XDG/TMP tree inside the sandbox.
type SandboxLayout struct {
	Dir    string
	Home   string
	Config string
	Data   string
	Cache  string
	Temp   string
	Policy string
}

// PrepareSandbox creates the directories A2 requires.
func PrepareSandbox(sandboxDir string) (SandboxLayout, error) {
	if strings.TrimSpace(sandboxDir) == "" {
		return SandboxLayout{}, errors.New("agent: sandbox directory is required")
	}
	abs, err := filepath.Abs(sandboxDir)
	if err != nil {
		return SandboxLayout{}, fmt.Errorf("agent: resolve sandbox: %w", err)
	}
	layout := SandboxLayout{
		Dir:    abs,
		Home:   filepath.Join(abs, "home"),
		Config: filepath.Join(abs, "xdg-config"),
		Data:   filepath.Join(abs, "xdg-data"),
		Cache:  filepath.Join(abs, "xdg-cache"),
		Temp:   filepath.Join(abs, "tmp"),
		Policy: filepath.Join(abs, "policy"),
	}
	for _, dir := range []string{layout.Dir, layout.Home, layout.Config, layout.Data, layout.Cache, layout.Temp, layout.Policy} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return SandboxLayout{}, fmt.Errorf("agent: create sandbox dir %s: %w", dir, err)
		}
	}
	return layout, nil
}

// Redirects are the variables the adapter must set for the child.
func (l SandboxLayout) Redirects() map[string]string {
	return map[string]string{
		"HOME":            l.Home,
		"XDG_CONFIG_HOME": l.Config,
		"XDG_DATA_HOME":   l.Data,
		"XDG_CACHE_HOME":  l.Cache,
		"TMPDIR":          l.Temp,
		"TEMP":            l.Temp,
		"TMP":             l.Temp,
	}
}

// ValidateRequest enforces A1–A3 and creates the sandbox tree. It is pure
// filesystem work: no agent process is started.
func ValidateRequest(req Request, workspaceRoot string) (SandboxLayout, error) {
	if strings.TrimSpace(req.RunID) == "" {
		return SandboxLayout{}, errors.New("agent: run id is required")
	}
	if req.Attempt < 1 {
		return SandboxLayout{}, errors.New("agent: attempt must be at least 1")
	}
	if strings.TrimSpace(req.WorkspaceDir) == "" {
		return SandboxLayout{}, ErrWorkspaceMissing
	}
	info, err := os.Stat(req.WorkspaceDir)
	if errors.Is(err, os.ErrNotExist) {
		return SandboxLayout{}, fmt.Errorf("%w: %s", ErrWorkspaceMissing, req.WorkspaceDir)
	}
	if err != nil {
		return SandboxLayout{}, fmt.Errorf("agent: inspect workspace: %w", err)
	}
	if !info.IsDir() {
		return SandboxLayout{}, fmt.Errorf("%w: %s is not a directory", ErrWorkspaceMissing, req.WorkspaceDir)
	}
	if err := git.EnsureWithin(workspaceRoot, req.WorkspaceDir); err != nil {
		return SandboxLayout{}, fmt.Errorf("%w: %v", ErrWorkspaceOutsideRoot, err)
	}
	if err := git.EnsureWithin(workspaceRoot, req.SandboxDir); err != nil {
		return SandboxLayout{}, fmt.Errorf("%w: %v", ErrSandboxOutsideRoot, err)
	}
	layout, err := PrepareSandbox(req.SandboxDir)
	if err != nil {
		return SandboxLayout{}, err
	}
	if req.PolicyFile != "" {
		if err := git.EnsureWithin(layout.Dir, req.PolicyFile); err != nil {
			return SandboxLayout{}, fmt.Errorf("agent: policy file must be inside the sandbox: %w", err)
		}
	}
	if _, err := BuildEnvironment(req, layout); err != nil {
		return SandboxLayout{}, err
	}
	return layout, nil
}

// BuildEnvironment returns exactly the allowlist plus the sandbox redirects.
// Credential-shaped variables are refused rather than silently scrubbed.
func BuildEnvironment(req Request, layout SandboxLayout) ([]string, error) {
	redirects := layout.Redirects()
	seen := make(map[string]bool, len(req.EnvAllowlist)+len(redirects))
	out := make([]string, 0, len(req.EnvAllowlist)+len(redirects))

	for _, entry := range req.EnvAllowlist {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("agent: environment entry %q must be KEY=VALUE", entry)
		}
		if credentialLike(key) {
			return nil, fmt.Errorf("%w: %s", ErrCredentialInEnv, key)
		}
		if _, overridden := redirects[key]; overridden {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key+"="+value)
	}

	keys := make([]string, 0, len(redirects))
	for key := range redirects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key+"="+redirects[key])
	}
	return out, nil
}

func credentialLike(key string) bool {
	normalized := strings.ToUpper(key)
	for _, marker := range []string{"TOKEN", "API_KEY", "APIKEY", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// HashPrompt is the sha256 recorded before an invocation.
func HashPrompt(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// NewPrompt hashes the text and binds it to its transcript path.
func NewPrompt(text, transcriptPath string) Prompt {
	return Prompt{Text: text, SHA256: HashPrompt(text), Transcript: transcriptPath}
}
