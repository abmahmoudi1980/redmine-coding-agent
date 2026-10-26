package api

import (
	"context"
	"net/http"
	"time"
)

type healthComponent struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	LatencyMS   int64  `json:"latencyMs,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

type healthReport struct {
	Status     string            `json:"status"`
	CheckedAt  time.Time         `json:"checkedAt"`
	Components []healthComponent `json:"components"`
}

// handleHealth reports worker readiness only; deep dependency probes arrive
// with User Story 6. A degraded dependency never becomes a 5xx.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	component := healthComponent{Name: "database", Status: "ok"}

	switch s.store {
	case nil:
		component.Status = "failing"
		component.Detail = "No database is attached to this worker."
		component.Remediation = "Start the worker with `redmine-agent serve --config <path>`."
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := s.store.Reader().PingContext(ctx)
		cancel()
		if err != nil {
			component.Status = "failing"
			component.Detail = s.redact(err.Error())
			component.Remediation = "Check data.database and that the directory is writable."
		}
	}
	component.LatencyMS = time.Since(started).Milliseconds()

	status := "ok"
	if component.Status != "ok" {
		status = "degraded"
	}

	// The query parameters are accepted so the contract stays stable; deep
	// probing is added in User Story 6.
	_ = r.URL.Query().Get("deep")

	s.writeJSON(w, http.StatusOK, healthReport{
		Status:     status,
		CheckedAt:  time.Now().UTC(),
		Components: []healthComponent{component},
	})
}
