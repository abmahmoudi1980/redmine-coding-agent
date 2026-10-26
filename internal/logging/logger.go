package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Options configures the process logger.
type Options struct {
	LogDir   string
	Redactor *Redactor
	Stderr   io.Writer
	Level    slog.Level
}

// Logger writes JSON records to stderr and, inside WithRun, to a per-run file
// as well. Both sinks pass through the redaction writer.
type Logger struct {
	logDir        string
	redactor      *Redactor
	level         slog.Level
	stderrHandler slog.Handler
	stderrWriter  *redactingWriter
	base          *slog.Logger
}

// New builds the process logger.
func New(opts Options) *Logger {
	redactor := opts.Redactor
	if redactor == nil {
		redactor = NewRedactor()
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	writer := redactor.Writer(stderr)
	redacting, _ := writer.(*redactingWriter)
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: opts.Level})
	return &Logger{
		logDir:        opts.LogDir,
		redactor:      redactor,
		level:         opts.Level,
		stderrHandler: handler,
		stderrWriter:  redacting,
		base:          slog.New(handler),
	}
}

// Base is the process-wide logger without run fields.
func (l *Logger) Base() *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l.base
}

// Redactor exposes the registry so resolved secrets can be registered as they
// are read.
func (l *Logger) Redactor() *Redactor {
	if l == nil {
		return nil
	}
	return l.redactor
}

// RunLog is a logger scoped to one run, fanning out to stderr and its own file.
type RunLog struct {
	*slog.Logger
	file   *os.File
	writer *redactingWriter
	scope  *scope
}

// WithRun opens <log_dir>/<run_id>.log and returns a logger whose every record
// carries run_id and stage.
func (l *Logger) WithRun(runID, stage string) (*RunLog, error) {
	if l == nil {
		return nil, errors.New("logging: nil logger")
	}
	if err := os.MkdirAll(l.logDir, 0o750); err != nil {
		return nil, fmt.Errorf("logging: create log dir: %w", err)
	}
	path := filepath.Join(l.logDir, sanitizeRunID(runID)+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("logging: open run log: %w", err)
	}
	writer := l.redactor.Writer(file).(*redactingWriter)
	fileHandler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: l.level})
	sc := &scope{attrs: []slog.Attr{
		slog.String("run_id", runID),
		slog.String("stage", string(stage)),
	}}
	handler := &scopeHandler{
		next:  teeHandler{handlers: []slog.Handler{l.stderrHandler, fileHandler}},
		scope: sc,
	}
	return &RunLog{
		Logger: slog.New(handler),
		file:   file,
		writer: writer,
		scope:  sc,
	}, nil
}

// WithStage updates the stage attribute on every subsequent record.
func (r *RunLog) WithStage(stage string) *RunLog {
	if r == nil {
		return r
	}
	r.scope.SetStage(string(stage))
	return r
}

// Close flushes and closes the per-run file.
func (r *RunLog) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	if r.writer != nil {
		if err := r.writer.Flush(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			errs = append(errs, err)
		}
		r.file = nil
	}
	return errors.Join(errs...)
}

// Flush flushes buffered stderr text.
func (l *Logger) Flush() error {
	if l == nil || l.stderrWriter == nil {
		return nil
	}
	return l.stderrWriter.Flush()
}

// RunLogPath is where WithRun writes a run's log file. The API uses it to tail
// the already-redacted file on demand (T103).
func (l *Logger) RunLogPath(runID string) string {
	if l == nil {
		return ""
	}
	return filepath.Join(l.logDir, sanitizeRunID(runID)+".log")
}

func sanitizeRunID(runID string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return "unknown"
	}
	replacer := strings.NewReplacer(
		"/", "_", `\`, "_", ":", "_", "*", "_", "?", "_",
		`"`, "_", "<", "_", ">", "_", "|", "_", "\x00", "_",
	)
	cleaned := replacer.Replace(runID)
	if cleaned == "." || cleaned == ".." {
		return "unknown"
	}
	return cleaned
}

type teeHandler struct {
	handlers []slog.Handler
}

func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range t.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (t teeHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, handler := range t.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(t.handlers))
	for _, handler := range t.handlers {
		next = append(next, handler.WithAttrs(attrs))
	}
	return teeHandler{handlers: next}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(t.handlers))
	for _, handler := range t.handlers {
		next = append(next, handler.WithGroup(name))
	}
	return teeHandler{handlers: next}
}

type scope struct {
	mu    sync.RWMutex
	attrs []slog.Attr
}

func (s *scope) snapshot() []slog.Attr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]slog.Attr, len(s.attrs))
	copy(out, s.attrs)
	return out
}

func (s *scope) SetStage(stage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.attrs {
		if s.attrs[i].Key == "stage" {
			s.attrs[i] = slog.String("stage", stage)
			return
		}
	}
	s.attrs = append(s.attrs, slog.String("stage", stage))
}

type scopeHandler struct {
	next  slog.Handler
	scope *scope
}

func (h *scopeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *scopeHandler) Handle(ctx context.Context, record slog.Record) error {
	scoped := record.Clone()
	scoped.AddAttrs(h.scope.snapshot()...)
	return h.next.Handle(ctx, scoped)
}

func (h *scopeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &scopeHandler{next: h.next.WithAttrs(attrs), scope: h.scope}
}

func (h *scopeHandler) WithGroup(name string) slog.Handler {
	return &scopeHandler{next: h.next.WithGroup(name), scope: h.scope}
}
