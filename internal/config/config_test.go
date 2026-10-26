package config_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
)

const validYAML = `
schema_version: 1
http:
  listen: "127.0.0.1:8080"
data:
  database: "./data/agent.db"
  workspace_root: "./data/workspaces"
  log_dir: "./data/logs"
  retention_days: 7
redmine:
  base_url: "https://redmine.example"
  api_key: "env:REDMINE_TEST_KEY"
  comment_on_finish: true
  update_status: false
gitlab:
  base_url: "https://gitlab.example"
  token: "file:/run/secrets/gitlab_token"
  push_enabled: true
  merge_request_enabled: true
repositories:
  - redmine_project: "billing"
    repository_path: "group/billing-api"
    base_branch: "main"
    is_default: true
    checks:
      - name: "unit-tests"
        command_linux: "go test ./..."
        command_windows: "go test ./..."
        required: true
        timeout_seconds: 900
      - name: "lint"
        command_linux: "golangci-lint run"
        command_windows: "golangci-lint run"
agent:
  adapter: "opencode"
  command: "git"
  model: "anthropic/claude-sonnet-4"
  profile: "restricted"
  timeout_minutes: 45
  max_repairs: 1
concurrency:
  max_active_runs: 1
`

func decodeValid(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return cfg
}

func TestDecodeAndValidate(t *testing.T) {
	cfg := decodeValid(t)
	if cfg.SchemaVersion != 1 {
		t.Fatalf("schema version %d", cfg.SchemaVersion)
	}
	if cfg.Agent.Command != "git" {
		t.Fatalf("agent command %q", cfg.Agent.Command)
	}
	if !cfg.Repositories[0].Checks[0].IsRequired() {
		t.Fatal("explicit required: true lost")
	}
	if got := cfg.Repositories[0].Checks[1].EffectiveTimeoutSeconds(); got != 900 {
		t.Fatalf("default timeout = %d", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDecodeRejectsUnknownKeys(t *testing.T) {
	invalid := validYAML + "\nunknown_key: true\n"
	_, err := config.Decode(strings.NewReader(invalid))
	if err == nil {
		t.Fatal("expected unknown-key rejection")
	}
	if !strings.Contains(err.Error(), "unknown_key") {
		t.Fatalf("error should name the key: %v", err)
	}
}

func TestDecodeRejectsUnsupportedSchema(t *testing.T) {
	_, err := config.Decode(strings.NewReader(strings.Replace(validYAML, "schema_version: 1", "schema_version: 2", 1)))
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("got %v", err)
	}
}

func TestSecretReferenceNeverExposesValue(t *testing.T) {
	const secret = "sup3r-secret-value"
	t.Setenv("REDMINE_TEST_KEY", secret)

	cfg := decodeValid(t)
	value, err := cfg.Redmine.APIKey.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if value != secret {
		t.Fatalf("resolved %q", value)
	}
	formatted := fmt.Sprintf("%v|%+v|%#v|%s", cfg.Redmine.APIKey, cfg.Redmine.APIKey, cfg.Redmine.APIKey, cfg.Redmine.APIKey)
	if strings.Contains(formatted, secret) {
		t.Fatalf("formatting exposed the value: %s", formatted)
	}
	if !strings.Contains(formatted, "env:REDMINE_TEST_KEY") {
		t.Fatalf("formatting should show the reference: %s", formatted)
	}
}

func TestUnresolvedReferenceNamesReferenceOnly(t *testing.T) {
	ref := config.NewSecretRef("env:REDMINE_AGENT_TEST_DOES_NOT_EXIST")
	os.Unsetenv("REDMINE_AGENT_TEST_DOES_NOT_EXIST")
	_, err := ref.Resolve()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "env:REDMINE_AGENT_TEST_DOES_NOT_EXIST") {
		t.Fatalf("error should name the reference: %v", err)
	}
}

func TestValidationRejectsBadConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"empty base branch", func(c *config.Config) { c.Repositories[0].BaseBranch = "" }, "base_branch"},
		{"duplicate mapping", func(c *config.Config) {
			c.Repositories = append(c.Repositories, c.Repositories[0])
		}, "duplicates the mapping"},
		{"two defaults", func(c *config.Config) {
			second := c.Repositories[0]
			second.RepositoryPath = "group/other"
			c.Repositories = append(c.Repositories, second)
		}, "second is_default"},
		{"zero concurrency", func(c *config.Config) { c.Concurrency.MaxActiveRuns = 0 }, "max_active_runs"},
		{"missing agent command", func(c *config.Config) { c.Agent.Command = "definitely-not-a-real-binary-xyz" }, "not executable"},
		{"bad secret scheme", func(c *config.Config) {
			c.Redmine.APIKey = config.NewSecretRef("literal:value")
		}, "unsupported scheme"},
		{"empty http listen", func(c *config.Config) { c.HTTP.Listen = "" }, "http.listen"},
		{"bad base url", func(c *config.Config) { c.GitLab.BaseURL = "not-a-url" }, "gitlab.base_url"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := decodeValid(t)
			testCase.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation failure")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

func TestLoadUsesEnvironmentFallback(t *testing.T) {
	dir := t.TempDir()
	path := dir + string(os.PathSeparator) + "config.yaml"
	if err := os.WriteFile(path, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigPath, path)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SourcePath == "" {
		t.Fatal("SourcePath not set")
	}
	if cfg.LoadedAt.IsZero() {
		t.Fatal("LoadedAt not set")
	}
}

func TestExampleConfigDecodes(t *testing.T) {
	file, err := os.Open("../../config.example.yaml")
	if err != nil {
		t.Fatalf("open example: %v", err)
	}
	defer file.Close()
	cfg, err := config.Decode(file)
	if err != nil {
		t.Fatalf("example config must strictly decode: %v", err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].RedmineProject != "billing" {
		t.Fatalf("unexpected repositories: %+v", cfg.Repositories)
	}
	if cfg.Agent.Command != "opencode" {
		t.Fatalf("default agent command = %q", cfg.Agent.Command)
	}
}

func TestRedactedDocumentKeepsReferencesOnly(t *testing.T) {
	t.Setenv("REDMINE_TEST_KEY", "another-secret")
	cfg := decodeValid(t)
	doc := cfg.RedactedDocument()
	redmine, ok := doc["redmine"].(map[string]any)
	if !ok {
		t.Fatalf("redmine block is %T", doc["redmine"])
	}
	if redmine["apiKey"] != "env:REDMINE_TEST_KEY" {
		t.Fatalf("apiKey = %v", redmine["apiKey"])
	}
}

func TestAllLocalRepositoriesNeedNoGitLab(t *testing.T) {
	cfg := decodeValid(t)
	cfg.Repositories[0].RepositoryPath = t.TempDir()
	cfg.GitLab = config.GitLab{PushEnabled: true}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("an all-local deployment must not require GitLab: %v", err)
	}
}

func TestLocalRepositoryMayStillDisablePublishing(t *testing.T) {
	cfg := decodeValid(t)
	cfg.Repositories[0].RepositoryPath = t.TempDir()
	cfg.GitLab = config.GitLab{}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a silent GitLab section must be accepted for local repositories: %v", err)
	}
}

func TestRemoteRepositoryStillRequiresGitLab(t *testing.T) {
	cfg := decodeValid(t)
	cfg.GitLab = config.GitLab{}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a GitLab project path must still require gitlab.base_url and gitlab.token")
	}
	if !strings.Contains(err.Error(), "gitlab.base_url") {
		t.Fatalf("error %q does not name gitlab.base_url", err)
	}
	if !strings.Contains(err.Error(), "gitlab.token") {
		t.Fatalf("error %q does not name gitlab.token", err)
	}
}

func TestHalfConfiguredGitLabIsRejected(t *testing.T) {
	cfg := decodeValid(t)
	cfg.Repositories[0].RepositoryPath = t.TempDir()
	cfg.GitLab = config.GitLab{BaseURL: "https://gitlab.example"}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "gitlab.token") {
		t.Fatalf("a token-less GitLab section must be rejected: %v", err)
	}
}

func TestAgentContainmentDefaultsAndPassthroughValidation(t *testing.T) {
	cfg := decodeValid(t)
	if !cfg.Agent.IsolateHome() {
		t.Fatal("sandbox_home must default to true")
	}

	off := false
	cfg.Agent.SandboxHome = &off
	if cfg.Agent.IsolateHome() {
		t.Fatal("an explicit sandbox_home: false must be honoured")
	}

	cfg.Agent.EnvPassthrough = []string{"HTTPS_PROXY"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid passthrough name must be accepted: %v", err)
	}

	cfg.Agent.EnvPassthrough = []string{"NOT-A-NAME"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "env_passthrough") {
		t.Fatalf("an invalid passthrough name must be rejected: %v", err)
	}
}
