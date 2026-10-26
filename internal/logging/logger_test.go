package logging_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

type logRecord struct {
	Message string `json:"msg"`
	RunID   string `json:"run_id"`
	Stage   string `json:"stage"`
	Detail  string `json:"detail"`
}

func TestRunLoggerCarriesScopeAndRedacts(t *testing.T) {
	dir := t.TempDir()
	redactor := logging.NewRedactor()
	redactor.Register("very-secret")

	var stderr bytes.Buffer
	process := logging.New(logging.Options{LogDir: dir, Redactor: redactor, Stderr: &stderr})

	run, err := process.WithRun("run-1", "preparing")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	run.Info("first", "detail", "token=very-secret")
	run.WithStage("implementing").Info("second", "detail", "still very-secret")
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, "run-1.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	if strings.Contains(string(raw), "very-secret") {
		t.Fatalf("run log leaked the secret: %s", raw)
	}

	records := decodeLogLines(t, raw)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].RunID != "run-1" || records[0].Stage != "preparing" {
		t.Fatalf("first record scope: %+v", records[0])
	}
	if records[1].RunID != "run-1" || records[1].Stage != "implementing" {
		t.Fatalf("stage update not applied: %+v", records[1])
	}

	if strings.Contains(stderr.String(), "very-secret") {
		t.Fatalf("stderr leaked the secret: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"run_id":"run-1"`) {
		t.Fatalf("stderr missing run scope: %s", stderr.String())
	}
}

func TestRunLogsAreSeparatePerRun(t *testing.T) {
	dir := t.TempDir()
	process := logging.New(logging.Options{LogDir: dir, Stderr: &bytes.Buffer{}})

	first, err := process.WithRun("run-a", "queued")
	if err != nil {
		t.Fatal(err)
	}
	first.Info("a")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := process.WithRun("run-b", "queued")
	if err != nil {
		t.Fatal(err)
	}
	second.Info("b")
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d log files, want 2", len(entries))
	}
}

func decodeLogLines(t *testing.T, raw []byte) []logRecord {
	t.Helper()
	var records []logRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var record logRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
