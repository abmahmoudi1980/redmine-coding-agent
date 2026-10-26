package redmine_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
)

const notesAPIKey = "fixture-redmine-key" // #nosec G101 -- fake key for an httptest server

type notesServer struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	notes   []string
	journal []string
	fail    bool
	headers []string
}

func newNotesServer(t *testing.T) *notesServer {
	t.Helper()
	fake := &notesServer{t: t}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *notesServer) URL() string { return f.server.URL }

func (f *notesServer) Notes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notes...)
}

func (f *notesServer) SetJournal(notes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.journal = notes
}

func (f *notesServer) SetFailure(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *notesServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Get("X-Redmine-API-Key"))
	f.mu.Unlock()

	if r.Header.Get("X-Redmine-API-Key") != notesAPIKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		journals := make([]map[string]any, 0, len(f.journal))
		for index, notes := range f.journal {
			journals = append(journals, map[string]any{
				"id":         index + 1,
				"user":       map[string]any{"name": "Operator"},
				"created_on": "2026-10-20T09:00:00Z",
				"notes":      notes,
			})
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"issue": map[string]any{
			"id":         4821,
			"subject":    "Invoice totals round incorrectly",
			"project":    map[string]any{"id": 7, "identifier": "billing", "name": "Billing"},
			"journals":   journals,
			"created_on": "2026-09-30T08:00:00Z",
			"updated_on": "2026-10-01T09:00:00Z",
		}})
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Issue struct {
				Notes string `json:"notes"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		fail := f.fail
		if !fail {
			f.notes = append(f.notes, payload.Issue.Notes)
		}
		f.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"errors":["note rejected"]}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func newNotesClient(t *testing.T, baseURL string) *redmine.Client {
	t.Helper()
	client, err := redmine.New(redmine.Options{
		BaseURL: baseURL,
		APIKey:  config.NewSecretRef("env:RCA_TEST_REDMINE_KEY"),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func TestPostNoteSendsTheJournalNoteWithTheCredential(t *testing.T) {
	t.Setenv("RCA_TEST_REDMINE_KEY", notesAPIKey)
	fake := newNotesServer(t)
	client := newNotesClient(t, fake.URL())

	body := "## RedmineCodingAgent report\n\n<!-- redmine-agent:report:abc -->\n"
	if err := client.PostNote(context.Background(), 4821, body); err != nil {
		t.Fatalf("PostNote: %v", err)
	}
	notes := fake.Notes()
	if len(notes) != 1 || notes[0] != body {
		t.Fatalf("notes = %+v", notes)
	}
}

func TestPostNoteFailsWithoutUndoingAnything(t *testing.T) {
	t.Setenv("RCA_TEST_REDMINE_KEY", notesAPIKey)
	fake := newNotesServer(t)
	fake.SetFailure(true)
	client := newNotesClient(t, fake.URL())

	if err := client.PostNote(context.Background(), 4821, "report body"); err == nil {
		t.Fatal("expected an error when Redmine rejects the note")
	}
	if len(fake.Notes()) != 0 {
		t.Fatal("the rejected note must not be recorded")
	}
}

func TestNoteContainsFindsTheDedupeMarker(t *testing.T) {
	t.Setenv("RCA_TEST_REDMINE_KEY", notesAPIKey)
	fake := newNotesServer(t)
	client := newNotesClient(t, fake.URL())
	marker := "<!-- redmine-agent:report:abc123 -->"

	fake.SetJournal("an unrelated comment")
	found, err := client.NoteContains(context.Background(), 4821, marker)
	if err != nil {
		t.Fatalf("NoteContains: %v", err)
	}
	if found {
		t.Fatal("the marker is not on the issue yet")
	}

	fake.SetJournal("an unrelated comment", "the report\n"+marker)
	found, err = client.NoteContains(context.Background(), 4821, marker)
	if err != nil {
		t.Fatalf("NoteContains: %v", err)
	}
	if !found {
		t.Fatal("the marker must be found, so a repeated report is skipped")
	}
}

func TestPostNoteRefusesAnEmptyBody(t *testing.T) {
	t.Setenv("RCA_TEST_REDMINE_KEY", notesAPIKey)
	client := newNotesClient(t, newNotesServer(t).URL())
	if err := client.PostNote(context.Background(), 4821, "   "); err == nil {
		t.Fatal("an empty note must be refused before any request")
	}
}
