package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
)

func TestHarnessWiring(t *testing.T) {
	h := New(t)

	request, err := http.NewRequest(http.MethodGet, h.Redmine.URL()+"/issues/4821.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Redmine-API-Key", fixtureRedmineKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("redmine request: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("redmine status %d: %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "Invoice totals round incorrectly") {
		t.Fatalf("redmine payload: %s", body)
	}

	unauthorized, err := http.Get(h.Redmine.URL() + "/issues/4821.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing API key status = %d", unauthorized.StatusCode)
	}

	projectRequest, err := http.NewRequest(http.MethodGet, h.GitLab.URL()+"/api/v4/projects/group%2Fbilling-api", nil)
	if err != nil {
		t.Fatal(err)
	}
	projectRequest.Header.Set("PRIVATE-TOKEN", fixtureGitLabToken)
	projectResponse, err := http.DefaultClient.Do(projectRequest)
	if err != nil {
		t.Fatalf("gitlab request: %v", err)
	}
	projectBody, _ := io.ReadAll(projectResponse.Body)
	_ = projectResponse.Body.Close()
	if projectResponse.StatusCode != http.StatusOK {
		t.Fatalf("gitlab status %d: %s", projectResponse.StatusCode, projectBody)
	}
	var projectPayload map[string]any
	if err := json.Unmarshal(projectBody, &projectPayload); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if recordURL, _ := projectPayload["http_url_to_repo"].(string); recordURL != h.Origin.Path {
		t.Fatalf("gitlab http_url_to_repo = %q, want %q", recordURL, h.Origin.Path)
	}

	loaded, err := config.Load(h.ConfigPath)
	if err != nil {
		t.Fatalf("load harness config: %v", err)
	}
	if loaded.Redmine.BaseURL != h.Redmine.URL() {
		t.Fatalf("config redmine URL = %s", loaded.Redmine.BaseURL)
	}

	agentPath := h.FakeAgent()
	workspace := t.TempDir()
	command := exec.Command(agentPath, "--mode", "ok")
	command.Dir = workspace
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fake agent: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "## RESULT") || !strings.Contains(string(output), "status: done") {
		t.Fatalf("fake agent output: %s", output)
	}
	if _, err := os.Stat(filepath.Join(workspace, "agent-change.txt")); err != nil {
		t.Fatalf("fake agent did not change the workspace: %v", err)
	}
}

func TestGitLabFakeMergeRequestLifecycle(t *testing.T) {
	h := New(t)
	base := h.GitLab.URL() + "/api/v4/projects/101/merge_requests"

	call := func(method, url, body string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("PRIVATE-TOKEN", fixtureGitLabToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	response := call(http.MethodPost, base,
		`{"source_branch":"agent/issue-1","target_branch":"main","title":"Issue 1","description":"body"}`)
	payload, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d: %s", response.StatusCode, payload)
	}
	var created map[string]any
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatal(err)
	}
	if created["web_url"] == "" {
		t.Fatalf("no web_url: %s", payload)
	}

	response = call(http.MethodGet, base+"?source_branch=agent/issue-1&target_branch=main&state=opened", "")
	listPayload, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	var list []map[string]any
	if err := json.Unmarshal(listPayload, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %s", listPayload)
	}

	response = call(http.MethodPut, base+"/1", `{"description":"updated"}`)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("update status %d", response.StatusCode)
	}

	response = call(http.MethodPost, base+"/1/merge", "")
	_ = response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("the fake must never report a merge")
	}
	if h.GitLab.MergeCalls() == 0 {
		t.Fatal("merge attempt was not observed")
	}
}

func TestHarnessFakeAgentModes(t *testing.T) {
	h := New(t)
	agentPath := h.FakeAgent()

	cases := map[string]string{
		"blocked":    "status: blocked",
		"no-changes": "files: (none)",
	}
	for mode, expected := range cases {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			command := exec.Command(agentPath, "--mode", mode)
			command.Dir = workspace
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("fake agent %s: %v\n%s", mode, err, output)
			}
			if !strings.Contains(string(output), expected) {
				t.Fatalf("mode %s output: %s", mode, output)
			}
		})
	}

	workspace := t.TempDir()
	command := exec.Command(agentPath, "--mode", "crash")
	command.Dir = workspace
	if err := command.Run(); err == nil {
		t.Fatal("crash mode must exit non-zero")
	}
}
