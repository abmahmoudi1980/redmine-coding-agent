package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/proc"
)

// openCodeName is the adapter identifier stored on the run and in config.
const openCodeName = "opencode"

// probeTimeout bounds the version probe; it never touches the network.
const probeTimeout = 15 * time.Second

// OpenCode runs the OpenCode command-line agent (contract §8). Every claim it
// returns is untrusted: validation is the worker's own work.
type OpenCode struct {
	config Config
}

// NewOpenCode builds the adapter. It resolves no secret and starts nothing.
func NewOpenCode(cfg Config) (*OpenCode, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("agent: opencode command is required")
	}
	if strings.TrimSpace(cfg.WorkspaceRoot) == "" {
		return nil, errors.New("agent: workspace root is required")
	}
	return &OpenCode{config: cfg}, nil
}

// Name is the stable adapter identifier.
func (o *OpenCode) Name() string { return openCodeName }

// Probe proves the executable exists and can report a version. It runs in a
// throwaway directory so a probe can never mutate a workspace (contract A1).
func (o *OpenCode) Probe(ctx context.Context, cfg Config) (ProbeResult, error) {
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		command = o.config.Command
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return ProbeResult{Available: false, Detail: fmt.Sprintf("%s is not on PATH", command)}, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	scratch, err := os.MkdirTemp("", "redmine-agent-probe-")
	if err != nil {
		return ProbeResult{Available: true, Detail: "the executable was found; the probe directory could not be created"}, nil
	}
	defer os.RemoveAll(scratch)

	// #nosec G204 -- the path came from LookPath of the configured command.
	probe := exec.CommandContext(probeCtx, path, "--version")
	probe.Dir = scratch
	probe.Env = []string{"PATH=" + os.Getenv("PATH")}
	if runtime.GOOS == "windows" {
		probe.Env = append(probe.Env, "SystemRoot="+os.Getenv("SystemRoot"), "ComSpec="+os.Getenv("ComSpec"))
	}
	output, runErr := probe.CombinedOutput()
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return ProbeResult{Available: false, Detail: fmt.Sprintf("%s could not be started", command)}, nil
		}
	}
	version := firstLine(string(output))
	return ProbeResult{Available: true, Version: version, Detail: "the executable answered --version"}, nil
}

// Run performs one non-interactive invocation in the run's workspace.
func (o *OpenCode) Run(ctx context.Context, req Request) (Result, error) {
	started := time.Now().UTC()
	result := Result{StartedAt: started, Status: StatusFailed, Transcript: req.Prompt.Transcript}

	layout, err := ValidateRequest(req, o.config.WorkspaceRoot)
	if err != nil {
		result.EndedAt = time.Now().UTC()
		return result, err
	}
	environment, err := BuildEnvironment(req, layout)
	if err != nil {
		result.EndedAt = time.Now().UTC()
		return result, err
	}

	if strings.TrimSpace(req.Prompt.Text) == "" {
		result.EndedAt = time.Now().UTC()
		return result, errors.New("agent: prompt text is required")
	}
	transcriptPath := strings.TrimSpace(req.Prompt.Transcript)
	if transcriptPath == "" {
		result.EndedAt = time.Now().UTC()
		return result, errors.New("agent: transcript path is required")
	}
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		result.EndedAt = time.Now().UTC()
		return result, fmt.Errorf("agent: create transcript directory: %w", err)
	}

	stdoutFile, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		result.EndedAt = time.Now().UTC()
		return result, fmt.Errorf("agent: open transcript: %w", err)
	}
	defer stdoutFile.Close()

	stderrPath := errorTranscriptPath(transcriptPath)
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		result.EndedAt = time.Now().UTC()
		return result, fmt.Errorf("agent: open error transcript: %w", err)
	}
	defer stderrFile.Close()

	redact := o.redactor()
	captured := newTailBuffer(maxTranscriptParseBytes)
	stdout := io.MultiWriter(newRedactingWriter(stdoutFile, redact), captured)
	stderrBuffer := newTailBuffer(64 << 10)
	stderr := io.MultiWriter(newRedactingWriter(stderrFile, redact), stderrBuffer)

	args := o.arguments(req)
	handle, err := proc.Start(ctx, proc.Options{
		Path:   o.config.Command,
		Args:   args,
		Dir:    req.WorkspaceDir,
		Env:    environment,
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil {
		result.EndedAt = time.Now().UTC()
		result.ExitCode = -1
		return result, err
	}

	observed := handle.Wait()
	result.EndedAt = time.Now().UTC()
	result.ExitCode = observed.ExitCode
	result.TimedOut = observed.TimedOut

	transcript := ParseTranscript(captured.String())
	result.Activity = transcript.Activity
	result.PolicyViolations = transcript.PolicyViolations
	result.Error = transcript.Error

	summary, parsed := ParseResultBlock(captured.String())
	if parsed {
		result.Summary = summary
		result.ReportedStatus = summary.ReportedStatus()
	}

	switch {
	case observed.Cancelled:
		result.Status = StatusCancelled
	case observed.TimedOut:
		result.Status = StatusTimedOut
	case len(result.PolicyViolations) > 0:
		result.Status = StatusFailed
	case observed.ExitCode != 0:
		result.Status = StatusFailed
	default:
		result.Status = Classify(observed.ExitCode, summary, parsed)
	}
	result.Transcript = transcriptPath

	if waitErr := handle.Err(); waitErr != nil && result.Status == StatusSucceeded {
		result.Status = StatusFailed
	}
	return result, nil
}

// arguments composes the documented invocation. The working directory, not a
// flag, is what scopes the agent (contract §8).
func (o *OpenCode) arguments(req Request) []string {
	title := fmt.Sprintf("Issue %d", req.IssueID)
	args := []string{"run", "--standalone", "--format", "json"}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(o.config.Model)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	agentName := strings.TrimSpace(req.AgentName)
	if agentName == "" {
		agentName = strings.TrimSpace(o.config.AgentName)
	}
	if agentName != "" {
		args = append(args, "--agent", agentName)
	}
	args = append(args, "--title", title, req.Prompt.Text)
	return args
}

func (o *OpenCode) redactor() func(string) string {
	if o.config.Redactor == nil {
		return nil
	}
	return o.config.Redactor.Redact
}

func errorTranscriptPath(transcriptPath string) string {
	if strings.HasSuffix(transcriptPath, ".jsonl") {
		return strings.TrimSuffix(transcriptPath, ".jsonl") + ".err"
	}
	return transcriptPath + ".err"
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimSpace(line)
}
