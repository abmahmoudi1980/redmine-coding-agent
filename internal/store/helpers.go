package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

func nowUTC() time.Time { return time.Now().UTC() }

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return domain.FormatTime(*t)
}

func stringArg(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scanTime(raw sql.NullString) (*time.Time, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	parsed, err := domain.ParseTime(raw.String)
	if err != nil {
		return nil, fmt.Errorf("store: parse time %q: %w", raw.String, err)
	}
	return &parsed, nil
}

func mustJSON(value any) string {
	if value == nil {
		return "[]"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intBool(value int64) bool { return value != 0 }

func scanJSONArray[T any](raw string) []T {
	if raw == "" {
		return nil
	}
	var out []T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// scanRun consumes the column list in runSelect.
func scanRun(scanner interface{ Scan(...any) error }) (*domain.Run, error) {
	var (
		run       domain.Run
		snapshot  int64
		repoID    sql.NullInt64
		prevID    sql.NullInt64
		profile   sql.NullInt64
		state     string
		stage     string
		pushed    sql.NullString
		created   sql.NullString
		started   sql.NullString
		ended     sql.NullString
		cancel    sql.NullString
		interrupt string
		failure   string
	)
	err := scanner.Scan(
		&run.ID, &run.PublicID, &run.IssueID, &run.IssueSubject, &snapshot,
		&repoID, &prevID, &state, &stage,
		&run.BaseBranch, &run.BaseRevision, &run.Branch, &run.CommitSHA, &pushed,
		&run.AgentName, &run.AgentModel, &run.AgentConfigID, &profile, &run.RequestedBy,
		&created, &started, &ended, &cancel, &interrupt, &failure,
		&run.FailureMessage, &run.Summary,
	)
	if err != nil {
		return nil, err
	}
	run.IssueSnapshotID = snapshot
	run.State = domain.RunState(state)
	run.Stage = domain.Stage(stage)
	run.InterruptedStage = domain.Stage(interrupt)
	run.FailureCode = domain.FailureCode(failure)
	if repoID.Valid {
		value := repoID.Int64
		run.RepositoryID = &value
	}
	if prevID.Valid {
		value := prevID.Int64
		run.PreviousRunID = &value
	}
	if profile.Valid {
		value := profile.Int64
		run.ConfigProfileID = &value
	}
	if run.PushedAt, err = scanTime(pushed); err != nil {
		return nil, err
	}
	if run.CreatedAt, err = requireTime(created); err != nil {
		return nil, err
	}
	if run.StartedAt, err = scanTime(started); err != nil {
		return nil, err
	}
	if run.EndedAt, err = scanTime(ended); err != nil {
		return nil, err
	}
	if run.CancelRequestedAt, err = scanTime(cancel); err != nil {
		return nil, err
	}
	return &run, nil
}

func requireTime(raw sql.NullString) (time.Time, error) {
	parsed, err := scanTime(raw)
	if err != nil {
		return time.Time{}, err
	}
	if parsed == nil {
		return time.Time{}, fmt.Errorf("store: required timestamp is missing")
	}
	return *parsed, nil
}
