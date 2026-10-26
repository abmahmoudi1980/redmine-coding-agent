package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

var (
	cliBuildOnce sync.Once
	cliBuildPath string
	cliBuildErr  error
)

// cliBinary builds the real command once per test process, so every assertion
// below exercises the public CLI rather than an internal seam.
func cliBinary(t *testing.T) string {
	t.Helper()
	cliBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "redmine-agent-cli-*")
		if err != nil {
			cliBuildErr = err
			return
		}
		name := "redmine-agent"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		cliBuildPath = filepath.Join(dir, name)
		// #nosec G204 -- fixed package path, output in a test temp directory.
		command := exec.Command("go", "build", "-trimpath", "-o", cliBuildPath, "./cmd/redmine-agent")
		command.Dir = filepath.Join(sourceDir(), "..", "..")
		command.Env = append(command.Environ(), "CGO_ENABLED=0")
		if output, err := command.CombinedOutput(); err != nil {
			cliBuildErr = err
			t.Logf("go build output:\n%s", output)
		}
	})
	if cliBuildErr != nil {
		t.Fatalf("build cli: %v", cliBuildErr)
	}
	return cliBuildPath
}

type cliResult struct {
	Stdout string
	Stderr string
	Code   int
}

func runCLI(t *testing.T, h *Harness, configPath string, args ...string) cliResult {
	t.Helper()
	full := append(append([]string{}, args...), "--config", configPath)
	// #nosec G204 -- the binary and flags are controlled by the test.
	command := exec.Command(cliBinary(t), full...)
	command.Dir = h.Root
	command.Env = append(command.Environ(),
		"RCA_TEST_REDMINE_KEY="+fixtureRedmineKey,
		"RCA_TEST_GITLAB_TOKEN="+fixtureGitLabToken,
	)
	command.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			t.Fatalf("run cli %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return cliResult{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

func jsonLines(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	documents := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var document map[string]any
		if err := json.Unmarshal([]byte(line), &document); err != nil {
			t.Fatalf("stdout is not one JSON document per line: %v\nline: %s", err, line)
		}
		documents = append(documents, document)
	}
	return documents
}

func lastDocument(t *testing.T, stdout string) map[string]any {
	t.Helper()
	documents := jsonLines(t, stdout)
	if len(documents) == 0 {
		t.Fatal("expected at least one JSON document")
	}
	return documents[len(documents)-1]
}

func TestCLIVersionJSON(t *testing.T) {
	h := New(t)
	result := runCLI(t, h, h.SaveConfig(t), "version", "--json")
	if result.Code != 0 {
		t.Fatalf("exit = %d, stderr = %s", result.Code, result.Stderr)
	}
	document := lastDocument(t, result.Stdout)
	for _, key := range []string{"version", "commit", "builtAt", "goVersion", "os", "arch", "schemaVersion"} {
		if _, ok := document[key]; !ok {
			t.Fatalf("version --json is missing %q: %v", key, document)
		}
	}
	if document["schemaVersion"].(float64) != float64(config.CurrentSchemaVersion) {
		t.Fatalf("schemaVersion = %v", document["schemaVersion"])
	}
}

func TestCLIRunRequiresYesAndStartsNothing(t *testing.T) {
	h := New(t)
	h.FakeAgent()
	h.AddIssue(issueWithMode("ok"))
	modeConfig := h.SaveConfig(t)

	result := runCLI(t, h, modeConfig, "run", "--issue", "4821")
	if result.Code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", result.Code, result.Stderr)
	}
	if !strings.Contains(result.Stderr, "confirmation") {
		t.Fatalf("stderr should name the missing confirmation: %s", result.Stderr)
	}

	runs, _, err := h.Store.ListRuns(t.Context(), store.RunFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("an unconfirmed run created %d row(s)", len(runs))
	}
}

func TestCLIRunExitCodesFollowTerminalState(t *testing.T) {
	cases := []struct {
		name       string
		issueID    int
		mode       string
		tune       func(*config.Config)
		wantExit   int
		wantState  domain.RunState
		wantFailed domain.FailureCode
	}{
		{
			name:      "completed",
			issueID:   4821,
			mode:      "ok",
			wantExit:  0,
			wantState: domain.StateCompleted,
		},
		{
			name:      "blocked",
			issueID:   4822,
			mode:      "blocked",
			wantExit:  7,
			wantState: domain.StateBlocked,
		},
		{
			name:      "no changes",
			issueID:   4823,
			mode:      "no-changes",
			wantExit:  6,
			wantState: domain.StateFailed,
		},
		{
			name:     "validation failed",
			issueID:  4824,
			mode:     "ok",
			wantExit: 5,
			tune: func(cfg *config.Config) {
				cfg.Repositories[0].Checks[0].CommandLinux = "exit 1"
				cfg.Repositories[0].Checks[0].CommandWindows = "exit /b 1"
				cfg.Agent.MaxRepairs = 0
			},
			wantState:  domain.StateFailed,
			wantFailed: domain.FailValidationFailed,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := New(t)
			h.FakeAgent()
			if testCase.tune != nil {
				testCase.tune(h.Config)
			}
			h.AddIssue(func() Issue {
				issue := issueWithMode(testCase.mode)
				issue.ID = testCase.issueID
				return issue
			}())
			configPath := h.SaveConfig(t)

			result := runCLI(t, h, configPath, "run", "--issue", strconv.Itoa(testCase.issueID), "--yes", "--format", "json")
			if result.Code != testCase.wantExit {
				t.Fatalf("exit = %d, want %d\nstderr:\n%s", result.Code, testCase.wantExit, result.Stderr)
			}

			final := lastDocument(t, result.Stdout)
			if final["runId"] == nil || final["state"] != string(testCase.wantState) {
				t.Fatalf("final run document = %v", final)
			}
			if testCase.wantFailed != "" && final["failureCode"] != string(testCase.wantFailed) {
				t.Fatalf("failureCode = %v, want %s", final["failureCode"], testCase.wantFailed)
			}

			runID := final["runId"].(string)
			show := runCLI(t, h, configPath, "runs", "show", runID, "--json")
			if show.Code != 0 {
				t.Fatalf("runs show exit = %d, stderr = %s", show.Code, show.Stderr)
			}
			detail := lastDocument(t, show.Stdout)
			if detail["runId"] != runID {
				t.Fatalf("runs show runId = %v, want %s", detail["runId"], runID)
			}

			list := runCLI(t, h, configPath, "runs", "list", "--json", "--limit", "10")
			if list.Code != 0 {
				t.Fatalf("runs list exit = %d, stderr = %s", list.Code, list.Stderr)
			}
			page := lastDocument(t, list.Stdout)
			if _, ok := page["items"]; !ok {
				t.Fatalf("runs list document = %v", page)
			}
		})
	}
}
