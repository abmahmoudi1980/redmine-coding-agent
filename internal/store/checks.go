package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// CreateInvocation records one agent attempt before it starts.
func (s *Store) CreateInvocation(ctx context.Context, invocation *domain.AgentInvocation) (int64, error) {
	if invocation == nil {
		return 0, errors.New("store: nil invocation")
	}
	if invocation.Status == "" {
		invocation.Status = "running"
	}
	if err := validInvocationStatus(invocation.Status); err != nil {
		return 0, err
	}
	result, err := s.write.ExecContext(ctx, `
		INSERT INTO agent_invocations (
			run_id, attempt_no, kind, adapter, model, config_id, prompt_path, prompt_sha256,
			transcript_path, started_at, ended_at, exit_code, timed_out, status,
			reported_status, result_summary
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		invocation.RunID, invocation.AttemptNo, defaultString(invocation.Kind, "implement"),
		invocation.Adapter, invocation.Model, invocation.ConfigID, invocation.PromptPath,
		invocation.PromptSHA256, invocation.TranscriptPath, timeArg(invocation.StartedAt),
		timeArg(invocation.EndedAt), nullableInt(invocation.ExitCode), boolInt(invocation.TimedOut),
		invocation.Status, stringArg(invocation.ReportedStatus), stringArg(s.redact(invocation.ResultSummary)),
	)
	if err != nil {
		return 0, fmt.Errorf("store: create invocation: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: invocation id: %w", err)
	}
	invocation.ID = id
	return id, nil
}

// InvocationFinish is the terminal state of one agent attempt.
type InvocationFinish struct {
	Status         string
	ExitCode       *int
	TimedOut       bool
	ReportedStatus string
	ResultSummary  string
	EndedAt        *time.Time
}

// FinishInvocation closes an attempt with its observed exit facts.
func (s *Store) FinishInvocation(ctx context.Context, id int64, finish InvocationFinish) error {
	if err := validInvocationStatus(finish.Status); err != nil {
		return err
	}
	if finish.EndedAt == nil {
		now := nowUTC()
		finish.EndedAt = &now
	}
	result, err := s.write.ExecContext(ctx, `
		UPDATE agent_invocations SET status = ?, exit_code = ?, timed_out = ?,
		       reported_status = ?, result_summary = ?, ended_at = ?
		WHERE id = ?`,
		finish.Status, nullableInt(finish.ExitCode), boolInt(finish.TimedOut),
		stringArg(finish.ReportedStatus), stringArg(s.redact(finish.ResultSummary)),
		timeArg(finish.EndedAt), id,
	)
	if err != nil {
		return fmt.Errorf("store: finish invocation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: invocation rows affected: %w", err)
	}
	if affected != 1 {
		return ErrNotFound
	}
	return nil
}

func validInvocationStatus(status string) error {
	switch status {
	case "running", "succeeded", "failed", "timed_out", "cancelled", "blocked":
		return nil
	default:
		return fmt.Errorf("store: unknown invocation status %q", status)
	}
}

// ListInvocations returns every attempt for a run, in attempt order.
func (s *Store) ListInvocations(ctx context.Context, runID int64) ([]domain.AgentInvocation, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, run_id, attempt_no, kind, adapter, model, config_id, prompt_path,
		       prompt_sha256, transcript_path, started_at, ended_at, exit_code, timed_out,
		       status, COALESCE(reported_status, ''), COALESCE(result_summary, '')
		FROM agent_invocations WHERE run_id = ? ORDER BY attempt_no ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list invocations: %w", err)
	}
	defer rows.Close()

	var invocations []domain.AgentInvocation
	for rows.Next() {
		var (
			invocation domain.AgentInvocation
			started    sql.NullString
			ended      sql.NullString
			exitCode   sql.NullInt64
			timedOut   int64
		)
		if err := rows.Scan(
			&invocation.ID, &invocation.RunID, &invocation.AttemptNo, &invocation.Kind,
			&invocation.Adapter, &invocation.Model, &invocation.ConfigID, &invocation.PromptPath,
			&invocation.PromptSHA256, &invocation.TranscriptPath, &started, &ended,
			&exitCode, &timedOut, &invocation.Status, &invocation.ReportedStatus,
			&invocation.ResultSummary,
		); err != nil {
			return nil, fmt.Errorf("store: scan invocation: %w", err)
		}
		if invocation.StartedAt, err = scanTime(started); err != nil {
			return nil, err
		}
		if invocation.EndedAt, err = scanTime(ended); err != nil {
			return nil, err
		}
		if exitCode.Valid {
			value := int(exitCode.Int64)
			invocation.ExitCode = &value
		}
		invocation.TimedOut = intBool(timedOut)
		invocations = append(invocations, invocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate invocations: %w", err)
	}
	return invocations, nil
}

// InsertValidationResult records one check verdict. The table's CHECKs make a
// pass without exit 0 unrepresentable.
func (s *Store) InsertValidationResult(ctx context.Context, check *domain.ValidationResult) (int64, error) {
	if check == nil {
		return 0, errors.New("store: nil validation result")
	}
	if !check.Outcome.Valid() {
		return 0, fmt.Errorf("store: unknown outcome %q", check.Outcome)
	}
	result, err := s.write.ExecContext(ctx, `
		INSERT INTO validation_results (
			run_id, invocation_id, check_name, command, required, outcome, exit_code,
			duration_ms, output_excerpt, reason, executed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		check.RunID, nullableInt64(check.InvocationID), check.CheckName, check.Command,
		boolInt(check.Required), string(check.Outcome), nullableInt(check.ExitCode),
		check.DurationMS, stringArg(s.redact(check.OutputExcerpt)), stringArg(check.Reason),
		timeArg(check.ExecutedAt),
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert validation result: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: validation result id: %w", err)
	}
	check.ID = id
	return id, nil
}

// ListValidationResults returns every check verdict for a run in insertion order.
func (s *Store) ListValidationResults(ctx context.Context, runID int64) ([]domain.ValidationResult, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, run_id, invocation_id, check_name, command, required, outcome, exit_code,
		       COALESCE(duration_ms, 0), COALESCE(output_excerpt, ''), COALESCE(reason, ''),
		       executed_at
		FROM validation_results WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list validation results: %w", err)
	}
	defer rows.Close()

	var results []domain.ValidationResult
	for rows.Next() {
		var (
			check      domain.ValidationResult
			invocation sql.NullInt64
			outcome    string
			exitCode   sql.NullInt64
			required   int64
			executed   sql.NullString
		)
		if err := rows.Scan(
			&check.ID, &check.RunID, &invocation, &check.CheckName, &check.Command,
			&required, &outcome, &exitCode, &check.DurationMS, &check.OutputExcerpt,
			&check.Reason, &executed,
		); err != nil {
			return nil, fmt.Errorf("store: scan validation result: %w", err)
		}
		if invocation.Valid {
			value := invocation.Int64
			check.InvocationID = &value
		}
		check.Required = intBool(required)
		check.Outcome = domain.Outcome(outcome)
		if exitCode.Valid {
			value := int(exitCode.Int64)
			check.ExitCode = &value
		}
		if check.ExecutedAt, err = scanTime(executed); err != nil {
			return nil, err
		}
		results = append(results, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate validation results: %w", err)
	}
	return results, nil
}

// CheckAttempts groups check verdicts by the invocation they followed.
func (s *Store) CheckAttempts(ctx context.Context, runID int64) ([]domain.CheckAttempt, error) {
	invocations, err := s.ListInvocations(ctx, runID)
	if err != nil {
		return nil, err
	}
	results, err := s.ListValidationResults(ctx, runID)
	if err != nil {
		return nil, err
	}

	byInvocation := make(map[int64][]domain.ValidationResult, len(invocations))
	var orphan []domain.ValidationResult
	for _, check := range results {
		if check.InvocationID == nil {
			orphan = append(orphan, check)
			continue
		}
		byInvocation[*check.InvocationID] = append(byInvocation[*check.InvocationID], check)
	}

	attempts := make([]domain.CheckAttempt, 0, len(invocations))
	for index, invocation := range invocations {
		attempt := domain.CheckAttempt{
			AttemptNo:        invocation.AttemptNo,
			InvocationStatus: invocation.Status,
			Results:          byInvocation[invocation.ID],
			RepairPrompted:   index < len(invocations)-1,
		}
		attempt.AllRequiredPassed = allRequiredPassed(attempt.Results)
		attempts = append(attempts, attempt)
	}
	if len(attempts) == 0 && len(orphan) > 0 {
		attempt := domain.CheckAttempt{
			AttemptNo:        1,
			InvocationStatus: "unknown",
			Results:          orphan,
		}
		attempt.AllRequiredPassed = allRequiredPassed(orphan)
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}

func allRequiredPassed(results []domain.ValidationResult) bool {
	required := 0
	for _, check := range results {
		if !check.Required {
			continue
		}
		required++
		if check.Outcome != domain.OutcomePassed {
			return false
		}
	}
	return required > 0
}
