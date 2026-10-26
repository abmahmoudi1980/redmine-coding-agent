package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// streamEnv is a real store plus an HTTP server, so SSE replay runs against
// the same query path production uses.
type streamEnv struct {
	t      *testing.T
	store  *store.Store
	run    *domain.Run
	server *httptest.Server
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	run := &domain.Run{
		PublicID: "018f2e4a-0000-7000-8000-000000000001",
		IssueID:  4821,
		State:    domain.StateQueued,
		Stage:    domain.StageCreated,
	}
	snapshot := &domain.IssueSnapshot{
		RedmineIssueID:    4821,
		Subject:           "Invoice totals round incorrectly",
		ProjectIdentifier: "billing",
		ContentHash:       "fixture-hash",
	}
	if err := st.ReserveRun(context.Background(), snapshot, run, 1); err != nil {
		t.Fatalf("reserve run: %v", err)
	}

	server := NewServer(Deps{
		Store:  st,
		Logger: logging.New(logging.Options{LogDir: t.TempDir(), Stderr: io.Discard}),
		Hub:    NewHub(),
	})
	server.heartbeatInterval = 25 * time.Millisecond
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	return &streamEnv{t: t, store: st, run: run, server: httpServer}
}

func (e *streamEnv) appendEvent(event domain.RunEvent) {
	e.t.Helper()
	if _, err := e.store.AppendEvent(context.Background(), e.run.ID, event); err != nil {
		e.t.Fatalf("append event: %v", err)
	}
}

type sseFrame struct {
	id    string
	event string
	data  string
}

func readFrame(t *testing.T, reader *bufio.Reader) sseFrame {
	t.Helper()
	frame := sseFrame{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line: %v (so far %+v)", err, frame)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if frame.id != "" || frame.event != "" || frame.data != "" {
				return frame
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "id":
			frame.id = value
		case "event":
			frame.event = value
		case "data":
			frame.data = value
		}
	}
}

func (e *streamEnv) open(lastEventID string) (*http.Response, *bufio.Reader) {
	e.t.Helper()
	request, err := http.NewRequest(http.MethodGet, e.server.URL+"/api/runs/"+e.run.PublicID+"/events", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	//nolint:bodyclose // the body stays open so the stream can be read; the cleanup below closes it.
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		e.t.Fatalf("open stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		e.t.Fatalf("stream status = %d: %s", response.StatusCode, body)
	}
	e.t.Cleanup(func() { _ = response.Body.Close() })
	return response, bufio.NewReader(response.Body)
}

func TestRunEventStreamEmitsReplayLiveAndClosesOnTerminal(t *testing.T) {
	env := newStreamEnv(t)
	env.appendEvent(domain.RunEvent{Type: domain.EventActivity, Message: "one"})
	env.appendEvent(domain.RunEvent{Type: domain.EventActivity, Message: "two"})

	//nolint:bodyclose // the stream body is owned by env.open's cleanup.
	response, reader := env.open("1")
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("content type = %q", contentType)
	}
	if cache := response.Header.Get("Cache-Control"); cache != "no-cache" {
		t.Fatalf("cache control = %q", cache)
	}

	replayed := readFrame(t, reader)
	if replayed.id != "2" || replayed.event != "activity" {
		t.Fatalf("replayed frame = %+v", replayed)
	}
	if !strings.Contains(replayed.data, `"two"`) {
		t.Fatalf("replayed data = %q", replayed.data)
	}
	if strings.Contains(replayed.data, `"one"`) {
		t.Fatalf("replay must start strictly after Last-Event-ID: %q", replayed.data)
	}

	env.appendEvent(domain.RunEvent{Type: domain.EventActivity, Message: "three"})
	live := readFrame(t, reader)
	if live.id != "3" || !strings.Contains(live.data, `"three"`) {
		t.Fatalf("live frame = %+v", live)
	}

	err := env.store.TransitionRun(context.Background(), env.run.ID, store.Transition{
		To:    domain.StateFailed,
		Stage: domain.AgentInvokedStage(1),
		Event: domain.RunEvent{
			Type:     domain.EventState,
			Severity: domain.SeverityWarn,
			Message:  "run ended failed",
			Data:     `{"state":"failed","stage":"agent.invoked:1"}`,
		},
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}

	terminal := readFrame(t, reader)
	if terminal.event != "state" || !strings.Contains(terminal.data, `"failed"`) {
		t.Fatalf("terminal frame = %+v", terminal)
	}

	closed := make(chan struct{})
	go func() {
		for {
			if _, err := reader.ReadByte(); err != nil {
				close(closed)
				return
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream must close after the terminal event")
	}
}

func TestRunEventStreamSendsHeartbeat(t *testing.T) {
	env := newStreamEnv(t)
	//nolint:bodyclose // the stream body is owned by env.open's cleanup.
	_, reader := env.open("")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(strings.TrimSpace(line), ": heartbeat") {
			return
		}
	}
	t.Fatal("no heartbeat comment was sent")
}

func TestRunEventStreamUnknownRunIs404(t *testing.T) {
	env := newStreamEnv(t)
	response, err := http.Get(env.server.URL + "/api/runs/01999999-0000-7000-8000-000000000000/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("content type = %q", contentType)
	}
}

func TestHubDropsOldestForASlowSubscriber(t *testing.T) {
	hub := NewHub()
	hub.buffer = 4
	subscription, cancel := hub.Subscribe(1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			hub.Publish(1, domain.RunEvent{Type: domain.EventActivity, Message: fmt.Sprintf("event-%d", i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a slow subscriber")
	}

	var received []domain.RunEvent
	for {
		select {
		case event := <-subscription.C:
			received = append(received, event)
			continue
		default:
		}
		break
	}
	if len(received) != 4 {
		t.Fatalf("received %d events, want the bounded buffer of 4", len(received))
	}
	if received[len(received)-1].Message != "event-199" {
		t.Fatalf("the newest event must survive; last = %q", received[len(received)-1].Message)
	}
}

func TestSsePayloadMergesStructuredEventData(t *testing.T) {
	event := domain.RunEvent{
		Seq:      7,
		At:       time.Date(2026, 10, 26, 9, 41, 7, 0, time.UTC),
		Type:     domain.EventStage,
		Severity: domain.SeverityInfo,
		Message:  "stage checks.running",
		Data:     `{"stage":"checks.running"}`,
	}
	payload, err := ssePayload(event)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded["stage"] != "checks.running" {
		t.Fatalf("payload stage = %v", decoded["stage"])
	}
	if decoded["message"] != "stage checks.running" {
		t.Fatalf("payload message = %v", decoded["message"])
	}
}
