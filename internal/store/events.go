package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// AppendEvent adds one timeline row, allocating the next per-run sequence
// inside the write transaction.
func (s *Store) AppendEvent(ctx context.Context, runID int64, event domain.RunEvent) (int64, error) {
	var seq int64
	err := s.InTx(ctx, func(tx *Tx) error {
		allocated, err := appendEvent(ctx, tx, runID, event, s.redact)
		if err != nil {
			return err
		}
		seq = allocated
		return nil
	})
	return seq, err
}

func appendEvent(ctx context.Context, tx *Tx, runID int64, event domain.RunEvent, redact func(string) string) (int64, error) {
	if runID == 0 {
		return 0, errors.New("store: event run id is required")
	}
	if event.Type == "" {
		event.Type = domain.EventActivity
	}
	if !validEventType(event.Type) {
		return 0, fmt.Errorf("store: unknown event type %q", event.Type)
	}
	if event.Severity == "" {
		event.Severity = domain.SeverityInfo
	}
	if event.Message == "" {
		return 0, errors.New("store: event message is required")
	}
	if event.At.IsZero() {
		event.At = nowUTC()
	}

	data := event.Data
	if data == "" {
		data = "{}"
	}
	if redact != nil {
		data = redact(data)
		event.Message = redact(event.Message)
	}
	if !json.Valid([]byte(data)) {
		encoded, err := json.Marshal(data)
		if err != nil {
			encoded = []byte(`"unserialisable"`)
		}
		data = `{"text":` + string(encoded) + `}`
	}

	var seq int64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = ?`, runID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: allocate event seq: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_events (run_id, seq, at, type, severity, message, data)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, seq, domain.FormatTime(event.At), string(event.Type), event.Severity,
		event.Message, data,
	); err != nil {
		return 0, fmt.Errorf("store: append event: %w", err)
	}
	return seq, nil
}

func validEventType(eventType domain.EventType) bool {
	switch eventType {
	case domain.EventState, domain.EventStage, domain.EventActivity, domain.EventCheck,
		domain.EventAgent, domain.EventWorkspace, domain.EventPublish, domain.EventReport,
		domain.EventWarning, domain.EventError:
		return true
	}
	return false
}

// EventsAfter replays the timeline strictly after seq, oldest first.
func (s *Store) EventsAfter(ctx context.Context, runID, seq int64, limit int) ([]domain.RunEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, run_id, seq, at, type, severity, message, data
		FROM run_events WHERE run_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		runID, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events after: %w", err)
	}
	return scanEvents(rows)
}

// LatestEvents returns the newest n events, re-ordered oldest first so a
// detail view can render them directly.
func (s *Store) LatestEvents(ctx context.Context, runID int64, n int) ([]domain.RunEvent, error) {
	if n <= 0 {
		n = 200
	}
	if n > 1000 {
		n = 1000
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, run_id, seq, at, type, severity, message, data
		FROM run_events WHERE run_id = ? ORDER BY seq DESC LIMIT ?`, runID, n)
	if err != nil {
		return nil, fmt.Errorf("store: latest events: %w", err)
	}
	events, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, nil
}

// EventCount is the number of timeline rows for a run.
func (s *Store) EventCount(ctx context.Context, runID int64) (int64, error) {
	var count int64
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id = ?`, runID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: event count: %w", err)
	}
	return count, nil
}

func scanEvents(rows *sql.Rows) ([]domain.RunEvent, error) {
	defer rows.Close()
	var events []domain.RunEvent
	for rows.Next() {
		var (
			event domain.RunEvent
			at    string
			kind  string
		)
		if err := rows.Scan(&event.ID, &event.RunID, &event.Seq, &at, &kind,
			&event.Severity, &event.Message, &event.Data); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		event.Type = domain.EventType(kind)
		parsed, err := domain.ParseTime(at)
		if err != nil {
			return nil, fmt.Errorf("store: parse event time: %w", err)
		}
		event.At = parsed
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate events: %w", err)
	}
	return events, nil
}
