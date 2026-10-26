package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/health"
)

// handleHealth reports worker and dependency readiness. A failing dependency
// degrades the document; the request itself stays a 200 so the UI can render
// partial failure (FR-058).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.healthReport(r, wantsDeepProbe(r)))
}

func wantsDeepProbe(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("deep"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func (s *Server) healthReport(r *http.Request, deep bool) health.Report {
	if s.health != nil {
		return s.health.Run(r.Context(), deep)
	}
	// A server built without a checker still answers: the database is the one
	// dependency it can establish on its own.
	component := s.databaseComponent(r.Context())
	status := "ok"
	if component.Status == health.StatusFailing {
		status = "degraded"
	}
	return health.Report{
		Status:     status,
		CheckedAt:  time.Now().UTC(),
		Components: []health.Component{component},
	}
}

func (s *Server) databaseComponent(ctx context.Context) health.Component {
	started := time.Now()
	component := health.Component{
		Name:        "database",
		Status:      health.StatusOK,
		Remediation: "Check data.database and that the directory is writable.",
	}
	if s.store == nil {
		component.Status = health.StatusFailing
		component.Detail = "No database is attached to this worker."
		component.Remediation = "Start the worker with `redmine-agent serve --config <path>`."
		component.LatencyMS = time.Since(started).Milliseconds()
		return component
	}

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.store.Reader().PingContext(probeCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			component.Detail = "The database did not answer within 2 seconds."
		} else {
			component.Detail = s.redact(err.Error())
		}
		component.Status = health.StatusFailing
	}
	component.LatencyMS = time.Since(started).Milliseconds()
	return component
}
