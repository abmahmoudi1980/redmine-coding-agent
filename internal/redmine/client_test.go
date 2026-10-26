package redmine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

const testAPIKey = "redmine-secret-key-42" // #nosec G101 -- fake credential for an httptest server

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *logging.Redactor, *bytes.Buffer) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	redactor := logging.NewRedactor()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client, err := New(Options{
		BaseURL:    server.URL,
		APIKey:     config.NewSecretRef("env:TEST_REDMINE_KEY"),
		HTTPClient: server.Client(),
		Redactor:   redactor,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client, redactor, logs
}

func issuePayload() map[string]any {
	return map[string]any{
		"issue": map[string]any{
			"id":          4821,
			"subject":     "Invoice totals round incorrectly",
			"description": "Totals are rounded down.\n\n## Acceptance Criteria\n- totals round half up\n",
			"project":     map[string]any{"id": 7, "identifier": "billing", "name": "Billing"},
			"status":      map[string]any{"id": 1, "name": "New"},
			"tracker":     map[string]any{"id": 2, "name": "Feature"},
			"priority":    map[string]any{"id": 3, "name": "Normal"},
			"assigned_to": map[string]any{"id": 4, "name": "Operator"},
			"parent":      map[string]any{"id": 4800},
			"created_on":  "2026-09-30T08:00:00Z",
			"updated_on":  "2026-10-01T09:00:00Z",
			"custom_fields": []map[string]any{
				{"id": 3, "name": "Team", "value": "Payments"},
				{"id": 4, "name": "Estimate", "value": 5},
			},
			"journals": []map[string]any{
				{"id": 11, "user": map[string]any{"id": 4, "name": "Reporter"}, "created_on": "2026-10-01T09:00:00Z", "notes": "Seen on invoice 1001."},
				{"id": 12, "user": map[string]any{"id": 4, "name": "Reporter"}, "created_on": "2026-10-01T10:00:00Z", "notes": ""},
			},
			"relations": []map[string]any{
				{"id": 1, "issue_id": 4821, "issue_to_id": 4900, "relation_type": "relates"},
			},
			"children": []map[string]any{{"id": 4901}},
			"attachments": []map[string]any{
				{"id": 55, "filename": "screen.txt", "filesize": 12, "content_url": "/attachments/download/55/screen.txt"},
			},
		},
	}
}

func TestGetIssueIncludesJournalsRelationsAndAttachments(t *testing.T) {
	t.Setenv("TEST_REDMINE_KEY", testAPIKey)

	headerSeen := false
	client, redactor, logs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/issues/4821.json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("include"); got != "journals,relations,children,attachments" {
			t.Errorf("include = %q", got)
		}
		if r.Header.Get("X-Redmine-API-Key") != testAPIKey {
			t.Errorf("missing X-Redmine-API-Key header")
		}
		headerSeen = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issuePayload())
	})

	issue, err := client.GetIssue(context.Background(), 4821)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if !headerSeen {
		t.Fatal("request never reached the server")
	}
	if issue.ID != 4821 || issue.Subject != "Invoice totals round incorrectly" {
		t.Fatalf("issue = %+v", issue)
	}
	if issue.Project.Identifier != "billing" || issue.Project.Name != "Billing" || issue.Project.ID.String() != "7" {
		t.Fatalf("project = %+v", issue.Project)
	}
	if issue.Status.Name != "New" || issue.Tracker.Name != "Feature" || issue.Priority.Name != "Normal" {
		t.Fatalf("named fields = %+v/%+v/%+v", issue.Status, issue.Tracker, issue.Priority)
	}
	if issue.AssignedTo == nil || issue.AssignedTo.Name != "Operator" {
		t.Fatalf("assigned_to = %+v", issue.AssignedTo)
	}
	if issue.Parent == nil || issue.Parent.ID != 4800 {
		t.Fatalf("parent = %+v", issue.Parent)
	}
	if len(issue.Journals) != 2 {
		t.Fatalf("journals = %d", len(issue.Journals))
	}
	if issue.Journals[0].Notes != "Seen on invoice 1001." || issue.Journals[0].User.Name != "Reporter" {
		t.Fatalf("journal = %+v", issue.Journals[0])
	}
	if len(issue.Relations) != 1 || issue.Relations[0].IssueToID != 4900 {
		t.Fatalf("relations = %+v", issue.Relations)
	}
	if len(issue.Children) != 1 || issue.Children[0].ID != 4901 {
		t.Fatalf("children = %+v", issue.Children)
	}
	if len(issue.Attachments) != 1 || issue.Attachments[0].Filename != "screen.txt" || issue.Attachments[0].Filesize != 12 {
		t.Fatalf("attachments = %+v", issue.Attachments)
	}
	if len(issue.CustomFields) != 2 || issue.CustomFields[0].Value.String() != "Payments" || issue.CustomFields[1].Value.String() != "5" {
		t.Fatalf("custom fields = %+v", issue.CustomFields)
	}
	if issue.RawJSON == "" {
		t.Fatal("raw issue JSON was not retained")
	}
	if redactor.Redact("prefix "+testAPIKey+" suffix") != "prefix *** suffix" {
		t.Fatal("the resolved credential was not registered with the redactor")
	}
	if strings.Contains(logs.String(), testAPIKey) {
		t.Fatalf("credential leaked into logs: %s", logs.String())
	}
}

func TestGetIssueStatusMappingToFailureCodes(t *testing.T) {
	t.Setenv("TEST_REDMINE_KEY", testAPIKey)

	cases := []struct {
		name   string
		status int
		want   domain.FailureCode
	}{
		{name: "not found", status: http.StatusNotFound, want: domain.FailIssueNotFound},
		{name: "forbidden", status: http.StatusForbidden, want: domain.FailIssueInaccessible},
		{name: "unauthorized", status: http.StatusUnauthorized, want: domain.FailIssueInaccessible},
		{name: "server error", status: http.StatusInternalServerError, want: domain.FailConnectivityRedmine},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(`{"error":"nope"}`))
			})

			_, err := client.GetIssue(context.Background(), 4821)
			if err == nil {
				t.Fatal("expected an error")
			}
			if testCase.status == http.StatusNotFound && !errors.Is(err, ErrIssueNotFound) {
				t.Fatalf("error %v is not ErrIssueNotFound", err)
			}
			if testCase.status == http.StatusForbidden && !errors.Is(err, ErrIssueInaccessible) {
				t.Fatalf("error %v is not ErrIssueInaccessible", err)
			}
			if got := FailureFor(err, 4821).Code; got != testCase.want {
				t.Fatalf("failure code = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestGetIssueRejectsInvalidIdentifierWithoutRequest(t *testing.T) {
	t.Setenv("TEST_REDMINE_KEY", testAPIKey)

	requests := 0
	client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
	})

	if _, err := client.GetIssue(context.Background(), 0); err == nil {
		t.Fatal("expected an error for issue 0")
	}
	if _, err := client.GetIssue(context.Background(), -3); err == nil {
		t.Fatal("expected an error for a negative issue number")
	}
	if requests != 0 {
		t.Fatalf("client made %d requests for invalid identifiers", requests)
	}
}

func TestDownloadAttachmentsRecordsAvailability(t *testing.T) {
	t.Setenv("TEST_REDMINE_KEY", testAPIKey)

	client, redactor, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Redmine-API-Key") != testAPIKey {
			t.Errorf("attachment request has no API key")
		}
		switch r.URL.Path {
		case "/attachments/download/55/screen.txt":
			_, _ = w.Write([]byte("screenshot\n"))
		default:
			http.NotFound(w, r)
		}
	})

	dir := t.TempDir()
	results, err := client.DownloadAttachments(context.Background(), []Attachment{
		{ID: 55, Filename: "../../screen.txt", Filesize: 11, ContentURL: "/attachments/download/55/screen.txt"},
		{ID: 56, Filename: "missing.txt", Filesize: 4, ContentURL: "/attachments/download/56/missing.txt"},
		{ID: 57, Filename: "huge.bin", Filesize: MaxAttachmentBytes + 1, ContentURL: "/attachments/download/57/huge.bin"},
		{ID: 58, Filename: "no-url.txt"},
	}, dir)
	if err != nil {
		t.Fatalf("download attachments: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d", len(results))
	}

	if !results[0].Available {
		t.Fatalf("first attachment unavailable: %s", results[0].Reason)
	}
	if filepath.Dir(results[0].Path) != dir {
		t.Fatalf("attachment escaped the destination: %s", results[0].Path)
	}
	if filepath.Base(results[0].Path) != "screen.txt" {
		t.Fatalf("unsafe filename was not sanitised: %s", results[0].Path)
	}
	content, err := os.ReadFile(results[0].Path)
	if err != nil || string(content) != "screenshot\n" {
		t.Fatalf("content = %q, err = %v", content, err)
	}

	for index := 1; index < 4; index++ {
		if results[index].Available {
			t.Fatalf("attachment %d should be unavailable", results[index].Attachment.ID)
		}
		if results[index].Reason == "" {
			t.Fatalf("attachment %d has no reason", results[index].Attachment.ID)
		}
	}
	if redactor.Redact(testAPIKey) != "***" {
		t.Fatal("the credential was not registered during the attachment download")
	}
}

func TestProbeAndGetProjectUseTheCredentialWithoutMutating(t *testing.T) {
	t.Setenv("TEST_REDMINE_KEY", testAPIKey)

	methods := make([]string, 0, 2)
	client, redactor, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Redmine-API-Key") != testAPIKey {
			t.Errorf("%s has no API key", r.URL.Path)
		}
		methods = append(methods, r.Method)
		switch r.URL.Path {
		case "/users/current.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{"login": "worker", "firstname": "Worker", "lastname": "Bot"},
			})
		case "/projects/billing.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": 7, "identifier": "billing", "name": "Billing"},
			})
		default:
			http.NotFound(w, r)
		}
	})

	login, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if login != "worker" {
		t.Fatalf("login = %q", login)
	}

	project, err := client.GetProject(context.Background(), "billing")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.ID.String() != "7" || project.Name != "Billing" {
		t.Fatalf("project = %+v", project)
	}
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("%s was used; probes must be read-only", method)
		}
	}
	if redactor.Redact(testAPIKey) != "***" {
		t.Fatal("the credential was not registered with the redactor")
	}
}

func TestProbeReportsARejectedCredential(t *testing.T) {
	t.Setenv("TEST_REDMINE_KEY", testAPIKey)
	client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := client.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error = %v, want a rejected-credential message", err)
	}
}
