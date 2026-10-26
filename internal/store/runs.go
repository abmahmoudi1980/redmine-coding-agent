package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrRunTerminal is returned when a write targets a finished run.
var ErrRunTerminal = errors.New("store: run is terminal")

// CreateIssueSnapshot inserts an immutable issue copy.
func (s *Store) CreateIssueSnapshot(ctx context.Context, snapshot *domain.IssueSnapshot) (int64, error) {
	return insertIssueSnapshot(ctx, s.write, snapshot)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertIssueSnapshot(ctx context.Context, exec execer, snapshot *domain.IssueSnapshot) (int64, error) {
	if snapshot == nil {
		return 0, errors.New("store: nil issue snapshot")
	}
	fetchedAt := snapshot.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = nowUTC()
	}
	result, err := exec.ExecContext(ctx, `
		INSERT INTO issue_snapshots (
			redmine_issue_id, subject, description, acceptance_criteria, criteria_source,
			project_id, project_identifier, project_name, status_name, tracker_name,
			priority_name, assignee_name, parent_issue_id, related_issue_ids, custom_fields,
			selected_comments, attachments, raw_json, content_hash, fetched_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshot.RedmineIssueID, snapshot.Subject, snapshot.Description,
		snapshot.AcceptanceCriteria, defaultString(snapshot.CriteriaSource, "none"),
		snapshot.ProjectID, snapshot.ProjectIdentifier, snapshot.ProjectName,
		snapshot.StatusName, snapshot.TrackerName, snapshot.PriorityName, snapshot.AssigneeName,
		nullableInt(snapshot.ParentIssueID), mustJSON(snapshot.RelatedIssueIDs),
		mustJSON(snapshot.CustomFields), mustJSON(snapshot.SelectedComments),
		mustJSON(snapshot.Attachments), defaultString(snapshot.RawJSON, "{}"),
		snapshot.ContentHash, domain.FormatTime(fetchedAt),
	)
	if err != nil {
		return 0, fmt.Errorf("store: create issue snapshot: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: issue snapshot id: %w", err)
	}
	snapshot.ID = id
	snapshot.FetchedAt = fetchedAt
	return id, nil
}

// GetIssueSnapshot loads one snapshot by its primary key.
func (s *Store) GetIssueSnapshot(ctx context.Context, id int64) (*domain.IssueSnapshot, error) {
	var (
		snapshot domain.IssueSnapshot
		related  string
		fields   string
		comments string
		attach   string
		parent   sql.NullInt64
		fetched  sql.NullString
	)
	err := s.read.QueryRowContext(ctx, `
		SELECT id, redmine_issue_id, subject, description, acceptance_criteria, criteria_source,
		       project_id, project_identifier, project_name, status_name, tracker_name,
		       priority_name, assignee_name, parent_issue_id, related_issue_ids, custom_fields,
		       selected_comments, attachments, raw_json, content_hash, fetched_at
		FROM issue_snapshots WHERE id = ?`, id).Scan(
		&snapshot.ID, &snapshot.RedmineIssueID, &snapshot.Subject, &snapshot.Description,
		&snapshot.AcceptanceCriteria, &snapshot.CriteriaSource, &snapshot.ProjectID,
		&snapshot.ProjectIdentifier, &snapshot.ProjectName, &snapshot.StatusName,
		&snapshot.TrackerName, &snapshot.PriorityName, &snapshot.AssigneeName, &parent,
		&related, &fields, &comments, &attach, &snapshot.RawJSON, &snapshot.ContentHash, &fetched,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get issue snapshot: %w", err)
	}
	if parent.Valid {
		value := int(parent.Int64)
		snapshot.ParentIssueID = &value
	}
	snapshot.RelatedIssueIDs = scanJSONArray[int](related)
	snapshot.CustomFields = scanJSONArray[domain.CustomField](fields)
	snapshot.SelectedComments = scanJSONArray[domain.Comment](comments)
	snapshot.Attachments = scanJSONArray[domain.Attachment](attach)
	if snapshot.FetchedAt, err = requireTime(fetched); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// CreateRun inserts a run in queued/created unless the caller already set a
// state. The partial unique index rejects a second active run for the issue.
func (s *Store) CreateRun(ctx context.Context, run *domain.Run) (int64, error) {
	return insertRun(ctx, s.write, run)
}

func insertRun(ctx context.Context, exec execer, run *domain.Run) (int64, error) {
	if run == nil {
		return 0, errors.New("store: nil run")
	}
	if run.PublicID == "" {
		return 0, errors.New("store: run public_id is required")
	}
	if run.IssueSnapshotID == 0 {
		return 0, errors.New("store: run issue_snapshot_id is required")
	}
	if run.State == "" {
		run.State = domain.StateQueued
	}
	if run.Stage == "" {
		run.Stage = domain.StageCreated
	}
	if !run.State.Valid() {
		return 0, fmt.Errorf("store: unknown run state %q", run.State)
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = nowUTC()
	}
	result, err := exec.ExecContext(ctx, `
		INSERT INTO runs (
			public_id, issue_id, issue_snapshot_id, repository_id, previous_run_id, state, stage,
			base_branch, base_revision, branch, commit_sha, pushed_at, agent_name, agent_model,
			agent_config_id, config_profile_id, requested_by, created_at, started_at, ended_at,
			cancel_requested_at, interrupted_stage, failure_code, failure_message, summary
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.PublicID, run.IssueID, run.IssueSnapshotID, nullableInt64(run.RepositoryID),
		nullableInt64(run.PreviousRunID), string(run.State), string(run.Stage),
		stringArg(run.BaseBranch), stringArg(run.BaseRevision), stringArg(run.Branch),
		stringArg(run.CommitSHA), timeArg(run.PushedAt), stringArg(run.AgentName),
		stringArg(run.AgentModel), stringArg(run.AgentConfigID), nullableInt64(run.ConfigProfileID),
		stringArg(run.RequestedBy), domain.FormatTime(run.CreatedAt), timeArg(run.StartedAt),
		timeArg(run.EndedAt), timeArg(run.CancelRequestedAt), stringArg(string(run.InterruptedStage)),
		stringArg(string(run.FailureCode)), stringArg(run.FailureMessage), stringArg(run.Summary),
	)
	if err != nil {
		return 0, fmt.Errorf("store: create run: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: run id: %w", err)
	}
	run.ID = id
	return id, nil
}

const runSelect = `
	SELECT r.id, r.public_id, r.issue_id, s.subject, r.issue_snapshot_id, r.repository_id,
	       r.previous_run_id, r.state, r.stage, COALESCE(r.base_branch, ''),
	       COALESCE(r.base_revision, ''), COALESCE(r.branch, ''), COALESCE(r.commit_sha, ''),
	       r.pushed_at, COALESCE(r.agent_name, ''), COALESCE(r.agent_model, ''),
	       COALESCE(r.agent_config_id, ''), r.config_profile_id, COALESCE(r.requested_by, ''),
	       r.created_at, r.started_at, r.ended_at, r.cancel_requested_at,
	       COALESCE(r.interrupted_stage, ''), COALESCE(r.failure_code, ''),
	       COALESCE(r.failure_message, ''), COALESCE(r.summary, '')
	FROM runs r JOIN issue_snapshots s ON s.id = r.issue_snapshot_id`

// ErrActiveRunExists reports that the issue already has a non-terminal run
// (FR-010).
var ErrActiveRunExists = errors.New("store: issue already has an active run")

// ErrConcurrencyLimit reports that the global active-run limit is reached
// (FR-011).
var ErrConcurrencyLimit = errors.New("store: concurrency limit reached")

// ReserveRun atomically claims a concurrency slot and inserts the snapshot and
// run. Both checks and both inserts happen inside one BEGIN IMMEDIATE
// transaction, so two concurrent starts cannot both win.
func (s *Store) ReserveRun(ctx context.Context, snapshot *domain.IssueSnapshot, run *domain.Run, maxActive int) error {
	if snapshot == nil || run == nil {
		return errors.New("store: reserve requires a snapshot and a run")
	}
	if maxActive < 1 {
		maxActive = 1
	}
	return s.InTx(ctx, func(tx *Tx) error {
		placeholders := activeStatePlaceholders()
		args := make([]any, 0, len(domain.ActiveRunStates))
		for _, state := range domain.ActiveRunStates {
			args = append(args, string(state))
		}

		var active int
		// #nosec G202 -- the clause is fixed and values stay in placeholders.
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM runs WHERE state IN (`+placeholders+`)`, args...).Scan(&active); err != nil {
			return fmt.Errorf("store: count active runs: %w", err)
		}
		if active >= maxActive {
			return ErrConcurrencyLimit
		}

		var existing int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM runs WHERE issue_id = ? AND state IN (`+placeholders+`) LIMIT 1`,
			append([]any{run.IssueID}, args...)...).Scan(&existing)
		switch {
		case err == nil:
			return ErrActiveRunExists
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("store: active run lookup: %w", err)
		}

		if _, err := insertIssueSnapshot(ctx, tx, snapshot); err != nil {
			return err
		}
		run.IssueSnapshotID = snapshot.ID
		if _, err := insertRun(ctx, tx, run); err != nil {
			if isUniqueViolation(err) {
				return ErrActiveRunExists
			}
			return err
		}
		return nil
	})
}

func isUniqueViolation(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed")
}

// SetStage records progress inside the current state without changing it.
func (s *Store) SetStage(ctx context.Context, runID int64, stage domain.Stage, event domain.RunEvent) error {
	if stage == "" {
		return errors.New("store: stage is required")
	}
	return s.InTx(ctx, func(tx *Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM runs WHERE id = ?`, runID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read run state: %w", err)
		}
		if domain.IsTerminal(domain.RunState(state)) {
			return ErrRunTerminal
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET stage = ? WHERE id = ?`, string(stage), runID); err != nil {
			return fmt.Errorf("store: set stage: %w", err)
		}
		if event.Type == "" {
			event.Type = domain.EventStage
		}
		if event.Message == "" {
			event.Message = "stage " + stage.String()
		}
		_, err = appendEvent(ctx, tx, runID, event, s.redact)
		return err
	})
}

// GetRun loads a run by its numeric primary key.
func (s *Store) GetRun(ctx context.Context, id int64) (*domain.Run, error) {
	run, err := scanRun(s.read.QueryRowContext(ctx, runSelect+` WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run: %w", err)
	}
	return run, nil
}

// GetRunByPublicID loads a run by the UUID shown to operators.
func (s *Store) GetRunByPublicID(ctx context.Context, publicID string) (*domain.Run, error) {
	run, err := scanRun(s.read.QueryRowContext(ctx, runSelect+` WHERE r.public_id = ?`, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run: %w", err)
	}
	return run, nil
}

// RunPatch carries the mutable run columns. Nil fields are left unchanged.
type RunPatch struct {
	BaseBranch      *string
	BaseRevision    *string
	Branch          *string
	CommitSHA       *string
	PushedAt        *time.Time
	AgentName       *string
	AgentModel      *string
	AgentConfigID   *string
	ConfigProfileID *int64
	RequestedBy     *string
	Summary         *string
}

// UpdateRun applies a patch to a non-terminal run.
func (s *Store) UpdateRun(ctx context.Context, runID int64, patch RunPatch) error {
	return s.InTx(ctx, func(tx *Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM runs WHERE id = ?`, runID).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: read run state: %w", err)
		}
		if domain.IsTerminal(domain.RunState(state)) {
			return ErrRunTerminal
		}
		assignments := make([]string, 0, 12)
		args := make([]any, 0, 13)
		add := func(column string, value any) {
			assignments = append(assignments, column+" = ?")
			args = append(args, value)
		}
		if patch.BaseBranch != nil {
			add("base_branch", stringArg(*patch.BaseBranch))
		}
		if patch.BaseRevision != nil {
			add("base_revision", stringArg(*patch.BaseRevision))
		}
		if patch.Branch != nil {
			add("branch", stringArg(*patch.Branch))
		}
		if patch.CommitSHA != nil {
			add("commit_sha", stringArg(*patch.CommitSHA))
		}
		if patch.PushedAt != nil {
			add("pushed_at", timeArg(patch.PushedAt))
		}
		if patch.AgentName != nil {
			add("agent_name", stringArg(*patch.AgentName))
		}
		if patch.AgentModel != nil {
			add("agent_model", stringArg(*patch.AgentModel))
		}
		if patch.AgentConfigID != nil {
			add("agent_config_id", stringArg(*patch.AgentConfigID))
		}
		if patch.ConfigProfileID != nil {
			add("config_profile_id", *patch.ConfigProfileID)
		}
		if patch.RequestedBy != nil {
			add("requested_by", stringArg(*patch.RequestedBy))
		}
		if patch.Summary != nil {
			add("summary", stringArg(*patch.Summary))
		}
		if len(assignments) == 0 {
			return nil
		}
		args = append(args, runID)
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET `+strings.Join(assignments, ", ")+` WHERE id = ?`, args...); err != nil {
			return fmt.Errorf("store: update run: %w", err)
		}
		return nil
	})
}

// Transition describes one state change and the timeline row that records it.
type Transition struct {
	To         domain.RunState
	Stage      domain.Stage
	Event      domain.RunEvent
	Conditions domain.Conditions
	Failure    *domain.Failure
}

// TransitionRun changes state and stage and appends the matching run_event in
// one transaction, so the timeline can never disagree with the machine.
func (s *Store) TransitionRun(ctx context.Context, runID int64, transition Transition) error {
	if transition.To == "" {
		return errors.New("store: transition target is required")
	}
	if transition.To == domain.StateInterrupted {
		return fmt.Errorf("%w: use MarkRunInterrupted", domain.ErrInvalidTransition)
	}
	return s.InTx(ctx, func(tx *Tx) error {
		var (
			currentState    string
			currentStage    string
			cancelRequested sql.NullString
			startedAt       sql.NullString
			endedAt         sql.NullString
		)
		err := tx.QueryRowContext(ctx, `
			SELECT state, stage, cancel_requested_at, started_at, ended_at
			FROM runs WHERE id = ?`, runID).
			Scan(&currentState, &currentStage, &cancelRequested, &startedAt, &endedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read run: %w", err)
		}
		from := domain.RunState(currentState)
		if domain.IsTerminal(from) {
			return ErrRunTerminal
		}
		conditions := transition.Conditions
		if cancelRequested.Valid {
			conditions.CancelRequested = true
		}
		if err := domain.CheckTransition(from, transition.To, conditions); err != nil {
			return err
		}

		now := nowUTC()
		stage := transition.Stage
		if stage == "" {
			stage = domain.Stage(currentStage)
		}

		newStartedAt := startedAt
		if !startedAt.Valid && transition.To != domain.StateQueued {
			newStartedAt = sql.NullString{String: domain.FormatTime(now), Valid: true}
		}
		newEndedAt := endedAt
		if domain.IsTerminal(transition.To) {
			newEndedAt = sql.NullString{String: domain.FormatTime(now), Valid: true}
		}
		newCancelRequested := cancelRequested
		if transition.To == domain.StateCancelled && !newCancelRequested.Valid {
			newCancelRequested = sql.NullString{String: domain.FormatTime(now), Valid: true}
		}

		failureCode := any(nil)
		failureMessage := any(nil)
		if transition.Failure != nil {
			failureCode = string(transition.Failure.Code)
			failureMessage = stringArg(s.redact(transition.Failure.Message))
		}

		result, err := tx.ExecContext(ctx, `
			UPDATE runs SET state = ?, stage = ?, started_at = ?, ended_at = ?,
			       cancel_requested_at = ?, failure_code = COALESCE(?, failure_code),
			       failure_message = COALESCE(?, failure_message)
			WHERE id = ? AND state = ?`,
			string(transition.To), string(stage), nullStringValue(newStartedAt),
			nullStringValue(newEndedAt), nullStringValue(newCancelRequested),
			failureCode, failureMessage, runID, currentState,
		)
		if err != nil {
			return fmt.Errorf("store: update run state: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: rows affected: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf("%w: run %d changed concurrently", domain.ErrInvalidTransition, runID)
		}

		event := transition.Event
		if event.Type == "" {
			event.Type = domain.EventState
		}
		if event.Message == "" {
			event.Message = fmt.Sprintf("state %s -> %s (%s)", from, transition.To, stage)
		}
		event.At = now
		if _, err := appendEvent(ctx, tx, runID, event, s.redact); err != nil {
			return err
		}
		return nil
	})
}

// RequestCancel records the operator's cancel request without changing state.
// It is idempotent and refuses terminal runs.
func (s *Store) RequestCancel(ctx context.Context, runID int64) (bool, error) {
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		var state string
		var cancel sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT state, cancel_requested_at FROM runs WHERE id = ?`, runID).
			Scan(&state, &cancel)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read run: %w", err)
		}
		if domain.IsTerminal(domain.RunState(state)) {
			return ErrRunTerminal
		}
		if cancel.Valid {
			return nil
		}
		changed = true
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET cancel_requested_at = ? WHERE id = ? AND cancel_requested_at IS NULL`,
			domain.FormatTime(nowUTC()), runID,
		); err != nil {
			return fmt.Errorf("store: request cancel: %w", err)
		}
		_, err = appendEvent(ctx, tx, runID, domain.RunEvent{
			Type:     domain.EventWarning,
			Severity: domain.SeverityWarn,
			Message:  "cancel requested",
		}, s.redact)
		return err
	})
	return changed, err
}

// MarkRunInterrupted is the startup sweep's only write: a non-terminal run is
// frozen at its last stage with no chance of resuming into a push.
func (s *Store) MarkRunInterrupted(ctx context.Context, runID int64) error {
	return s.InTx(ctx, func(tx *Tx) error {
		var state, stage string
		err := tx.QueryRowContext(ctx, `SELECT state, stage FROM runs WHERE id = ?`, runID).Scan(&state, &stage)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read run: %w", err)
		}
		from := domain.RunState(state)
		if domain.IsTerminal(from) {
			return ErrRunTerminal
		}
		if err := domain.MarkInterrupted(from); err != nil {
			return err
		}
		now := domain.FormatTime(nowUTC())
		if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET state = ?, interrupted_stage = ?, ended_at = ?,
			       failure_code = ?, failure_message = ?
			WHERE id = ? AND state = ?`,
			string(domain.StateInterrupted), stage, now,
			string(domain.FailInterrupted), "worker stopped before the run finished", runID, state,
		); err != nil {
			return fmt.Errorf("store: mark interrupted: %w", err)
		}
		_, err = appendEvent(ctx, tx, runID, domain.RunEvent{
			Type:     domain.EventState,
			Severity: domain.SeverityWarn,
			Message:  fmt.Sprintf("run interrupted in stage %s by restart recovery", stage),
		}, s.redact)
		return err
	})
}

// SetRunFailure records a failure reason without changing state.
func (s *Store) SetRunFailure(ctx context.Context, runID int64, failure domain.Failure) error {
	return s.InTx(ctx, func(tx *Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM runs WHERE id = ?`, runID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read run: %w", err)
		}
		if domain.IsTerminal(domain.RunState(state)) {
			return ErrRunTerminal
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET failure_code = ?, failure_message = ? WHERE id = ?`,
			string(failure.Code), stringArg(s.redact(failure.Message)), runID,
		); err != nil {
			return fmt.Errorf("store: set run failure: %w", err)
		}
		return nil
	})
}

// ActiveRunByIssue returns the non-terminal run for an issue, if any.
func (s *Store) ActiveRunByIssue(ctx context.Context, issueID int) (*domain.Run, error) {
	query := runSelect + ` WHERE r.issue_id = ? AND r.state IN (` + activeStatePlaceholders() + `)
		ORDER BY r.created_at DESC LIMIT 1`
	args := []any{issueID}
	for _, state := range domain.ActiveRunStates {
		args = append(args, string(state))
	}
	run, err := scanRun(s.read.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: active run lookup: %w", err)
	}
	return run, nil
}

// ActiveRunCount is the number of non-terminal runs.
func (s *Store) ActiveRunCount(ctx context.Context) (int, error) {
	query := `SELECT COUNT(*) FROM runs WHERE state IN (` + activeStatePlaceholders() + `)`
	args := make([]any, 0, len(domain.ActiveRunStates))
	for _, state := range domain.ActiveRunStates {
		args = append(args, string(state))
	}
	var count int
	if err := s.read.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: active run count: %w", err)
	}
	return count, nil
}

func activeStatePlaceholders() string {
	return strings.TrimSuffix(strings.Repeat("?,", len(domain.ActiveRunStates)), ",")
}

// RunFilter selects a page of history.
type RunFilter struct {
	State   domain.RunState
	IssueID int
	Limit   int
	Cursor  string
}

// ListRuns returns newest-first runs and the cursor for the next page.
func (s *Store) ListRuns(ctx context.Context, filter RunFilter) ([]domain.Run, string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}

	where := []string{"1 = 1"}
	args := make([]any, 0, 6)
	if filter.State != "" {
		where = append(where, "r.state = ?")
		args = append(args, string(filter.State))
	}
	if filter.IssueID > 0 {
		where = append(where, "r.issue_id = ?")
		args = append(args, filter.IssueID)
	}
	if filter.Cursor != "" {
		createdAt, id, err := decodeCursor(filter.Cursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(r.created_at < ? OR (r.created_at = ? AND r.id < ?))")
		args = append(args, createdAt, createdAt, id)
	}

	// #nosec G202 -- clauses are fixed strings; values stay in placeholders.
	query := runSelect + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY r.created_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	var runs []domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, "", fmt.Errorf("store: scan run: %w", err)
		}
		runs = append(runs, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: iterate runs: %w", err)
	}

	next := ""
	if len(runs) > limit {
		runs = runs[:limit]
		last := runs[len(runs)-1]
		next = encodeCursor(domain.FormatTime(last.CreatedAt), last.ID)
	}
	return runs, next, nil
}

func encodeCursor(createdAt string, id int64) string {
	raw := createdAt + "|" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", 0, fmt.Errorf("store: invalid cursor: %w", err)
	}
	createdAt, idText, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", 0, errors.New("store: invalid cursor")
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("store: invalid cursor: %w", err)
	}
	return createdAt, id, nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullStringValue(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
