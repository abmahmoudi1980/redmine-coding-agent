package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/api"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
)

type previewEnvironment struct {
	harness *Harness
	server  *httptest.Server
	repoIDs map[string]int64
}

func newPreviewEnvironment(t *testing.T, repositories []config.Repository) *previewEnvironment {
	t.Helper()
	h := New(t)

	cfg := *h.Config
	cfg.Repositories = repositories
	ids, err := h.Store.SeedRepositories(context.Background(), &cfg)
	if err != nil {
		t.Fatalf("seed repositories: %v", err)
	}

	client, err := redmine.New(redmine.Options{
		BaseURL:  h.Redmine.URL(),
		APIKey:   h.Config.Redmine.APIKey,
		Redactor: h.Redactor,
	})
	if err != nil {
		t.Fatalf("new redmine client: %v", err)
	}

	logger := logging.New(logging.Options{Redactor: h.Redactor, Stderr: io.Discard})
	server := httptest.NewServer(api.NewServer(api.Deps{
		Config:   h.Config,
		Store:    h.Store,
		Logger:   logger,
		Redactor: h.Redactor,
		Redmine:  client,
	}).Handler())
	t.Cleanup(server.Close)

	return &previewEnvironment{harness: h, server: server, repoIDs: ids}
}

func (e *previewEnvironment) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	response, err := http.Get(e.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return response.StatusCode, body
}

func (e *previewEnvironment) countRuns(t *testing.T) int {
	t.Helper()
	var count int
	if err := e.harness.Store.Reader().
		QueryRowContext(context.Background(), `SELECT COUNT(*) FROM runs`).
		Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return count
}

func TestPreviewCreatesNoRunWorkspaceOrBranch(t *testing.T) {
	h := New(t)
	env := newPreviewEnvironment(t, h.Config.Repositories)

	status, body := env.get(t, "/api/issues/4821/preview")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	var preview struct {
		Issue struct {
			IssueID            int    `json:"issueId"`
			Subject            string `json:"subject"`
			Description        string `json:"description"`
			AcceptanceCriteria string `json:"acceptanceCriteria"`
			CriteriaSource     string `json:"criteriaSource"`
			ContentHash        string `json:"contentHash"`
			Project            struct {
				Identifier string `json:"identifier"`
				Name       string `json:"name"`
			} `json:"project"`
			Status   string `json:"status"`
			Priority string `json:"priority"`
			Comments []struct {
				Notes string `json:"notes"`
			} `json:"comments"`
			Attachments []struct {
				Filename  string `json:"filename"`
				Available bool   `json:"available"`
			} `json:"attachments"`
		} `json:"issue"`
		Repository struct {
			RepositoryID int64  `json:"repositoryId"`
			Path         string `json:"path"`
			BaseBranch   string `json:"baseBranch"`
		} `json:"repository"`
		BaseBranch string   `json:"baseBranch"`
		Warnings   []string `json:"warnings"`
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatalf("decode preview: %v (%s)", err, body)
	}

	if preview.Issue.IssueID != 4821 || preview.Issue.Subject != "Invoice totals round incorrectly" {
		t.Fatalf("issue = %+v", preview.Issue)
	}
	if preview.Issue.Project.Identifier != "billing" || preview.Issue.Project.Name != "Billing" {
		t.Fatalf("project = %+v", preview.Issue.Project)
	}
	if preview.Issue.Status != "New" || preview.Issue.Priority != "Normal" {
		t.Fatalf("status/priority = %q/%q", preview.Issue.Status, preview.Issue.Priority)
	}
	if preview.Issue.CriteriaSource != "heading" || !strings.Contains(preview.Issue.AcceptanceCriteria, "totals round half up") {
		t.Fatalf("acceptance criteria = %q (source %q)", preview.Issue.AcceptanceCriteria, preview.Issue.CriteriaSource)
	}
	if len(preview.Issue.Comments) != 1 || preview.Issue.Comments[0].Notes != "Seen on invoice 1001." {
		t.Fatalf("comments = %+v", preview.Issue.Comments)
	}
	if len(preview.Issue.Attachments) != 1 || !preview.Issue.Attachments[0].Available {
		t.Fatalf("attachments = %+v", preview.Issue.Attachments)
	}
	if preview.Repository.Path != "group/billing-api" || preview.Repository.BaseBranch != "main" || preview.BaseBranch != "main" {
		t.Fatalf("repository = %+v, base branch %q", preview.Repository, preview.BaseBranch)
	}
	if preview.Issue.ContentHash == "" {
		t.Fatal("content hash is empty")
	}

	if count := env.countRuns(t); count != 0 {
		t.Fatalf("preview created %d run rows", count)
	}
	entries, err := os.ReadDir(h.WorkspaceRoot)
	if err != nil {
		t.Fatalf("read workspace root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("preview created workspace entries: %+v", entries)
	}
	branches := gitOutput(t, h.Origin.Path, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	for _, branch := range strings.Split(strings.TrimSpace(branches), "\n") {
		if branch != "" && branch != "main" {
			t.Fatalf("preview created branch %q", branch)
		}
	}

	_, secondBody := env.get(t, "/api/issues/4821/preview")
	var secondPreview struct {
		Issue struct {
			ContentHash string `json:"contentHash"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(secondBody, &secondPreview); err != nil {
		t.Fatalf("decode second preview: %v", err)
	}
	if secondPreview.Issue.ContentHash != preview.Issue.ContentHash {
		t.Fatalf("content hash changed between previews: %q -> %q", preview.Issue.ContentHash, secondPreview.Issue.ContentHash)
	}
	if count := env.countRuns(t); count != 0 {
		t.Fatalf("second preview created %d run rows", count)
	}
}

func TestPreviewRejectsInvalidIssueNumberBeforeCallingRedmine(t *testing.T) {
	h := New(t)
	env := newPreviewEnvironment(t, h.Config.Repositories)
	before := h.Redmine.RequestCount()

	for _, raw := range []string{"%20", "abc", "0", "-3", "1.5", "+3", "4821abc"} {
		path := "/api/issues/" + raw + "/preview"
		status, body := env.get(t, path)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (%s)", path, status, body)
		}
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(body, &problem); err != nil {
			t.Fatalf("%s: decode problem: %v", path, err)
		}
		if problem.Code != "invalid_issue_id" {
			t.Fatalf("%s: code = %q", path, problem.Code)
		}
		if problem.Detail == "" {
			t.Fatalf("%s: detail is empty", path)
		}
	}
	if after := h.Redmine.RequestCount(); after != before {
		t.Fatalf("invalid identifiers reached Redmine (%d -> %d requests)", before, after)
	}
}

func TestPreviewAmbiguousRepositoryListsCandidatesAndHonoursChoice(t *testing.T) {
	h := New(t)
	repositories := append([]config.Repository{}, h.Config.Repositories...)
	repositories[0].IsDefault = false
	repositories = append(repositories, config.Repository{
		RedmineProject: "billing",
		RepositoryPath: "group/billing-ui",
		DisplayName:    "Billing UI",
		BaseBranch:     "develop",
	})
	env := newPreviewEnvironment(t, repositories)

	status, body := env.get(t, "/api/issues/4821/preview")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
	var problem struct {
		Code       string `json:"code"`
		Candidates []struct {
			RepositoryID int64  `json:"repositoryId"`
			Path         string `json:"path"`
			BaseBranch   string `json:"baseBranch"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "repository_ambiguous" {
		t.Fatalf("code = %q", problem.Code)
	}
	if len(problem.Candidates) != 2 {
		t.Fatalf("candidates = %+v", problem.Candidates)
	}

	uiID := env.repoIDs["group/billing-ui"]
	status, body = env.get(t, "/api/issues/4821/preview?repositoryId="+itoa(uiID))
	if status != http.StatusOK {
		t.Fatalf("explicit choice status = %d: %s", status, body)
	}
	var preview struct {
		Repository struct {
			Path       string `json:"path"`
			BaseBranch string `json:"baseBranch"`
		} `json:"repository"`
		BaseBranch string `json:"baseBranch"`
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Repository.Path != "group/billing-ui" || preview.BaseBranch != "develop" {
		t.Fatalf("preview = %+v", preview)
	}
}

func TestListRepositoriesReturnsMappingsAndChecks(t *testing.T) {
	h := New(t)
	env := newPreviewEnvironment(t, h.Config.Repositories)

	status, body := env.get(t, "/api/repositories?project=billing")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var payload struct {
		Items []struct {
			RepositoryID   int64  `json:"repositoryId"`
			Path           string `json:"path"`
			RedmineProject string `json:"redmineProject"`
			BaseBranch     string `json:"baseBranch"`
			Checks         []struct {
				Name           string `json:"name"`
				Command        string `json:"command"`
				Required       bool   `json:"required"`
				TimeoutSeconds int    `json:"timeoutSeconds"`
			} `json:"checks"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode repositories: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items = %+v", payload.Items)
	}
	item := payload.Items[0]
	if item.Path != "group/billing-api" || item.RedmineProject != "billing" || item.BaseBranch != "main" {
		t.Fatalf("repository = %+v", item)
	}
	if len(item.Checks) != 1 || item.Checks[0].Name != "unit-tests" || !item.Checks[0].Required {
		t.Fatalf("checks = %+v", item.Checks)
	}
	if item.Checks[0].TimeoutSeconds != 120 {
		t.Fatalf("timeout = %d", item.Checks[0].TimeoutSeconds)
	}
}

func TestPreviewReportsIssueNotFound(t *testing.T) {
	h := New(t)
	h.Redmine.RemoveIssue(9999)
	env := newPreviewEnvironment(t, h.Config.Repositories)

	status, body := env.get(t, "/api/issues/9999/preview")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "issue_not_found" {
		t.Fatalf("code = %q", problem.Code)
	}
	if !strings.Contains(problem.Detail, "9999") {
		t.Fatalf("detail does not name the issue: %q", problem.Detail)
	}
	if count := env.countRuns(t); count != 0 {
		t.Fatalf("failed preview created %d run rows", count)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
