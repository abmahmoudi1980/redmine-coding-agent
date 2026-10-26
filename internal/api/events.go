package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// replayPageSize bounds one read while replaying a run's timeline.
const replayPageSize = 500

// SSE event names documented in contracts/openapi.yaml. A persisted event type
// is forwarded as its own SSE event name.
const sseHeartbeatComment = ": heartbeat\n\n"

// handleStreamRunEvents serves text/event-stream: it replays every event after
// Last-Event-ID, then follows the live hub until a terminal state event lands
// (FR-049, SC-011).
func (s *Server) handleStreamRunEvents(w http.ResponseWriter, r *http.Request) {
	publicID := strings.TrimSpace(r.PathValue("runId"))
	if publicID == "" {
		s.writeProblem(w, http.StatusNotFound, "run_not_found", "No run matches that identifier.")
		return
	}
	if s.store == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The database is not available in this process.")
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	run, err := s.store.GetRunByPublicID(ctx, publicID)
	cancel()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeProblem(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("Run %s does not exist.", publicID))
			return
		}
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The run could not be read.")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeProblem(w, http.StatusInternalServerError, "streaming_unsupported", "This connection cannot stream events.")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	lastSent := lastEventID(r)
	// Subscribe before replaying so no event can slip through the gap between
	// the durable read and the live tail.
	subscription, unsubscribe := s.hub.Subscribe(run.ID)
	defer unsubscribe()

	state, err := s.replayEvents(r.Context(), w, flusher, run.ID, &lastSent)
	if err != nil {
		return
	}
	if state.Terminal() {
		return
	}

	heartbeat := time.NewTicker(s.heartbeat())
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, sseHeartbeatComment); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-subscription.C:
			if !ok {
				return
			}
			if event.Seq <= lastSent {
				continue
			}
			terminal, err := writeSSEEvent(w, flusher, event)
			if err != nil {
				return
			}
			lastSent = event.Seq
			if terminal != "" {
				return
			}
		}
	}
}

// replayEvents writes every stored event after lastSent and reports the run's
// state as the replay leaves it.
func (s *Server) replayEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, runID int64, lastSent *int64) (domain.RunState, error) {
	for {
		events, err := s.store.EventsAfter(ctx, runID, *lastSent, replayPageSize)
		if err != nil {
			return domain.StateQueued, err
		}
		for _, event := range events {
			terminal, writeErr := writeSSEEvent(w, flusher, event)
			if writeErr != nil {
				return domain.StateQueued, writeErr
			}
			*lastSent = event.Seq
			if terminal != "" {
				return terminal, nil
			}
		}
		if len(events) < replayPageSize {
			break
		}
	}

	// The run may already be terminal even when this reconnect asked only for
	// events it has seen; the stream closes in that case (documented contract).
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return domain.StateQueued, err
	}
	if !run.State.Terminal() {
		return run.State, nil
	}

	// Drain anything committed between the last read and the state read, so the
	// terminal marker is never the one frame a client misses.
	trailing, err := s.store.EventsAfter(ctx, runID, *lastSent, replayPageSize)
	if err != nil {
		return run.State, err
	}
	for _, event := range trailing {
		if _, writeErr := writeSSEEvent(w, flusher, event); writeErr != nil {
			return run.State, writeErr
		}
		*lastSent = event.Seq
	}
	return run.State, nil
}

// writeSSEEvent frames one persisted event. The returned state is non-empty
// when the event is the terminal marker.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event domain.RunEvent) (domain.RunState, error) {
	data, err := ssePayload(event)
	if err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, data); err != nil {
		return "", err
	}
	flusher.Flush()
	return terminalStateIn(event), nil
}

// ssePayload merges the persisted message with any structured fields the
// producer stored, so the UI can read `state`/`stage` straight off the frame.
func ssePayload(event domain.RunEvent) ([]byte, error) {
	payload := map[string]any{
		"seq":      event.Seq,
		"at":       event.At,
		"type":     string(event.Type),
		"severity": event.Severity,
		"message":  event.Message,
	}
	if raw := strings.TrimSpace(event.Data); raw != "" {
		var structured map[string]any
		if err := json.Unmarshal([]byte(raw), &structured); err == nil {
			for key, value := range structured {
				if _, reserved := payload[key]; reserved {
					continue
				}
				payload[key] = value
			}
		}
	}
	return json.Marshal(payload)
}

// EventPayload exposes the SSE data document so `run --format json` can emit
// the same event shapes as the browser stream (contracts/cli.md §Machine output).
func EventPayload(event domain.RunEvent) ([]byte, error) {
	return ssePayload(event)
}

// terminalStateIn reads the state a `state` event reported, if it is terminal.
func terminalStateIn(event domain.RunEvent) domain.RunState {
	if event.Type != domain.EventState {
		return ""
	}
	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		return ""
	}
	state := domain.RunState(payload.State)
	if !state.Valid() || !state.Terminal() {
		return ""
	}
	return state
}

// lastEventID reads the resume point from the header the browser sends, with a
// query fallback for clients that reconnect by hand.
func lastEventID(r *http.Request) int64 {
	candidates := []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("lastEventId")}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		value, err := strconv.ParseInt(candidate, 10, 64)
		if err == nil && value >= 0 {
			return value
		}
	}
	return 0
}

func (s *Server) heartbeat() time.Duration {
	if s != nil && s.heartbeatInterval > 0 {
		return s.heartbeatInterval
	}
	return 15 * time.Second
}
