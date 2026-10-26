package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

func quietServer() *Server {
	return NewServer(Deps{Logger: logging.New(logging.Options{Stderr: io.Discard})})
}

func samplePath(path string) string {
	path = strings.ReplaceAll(path, "{issueId}", "4821")
	path = strings.ReplaceAll(path, "{runId}", "018f2e4a-0000-7000-8000-000000000001")
	return path
}

func TestDocumentedRoutesExist(t *testing.T) {
	server := quietServer()
	handler := server.Handler()

	for _, route := range server.Routes() {
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(route.Method, samplePath(route.Path), nil)
			handler.ServeHTTP(recorder, request)
			if recorder.Code == http.StatusNotFound || recorder.Code == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s is not routed (status %d)", route.Method, route.Path, recorder.Code)
			}
		})
	}
}

func TestRoutesMatchOpenAPIDocument(t *testing.T) {
	raw, err := os.ReadFile("../../specs/010-redmine-coding-agent/contracts/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var document struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	documented := make(map[string]bool)
	for path, operations := range document.Paths {
		for method := range operations {
			method = strings.ToUpper(method)
			switch method {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
				documented[method+" "+path] = true
			}
		}
	}
	if len(documented) == 0 {
		t.Fatal("no routes parsed from openapi.yaml")
	}

	server := quietServer()
	implemented := make(map[string]bool)
	for _, route := range server.Routes() {
		implemented[route.Method+" "+route.Path] = true
		if !documented[route.Method+" "+route.Path] {
			t.Errorf("route %s %s is not in openapi.yaml", route.Method, route.Path)
		}
	}
	for route := range documented {
		if !implemented[route] {
			t.Errorf("documented route %s is not registered", route)
		}
	}
}

func TestUnknownAPIRouteReturnsProblemJSON(t *testing.T) {
	handler := quietServer().Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/definitely-not-a-route", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("content type = %q", contentType)
	}
	var problem Problem
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "not_found" || problem.Status != http.StatusNotFound {
		t.Fatalf("problem = %+v", problem)
	}
	if problem.Type == "" {
		t.Fatal("problem type is empty")
	}
}

func TestSPAFallbackServesIndex(t *testing.T) {
	handler := quietServer().Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/some-run", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("content type = %q", contentType)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("index body is empty")
	}
}

func TestProblemTitleMapping(t *testing.T) {
	cases := map[string]string{
		"invalid_issue_id":     "Invalid issue number",
		"repository_ambiguous": "Repository selection required",
		"connectivity_gitlab":  "GitLab unreachable",
	}
	for code, want := range cases {
		if got := TitleFor(code); got != want {
			t.Errorf("TitleFor(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestRouteTableHasNoDuplicates(t *testing.T) {
	server := quietServer()
	keys := make([]string, 0, len(server.Routes()))
	for _, route := range server.Routes() {
		keys = append(keys, route.Method+" "+route.Path)
	}
	slices.Sort(keys)
	for index := 1; index < len(keys); index++ {
		if keys[index] == keys[index-1] {
			t.Fatalf("duplicate route %s", keys[index])
		}
	}
}
