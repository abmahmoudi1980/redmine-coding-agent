package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestDeepHealthProbesEveryDependency proves the health report is wired to the
// real clients, not only to the database (FR-058).
func TestDeepHealthProbesEveryDependency(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})

	response, err := http.Get(env.server.URL + "/api/health?deep=true")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	var report struct {
		Status     string `json:"status"`
		Components []struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			LatencyMS *int64 `json:"latencyMs"`
			Detail    string `json:"detail"`
		} `json:"components"`
	}
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatalf("decode health: %v", err)
	}

	want := []string{"database", "redmine", "gitlab", "repository", "agent", "disk"}
	if len(report.Components) != len(want) {
		t.Fatalf("components = %d, want %d: %+v", len(report.Components), len(want), report.Components)
	}
	for index, name := range want {
		component := report.Components[index]
		if component.Name != name {
			t.Fatalf("component %d = %q, want %q", index, component.Name, name)
		}
		if component.Status != "ok" {
			t.Errorf("%s status = %q, detail = %q", name, component.Status, component.Detail)
		}
		if component.LatencyMS == nil {
			t.Errorf("%s has no latency", name)
		}
	}
	if report.Status != "ok" {
		t.Fatalf("overall status = %q, want ok", report.Status)
	}
}

// TestFastHealthSkipsDeepProbes keeps the header request cheap.
func TestFastHealthSkipsDeepProbes(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})

	response, err := http.Get(env.server.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer response.Body.Close()

	var report struct {
		Components []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"components"`
	}
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	for _, component := range report.Components {
		if component.Name == "redmine" {
			if component.Status != "unknown" {
				t.Fatalf("redmine = %q, want unknown on the fast path", component.Status)
			}
			if component.Detail == "" {
				t.Fatal("a skipped probe must explain why")
			}
		}
	}
}
