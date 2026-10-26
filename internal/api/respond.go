package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// contextWithTimeout bounds one handler's upstream work.
func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

// writeJSON emits a JSON document. The middleware's redaction writer scrubs
// the buffered body, so every JSON response is safe by construction.
func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The response could not be encoded.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
