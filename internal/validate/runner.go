// Package validate executes the repository's configured checks. It is the only
// writer of validation results: an agent's claim is never a verdict (FR-033).
package validate

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/proc"
)

// DefaultTimeoutSeconds matches the documented default for a configured check.
const DefaultTimeoutSeconds = 900

// MaxExcerptBytes is how much of a check's output is kept: the last 64 KiB.
const MaxExcerptBytes = 64 << 10

// Redactor scrubs a captured excerpt before it is persisted.
type Redactor interface {
	Redact(string) string
}

// Options configures a Runner.
type Options struct {
	Redactor Redactor
	Goos     string
}

// Runner executes checks sequentially in one workspace.
type Runner struct {
	redactor Redactor
	goos     string
}

// New builds a Runner bound to the host OS.
func New(opts Options) *Runner {
	goos := opts.Goos
	if goos == "" {
		goos = runtime.GOOS
	}
	return &Runner{redactor: opts.Redactor, goos: goos}
}

// RunCheck executes one configured command and classifies its outcome. It
// never returns an error: a failure to run is itself a recorded result.
func (r *Runner) RunCheck(ctx context.Context, check domain.CheckDefinition, workspace string) domain.ValidationResult {
	now := time.Now().UTC()
	result := domain.ValidationResult{
		CheckName:  check.Name,
		Command:    check.CommandFor(r.goos),
		Required:   check.Required,
		DurationMS: 0,
		ExecutedAt: &now,
	}

	if strings.TrimSpace(result.Command) == "" {
		result.Outcome = domain.OutcomeNotExecuted
		result.Reason = "not_configured"
		result.ExecutedAt = nil
		return result
	}
	if strings.TrimSpace(workspace) == "" {
		result.Outcome = domain.OutcomeNotExecuted
		result.Reason = "no_workspace"
		result.ExecutedAt = nil
		return result
	}

	timeout := time.Duration(check.TimeoutSeconds) * time.Second
	if check.TimeoutSeconds <= 0 {
		timeout = DefaultTimeoutSeconds * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output := newTailWriter(MaxExcerptBytes)
	var writeOutput io.Writer = output
	if r.redactor != nil {
		writeOutput = &redactingWriter{target: output, redact: r.redactor.Redact}
	}

	started := time.Now()
	handle, err := proc.Start(runCtx, proc.Options{
		Path:   shellPath(r.goos),
		Args:   shellArgs(r.goos, result.Command),
		Dir:    workspace,
		Env:    scrubbedEnvironment(),
		Stdout: writeOutput,
		Stderr: writeOutput,
	})
	if err != nil {
		result.Outcome = domain.OutcomeNotExecuted
		result.Reason = "not_started: " + err.Error()
		result.OutputExcerpt = r.scrub(output.String())
		result.DurationMS = time.Since(started).Milliseconds()
		return result
	}

	observed := handle.Wait()
	result.DurationMS = time.Since(started).Milliseconds()
	result.OutputExcerpt = r.scrub(output.String())

	switch {
	case observed.Cancelled && ctx.Err() != nil:
		result.Outcome = domain.OutcomeNotExecuted
		result.Reason = "cancelled"
		return result
	case observed.TimedOut:
		code := observed.ExitCode
		result.ExitCode = &code
		result.Outcome = domain.OutcomeFailed
		result.Reason = fmt.Sprintf("timeout after %s", timeout)
		return result
	default:
		code := observed.ExitCode
		result.ExitCode = &code
		if code == 0 {
			result.Outcome = domain.OutcomePassed
			return result
		}
		result.Outcome = domain.OutcomeFailed
		return result
	}
}

// RunChecks executes every check in the given order.
func (r *Runner) RunChecks(ctx context.Context, checks []domain.CheckDefinition, workspace string) []domain.ValidationResult {
	results := make([]domain.ValidationResult, 0, len(checks))
	for _, check := range checks {
		if ctx.Err() != nil {
			results = append(results, domain.ValidationResult{
				CheckName: check.Name,
				Command:   check.CommandFor(r.goos),
				Required:  check.Required,
				Outcome:   domain.OutcomeNotExecuted,
				Reason:    "cancelled",
			})
			continue
		}
		results = append(results, r.RunCheck(ctx, check, workspace))
	}
	return results
}

func (r *Runner) scrub(text string) string {
	if r.redactor == nil {
		return text
	}
	return r.redactor.Redact(text)
}

func shellPath(goos string) string {
	if goos == "windows" {
		return "cmd.exe"
	}
	return "sh"
}

func shellArgs(goos, command string) []string {
	if goos == "windows" {
		return []string{"/c", command}
	}
	return []string{"-c", command}
}

// scrubbedEnvironment keeps the variables a build or test command needs while
// removing anything that looks like a credential.
func scrubbedEnvironment() []string {
	inherited := os.Environ()
	kept := make([]string, 0, len(inherited))
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if credentialLike(key) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func credentialLike(key string) bool {
	normalized := strings.ToUpper(key)
	for _, marker := range []string{"TOKEN", "API_KEY", "APIKEY", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// tailWriter keeps the last n bytes written to it.
type tailWriter struct {
	limit int
	data  []byte
}

func newTailWriter(limit int) *tailWriter {
	return &tailWriter{limit: limit}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	if overflow := len(w.data) - w.limit; overflow > 0 {
		w.data = append(w.data[:0], w.data[overflow:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	return string(w.data)
}

type redactingWriter struct {
	target io.Writer
	redact func(string) string
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.target, w.redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
