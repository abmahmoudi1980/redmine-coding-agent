package validate_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/validate"
)

func newRunner() *validate.Runner {
	return validate.New(validate.Options{Redactor: logging.NewRedactor()})
}

func TestRunCheckClassifiesExitCodes(t *testing.T) {
	runner := newRunner()
	ctx := context.Background()
	workspace := t.TempDir()

	passed := runner.RunCheck(ctx, domain.CheckDefinition{Name: "ok", CommandLinux: "exit 0", CommandWindows: "exit 0", Required: true}, workspace)
	if passed.Outcome != domain.OutcomePassed {
		t.Fatalf("outcome = %q (%s), want passed", passed.Outcome, passed.OutputExcerpt)
	}
	if passed.ExitCode == nil || *passed.ExitCode != 0 {
		t.Fatalf("exit code = %v", passed.ExitCode)
	}
	if passed.DurationMS < 0 {
		t.Fatalf("duration = %d", passed.DurationMS)
	}
	if passed.ExecutedAt == nil {
		t.Fatal("executedAt must be set for a run command")
	}

	failed := runner.RunCheck(ctx, domain.CheckDefinition{Name: "bad", CommandLinux: "exit 3", CommandWindows: "exit 3", Required: true}, workspace)
	if failed.Outcome != domain.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", failed.Outcome)
	}
	if failed.ExitCode == nil || *failed.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", failed.ExitCode)
	}
}

func TestRunCheckRecordsATimeoutAsFailure(t *testing.T) {
	runner := newRunner()
	check := domain.CheckDefinition{
		Name:           "slow",
		CommandLinux:   slowCommand("linux"),
		CommandWindows: slowCommand("windows"),
		Required:       true,
		TimeoutSeconds: 1,
	}
	started := time.Now()
	result := runner.RunCheck(context.Background(), check, t.TempDir())
	if result.Outcome != domain.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", result.Outcome)
	}
	if !strings.Contains(result.Reason, "timeout") {
		t.Fatalf("reason = %q", result.Reason)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("the timeout was not enforced quickly: %s", elapsed)
	}
}

func TestRunCheckRecordsCancelAsNotExecuted(t *testing.T) {
	runner := newRunner()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := runner.RunCheck(ctx, domain.CheckDefinition{Name: "cancelled", CommandLinux: "exit 0", CommandWindows: "exit 0", Required: true}, t.TempDir())
	if result.Outcome != domain.OutcomeNotExecuted {
		t.Fatalf("outcome = %q, want not_executed", result.Outcome)
	}
	if result.Reason != "cancelled" {
		t.Fatalf("reason = %q", result.Reason)
	}
	if result.ExitCode != nil {
		t.Fatalf("exit code = %v, want nil", result.ExitCode)
	}
}

func TestRunCheckReportsAMissingCommandAsNotExecuted(t *testing.T) {
	runner := newRunner()
	result := runner.RunCheck(context.Background(), domain.CheckDefinition{Name: "empty", Required: true}, t.TempDir())
	if result.Outcome != domain.OutcomeNotExecuted || result.Reason != "not_configured" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunCheckTruncatesOutputToTheLast64KiB(t *testing.T) {
	runner := newRunner()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "big.bin"), bytes.Repeat([]byte("x"), 70000), 0o600); err != nil {
		t.Fatal(err)
	}
	check := domain.CheckDefinition{
		Name:           "chatty",
		CommandLinux:   bigOutputCommand("linux"),
		CommandWindows: bigOutputCommand("windows"),
		Required:       true,
		TimeoutSeconds: 60,
	}
	result := runner.RunCheck(context.Background(), check, workspace)
	if result.Outcome != domain.OutcomePassed {
		t.Fatalf("outcome = %q (%s)", result.Outcome, result.OutputExcerpt)
	}
	if len(result.OutputExcerpt) > validate.MaxExcerptBytes {
		t.Fatalf("excerpt = %d bytes, want <= %d", len(result.OutputExcerpt), validate.MaxExcerptBytes)
	}
	if len(result.OutputExcerpt) < validate.MaxExcerptBytes-1024 {
		t.Fatalf("excerpt = %d bytes, want the full 64 KiB tail", len(result.OutputExcerpt))
	}
}

func TestRunCheckNeverKeepsARegisteredSecret(t *testing.T) {
	redactor := logging.NewRedactor()
	const secret = "validate-secret-value"
	redactor.Register(secret)
	runner := validate.New(validate.Options{Redactor: redactor})

	t.Setenv("RCA_VALIDATE_TOKEN", secret)
	check := domain.CheckDefinition{
		Name:           "leak",
		CommandLinux:   "echo $RCA_VALIDATE_TOKEN",
		CommandWindows: "echo %RCA_VALIDATE_TOKEN%",
		Required:       true,
	}
	result := runner.RunCheck(context.Background(), check, t.TempDir())
	if strings.Contains(result.OutputExcerpt, secret) {
		t.Fatalf("the excerpt contains the secret: %q", result.OutputExcerpt)
	}
}

func TestRunChecksStopsRecordingAfterCancel(t *testing.T) {
	runner := newRunner()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	checks := []domain.CheckDefinition{
		{Name: "one", CommandLinux: "exit 0", CommandWindows: "exit 0", Required: true},
		{Name: "two", CommandLinux: "exit 0", CommandWindows: "exit 0", Required: false},
	}
	results := runner.RunChecks(ctx, checks, t.TempDir())
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	for _, result := range results {
		if result.Outcome != domain.OutcomeNotExecuted {
			t.Fatalf("%s outcome = %q, want not_executed", result.CheckName, result.Outcome)
		}
	}
}

func slowCommand(goos string) string {
	if goos == "windows" {
		return "ping -n 6 127.0.0.1 >NUL"
	}
	return "sleep 5"
}

func bigOutputCommand(goos string) string {
	if goos == "windows" {
		return "type big.bin"
	}
	return "cat big.bin"
}

func TestSlowCommandsMatchTheHost(t *testing.T) {
	if runtime.GOOS == "windows" && !strings.Contains(slowCommand("windows"), "ping") {
		t.Fatal("windows slow command is wrong")
	}
	if runtime.GOOS != "windows" && !strings.Contains(slowCommand("linux"), "sleep") {
		t.Fatal("unix slow command is wrong")
	}
}

// TestCommandSelectionFollowsTheConfiguredOS proves the Windows/Linux command
// pair is chosen from the runner's OS, not from the host the test runs on (T140).
func TestCommandSelectionFollowsTheConfiguredOS(t *testing.T) {
	check := domain.CheckDefinition{
		Name:           "selected",
		CommandLinux:   "echo linux",
		CommandWindows: "echo windows",
		Required:       true,
	}
	for _, testCase := range []struct {
		goos string
		want string
	}{
		{"linux", "echo linux"},
		{"windows", "echo windows"},
	} {
		result := validate.New(validate.Options{Goos: testCase.goos}).RunCheck(context.Background(), check, "")
		if result.Command != testCase.want {
			t.Fatalf("goos %s: command = %q, want %q", testCase.goos, result.Command, testCase.want)
		}
		if result.Outcome != domain.OutcomeNotExecuted || result.Reason != "no_workspace" {
			t.Fatalf("goos %s: outcome = %s/%s", testCase.goos, result.Outcome, result.Reason)
		}
	}
}

// TestRunCheckInAPathWithSpacesAndUnicode executes a real command from a
// workspace whose path would break naive shell quoting.
func TestRunCheckInAPathWithSpacesAndUnicode(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "space dir", "کارگاه")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	command := "echo ok"
	if runtime.GOOS == "windows" {
		command = "echo ok"
	}
	result := validate.New(validate.Options{}).RunCheck(context.Background(), domain.CheckDefinition{
		Name:           "path",
		CommandLinux:   command,
		CommandWindows: command,
		Required:       true,
	}, workspace)
	if result.Outcome != domain.OutcomePassed {
		t.Fatalf("outcome = %s (%s), excerpt = %q", result.Outcome, result.Reason, result.OutputExcerpt)
	}
}
