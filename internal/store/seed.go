package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// SeedRepositories upserts the configuration's mappings and their checks. The
// table, not the YAML, is what resolution reads, so history survives config
// edits (FR-055). Removed mappings are deactivated by a later seeding step.
func (s *Store) SeedRepositories(ctx context.Context, cfg *config.Config) (map[string]int64, error) {
	if cfg == nil {
		return nil, errors.New("store: nil configuration")
	}

	ids := make(map[string]int64, len(cfg.Repositories))
	err := s.InTx(ctx, func(tx *Tx) error {
		now := domain.FormatTime(nowUTC())
		for index, repository := range cfg.Repositories {
			project := strings.TrimSpace(repository.RedmineProject)
			path := strings.TrimSpace(repository.RepositoryPath)
			if project == "" || path == "" {
				return fmt.Errorf("store: repositories[%d] is missing redmine_project or repository_path", index)
			}
			if strings.TrimSpace(repository.BaseBranch) == "" {
				return fmt.Errorf("store: repositories[%d] (%s) has no base_branch", index, path)
			}

			if repository.IsDefault {
				if _, err := tx.ExecContext(ctx, `
					UPDATE repositories SET is_default = 0
					WHERE redmine_project = ? AND is_default = 1 AND repository_path <> ?`,
					project, path,
				); err != nil {
					return fmt.Errorf("store: clear default for %s: %w", project, err)
				}
			}

			var id int64
			err := tx.QueryRowContext(ctx, `
				INSERT INTO repositories (
					redmine_project, repository_path, display_name, base_branch, is_default,
					publish_enabled, merge_request_enabled, enabled, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(redmine_project, repository_path) DO UPDATE SET
					display_name = excluded.display_name,
					base_branch = excluded.base_branch,
					is_default = excluded.is_default,
					publish_enabled = excluded.publish_enabled,
					merge_request_enabled = excluded.merge_request_enabled,
					enabled = excluded.enabled,
					updated_at = excluded.updated_at
				RETURNING id`,
				project, path, repository.DisplayName, strings.TrimSpace(repository.BaseBranch),
				boolInt(repository.IsDefault),
				boolInt(repository.PublishEnabledFor(cfg.GitLab.PushEnabled)),
				boolInt(repository.MergeRequestEnabledFor(cfg.GitLab.MergeRequestEnabled)),
				boolInt(repository.IsEnabled()), now, now,
			).Scan(&id)
			if err != nil {
				return fmt.Errorf("store: upsert repository %s: %w", path, err)
			}
			ids[path] = id

			for position, check := range repository.Checks {
				name := strings.TrimSpace(check.Name)
				if name == "" {
					return fmt.Errorf("store: repository %s has a check without a name", path)
				}
				timeout := check.EffectiveTimeoutSeconds()
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO repository_checks (
						repository_id, name, command_linux, command_windows, required,
						timeout_seconds, position, created_at
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					ON CONFLICT(repository_id, name) DO UPDATE SET
						command_linux = excluded.command_linux,
						command_windows = excluded.command_windows,
						required = excluded.required,
						timeout_seconds = excluded.timeout_seconds,
						position = excluded.position`,
					id, name, check.CommandLinux, check.CommandWindows,
					boolInt(check.IsRequired()), timeout, position, now,
				); err != nil {
					return fmt.Errorf("store: upsert check %s/%s: %w", path, name, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}
