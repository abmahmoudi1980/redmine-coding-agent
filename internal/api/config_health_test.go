package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/health"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

func healthTestServer(t *testing.T, checker *health.Checker) http.Handler {
	t.Helper()
	return NewServer(Deps{
		Logger: logging.New(logging.Options{Stderr: io.Discard}),
		Health: checker,
	}).Handler()
}

func TestHealthDegradesWithoutFailingTheRequest(t *testing.T) {
	checker := health.NewChecker(health.Options{Probes: []health.Probe{
		{
			Name:        "database",
			Remediation: "fix the database",
			Check:       func(context.Context) (string, error) { return "writable", nil },
		},
		{
			Name:        "redmine",
			Remediation: "check redmine.api_key",
			Check:       func(context.Context) (string, error) { return "", errors.New("401 from Redmine") },
		},
	}})
	handler := healthTestServer(t, checker)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health?deep=true", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a degraded dependency is not a 5xx)", recorder.Code)
	}
	var report struct {
		Status     string `json:"status"`
		Components []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			LatencyMS   *int64 `json:"latencyMs"`
			Remediation string `json:"remediation"`
		} `json:"components"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if report.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", report.Status)
	}
	if len(report.Components) != 2 {
		t.Fatalf("components = %d, want 2", len(report.Components))
	}
	for _, component := range report.Components {
		if component.Status == "failing" && component.Remediation == "" {
			t.Fatalf("%s has no remediation", component.Name)
		}
		if component.LatencyMS == nil {
			t.Fatalf("%s has no latency", component.Name)
		}
	}
}

func TestConfigDocumentKeepsOnlySecretReferences(t *testing.T) {
	cfg := &config.Config{
		SchemaVersion: 1,
		HTTP:          config.HTTP{Listen: "127.0.0.1:8080"},
		Data:          config.Data{Database: "data/agent.db", WorkspaceRoot: "data/ws", LogDir: "data/logs"},
		Redmine: config.Redmine{
			BaseURL: "https://redmine.example",
			APIKey:  config.NewSecretRef("env:MY_REDMINE_KEY"),
		},
		GitLab: config.GitLab{
			BaseURL: "https://gitlab.example",
			Token:   config.NewSecretRef("file:/run/secrets/gitlab"),
		},
		LoadedAt: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
	}
	handler := NewServer(Deps{
		Config: cfg,
		Logger: logging.New(logging.Options{Stderr: io.Discard}),
	}).Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}

	var document struct {
		SchemaVersion int            `json:"schemaVersion"`
		ContentHash   string         `json:"contentHash"`
		LoadedAt      time.Time      `json:"loadedAt"`
		Redacted      map[string]any `json:"redacted"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if document.SchemaVersion != 1 || document.ContentHash == "" {
		t.Fatalf("document = %+v", document)
	}
	if document.ContentHash != cfg.ContentHash() {
		t.Fatal("the served hash does not identify the loaded configuration")
	}
	if !document.LoadedAt.Equal(cfg.LoadedAt) {
		t.Fatalf("loadedAt = %s, want %s", document.LoadedAt, cfg.LoadedAt)
	}
	redmine, ok := document.Redacted["redmine"].(map[string]any)
	if !ok {
		t.Fatalf("redacted.redmine = %T", document.Redacted["redmine"])
	}
	if redmine["apiKey"] != "env:MY_REDMINE_KEY" {
		t.Fatalf("apiKey = %v, want the reference only", redmine["apiKey"])
	}
	if strings.Contains(recorder.Body.String(), "secret-value") {
		t.Fatal("the config document must never carry a resolved secret")
	}
}

func TestMetaReportsBuildIdentity(t *testing.T) {
	handler := NewServer(Deps{
		Logger: logging.New(logging.Options{Stderr: io.Discard}),
		Build: BuildInfo{
			Version: "1.2.3", Commit: "abc123", BuiltAt: "2026-10-26T09:00:00Z",
			OS: "linux", Arch: "amd64", SchemaVersion: 1,
		},
	}).Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var meta metaDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.Version != "1.2.3" || meta.Commit != "abc123" || meta.SchemaVersion != 1 {
		t.Fatalf("meta = %+v", meta)
	}
}
