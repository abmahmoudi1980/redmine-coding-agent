package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// UpsertReport writes the final report for a run. Uniqueness on run_id keeps
// repeated publication idempotent (FR-043, FR-045).
func (s *Store) UpsertReport(ctx context.Context, report *domain.Report) (int64, error) {
	if report == nil {
		return 0, errors.New("store: nil report")
	}
	if report.DeliveryStatus == "" {
		report.DeliveryStatus = "pending"
	}
	if report.CreatedAt.IsZero() {
		report.CreatedAt = nowUTC()
	}
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO reports (
			run_id, body_md, commit_sha, merge_request_url, delivery_status,
			delivery_error, dedupe_key, created_at, delivered_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			body_md = excluded.body_md,
			commit_sha = excluded.commit_sha,
			merge_request_url = excluded.merge_request_url,
			delivery_status = excluded.delivery_status,
			delivery_error = excluded.delivery_error,
			dedupe_key = excluded.dedupe_key,
			delivered_at = excluded.delivered_at`,
		report.RunID, s.redact(report.BodyMD), stringArg(report.CommitSHA),
		stringArg(report.MergeRequestURL), report.DeliveryStatus,
		stringArg(s.redact(report.DeliveryError)), report.DedupeKey,
		domain.FormatTime(report.CreatedAt), timeArg(report.DeliveredAt),
	)
	if err != nil {
		return 0, fmt.Errorf("store: upsert report: %w", err)
	}
	var id int64
	if err := s.write.QueryRowContext(ctx, `SELECT id FROM reports WHERE run_id = ?`, report.RunID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: report id: %w", err)
	}
	report.ID = id
	return id, nil
}

// GetReport loads the report for a run.
func (s *Store) GetReport(ctx context.Context, runID int64) (*domain.Report, error) {
	var (
		report    domain.Report
		created   sql.NullString
		delivered sql.NullString
	)
	err := s.read.QueryRowContext(ctx, `
		SELECT id, run_id, body_md, COALESCE(commit_sha, ''), COALESCE(merge_request_url, ''),
		       delivery_status, COALESCE(delivery_error, ''), dedupe_key, created_at, delivered_at
		FROM reports WHERE run_id = ?`, runID).Scan(
		&report.ID, &report.RunID, &report.BodyMD, &report.CommitSHA,
		&report.MergeRequestURL, &report.DeliveryStatus, &report.DeliveryError,
		&report.DedupeKey, &created, &delivered,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get report: %w", err)
	}
	if report.CreatedAt, err = requireTime(created); err != nil {
		return nil, err
	}
	if report.DeliveredAt, err = scanTime(delivered); err != nil {
		return nil, err
	}
	return &report, nil
}

// GetReportByDedupeKey finds a report by its stable marker.
func (s *Store) GetReportByDedupeKey(ctx context.Context, dedupeKey string) (*domain.Report, error) {
	var runID int64
	err := s.read.QueryRowContext(ctx, `SELECT run_id FROM reports WHERE dedupe_key = ? LIMIT 1`, dedupeKey).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get report by dedupe key: %w", err)
	}
	return s.GetReport(ctx, runID)
}

// SaveConfigurationProfile stores a redacted config snapshot, reusing the row
// when the hash is already known.
func (s *Store) SaveConfigurationProfile(ctx context.Context, profile *domain.ConfigurationProfile) (int64, error) {
	if profile == nil {
		return 0, errors.New("store: nil configuration profile")
	}
	if profile.ContentHash == "" {
		return 0, errors.New("store: configuration profile hash is required")
	}
	if profile.LoadedAt.IsZero() {
		profile.LoadedAt = nowUTC()
	}
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO configuration_profiles (content_hash, schema_version, source_path, content, loaded_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(content_hash) DO NOTHING`,
		profile.ContentHash, profile.SchemaVersion, profile.SourcePath,
		s.redact(profile.Content), domain.FormatTime(profile.LoadedAt),
	)
	if err != nil {
		return 0, fmt.Errorf("store: save configuration profile: %w", err)
	}
	existing, err := s.GetConfigurationProfileByHash(ctx, profile.ContentHash)
	if err != nil {
		return 0, err
	}
	profile.ID = existing.ID
	return existing.ID, nil
}

// GetConfigurationProfileByHash loads a profile by its content hash.
func (s *Store) GetConfigurationProfileByHash(ctx context.Context, hash string) (*domain.ConfigurationProfile, error) {
	profile, err := scanProfile(s.read.QueryRowContext(ctx, `
		SELECT id, content_hash, schema_version, source_path, content, loaded_at
		FROM configuration_profiles WHERE content_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get configuration profile: %w", err)
	}
	return profile, nil
}

// GetConfigurationProfile loads a profile by primary key.
func (s *Store) GetConfigurationProfile(ctx context.Context, id int64) (*domain.ConfigurationProfile, error) {
	profile, err := scanProfile(s.read.QueryRowContext(ctx, `
		SELECT id, content_hash, schema_version, source_path, content, loaded_at
		FROM configuration_profiles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get configuration profile: %w", err)
	}
	return profile, nil
}

func scanProfile(scanner interface{ Scan(...any) error }) (*domain.ConfigurationProfile, error) {
	var (
		profile domain.ConfigurationProfile
		loaded  sql.NullString
	)
	if err := scanner.Scan(&profile.ID, &profile.ContentHash, &profile.SchemaVersion,
		&profile.SourcePath, &profile.Content, &loaded); err != nil {
		return nil, err
	}
	loadedAt, err := requireTime(loaded)
	if err != nil {
		return nil, err
	}
	profile.LoadedAt = loadedAt
	return &profile, nil
}
