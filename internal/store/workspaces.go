package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// CreateWorkspace records the isolated working copy for a run.
func (s *Store) CreateWorkspace(ctx context.Context, workspace *domain.Workspace) (int64, error) {
	if workspace == nil {
		return 0, errors.New("store: nil workspace")
	}
	if workspace.RunID == 0 {
		return 0, errors.New("store: workspace run id is required")
	}
	if workspace.CreatedAt.IsZero() {
		workspace.CreatedAt = nowUTC()
	}
	result, err := s.write.ExecContext(ctx, `
		INSERT INTO workspaces (
			run_id, path, git_dir, branch, base_revision, created_at, cleaned_at,
			has_unpushed_changes, changed_paths
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		workspace.RunID, workspace.Path, workspace.GitDir, workspace.Branch,
		workspace.BaseRevision, domain.FormatTime(workspace.CreatedAt),
		timeArg(workspace.CleanedAt), boolInt(workspace.HasUnpushedChanges),
		mustJSON(workspace.ChangedPaths),
	)
	if err != nil {
		return 0, fmt.Errorf("store: create workspace: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: workspace id: %w", err)
	}
	workspace.ID = id
	return id, nil
}

// GetWorkspaceByRun loads the working copy facts for one run.
func (s *Store) GetWorkspaceByRun(ctx context.Context, runID int64) (*domain.Workspace, error) {
	var (
		workspace domain.Workspace
		created   sql.NullString
		cleaned   sql.NullString
		unpushed  int64
		changed   string
	)
	err := s.read.QueryRowContext(ctx, `
		SELECT id, run_id, path, git_dir, branch, base_revision, created_at, cleaned_at,
		       has_unpushed_changes, changed_paths
		FROM workspaces WHERE run_id = ?`, runID).Scan(
		&workspace.ID, &workspace.RunID, &workspace.Path, &workspace.GitDir,
		&workspace.Branch, &workspace.BaseRevision, &created, &cleaned,
		&unpushed, &changed,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get workspace: %w", err)
	}
	workspace.HasUnpushedChanges = intBool(unpushed)
	workspace.ChangedPaths = scanJSONArray[string](changed)
	if workspace.CreatedAt, err = requireTime(created); err != nil {
		return nil, err
	}
	if workspace.CleanedAt, err = scanTime(cleaned); err != nil {
		return nil, err
	}
	return &workspace, nil
}

// UpdateWorkspaceChanges records what the run changed and whether the work is
// still unpushed, before any cleanup decision can be made (FR-053).
func (s *Store) UpdateWorkspaceChanges(ctx context.Context, runID int64, changedPaths []string, hasUnpushed bool) error {
	result, err := s.write.ExecContext(ctx, `
		UPDATE workspaces SET changed_paths = ?, has_unpushed_changes = ? WHERE run_id = ?`,
		mustJSON(changedPaths), boolInt(hasUnpushed), runID,
	)
	if err != nil {
		return fmt.Errorf("store: update workspace changes: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: workspace rows affected: %w", err)
	}
	if affected != 1 {
		return ErrNotFound
	}
	return nil
}
