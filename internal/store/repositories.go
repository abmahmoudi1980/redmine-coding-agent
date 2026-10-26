package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// ListRepositories returns every stored mapping with its checks in position
// order. Resolution reads only this table (FR-014).
func (s *Store) ListRepositories(ctx context.Context) ([]domain.RepositoryMapping, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, redmine_project, repository_path, display_name, base_branch, is_default,
		       publish_enabled, merge_request_enabled, enabled
		FROM repositories
		ORDER BY redmine_project ASC, repository_path ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list repositories: %w", err)
	}
	defer rows.Close()

	var mappings []domain.RepositoryMapping
	index := make(map[int64]int)
	for rows.Next() {
		var (
			mapping                                   domain.RepositoryMapping
			isDefault, publish, mergeRequest, enabled int64
		)
		if err := rows.Scan(
			&mapping.ID, &mapping.RedmineProject, &mapping.RepositoryPath, &mapping.DisplayName,
			&mapping.BaseBranch, &isDefault, &publish, &mergeRequest, &enabled,
		); err != nil {
			return nil, fmt.Errorf("store: scan repository: %w", err)
		}
		mapping.IsDefault = intBool(isDefault)
		mapping.PublishEnabled = intBool(publish)
		mapping.MergeRequestEnabled = intBool(mergeRequest)
		mapping.Enabled = intBool(enabled)
		index[mapping.ID] = len(mappings)
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate repositories: %w", err)
	}
	if len(mappings) == 0 {
		return mappings, nil
	}

	checks, err := s.listChecks(ctx)
	if err != nil {
		return nil, err
	}
	for _, check := range checks {
		if at, ok := index[check.repositoryID]; ok {
			mappings[at].Checks = append(mappings[at].Checks, check.definition)
		}
	}
	return mappings, nil
}

// GetRepositoryByID loads one mapping. It is the explicit-choice lookup
// (FR-015).
func (s *Store) GetRepositoryByID(ctx context.Context, id int64) (*domain.RepositoryMapping, error) {
	var (
		mapping                                   domain.RepositoryMapping
		isDefault, publish, mergeRequest, enabled int64
	)
	err := s.read.QueryRowContext(ctx, `
		SELECT id, redmine_project, repository_path, display_name, base_branch, is_default,
		       publish_enabled, merge_request_enabled, enabled
		FROM repositories WHERE id = ?`, id).Scan(
		&mapping.ID, &mapping.RedmineProject, &mapping.RepositoryPath, &mapping.DisplayName,
		&mapping.BaseBranch, &isDefault, &publish, &mergeRequest, &enabled,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get repository: %w", err)
	}
	mapping.IsDefault = intBool(isDefault)
	mapping.PublishEnabled = intBool(publish)
	mapping.MergeRequestEnabled = intBool(mergeRequest)
	mapping.Enabled = intBool(enabled)

	checks, err := s.listChecks(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, check := range checks {
		mapping.Checks = append(mapping.Checks, check.definition)
	}
	return &mapping, nil
}

type storedCheck struct {
	repositoryID int64
	definition   domain.CheckDefinition
}

func (s *Store) listChecks(ctx context.Context, repositoryIDs ...int64) ([]storedCheck, error) {
	query := `
		SELECT id, repository_id, name, command_linux, command_windows, required,
		       timeout_seconds, position
		FROM repository_checks`
	args := make([]any, 0, len(repositoryIDs))
	if len(repositoryIDs) == 1 {
		query += ` WHERE repository_id = ?`
		args = append(args, repositoryIDs[0])
	}
	query += ` ORDER BY repository_id ASC, position ASC, id ASC`

	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list checks: %w", err)
	}
	defer rows.Close()

	var checks []storedCheck
	for rows.Next() {
		var (
			check    storedCheck
			required int64
		)
		if err := rows.Scan(
			&check.definition.ID, &check.repositoryID, &check.definition.Name,
			&check.definition.CommandLinux, &check.definition.CommandWindows, &required,
			&check.definition.TimeoutSeconds, &check.definition.Position,
		); err != nil {
			return nil, fmt.Errorf("store: scan check: %w", err)
		}
		check.definition.Required = intBool(required)
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate checks: %w", err)
	}
	return checks, nil
}
