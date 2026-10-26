// Package integration provides deterministic fakes for Redmine, GitLab, git,
// and the coding agent so the pipeline can be exercised without a network.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

const (
	fixtureRedmineKey  = "fixture-redmine-key"
	fixtureGitLabToken = "fixture-gitlab-token" // #nosec G101 -- fake token for an httptest server
	billingProjectID   = 101
)

// Harness is one isolated end-to-end environment.
type Harness struct {
	T          testing.TB
	Root       string
	Redmine    *RedmineFake
	GitLab     *GitLabFake
	Origin     *Origin
	Store      *store.Store
	Config     *config.Config
	ConfigPath string
	Redactor   *logging.Redactor

	DatabasePath  string
	WorkspaceRoot string
	LogDir        string
	ReposDir      string

	agentMu   sync.Mutex
	agentPath string
	agentErr  error
}

// New builds fakes, a bare origin, a real SQLite store, and a valid config.
func New(t testing.TB) *Harness {
	t.Helper()

	root := t.TempDir()
	t.Setenv("RCA_TEST_REDMINE_KEY", fixtureRedmineKey)
	t.Setenv("RCA_TEST_GITLAB_TOKEN", fixtureGitLabToken)

	h := &Harness{
		T:             t,
		Root:          root,
		Redmine:       NewRedmineFake(t, fixtureRedmineKey),
		GitLab:        NewGitLabFake(t, fixtureGitLabToken),
		DatabasePath:  filepath.Join(root, "data", "agent.db"),
		WorkspaceRoot: filepath.Join(root, "data", "workspaces"),
		LogDir:        filepath.Join(root, "data", "logs"),
		ReposDir:      filepath.Join(root, "data", "repos"),
		Redactor:      logging.NewRedactor(),
	}
	h.Redactor.Register(fixtureRedmineKey, fixtureGitLabToken)

	for _, dir := range []string{h.WorkspaceRoot, h.LogDir, h.ReposDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	h.Origin = NewOrigin(t, root)
	h.GitLab.AddProject("group/billing-api", billingProjectID, h.Origin.Path)
	h.Redmine.AddIssue(defaultIssue())

	h.Config = h.defaultConfig()
	if err := h.Config.Validate(); err != nil {
		t.Fatalf("harness config must be valid: %v", err)
	}

	st, err := store.Open(context.Background(), h.DatabasePath, store.WithRedactor(h.Redactor))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	h.Store = st
	t.Cleanup(func() { _ = st.Close() })

	configPath := filepath.Join(root, "config.yaml")
	encoded, err := yaml.Marshal(h.Config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h.ConfigPath = configPath

	t.Cleanup(func() {
		h.Redmine.Close()
		h.GitLab.Close()
	})
	return h
}

func (h *Harness) defaultConfig() *config.Config {
	return &config.Config{
		SchemaVersion: 1,
		HTTP:          config.HTTP{Listen: "127.0.0.1:8080"},
		Data: config.Data{
			Database:      h.DatabasePath,
			WorkspaceRoot: h.WorkspaceRoot,
			LogDir:        h.LogDir,
			RetentionDays: 7,
		},
		Redmine: config.Redmine{
			BaseURL:         h.Redmine.URL(),
			APIKey:          config.NewSecretRef("env:RCA_TEST_REDMINE_KEY"),
			CommentOnFinish: true,
			UpdateStatus:    false,
		},
		GitLab: config.GitLab{
			BaseURL:             h.GitLab.URL(),
			Token:               config.NewSecretRef("env:RCA_TEST_GITLAB_TOKEN"),
			PushEnabled:         true,
			MergeRequestEnabled: true,
		},
		Repositories: []config.Repository{
			{
				RedmineProject: "billing",
				RepositoryPath: "group/billing-api",
				DisplayName:    "Billing API",
				BaseBranch:     "main",
				IsDefault:      true,
				Checks: []config.Check{
					{
						Name:           "unit-tests",
						CommandLinux:   "go version",
						CommandWindows: "go version",
						TimeoutSeconds: 120,
					},
				},
			},
		},
		Agent: config.Agent{
			Adapter:        "opencode",
			Command:        "git",
			Model:          "fake/model",
			Profile:        "restricted",
			TimeoutMinutes: 1,
			MaxRepairs:     1,
		},
		Concurrency: config.Concurrency{MaxActiveRuns: 1},
	}
}

// FakeAgent compiles the controllable fake agent and points the config at it.
// The mode is selected by embedding FAKE_AGENT_MODE=<mode> in the issue text.
func (h *Harness) FakeAgent() string {
	h.T.Helper()
	h.agentMu.Lock()
	defer h.agentMu.Unlock()
	if h.agentPath != "" || h.agentErr != nil {
		return h.agentPath
	}
	name := "fakeagent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(h.Root, "bin", name)
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		h.agentErr = err
		h.T.Fatalf("create fake agent dir: %v", err)
	}
	// #nosec G204 -- the package path is fixed and the output path is a t.TempDir.
	command := exec.Command("go", "build", "-o", out, ".")
	command.Dir = filepath.Join(sourceDir(), "fakeagent")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		h.agentErr = err
		h.T.Fatalf("build fake agent: %v\n%s", err, output)
	}
	h.agentPath = out
	h.Config.Agent.Command = out
	return out
}

func sourceDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// AddIssue registers a Redmine fixture.
func (h *Harness) AddIssue(issue Issue) { h.Redmine.AddIssue(issue) }

// SetIssue replaces a Redmine fixture.
func (h *Harness) SetIssue(issue Issue) { h.Redmine.SetIssue(issue) }

// Issue is the fixture shape used to build Redmine payloads.
type Issue struct {
	ID                int
	Subject           string
	Description       string
	ProjectIdentifier string
	ProjectName       string
	ProjectID         int
	Status            string
	Tracker           string
	Priority          string
	Assignee          string
	ParentID          int
	Journals          []Journal
	Attachments       []Attachment
	CustomFields      []CustomField
	Relations         []Relation
}

type Journal struct {
	ID        int
	Author    string
	CreatedOn time.Time
	Notes     string
}

type Attachment struct {
	ID       int
	Filename string
	Size     int
	Content  []byte
}

type CustomField struct {
	ID    int
	Name  string
	Value string
}

type Relation struct {
	IssueToID int
	Type      string
}

func defaultIssue() Issue {
	return Issue{
		ID:                4821,
		Subject:           "Invoice totals round incorrectly",
		Description:       "Totals are rounded down.\n\n## Acceptance Criteria\n- totals round half up\n- existing invoices still load\n",
		ProjectIdentifier: "billing",
		ProjectName:       "Billing",
		ProjectID:         7,
		Status:            "New",
		Tracker:           "Feature",
		Priority:          "Normal",
		Assignee:          "Operator",
		Journals: []Journal{
			{
				ID:        1,
				Author:    "Reporter",
				CreatedOn: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
				Notes:     "Seen on invoice 1001.",
			},
		},
		Attachments: []Attachment{
			{ID: 55, Filename: "screen.txt", Size: 12, Content: []byte("screenshot\n")},
		},
		CustomFields: []CustomField{{ID: 3, Name: "Team", Value: "Payments"}},
	}
}

// RedmineFake is an httptest Redmine with issues, journals, attachments, and notes.
type RedmineFake struct {
	server *httptest.Server
	apiKey string

	mu        sync.Mutex
	issues    map[int]Issue
	notes     []PostedNote
	failNotes bool
	requests  int
}

// PostedNote is a journal note the worker posted.
type PostedNote struct {
	IssueID  int
	Notes    string
	PostedAt time.Time
}

// NewRedmineFake starts the server.
func NewRedmineFake(t testing.TB, apiKey string) *RedmineFake {
	fake := &RedmineFake{apiKey: apiKey, issues: make(map[int]Issue)}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

func (f *RedmineFake) URL() string { return f.server.URL }

func (f *RedmineFake) Close() {
	if f.server != nil {
		f.server.Close()
	}
}

func (f *RedmineFake) AddIssue(issue Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if issue.ProjectID == 0 {
		issue.ProjectID = 7
	}
	if issue.ProjectIdentifier == "" {
		issue.ProjectIdentifier = "billing"
	}
	if issue.ProjectName == "" {
		issue.ProjectName = "Billing"
	}
	f.issues[issue.ID] = issue
}

func (f *RedmineFake) SetIssue(issue Issue) { f.AddIssue(issue) }

func (f *RedmineFake) RemoveIssue(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.issues, id)
}

func (f *RedmineFake) SetNoteFailure(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNotes = fail
}

func (f *RedmineFake) Notes() []PostedNote {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PostedNote, len(f.notes))
	copy(out, f.notes)
	return out
}

func (f *RedmineFake) RequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *RedmineFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()

	if r.Header.Get("X-Redmine-API-Key") != f.apiKey {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
		return
	}

	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/issues/") && strings.HasSuffix(path, ".json"):
		f.serveIssue(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/issues/"), ".json"))
	case strings.HasPrefix(path, "/projects/") && strings.HasSuffix(path, ".json"):
		f.serveProject(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/projects/"), ".json"))
	case strings.HasPrefix(path, "/attachments/download/"):
		f.serveAttachment(w, r, strings.TrimPrefix(path, "/attachments/download/"))
	default:
		http.NotFound(w, r)
	}
}

// serveProject answers the authenticated project read the health check uses.
func (f *RedmineFake) serveProject(w http.ResponseWriter, r *http.Request, identifier string) {
	f.mu.Lock()
	var project Issue
	found := false
	for _, issue := range f.issues {
		if issue.ProjectIdentifier == identifier {
			project = issue
			found = true
			break
		}
	}
	f.mu.Unlock()
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": map[string]any{
			"id":         project.ProjectID,
			"identifier": project.ProjectIdentifier,
			"name":       project.ProjectName,
		},
	})
}

func (f *RedmineFake) serveIssue(w http.ResponseWriter, r *http.Request, idText string) {
	id, err := strconv.Atoi(idText)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	f.mu.Lock()
	issue, exists := f.issues[id]
	f.mu.Unlock()
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"issue not found"}`)
		return
	}

	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"issue": f.issueJSON(issue)})
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Issue struct {
			Notes string `json:"notes"`
		} `json:"issue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	fail := f.failNotes
	if !fail {
		f.notes = append(f.notes, PostedNote{IssueID: id, Notes: body.Issue.Notes, PostedAt: time.Now().UTC()})
	}
	f.mu.Unlock()
	if fail {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"note rejected"}`)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *RedmineFake) issueJSON(issue Issue) map[string]any {
	journals := make([]map[string]any, 0, len(issue.Journals))
	for _, journal := range issue.Journals {
		journals = append(journals, map[string]any{
			"id":         journal.ID,
			"user":       map[string]any{"name": journal.Author},
			"created_on": journal.CreatedOn.UTC().Format(time.RFC3339),
			"notes":      journal.Notes,
		})
	}
	attachments := make([]map[string]any, 0, len(issue.Attachments))
	for _, attachment := range issue.Attachments {
		attachments = append(attachments, map[string]any{
			"id":          attachment.ID,
			"filename":    attachment.Filename,
			"filesize":    attachment.Size,
			"content_url": fmt.Sprintf("/attachments/download/%d/%s", attachment.ID, url.PathEscape(attachment.Filename)),
		})
	}
	customFields := make([]map[string]any, 0, len(issue.CustomFields))
	for _, field := range issue.CustomFields {
		customFields = append(customFields, map[string]any{
			"id": field.ID, "name": field.Name, "value": field.Value,
		})
	}
	relations := make([]map[string]any, 0, len(issue.Relations))
	for _, relation := range issue.Relations {
		relations = append(relations, map[string]any{
			"issue_id": issue.ID, "issue_to_id": relation.IssueToID, "relation_type": relation.Type,
		})
	}

	payload := map[string]any{
		"id":          issue.ID,
		"subject":     issue.Subject,
		"description": issue.Description,
		"project": map[string]any{
			"id": issue.ProjectID, "identifier": issue.ProjectIdentifier, "name": issue.ProjectName,
		},
		"status":        map[string]any{"id": 1, "name": issue.Status},
		"tracker":       map[string]any{"id": 1, "name": issue.Tracker},
		"priority":      map[string]any{"id": 2, "name": issue.Priority},
		"custom_fields": customFields,
		"journals":      journals,
		"attachments":   attachments,
		"relations":     relations,
		"created_on":    time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"updated_on":    time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	if issue.Assignee != "" {
		payload["assigned_to"] = map[string]any{"id": 2, "name": issue.Assignee}
	}
	if issue.ParentID != 0 {
		payload["parent"] = map[string]any{"id": issue.ParentID}
	}
	return payload
}

func (f *RedmineFake) serveAttachment(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	var content []byte
	var filename string
	for _, issue := range f.issues {
		for _, attachment := range issue.Attachments {
			if attachment.ID == id {
				content = attachment.Content
				filename = attachment.Filename
			}
		}
	}
	f.mu.Unlock()
	if content == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// GitLabFake is an httptest GitLab v4 endpoint for projects and merge requests.
type GitLabFake struct {
	server *httptest.Server
	token  string

	mu         sync.Mutex
	byID       map[int]Project
	byPath     map[string]Project
	mergeReqs  []MergeRequest
	nextIID    int
	failMR     bool
	mergeCalls int
}

type Project struct {
	ID            int
	Path          string
	HTTPURL       string
	DefaultBranch string
}

type MergeRequest struct {
	IID          int
	ProjectID    int
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
	State        string
	WebURL       string
}

// NewGitLabFake starts the server.
func NewGitLabFake(t testing.TB, token string) *GitLabFake {
	fake := &GitLabFake{
		token:   token,
		byID:    make(map[int]Project),
		byPath:  make(map[string]Project),
		nextIID: 1,
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

func (f *GitLabFake) URL() string { return f.server.URL }

func (f *GitLabFake) Close() {
	if f.server != nil {
		f.server.Close()
	}
}

func (f *GitLabFake) AddProject(path string, id int, httpURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	project := Project{ID: id, Path: path, HTTPURL: httpURL, DefaultBranch: "main"}
	f.byID[id] = project
	f.byPath[path] = project
}

func (f *GitLabFake) SetMergeRequestFailure(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failMR = fail
}

func (f *GitLabFake) MergeRequests() []MergeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]MergeRequest, len(f.mergeReqs))
	copy(out, f.mergeReqs)
	return out
}

// MergeCalls is always zero: the fake has no merge endpoint at all.
func (f *GitLabFake) MergeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mergeCalls
}

// PreregisterMergeRequest seeds an open merge request so idempotent reuse can
// be exercised (FR-043).
func (f *GitLabFake) PreregisterMergeRequest(sourceBranch, targetBranch, title, description string) MergeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	mergeRequest := MergeRequest{
		IID:          f.nextIID,
		ProjectID:    billingProjectID,
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
		Title:        title,
		Description:  description,
		State:        "opened",
		WebURL:       fmt.Sprintf("%s/group/billing-api/-/merge_requests/%d", f.server.URL, f.nextIID),
	}
	f.nextIID++
	f.mergeReqs = append(f.mergeReqs, mergeRequest)
	return mergeRequest
}

func (f *GitLabFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("PRIVATE-TOKEN") != f.token {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"401 Unauthorized"}`)
		return
	}

	escaped := r.URL.EscapedPath()
	rest := strings.TrimPrefix(escaped, "/api/v4/projects/")
	if rest == escaped {
		http.NotFound(w, r)
		return
	}
	refPart, tail, _ := strings.Cut(rest, "/")
	ref, err := url.PathUnescape(refPart)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	f.mu.Lock()
	project, exists := f.projectLocked(ref)
	f.mu.Unlock()
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Project Not Found"})
		return
	}
	tail = strings.Trim(tail, "/")

	switch {
	case tail == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, f.projectJSON(project))
	case tail == "merge_requests" && r.Method == http.MethodGet:
		f.listMergeRequests(w, r, project)
	case tail == "merge_requests" && r.Method == http.MethodPost:
		f.createMergeRequest(w, r, project)
	case strings.HasPrefix(tail, "merge_requests/"):
		f.serveMergeRequest(w, r, project, strings.TrimPrefix(tail, "merge_requests/"))
	case strings.HasPrefix(tail, "repository/branches"):
		f.listBranches(w, project, strings.TrimPrefix(tail, "repository/branches"))
	case strings.HasPrefix(tail, "merge"):
		f.mu.Lock()
		f.mergeCalls++
		f.mu.Unlock()
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "merging is not implemented"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
	}
}

func (f *GitLabFake) projectLocked(ref string) (Project, bool) {
	if id, err := strconv.Atoi(ref); err == nil {
		project, ok := f.byID[id]
		return project, ok
	}
	project, ok := f.byPath[ref]
	return project, ok
}

func (f *GitLabFake) listBranches(w http.ResponseWriter, project Project, tail string) {
	name := strings.Trim(tail, "/")
	f.mu.Lock()
	names := map[string]bool{"main": true}
	for _, mergeRequest := range f.mergeReqs {
		if mergeRequest.ProjectID == project.ID && mergeRequest.SourceBranch != "" {
			names[mergeRequest.SourceBranch] = true
		}
	}
	f.mu.Unlock()

	if name != "" {
		if !names[name] {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Branch Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name": name, "default": name == "main",
			"commit": map[string]any{"id": ""},
		})
		return
	}

	ordered := make([]string, 0, len(names))
	for branch := range names {
		if branch != "main" {
			ordered = append(ordered, branch)
		}
	}
	sort.Strings(ordered)
	ordered = append([]string{"main"}, ordered...)
	out := make([]map[string]any, 0, len(ordered))
	for _, branch := range ordered {
		out = append(out, map[string]any{
			"name": branch, "default": branch == "main",
			"commit": map[string]any{"id": ""},
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *GitLabFake) projectJSON(project Project) map[string]any {
	return map[string]any{
		"id":                  project.ID,
		"path_with_namespace": project.Path,
		"http_url_to_repo":    project.HTTPURL,
		"ssh_url_to_repo":     project.HTTPURL,
		"default_branch":      project.DefaultBranch,
		"web_url":             f.server.URL + "/" + project.Path,
	}
}

func (f *GitLabFake) listMergeRequests(w http.ResponseWriter, r *http.Request, project Project) {
	source := r.URL.Query().Get("source_branch")
	target := r.URL.Query().Get("target_branch")
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "opened"
	}

	f.mu.Lock()
	var out []map[string]any
	for _, mergeRequest := range f.mergeReqs {
		if mergeRequest.ProjectID != project.ID {
			continue
		}
		if source != "" && mergeRequest.SourceBranch != source {
			continue
		}
		if target != "" && mergeRequest.TargetBranch != target {
			continue
		}
		if state != "all" && mergeRequest.State != state {
			continue
		}
		out = append(out, f.mergeRequestJSONLocked(mergeRequest))
	}
	f.mu.Unlock()
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *GitLabFake) createMergeRequest(w http.ResponseWriter, r *http.Request, project Project) {
	f.mu.Lock()
	fail := f.failMR
	f.mu.Unlock()
	if fail {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "merge request creation failed"})
		return
	}

	var body struct {
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Description  string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "bad body"})
		return
	}

	f.mu.Lock()
	mergeRequest := MergeRequest{
		IID:          f.nextIID,
		ProjectID:    project.ID,
		SourceBranch: body.SourceBranch,
		TargetBranch: body.TargetBranch,
		Title:        body.Title,
		Description:  body.Description,
		State:        "opened",
		WebURL:       fmt.Sprintf("%s/%s/-/merge_requests/%d", f.server.URL, project.Path, f.nextIID),
	}
	f.nextIID++
	f.mergeReqs = append(f.mergeReqs, mergeRequest)
	payload := f.mergeRequestJSONLocked(mergeRequest)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, payload)
}

func (f *GitLabFake) serveMergeRequest(w http.ResponseWriter, r *http.Request, project Project, tail string) {
	iidText, action, _ := strings.Cut(tail, "/")
	iid, err := strconv.Atoi(iidText)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	index := -1
	for i := range f.mergeReqs {
		if f.mergeReqs[i].ProjectID == project.ID && f.mergeReqs[i].IID == iid {
			index = i
			break
		}
	}
	if index < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, f.mergeRequestJSONLocked(f.mergeReqs[index]))
	case action == "" && r.Method == http.MethodPut:
		var body struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "bad body"})
			return
		}
		if body.Title != "" {
			f.mergeReqs[index].Title = body.Title
		}
		if body.Description != "" {
			f.mergeReqs[index].Description = body.Description
		}
		writeJSON(w, http.StatusOK, f.mergeRequestJSONLocked(f.mergeReqs[index]))
	case action == "merge":
		f.mergeCalls++
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "merging is not implemented"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
	}
}

func (f *GitLabFake) mergeRequestJSONLocked(mergeRequest MergeRequest) map[string]any {
	return map[string]any{
		"id":            mergeRequest.IID,
		"iid":           mergeRequest.IID,
		"project_id":    mergeRequest.ProjectID,
		"source_branch": mergeRequest.SourceBranch,
		"target_branch": mergeRequest.TargetBranch,
		"title":         mergeRequest.Title,
		"description":   mergeRequest.Description,
		"state":         mergeRequest.State,
		"web_url":       mergeRequest.WebURL,
	}
}

// Origin is a throwaway bare repository that acts as the GitLab remote.
type Origin struct {
	Path            string
	SeedDir         string
	InitialRevision string
}

// NewOrigin creates a bare origin with one commit on main.
func NewOrigin(t testing.TB, root string) *Origin {
	t.Helper()
	origin := &Origin{
		Path:    filepath.Join(root, "origins", "billing-api.git"),
		SeedDir: filepath.Join(root, "seed"),
	}
	if err := os.MkdirAll(filepath.Dir(origin.Path), 0o750); err != nil {
		t.Fatalf("create origin dir: %v", err)
	}
	gitCommand(t, root, "init", "--bare", "--initial-branch=main", origin.Path)
	gitCommand(t, root, "init", "--initial-branch=main", origin.SeedDir)
	if err := os.WriteFile(filepath.Join(origin.SeedDir, "README.md"), []byte("# Billing API\n"), 0o600); err != nil {
		t.Fatalf("seed README: %v", err)
	}
	gitCommand(t, origin.SeedDir, "add", "-A")
	gitCommand(t, origin.SeedDir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-m", "initial")
	gitCommand(t, origin.SeedDir, "remote", "add", "origin", origin.Path)
	gitCommand(t, origin.SeedDir, "push", "origin", "main")
	origin.InitialRevision = strings.TrimSpace(gitOutput(t, origin.Path, "rev-parse", "main"))
	return origin
}

// Tip is the current commit of a branch on the origin.
func (o *Origin) Tip(t testing.TB, branch string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, o.Path, "rev-parse", branch))
}

// CommitSeed adds and pushes one file to main.
func (o *Origin) CommitSeed(t testing.TB, name string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(o.SeedDir, name), content, 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	gitCommand(t, o.SeedDir, "add", "-A")
	gitCommand(t, o.SeedDir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-m", "update "+name)
	gitCommand(t, o.SeedDir, "push", "origin", "main")
	return strings.TrimSpace(gitOutput(t, o.Path, "rev-parse", "main"))
}

// RefusePushes installs a pre-receive hook that rejects every push, so the
// push-rejection path can be exercised without a real remote.
func (o *Origin) RefusePushes(t testing.TB) {
	t.Helper()
	hooks := filepath.Join(o.Path, "hooks")
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatalf("create hooks dir: %v", err)
	}
	hook := filepath.Join(hooks, "pre-receive")
	// #nosec G306 -- a git hook must be executable by the git process.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}
}

// Branches lists the branch refs currently on the origin.
func (o *Origin) Branches(t testing.TB) []string {
	t.Helper()
	output := strings.TrimSpace(gitOutput(t, o.Path, "for-each-ref", "--format=%(refname:short)", "refs/heads/"))
	if output == "" {
		return nil
	}
	return strings.Split(output, "\n")
}

func gitCommand(t testing.TB, dir string, args ...string) {
	t.Helper()
	// #nosec G204 -- git is the tool under test and args are literals in tests.
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t testing.TB, dir string, args ...string) string {
	t.Helper()
	// #nosec G204 -- git is the tool under test and args are literals in tests.
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
