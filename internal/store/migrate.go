package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrate applies every embedded migration that has not run yet, each in its
// own transaction, and records it in schema_migrations.
func (s *Store) migrate(ctx context.Context) error {
	const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`
	if _, err := s.write.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := make(map[int]bool)
	rows, err := s.write.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: close schema_migrations: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	sort.Strings(entries)

	seen := make(map[int]string, len(entries))
	for _, entry := range entries {
		version, err := migrationVersion(entry)
		if err != nil {
			return err
		}
		if previous, exists := seen[version]; exists {
			return fmt.Errorf("store: migrations %s and %s share version %d", previous, entry, version)
		}
		seen[version] = entry
		if applied[version] {
			continue
		}

		content, err := migrationFiles.ReadFile(entry)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", entry, err)
		}
		statements := splitStatements(string(content))
		if err := s.InTx(ctx, func(tx *Tx) error {
			for index, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("store: migration %s statement %d: %w", entry, index+1, err)
				}
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
				version, entry, domain.FormatTime(nowUTC()),
			)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion is the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version *int
	if err := s.read.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: schema version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}

// LatestSchemaVersion is the highest migration version this build embeds, so a
// health probe can tell "current" from "pending migration".
func LatestSchemaVersion() int {
	entries, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return 0
	}
	latest := 0
	for _, entry := range entries {
		version, err := migrationVersion(entry)
		if err != nil {
			continue
		}
		if version > latest {
			latest = version
		}
	}
	return latest
}

func migrationVersion(entry string) (int, error) {
	base := strings.TrimPrefix(entry, "migrations/")
	prefix, _, ok := strings.Cut(base, "_")
	if !ok {
		return 0, fmt.Errorf("store: migration %q must be named <version>_<name>.sql", entry)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("store: migration %q has an invalid version prefix", entry)
	}
	return version, nil
}

// splitStatements splits a SQL script on semicolons while ignoring semicolons
// inside single-quoted strings and comments. Embedded migrations stay readable
// as whole files instead of one giant Exec string.
func splitStatements(script string) []string {
	var statements []string
	var builder strings.Builder
	runes := []rune(script)

	inSingleQuote := false
	inLineComment := false
	inBlockComment := false

	flush := func() {
		statement := strings.TrimSpace(builder.String())
		if statement != "" {
			statements = append(statements, statement)
		}
		builder.Reset()
	}

	for i := 0; i < len(runes); i++ {
		current := runes[i]
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		switch {
		case inLineComment:
			if current == '\n' {
				inLineComment = false
				builder.WriteRune(current)
			}
		case inBlockComment:
			if current == '*' && next == '/' {
				inBlockComment = false
				i++
			}
		case inSingleQuote:
			builder.WriteRune(current)
			if current == '\'' {
				if next == '\'' {
					builder.WriteRune(next)
					i++
				} else {
					inSingleQuote = false
				}
			}
		case current == '\'':
			inSingleQuote = true
			builder.WriteRune(current)
		case current == '-' && next == '-':
			inLineComment = true
			i++
		case current == '/' && next == '*':
			inBlockComment = true
			i++
		case current == ';':
			flush()
		default:
			builder.WriteRune(current)
		}
	}
	flush()
	return statements
}
