package gitlab_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
)

const testToken = "fixture-gitlab-token" // #nosec G101 -- fake token for an httptest server

type recordedRequest struct {
	Method      string
	EscapedPath string
	Token       string
	Query       url.Values
	Body        string
}

type fakeGitLab struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	merge    []mergeRequest
	nextIID  int
}

type mergeRequest struct {
	IID          int    `json:"iid"`
	ProjectID    int    `json:"project_id"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	WebURL       string `json:"web_url"`
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	fake := &fakeGitLab{t: t, nextIID: 1}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeGitLab) URL() string { return f.server.URL }

func (f *fakeGitLab) Requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeGitLab) MergeRequests() []mergeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]mergeRequest, len(f.merge))
	copy(out, f.merge)
	return out
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	record := recordedRequest{
		Method:      r.Method,
		EscapedPath: r.URL.EscapedPath(),
		Token:       r.Header.Get("PRIVATE-TOKEN"),
		Query:       r.URL.Query(),
		Body:        string(body),
	}
	f.mu.Lock()
	f.requests = append(f.requests, record)
	f.mu.Unlock()

	if record.Token != testToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"})
		return
	}

	const prefix = "/api/v4/projects/"
	escaped := r.URL.EscapedPath()
	if !strings.HasPrefix(escaped, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(escaped, prefix)
	refPart, tail, _ := strings.Cut(rest, "/")
	ref, err := url.PathUnescape(refPart)
	if err != nil || ref != "group/sub/project" {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Project Not Found"})
		return
	}
	tail = strings.Trim(tail, "/")

	switch {
	case tail == "merge_requests" && r.Method == http.MethodGet:
		f.list(w, r)
	case tail == "merge_requests" && r.Method == http.MethodPost:
		f.create(w, body)
	case strings.HasPrefix(tail, "merge_requests/"):
		f.one(w, r, strings.TrimPrefix(tail, "merge_requests/"), body)
	case strings.HasSuffix(tail, "/merge"):
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "merging is not implemented"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
	}
}

func (f *fakeGitLab) list(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source_branch")
	target := r.URL.Query().Get("target_branch")
	state := r.URL.Query().Get("state")

	f.mu.Lock()
	out := make([]mergeRequest, 0, len(f.merge))
	for _, mergeRequest := range f.merge {
		if source != "" && mergeRequest.SourceBranch != source {
			continue
		}
		if target != "" && mergeRequest.TargetBranch != target {
			continue
		}
		if state != "" && mergeRequest.State != state {
			continue
		}
		out = append(out, mergeRequest)
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeGitLab) create(w http.ResponseWriter, body []byte) {
	var input struct {
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Description  string `json:"description"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "bad body"})
		return
	}
	f.mu.Lock()
	created := mergeRequest{
		IID:          f.nextIID,
		ProjectID:    101,
		SourceBranch: input.SourceBranch,
		TargetBranch: input.TargetBranch,
		Title:        input.Title,
		Description:  input.Description,
		State:        "opened",
		WebURL:       f.server.URL + "/group/sub/project/-/merge_requests/" + strconv.Itoa(f.nextIID),
	}
	f.nextIID++
	f.merge = append(f.merge, created)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, created)
}

func (f *fakeGitLab) one(w http.ResponseWriter, r *http.Request, tail string, body []byte) {
	iidText, action, _ := strings.Cut(tail, "/")
	iid, err := strconv.Atoi(iidText)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
		return
	}
	if action == "merge" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "merging is not implemented"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	index := -1
	for position := range f.merge {
		if f.merge[position].IID == iid {
			index = position
			break
		}
	}
	if index < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
		return
	}
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "not allowed"})
		return
	}
	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "bad body"})
		return
	}
	if input.Title != "" {
		f.merge[index].Title = input.Title
	}
	if input.Description != "" {
		f.merge[index].Description = input.Description
	}
	writeJSON(w, http.StatusOK, f.merge[index])
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func newClient(t *testing.T, baseURL string) *gitlab.Client {
	t.Helper()
	client, err := gitlab.New(gitlab.Options{
		BaseURL: baseURL,
		Token:   config.NewSecretRef("env:RCA_TEST_GITLAB_TOKEN"),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func TestMergeRequestLifecycleSendsPrivateTokenAndEncodesProjectPath(t *testing.T) {
	t.Setenv("RCA_TEST_GITLAB_TOKEN", testToken)
	fake := newFakeGitLab(t)
	client := newClient(t, fake.URL())
	ctx := context.Background()
	repositoryPath := "group/sub/project"
	source, target := "agent/issue-4821", "main"

	existing, err := client.FindOpenMergeRequest(ctx, repositoryPath, source, target)
	if err != nil {
		t.Fatalf("find (empty): %v", err)
	}
	if existing != nil {
		t.Fatalf("expected no merge request, got %+v", existing)
	}

	created, err := client.CreateMergeRequest(ctx, repositoryPath, gitlab.MergeRequestInput{
		SourceBranch: source,
		TargetBranch: target,
		Title:        "Issue #4821: Invoice totals",
		Description:  "## Issue\n#4821\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.IID != 1 || created.WebURL == "" {
		t.Fatalf("created = %+v", created)
	}
	if created.SourceBranch != source || created.TargetBranch != target {
		t.Fatalf("created branches = %q -> %q", created.SourceBranch, created.TargetBranch)
	}

	reused, err := client.FindOpenMergeRequest(ctx, repositoryPath, source, target)
	if err != nil {
		t.Fatalf("find (existing): %v", err)
	}
	if reused == nil {
		t.Fatal("the open merge request must be reused, not duplicated")
	}
	if reused.IID != created.IID {
		t.Fatalf("reused iid = %d, want %d", reused.IID, created.IID)
	}

	updated, err := client.UpdateMergeRequest(ctx, repositoryPath, reused.IID, gitlab.MergeRequestInput{
		Title:       "Issue #4821: Invoice totals (updated)",
		Description: "## Issue\n#4821 updated\n",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Description != "## Issue\n#4821 updated\n" {
		t.Fatalf("updated description = %q", updated.Description)
	}
	if got := len(fake.MergeRequests()); got != 1 {
		t.Fatalf("merge request count = %d, want 1", got)
	}

	requests := fake.Requests()
	if len(requests) == 0 {
		t.Fatal("the fake recorded no request")
	}
	for _, request := range requests {
		if request.Token != testToken {
			t.Fatalf("%s %s did not carry PRIVATE-TOKEN", request.Method, request.EscapedPath)
		}
		if !strings.Contains(request.EscapedPath, "group%2Fsub%2Fproject") {
			t.Fatalf("project path was not URL-encoded: %q", request.EscapedPath)
		}
		if request.Method == http.MethodDelete || strings.HasSuffix(request.EscapedPath, "/merge") {
			t.Fatalf("forbidden operation was attempted: %s %s", request.Method, request.EscapedPath)
		}
	}
	findRequests := 0
	for _, request := range requests {
		if request.Method == http.MethodGet {
			if request.Query.Get("source_branch") != source || request.Query.Get("target_branch") != target {
				t.Fatalf("lookup query = %v", request.Query)
			}
			if request.Query.Get("state") != "opened" {
				t.Fatalf("lookup state = %q, want opened", request.Query.Get("state"))
			}
			findRequests++
		}
	}
	if findRequests != 2 {
		t.Fatalf("lookup count = %d, want 2", findRequests)
	}
}

func TestFetchProjectNotFoundIsAMappingError(t *testing.T) {
	t.Setenv("RCA_TEST_GITLAB_TOKEN", testToken)
	fake := newFakeGitLab(t)
	client := newClient(t, fake.URL())

	_, err := client.GetProject(context.Background(), "other/project")
	if err == nil {
		t.Fatal("expected an error for an unknown project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v", err)
	}
}
