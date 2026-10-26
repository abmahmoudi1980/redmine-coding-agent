package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
)

func TestSeedPersistsPerRepositoryPolicyOverrides(t *testing.T) {
	root := t.TempDir()
	st, err := Open(context.Background(), filepath.Join(root, "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	disabled := false
	enabled := true
	required := false
	cfg := &config.Config{
		SchemaVersion: 1,
		GitLab: config.GitLab{
			PushEnabled:         true,
			MergeRequestEnabled: true,
		},
		Repositories: []config.Repository{{
			RedmineProject:      "billing",
			RepositoryPath:      "group/billing-api",
			BaseBranch:          "main",
			IsDefault:           true,
			PublishEnabled:      &disabled,
			MergeRequestEnabled: &enabled,
			Checks: []config.Check{{
				Name:           "lint",
				CommandLinux:   "golangci-lint run",
				CommandWindows: "golangci-lint run",
				Required:       &required,
				TimeoutSeconds: 30,
			}},
		}},
	}

	if _, err := st.SeedConfiguration(context.Background(), cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mappings, err := st.ListRepositories(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(mappings))
	}
	mapping := mappings[0]
	if mapping.PublishEnabled {
		t.Error("per-repository publish_enabled=false was not persisted")
	}
	if !mapping.MergeRequestEnabled {
		t.Error("per-repository merge_request_enabled=true was not persisted")
	}
	if len(mapping.Checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(mapping.Checks))
	}
	check := mapping.Checks[0]
	if check.Required {
		t.Error("per-check required=false was not persisted")
	}
	if check.TimeoutSeconds != 30 {
		t.Errorf("timeout = %d, want 30", check.TimeoutSeconds)
	}
}
