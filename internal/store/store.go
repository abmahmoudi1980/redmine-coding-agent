// Package store owns the SQLite database: connections, migrations, and the
// small set of hand-written queries the orchestrator needs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // Blank import registers the pure-Go SQLite driver.
)

// Redactor scrubs secret values from text before it is persisted.
type Redactor interface {
	Redact(string) string
}

type noopRedactor struct{}

func (noopRedactor) Redact(s string) string { return s }

// Store holds one writer connection and a read pool over the same database.
type Store struct {
	path     string
	write    *sql.DB
	read     *sql.DB
	redactor Redactor

	closeOnce sync.Once
	closeErr  error
}

// Option customises Open.
type Option func(*Store)

// WithRedactor installs the secret scrubber used on persisted text.
func WithRedactor(r Redactor) Option {
	return func(s *Store) {
		if r != nil {
			s.redactor = r
		}
	}
}

// Open prepares the database, applies pragmas on every connection, and runs
// pending migrations. It never creates credentials or touches the network.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path is required")
	}
	s := &Store{path: path, redactor: noopRedactor{}}
	for _, opt := range opts {
		opt(s)
	}

	memory := path == ":memory:"
	if !memory {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("store: resolve database path: %w", err)
		}
		s.path = abs
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return nil, fmt.Errorf("store: create data dir: %w", err)
		}
	}

	write, err := sql.Open("sqlite", writeDSN(s.path, memory))
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)

	var read *sql.DB
	if memory {
		read = write
	} else {
		read, err = sql.Open("sqlite", readDSN(s.path))
		if err != nil {
			_ = write.Close()
			return nil, fmt.Errorf("store: open reader: %w", err)
		}
		read.SetMaxOpenConns(4)
		read.SetMaxIdleConns(4)
		read.SetConnMaxLifetime(0)
	}

	if err := write.PingContext(ctx); err != nil {
		_ = write.Close()
		if read != write {
			_ = read.Close()
		}
		return nil, fmt.Errorf("store: ping writer: %w", err)
	}
	if read != write {
		if err := read.PingContext(ctx); err != nil {
			_ = write.Close()
			_ = read.Close()
			return nil, fmt.Errorf("store: ping reader: %w", err)
		}
	}

	s.write = write
	s.read = read
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func writeDSN(path string, memory bool) string {
	return dsn(path, true, memory)
}

func readDSN(path string) string {
	return dsn(path, false, false)
}

func dsn(path string, writer, memory bool) string {
	values := url.Values{}
	values.Add("_pragma", "busy_timeout(5000)")
	if !memory {
		values.Add("_pragma", "journal_mode(WAL)")
	}
	values.Add("_pragma", "foreign_keys(on)")
	values.Add("_pragma", "synchronous(NORMAL)")
	if writer {
		values.Add("_txlock", "immediate")
	}
	return path + "?" + values.Encode()
}

// Writer exposes the single writer connection. Callers that need
// multiple statements in one transaction must use InTx.
func (s *Store) Writer() *sql.DB { return s.write }

// Reader exposes the read pool.
func (s *Store) Reader() *sql.DB { return s.read }

// Path is the resolved database file path.
func (s *Store) Path() string { return s.path }

// Tx is the subset of *sql.Tx used by store methods.
type Tx struct {
	tx *sql.Tx
}

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// InTx runs fn inside a write transaction. The writer connection is opened
// with _txlock=immediate, so the transaction takes the write lock up front and
// cannot lose a race between a read and a write.
func (s *Store) InTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(&Tx{tx: tx}); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return fmt.Errorf("%w (rollback: %v)", err, rollbackErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Close checkpoints the WAL and closes both pools. It is safe to call twice.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var errs []error
		if s.write != nil {
			if _, err := s.write.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				errs = append(errs, fmt.Errorf("store: checkpoint: %w", err))
			}
		}
		if s.read != nil && s.read != s.write {
			if err := s.read.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if s.write != nil {
			if err := s.write.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

func (s *Store) redact(text string) string {
	if s == nil || s.redactor == nil {
		return text
	}
	return s.redactor.Redact(text)
}
