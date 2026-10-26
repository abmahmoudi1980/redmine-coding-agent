package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// SeedResult reports what applying a configuration changed.
type SeedResult struct {
	RepositoryIDs map[string]int64
	Deactivated   []string
	ProfileID     int64
	ContentHash   string
}

// SeedRepositories upserts the configuration's mappings and their checks. The
// table, not the YAML, is what resolution reads, so history survives config
// edits (FR-055). Mappings removed from the configuration are deactivated, not
// deleted, so a past run still points at a real row.
func (s *Store) SeedRepositories(ctx context.Context, cfg *config.Config) (map[string]int64, error) {
	result, err := s.seedRepositories(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return result.RepositoryIDs, nil
}

// SeedConfiguration applies the mappings and records the redacted effective
// configuration profile. It is the startup write path: a restart is the only
// way a configuration change takes effect (FR-055, SC-012).
func (s *Store) SeedConfiguration(ctx context.Context, cfg *config.Config) (SeedResult, error) {
	result, err := s.seedRepositories(ctx, cfg)
	if err != nil {
		return SeedResult{}, err
	}
	profile, err := configurationProfile(cfg)
	if err != nil {
		return SeedResult{}, err
	}
	id, err := s.SaveConfigurationProfile(ctx, profile)
	if err != nil {
		return SeedResult{}, err
	}
	result.ProfileID = id
	result.ContentHash = profile.ContentHash
	return result, nil
}

// configurationProfile renders the redacted document persisted for audit. Only
// secret references are written, never values (FR-056).
func configurationProfile(cfg *config.Config) (*domain.ConfigurationProfile, error) {
	hash := cfg.ContentHash()
	if hash == "" {
		return nil, errors.New("store: configuration content hash is empty")
	}
	content, err := json.Marshal(cfg.RedactedDocument())
	if err != nil {
		return nil, fmt.Errorf("store: encode configuration profile: %w", err)
	}
	loadedAt := cfg.LoadedAt
	if loadedAt.IsZero() {
		loadedAt = nowUTC()
	}
	return &domain.ConfigurationProfile{
		ContentHash:   hash,
		SchemaVersion: cfg.SchemaVersion,
		SourcePath:    cfg.SourcePath,
		Content:       string(content),
		LoadedAt:      loadedAt,
	}, nil
}

func (s *Store) seedRepositories(ctx context.Context, cfg *config.Config) (SeedResult, error) {
	if cfg == nil {
		return SeedResult{}, errors.New("store: nil configuration")
	}

	result := SeedResult{RepositoryIDs: make(map[string]int64, len(cfg.Repositories))}
	err := s.InTx(ctx, func(tx *Tx) error {
		now := domain.FormatTime(nowUTC())
		configured := make(map[string]struct{}, len(cfg.Repositories))
		for index, repository := range cfg.Repositories {
			project := strings.TrimSpace(repository.RedmineProject)
			path := strings.TrimSpace(repository.RepositoryPath)
			if path == "" {
				return fmt.Errorf("store: repositories[%d] is missing repository_path", index)
			}
			if strings.TrimSpace(repository.BaseBranch) == "" {
				return fmt.Errorf("store: repositories[%d] (%s) has no base_branch", index, path)
			}
			configured[project+"\x00"+path] = struct{}{}

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
			result.RepositoryIDs[path] = id

			configuredChecks := make(map[string]struct{}, len(repository.Checks))
			for position, check := range repository.Checks {
				name := strings.TrimSpace(check.Name)
				if name == "" {
					return fmt.Errorf("store: repository %s has a check without a name", path)
				}
				configuredChecks[name] = struct{}{}
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
			if err := deleteMissingChecks(ctx, tx, id, configuredChecks); err != nil {
				return err
			}
		}

		deactivated, err := deactivateMissing(ctx, tx, configured, now)
		if err != nil {
			return err
		}
		result.Deactivated = deactivated
		return nil
	})
	if err != nil {
		return SeedResult{}, err
	}
	return result, nil
}

// deleteMissingChecks removes checks that a configuration edit dropped, so a
// renamed command stops running on the next restart (SC-012).
func deleteMissingChecks(ctx context.Context, tx *Tx, repositoryID int64, configured map[string]struct{}) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name FROM repository_checks WHERE repository_id = ?`, repositoryID)
	if err != nil {
		return fmt.Errorf("store: list checks for repository %d: %w", repositoryID, err)
	}
	var stale []int64
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan check for repository %d: %w", repositoryID, err)
		}
		if _, exists := configured[name]; !exists {
			stale = append(stale, id)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: close checks for repository %d: %w", repositoryID, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate checks for repository %d: %w", repositoryID, err)
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM repository_checks WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: delete stale check %d: %w", id, err)
		}
	}
	return nil
}

// deactivateMissing turns mappings that disappeared from the configuration off
// without deleting them, so run history keeps resolving its repository.
func deactivateMissing(ctx context.Context, tx *Tx, configured map[string]struct{}, now string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, redmine_project, repository_path, enabled FROM repositories`)
	if err != nil {
		return nil, fmt.Errorf("store: list repositories for deactivation: %w", err)
	}

	type stored struct {
		id      int64
		path    string
		enabled bool
	}
	var missing []stored
	for rows.Next() {
		var (
			id            int64
			project, path string
			enabled       int64
		)
		if err := rows.Scan(&id, &project, &path, &enabled); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: scan repository for deactivation: %w", err)
		}
		if _, exists := configured[project+"\x00"+path]; !exists && intBool(enabled) {
			missing = append(missing, stored{id: id, path: path, enabled: true})
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close repository deactivation rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate repository deactivation rows: %w", err)
	}

	deactivated := make([]string, 0, len(missing))
	for _, repository := range missing {
		if _, err := tx.ExecContext(ctx, `
			UPDATE repositories
			SET enabled = 0, is_default = 0, updated_at = ?
			WHERE id = ?`, now, repository.id); err != nil {
			return nil, fmt.Errorf("store: deactivate repository %s: %w", repository.path, err)
		}
		deactivated = append(deactivated, repository.path)
	}
	return deactivated, nil
}
