package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// ListPrunableRuns returns terminal runs created before the cutoff, oldest
// first. A run that a newer attempt points at is excluded so retry lineage is
// never silently broken.
func (s *Store) ListPrunableRuns(ctx context.Context, before time.Time, limit int) ([]domain.Run, error) {
	if limit <= 0 {
		limit = 500
	}
	query := runSelect + ` WHERE r.state IN ('completed','failed','blocked','cancelled','interrupted')
		AND r.created_at < ?
		AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.previous_run_id = r.id)
		ORDER BY r.created_at ASC LIMIT ?`
	rows, err := s.read.QueryContext(ctx, query, domain.FormatTime(before), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list prunable runs: %w", err)
	}
	defer rows.Close()

	var runs []domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	return runs, rows.Err()
}

// DeleteRun removes one run and every row that belongs to it. Child tables
// cascade; issue snapshots are removed only when no other run references them.
// Branches and merge requests are not this function's business (FR-054).
func (s *Store) DeleteRun(ctx context.Context, runID int64) error {
	return s.InTx(ctx, func(tx *Tx) error {
		var snapshotID sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT issue_snapshot_id FROM runs WHERE id = ?`, runID).Scan(&snapshotID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: read run %d for delete: %w", runID, err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, runID); err != nil {
			return fmt.Errorf("store: delete run %d: %w", runID, err)
		}
		if snapshotID.Valid {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM issue_snapshots WHERE id = ? AND NOT EXISTS (SELECT 1 FROM runs WHERE issue_snapshot_id = ?)`,
				snapshotID.Int64, snapshotID.Int64); err != nil {
				return fmt.Errorf("store: delete orphan snapshot %d: %w", snapshotID.Int64, err)
			}
		}
		return nil
	})
}
