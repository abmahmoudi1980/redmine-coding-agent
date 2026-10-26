package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
)

func TestRegistry(t *testing.T) {
	registry := agent.NewRegistry()
	first := &fakeAdapter{name: "one"}
	if err := registry.Register(first); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(first); err == nil {
		t.Fatal("duplicate registration must fail")
	}
	adapter, err := registry.Lookup("one")
	if err != nil || adapter.Name() != "one" {
		t.Fatalf("Lookup = %v, %v", adapter, err)
	}
	if _, err := registry.Lookup("missing"); err == nil {
		t.Fatal("unknown adapter must fail")
	}
	if names := registry.Names(); len(names) != 1 || names[0] != "one" {
		t.Fatalf("Names = %v", names)
	}
}

func TestValidateRequestPreconditions(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "run-1")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	request := agent.Request{
		RunID:        "run-1",
		Attempt:      1,
		IssueID:      4821,
		WorkspaceDir: workspace,
		SandboxDir:   filepath.Join(workspace, "sandbox"),
		EnvAllowlist: []string{"PATH=" + os.Getenv("PATH")},
	}

	layout, err := agent.ValidateRequest(request, root)
	if err != nil {
		t.Fatalf("ValidateRequest: %v", err)
	}
	for _, dir := range []string{layout.Home, layout.Config, layout.Data, layout.Cache, layout.Temp, layout.Policy} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("sandbox dir %s missing: %v", dir, err)
		}
	}

	missing := request
	missing.WorkspaceDir = filepath.Join(root, "nope")
	if _, err := agent.ValidateRequest(missing, root); err == nil {
		t.Fatal("missing workspace must fail")
	}

	outside := request
	outside.WorkspaceDir = t.TempDir()
	if _, err := agent.ValidateRequest(outside, root); err == nil {
		t.Fatal("workspace outside the root must fail")
	}

	outsideSandbox := request
	outsideSandbox.SandboxDir = filepath.Join(t.TempDir(), "sandbox")
	if _, err := agent.ValidateRequest(outsideSandbox, root); err == nil {
		t.Fatal("sandbox outside the root must fail")
	}
}

func TestBuildEnvironmentIsAllowlistPlusRedirects(t *testing.T) {
	sandbox := t.TempDir()
	layout, err := agent.PrepareSandbox(sandbox)
	if err != nil {
		t.Fatal(err)
	}
	request := agent.Request{
		EnvAllowlist: []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=/root"},
	}
	env, err := agent.BuildEnvironment(request, layout)
	if err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "HOME=/root") {
		t.Fatal("operator HOME leaked into the child environment")
	}
	if !strings.Contains(joined, "HOME="+layout.Home) {
		t.Fatalf("HOME redirect missing: %s", joined)
	}
	for _, key := range []string{"XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "XDG_CACHE_HOME=", "TMPDIR=", "TEMP=", "TMP="} {
		if !strings.Contains(joined, key+layout.Temp) && !strings.Contains(joined, key+layout.Home) &&
			!strings.Contains(joined, key+layout.Config) && !strings.Contains(joined, key+layout.Data) &&
			!strings.Contains(joined, key+layout.Cache) {
			t.Fatalf("%s redirect missing: %s", key, joined)
		}
	}

	request.EnvAllowlist = append(request.EnvAllowlist, "GITLAB_TOKEN=secret")
	if _, err := agent.BuildEnvironment(request, layout); err == nil {
		t.Fatal("credential-like variables must be refused")
	}
}

type fakeAdapter struct{ name string }

func (f *fakeAdapter) Name() string { return f.name }

func (f *fakeAdapter) Probe(_ context.Context, _ agent.Config) (agent.ProbeResult, error) {
	return agent.ProbeResult{Available: true}, nil
}

func (f *fakeAdapter) Run(_ context.Context, _ agent.Request) (agent.Result, error) {
	return agent.Result{Status: agent.StatusSucceeded}, nil
}

func TestInheritHomeKeepsTheOperatorHomeAndStillRedirectsTemp(t *testing.T) {
	layout, err := agent.PrepareSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env, err := agent.BuildEnvironment(agent.Request{InheritHome: true}, layout)
	if err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "HOME=") {
		t.Fatalf("HOME must be inherited, not set: %s", joined)
	}
	if strings.Contains(joined, "XDG_DATA_HOME=") {
		t.Fatalf("XDG data must be inherited, not set: %s", joined)
	}
	if !strings.Contains(joined, "TMPDIR="+layout.Temp) {
		t.Fatalf("TMPDIR must still point into the sandbox: %s", joined)
	}
}

func TestEnvironmentAllowlistForwardsOnlyNamedVariables(t *testing.T) {
	t.Setenv("RCA_PROXY", "http://127.0.0.1:10808")
	t.Setenv("RCA_PROVIDER_API_KEY", "provider-secret-value")

	env, err := agent.EnvironmentAllowlistWith(runtime.GOOS, []string{"RCA_PROXY", "RCA_PROVIDER_API_KEY"})
	if err != nil {
		t.Fatalf("EnvironmentAllowlistWith: %v", err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "RCA_PROXY=http://127.0.0.1:10808") {
		t.Fatalf("the named proxy was not forwarded: %s", joined)
	}
	if !strings.Contains(joined, "RCA_PROVIDER_API_KEY=provider-secret-value") {
		t.Fatalf("an explicitly named credential must be forwarded: %s", joined)
	}
	if strings.Contains(joined, "RCA_UNRELATED") {
		t.Fatalf("unnamed variables must not be forwarded: %s", joined)
	}
}

func TestEnvironmentAllowlistRejectsAMissingNamedVariable(t *testing.T) {
	if _, err := agent.EnvironmentAllowlistWith(runtime.GOOS, []string{"RCA_DOES_NOT_EXIST"}); err == nil {
		t.Fatal("a named but unset variable must be reported, not silently dropped")
	}
}
