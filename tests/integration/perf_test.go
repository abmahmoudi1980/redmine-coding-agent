package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/api"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
)

// TestPreviewPerformanceBudget proves SC-001's 3 s preview budget against the
// deterministic fake Redmine. It measures the real handler, store, and client,
// not a mock of them.
func TestPreviewPerformanceBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("performance budget is skipped with -short")
	}
	h := New(t)
	h.AddIssue(defaultIssue())
	if _, err := h.Store.SeedRepositories(context.Background(), h.Config); err != nil {
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
	server := httptest.NewServer(api.NewServer(api.Deps{
		Config:   h.Config,
		Store:    h.Store,
		Redactor: h.Redactor,
		Redmine:  client,
	}).Handler())
	defer server.Close()

	const samples = 40
	durations := make([]time.Duration, 0, samples)
	for index := 0; index < samples; index++ {
		started := time.Now()
		response, err := http.Get(server.URL + "/api/issues/4821/preview")
		if err != nil {
			t.Fatalf("preview request %d: %v", index, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("preview status = %d", response.StatusCode)
		}
		durations = append(durations, time.Since(started))
	}
	p50, p95 := percentiles(durations)
	if p95 > 3*time.Second {
		t.Fatalf("preview p95 = %s, budget is 3s (p50 %s)", p95, p50)
	}
	t.Logf("preview latency over %d requests: p50=%s p95=%s", samples, p50, p95)
}

// TestEmbeddedSPAIsServedQuickly checks the first paint comes from the binary
// itself, with no network round trip beyond loopback (SC-001 companion).
func TestEmbeddedSPAIsServedQuickly(t *testing.T) {
	h := New(t)
	server := httptest.NewServer(api.NewServer(api.Deps{
		Config:   h.Config,
		Store:    h.Store,
		Redactor: h.Redactor,
	}).Handler())
	defer server.Close()

	started := time.Now()
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	elapsed := time.Since(started)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", response.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("the embedded SPA returned an empty document")
	}
	if elapsed > time.Second {
		t.Fatalf("SPA first paint took %s, budget is 1s", elapsed)
	}
	t.Logf("embedded SPA first paint: %s (%d bytes)", elapsed, len(body))
}

// TestStrippedBinarySizeBudget guards the 40 MB single-binary promise.
func TestStrippedBinarySizeBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("binary build is skipped with -short")
	}
	name := "redmine-agent-size-check"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	output := filepath.Join(t.TempDir(), name)
	// #nosec G204 -- fixed package path and a temp output path.
	command := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", output, "./cmd/redmine-agent")
	command.Dir = filepath.Join(sourceDir(), "..", "..")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if buildOutput, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build stripped binary: %v\n%s", err, buildOutput)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	const budget = 40 << 20
	if info.Size() > budget {
		t.Fatalf("stripped binary is %s, budget is %s", humanBytes(info.Size()), humanBytes(budget))
	}
	t.Logf("stripped binary size: %s", humanBytes(info.Size()))
}

func percentiles(durations []time.Duration) (p50, p95 time.Duration) {
	if len(durations) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)/2]
	index := int(float64(len(sorted))*0.95) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return p50, sorted[index]
}

func humanBytes(bytes int64) string {
	const unit = 1 << 20
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/float64(unit))
}
