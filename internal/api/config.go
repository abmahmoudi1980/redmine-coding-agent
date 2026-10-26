package api

import (
	"net/http"
	"time"
)

// effectiveConfigDocument is the redacted configuration the UI shows. It is the
// same document the worker persists to configuration_profiles for audit.
type effectiveConfigDocument struct {
	SchemaVersion int            `json:"schemaVersion"`
	ContentHash   string         `json:"contentHash"`
	LoadedAt      time.Time      `json:"loadedAt"`
	Redacted      map[string]any `json:"redacted"`
}

// handleGetConfig returns the effective non-secret configuration, with every
// credential reduced to its reference (FR-055, FR-056).
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready",
			"No configuration is loaded in this process.")
		return
	}

	hash := s.config.ContentHash()
	if hash == "" {
		s.writeProblem(w, http.StatusInternalServerError, "internal_error",
			"The effective configuration could not be hashed.")
		return
	}

	loadedAt := s.config.LoadedAt
	if loadedAt.IsZero() {
		loadedAt = time.Now().UTC()
	}

	s.writeJSON(w, http.StatusOK, effectiveConfigDocument{
		SchemaVersion: s.config.SchemaVersion,
		ContentHash:   hash,
		LoadedAt:      loadedAt,
		Redacted:      s.config.RedactedDocument(),
	})
}
