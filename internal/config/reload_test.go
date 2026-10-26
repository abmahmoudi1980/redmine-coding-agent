package config_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// reloadYAML is one configuration file per restart. The operator edits these
// values in a text editor; nothing is recompiled (SC-012).
func reloadYAML(root, baseBranch, checkName, model, project, repository string) string {
	data := filepath.ToSlash(filepath.Join(root, "data"))
	return fmt.Sprintf(`
schema_version: 1
http:
  listen: "127.0.0.1:8080"
data:
  database: "%s/agent.db"
  workspace_root: "%s/workspaces"
  log_dir: "%s/logs"
  retention_days: 7
redmine:
  base_url: "https://redmine.example"
  api_key: "env:RCA_RELOAD_KEY"
  comment_on_finish: false
  update_status: false
gitlab:
  base_url: "https://gitlab.example"
  token: "env:RCA_RELOAD_TOKEN"
  push_enabled: false
  merge_request_enabled: false
repositories:
  - redmine_project: "%s"
    repository_path: "%s"
    display_name: "Reload fixture"
    base_branch: "%s"
    is_default: true
    checks:
      - name: "%s"
        command_linux: "go version"
        command_windows: "go version"
        required: true
        timeout_seconds: 60
agent:
  adapter: "opencode"
  command: "git"
  model: "%s"
  profile: "restricted"
  timeout_minutes: 10
  max_repairs: 0
concurrency:
  max_active_runs: 1
`, data, data, data, project, repository, baseBranch, checkName, model)
}

func writeReloadConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func restartStore(t *testing.T, previous *store.Store, database string) *store.Store {
	t.Helper()
	if previous != nil {
		if err := previous.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
	st, err := store.Open(context.Background(), database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func checkNames(mapping domain.RepositoryMapping) []string {
	names := make([]string, 0, len(mapping.Checks))
	for _, check := range mapping.Checks {
		names = append(names, check.Name)
	}
	return names
}

func TestConfigurationChangeAppliesOnRestartWithoutRebuilding(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RCA_RELOAD_KEY", "reload-redmine-key")
	t.Setenv("RCA_RELOAD_TOKEN", "reload-gitlab-token")

	configPath := filepath.Join(root, "config.yaml")
	database := filepath.Join(root, "data", "agent.db")
	writeReloadConfig(t, configPath, reloadYAML(root, "main", "unit-tests", "model-a", "billing", "group/billing-api"))

	first, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load first config: %v", err)
	}
	st := restartStore(t, nil, database)
	seeded, err := st.SeedConfiguration(context.Background(), first)
	if err != nil {
		t.Fatalf("seed first config: %v", err)
	}
	if seeded.ProfileID == 0 {
		t.Fatal("seeding did not persist a configuration profile")
	}
	if seeded.ContentHash != first.ContentHash() {
		t.Fatalf("content hash = %q, want %q", seeded.ContentHash, first.ContentHash())
	}

	mapping, err := runner.Resolve(context.Background(), st, "billing", 0)
	if err != nil {
		t.Fatalf("resolve before restart: %v", err)
	}
	if mapping.BaseBranch != "main" {
		t.Fatalf("base branch = %q, want main", mapping.BaseBranch)
	}
	if got := checkNames(mapping); len(got) != 1 || got[0] != "unit-tests" {
		t.Fatalf("checks = %v, want [unit-tests]", got)
	}
	if first.Agent.Model != "model-a" {
		t.Fatalf("agent model = %q, want model-a", first.Agent.Model)
	}

	// The operator edits the file and restarts. No rebuild, no migration.
	writeReloadConfig(t, configPath, reloadYAML(root, "release", "lint", "model-b", "billing", "group/billing-api"))
	second, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load second config: %v", err)
	}
	if second.Agent.Model != "model-b" {
		t.Fatalf("agent model after restart = %q, want model-b", second.Agent.Model)
	}
	if second.ContentHash() == first.ContentHash() {
		t.Fatal("a changed configuration must change the content hash")
	}

	st = restartStore(t, st, database)
	if _, err := st.SeedConfiguration(context.Background(), second); err != nil {
		t.Fatalf("seed second config: %v", err)
	}

	mapping, err = runner.Resolve(context.Background(), st, "billing", 0)
	if err != nil {
		t.Fatalf("resolve after restart: %v", err)
	}
	if mapping.BaseBranch != "release" {
		t.Fatalf("base branch after restart = %q, want release (SC-012)", mapping.BaseBranch)
	}
	if got := checkNames(mapping); len(got) != 1 || got[0] != "lint" {
		t.Fatalf("checks after restart = %v, want [lint]", got)
	}

	// Removing the mapping from the file must stop it resolving, while its row
	// survives so old runs still point at something real.
	writeReloadConfig(t, configPath, reloadYAML(root, "main", "unit-tests", "model-c", "crm", "group/crm-api"))
	third, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load third config: %v", err)
	}
	st = restartStore(t, st, database)
	seeded, err = st.SeedConfiguration(context.Background(), third)
	if err != nil {
		t.Fatalf("seed third config: %v", err)
	}
	if len(seeded.Deactivated) != 1 || seeded.Deactivated[0] != "group/billing-api" {
		t.Fatalf("deactivated = %v, want [group/billing-api]", seeded.Deactivated)
	}

	if _, err := runner.Resolve(context.Background(), st, "billing", 0); !errors.Is(err, runner.ErrNoRepositoryMapping) {
		t.Fatalf("resolve of a removed mapping = %v, want ErrNoRepositoryMapping", err)
	}

	mappings, err := st.ListRepositories(context.Background())
	if err != nil {
		t.Fatalf("list repositories: %v", err)
	}
	seen := make(map[string]domain.RepositoryMapping, len(mappings))
	for _, item := range mappings {
		seen[item.RepositoryPath] = item
	}
	if len(mappings) != 2 {
		t.Fatalf("repositories = %d, want 2 (history is not deleted)", len(mappings))
	}
	if seen["group/billing-api"].Enabled {
		t.Fatal("a mapping removed from config is still enabled")
	}
	if !seen["group/crm-api"].Enabled {
		t.Fatal("a new mapping is not enabled after restart")
	}

	profiles := 0
	if err := st.Reader().QueryRow(
		`SELECT COUNT(DISTINCT content_hash) FROM configuration_profiles`).Scan(&profiles); err != nil {
		t.Fatalf("count configuration profiles: %v", err)
	}
	if profiles < 2 {
		t.Fatalf("configuration profiles = %d, want one per distinct configuration", profiles)
	}
}

func TestSeedRejectsAnEmptyBaseBranch(t *testing.T) {
	root := t.TempDir()
	st := restartStore(t, nil, filepath.Join(root, "agent.db"))
	cfg := &config.Config{
		SchemaVersion: 1,
		Repositories: []config.Repository{{
			RedmineProject: "billing",
			RepositoryPath: "group/billing-api",
			BaseBranch:     "   ",
		}},
	}
	_, err := st.SeedConfiguration(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "base_branch") {
		t.Fatalf("seed error = %v, want a base_branch complaint", err)
	}
}
