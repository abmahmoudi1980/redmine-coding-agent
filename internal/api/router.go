package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// BuildInfo is stamped into the binary at build time.
type BuildInfo struct {
	Version       string
	Commit        string
	BuiltAt       string
	OS            string
	Arch          string
	SchemaVersion int
}

// Deps are the collaborators a Server needs. Only Config, Store, and Logger
// are required for real traffic; the contract test may leave them nil.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Logger   *logging.Logger
	Redactor *logging.Redactor
	Redmine  *redmine.Client
	Runs     RunService
	Hub      *Hub
	Build    BuildInfo
}

// Server owns routing and middleware.
type Server struct {
	config            *config.Config
	store             *store.Store
	logger            *logging.Logger
	redactor          *logging.Redactor
	redmine           *redmine.Client
	runs              RunService
	hub               *Hub
	build             BuildInfo
	heartbeatInterval time.Duration
}

// NewServer builds a Server, defaulting the redactor so scrubbing is never
// accidentally disabled. It also connects the store's committed-event tail to
// the hub, which is what makes the SSE stream live.
func NewServer(deps Deps) *Server {
	redactor := deps.Redactor
	if redactor == nil {
		redactor = logging.NewRedactor()
	}
	hub := deps.Hub
	if hub == nil {
		hub = NewHub()
	}
	if deps.Store != nil {
		deps.Store.SetEventSink(hub.Publish)
	}
	return &Server{
		config:   deps.Config,
		store:    deps.Store,
		logger:   deps.Logger,
		redactor: redactor,
		redmine:  deps.Redmine,
		runs:     deps.Runs,
		hub:      hub,
		build:    deps.Build,
	}
}

// Route is one documented endpoint.
type Route struct {
	Method    string
	Path      string
	Operation string
	Handler   http.HandlerFunc
}

// Routes returns the documented route table, used by the router and by the
// contract test that guards drift from contracts/openapi.yaml.
func (s *Server) Routes() []Route {
	return []Route{
		{http.MethodGet, "/api/meta", "getMeta", s.notImplemented("getMeta")},
		{http.MethodGet, "/api/config", "getEffectiveConfig", s.notImplemented("getEffectiveConfig")},
		{http.MethodGet, "/api/health", "getHealth", s.handleHealth},
		{http.MethodGet, "/api/repositories", "listRepositories", s.handleListRepositories},
		{http.MethodGet, "/api/issues/{issueId}/preview", "previewIssue", s.handlePreviewIssue},
		{http.MethodPost, "/api/runs", "createRun", s.handleCreateRun},
		{http.MethodGet, "/api/runs", "listRuns", s.handleListRuns},
		{http.MethodGet, "/api/runs/{runId}", "getRun", s.handleGetRun},
		{http.MethodPost, "/api/runs/{runId}/cancel", "cancelRun", s.handleCancelRun},
		{http.MethodPost, "/api/runs/{runId}/retry", "retryRun", s.handleRetryRun},
		{http.MethodGet, "/api/runs/{runId}/checks", "getRunChecks", s.handleGetRunChecks},
		{http.MethodGet, "/api/runs/{runId}/report", "getRunReport", s.handleGetRunReport},
		{http.MethodGet, "/api/runs/{runId}/logs", "getRunLogs", s.handleGetRunLogs},
		{http.MethodGet, "/api/runs/{runId}/events", "streamRunEvents", s.handleStreamRunEvents},
	}
}

// Handler builds the middleware chain:
// panic recovery -> request id -> access log -> redaction -> handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, route := range s.Routes() {
		mux.HandleFunc(route.Method+" "+route.Path, route.Handler)
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		s.writeProblem(w, http.StatusNotFound, "not_found", fmt.Sprintf("No API route matches %s %s.", r.Method, r.URL.Path))
	})
	mux.Handle("/", newStaticHandler())

	var handler http.Handler = mux
	handler = s.redactionMiddleware(handler)
	handler = s.accessLogMiddleware(handler)
	handler = s.requestIDMiddleware(handler)
	handler = s.recoveryMiddleware(handler)
	return handler
}

func (s *Server) notImplemented(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeProblem(w, http.StatusNotImplemented, "not_implemented", operation+" is not implemented in this build yet.")
	}
}

func (s *Server) redact(text string) string {
	if s == nil || s.redactor == nil {
		return text
	}
	return s.redactor.Redact(text)
}

func (s *Server) log() *slog.Logger {
	if s != nil && s.logger != nil {
		return s.logger.Base()
	}
	return slog.Default()
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		started := time.Now()
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log().Info("http request",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log().Error("panic while serving request",
					"request_id", requestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(recovered),
				)
				s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The worker hit an internal error; see its logs.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) redactionMiddleware(next http.Handler) http.Handler {
	if s.redactor == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &redactingResponseWriter{ResponseWriter: w, redactor: s.redactor}
		next.ServeHTTP(writer, r)
		writer.finish()
	})
}

// statusRecorder captures the status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	written, err := r.ResponseWriter.Write(p)
	r.bytes += written
	return written, err
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// redactingResponseWriter buffers JSON/text bodies so a secret is scrubbed
// before the client sees it. Streaming responses (SSE) pass straight through,
// because their events are redacted at the source.
type redactingResponseWriter struct {
	http.ResponseWriter
	redactor *logging.Redactor
	status   int
	buffered bool
	decided  bool
	body     bytes.Buffer
}

func (w *redactingResponseWriter) WriteHeader(code int) {
	if w.decided {
		return
	}
	w.decided = true
	w.status = code
	contentType := w.Header().Get("Content-Type")
	w.buffered = w.redactor != nil &&
		(strings.Contains(contentType, "json") || strings.HasPrefix(contentType, "text/plain")) &&
		w.Header().Get("Content-Length") == ""
	if !w.buffered {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *redactingResponseWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.buffered {
		return w.body.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *redactingResponseWriter) Flush() {
	if w.buffered {
		w.flushBuffered()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *redactingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *redactingResponseWriter) finish() {
	if !w.buffered {
		return
	}
	w.flushBuffered()
}

func (w *redactingResponseWriter) flushBuffered() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.buffered = false
	w.ResponseWriter.WriteHeader(status)
	_, _ = io.WriteString(w.ResponseWriter, w.redactor.Redact(w.body.String()))
	w.body.Reset()
}
