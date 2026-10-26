package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/api"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// runRun is the headless equivalent of confirming a preview in the browser
// (contracts/cli.md §run). It still refuses to start an agent without explicit
// confirmation (FR-008).
func runRun(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	issue := flags.Int("issue", 0, "Redmine issue number (required)")
	repository := flags.Int64("repository", 0, "repository id when the project is ambiguous")
	yes := flags.Bool("yes", false, "skip the interactive confirmation")
	wait := flags.Bool("wait", true, "stream progress until the run reaches a terminal state")
	format := flags.String("format", "human", "human or json")
	if err := flags.Parse(args); err != nil {
		return exitWith(exitUsage, "run: %v", err)
	}
	if len(flags.Args()) > 0 {
		return exitWith(exitUsage, "run: unexpected argument %q", flags.Args()[0])
	}
	if *issue < 1 {
		return exitWith(exitUsage, "run: --issue is required and must be a positive integer")
	}
	if *repository < 0 {
		return exitWith(exitUsage, "run: --repository must be a positive integer")
	}
	jsonMode, err := outputMode(*format, globals.json)
	if err != nil {
		return exitWith(exitUsage, "run: %v", err)
	}
	if jsonMode {
		*wait = true
	}
	level, err := parseLogLevel(globals.logLevel)
	if err != nil {
		return exitWith(exitUsage, "run: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker, err := newApp(ctx, appOptions{
		configPath: globals.configPath,
		stderr:     os.Stderr,
		logLevel:   level,
		seed:       true,
		withRunner: true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()

	output := newCLIOutput(worker.redactor, jsonMode)
	defer func() { _ = output.Close() }()

	// Validate the issue before any run row or workspace exists (FR-002).
	issuePayload, err := worker.redmine.GetIssue(ctx, *issue)
	if err != nil {
		failure := redmine.FailureFor(err, *issue)
		return writeFailure(output, failure, exitCodeForFailure(failure.Code))
	}

	mapping, err := runner.Resolve(ctx, worker.store, issuePayload.Project.Identifier, *repository)
	if err != nil {
		var ambiguous *runner.AmbiguousMappingError
		if errors.As(err, &ambiguous) {
			return writeAmbiguousFailure(output, ambiguous)
		}
		failure := runner.FailureForResolve(err)
		return writeFailure(output, failure, exitCodeForFailure(failure.Code))
	}

	snapshot := runner.BuildSnapshot(issuePayload, time.Now().UTC())
	if !jsonMode {
		renderPreview(output, issuePayload, snapshot, mapping)
	}

	if !*yes {
		if jsonMode {
			return exitWith(exitUsage, "run: --yes is required when --format json is in use")
		}
		confirmed, promptErr := confirmRun(os.Stdin, output)
		if promptErr != nil {
			return promptErr
		}
		if !confirmed {
			output.human("nothing was started\n")
			return nil
		}
	}

	if !*wait {
		return submitRun(ctx, worker.config, output, "/api/runs", map[string]any{
			"issueId":      *issue,
			"repositoryId": *repository,
			"confirm":      true,
		})
	}

	created, err := worker.runner.Create(ctx, runner.CreateRequest{
		IssueID:      *issue,
		RepositoryID: *repository,
		Confirm:      true,
		RequestedBy:  operatorName(),
	})
	if err != nil {
		return writeRunStartError(output, err)
	}

	output.human("run %s started for issue #%d\n", created.PublicID, created.IssueID)

	final, followErr := followRun(ctx, worker, output, created)
	if followErr != nil && !errors.Is(followErr, context.Canceled) {
		return exitWith(exitInternal, "run: %v", followErr)
	}
	if jsonMode {
		if err := output.document(worker.documents().RunDocumentFor(ctx, final)); err != nil {
			return exitWith(exitInternal, "run: %v", err)
		}
	}
	return exitForRun(final)
}

// outputMode resolves --format and --json into one binary decision.
func outputMode(format string, jsonFlag bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "human":
		return jsonFlag, nil
	case "json":
		return true, nil
	default:
		return false, fmt.Errorf("--format must be human or json")
	}
}

func renderPreview(out *cliOutput, issue *redmine.Issue, snapshot domain.IssueSnapshot, mapping domain.RepositoryMapping) {
	out.human("Issue #%d: %s\n", issue.ID, issue.Subject)
	out.human("  project:  %s (%s)\n", issue.Project.Name, issue.Project.Identifier)
	out.human("  status:   %s / %s\n", issue.Status.Name, issue.Tracker.Name)
	if issue.AssignedTo != nil && issue.AssignedTo.Name != "" {
		out.human("  assignee: %s\n", issue.AssignedTo.Name)
	}
	out.human("  repository: %s (%s)\n", mapping.RepositoryPath, mapping.DisplayName)
	out.human("  base branch: %s\n", mapping.BaseBranch)
	if strings.TrimSpace(snapshot.AcceptanceCriteria) == "" {
		out.human("  acceptance criteria: none found (warning)\n")
	} else {
		out.human("  acceptance criteria: found (%s)\n", snapshot.CriteriaSource)
	}
	if len(snapshot.Attachments) > 0 {
		unavailable := 0
		for _, attachment := range snapshot.Attachments {
			if !attachment.Available {
				unavailable++
			}
		}
		out.human("  attachments: %d (%d unavailable)\n", len(snapshot.Attachments), unavailable)
	}
}

// confirmRun asks the operator for the one word that may start an agent.
func confirmRun(stdin io.Reader, out *cliOutput) (bool, error) {
	info, err := os.Stdin.Stat()
	if err == nil && info.Mode()&os.ModeCharDevice == 0 {
		return false, exitWith(exitUsage, "run: explicit confirmation required (--yes); nothing was started")
	}
	_ = stdin
	out.human("Proceed with this run? [y/N]: ")
	reader := bufio.NewReader(stdin)
	line, readErr := reader.ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, exitWith(exitUsage, "run: read confirmation: %v", readErr)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// followRun polls the durable timeline so the CLI needs no live hub, prints
// every event it has not seen, and returns the terminal run.
func followRun(ctx context.Context, worker *app, out *cliOutput, run *domain.Run) (*domain.Run, error) {
	var lastSeq int64
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := drainEvents(ctx, worker, out, run, &lastSeq); err != nil {
			return run, err
		}
		latest, err := worker.store.GetRun(ctx, run.ID)
		if err != nil {
			return run, err
		}
		run = latest
		if domain.IsTerminal(run.State) {
			// The terminal transition and its event commit in one transaction;
			// one more drain makes sure the CLI printed it before exiting.
			_ = drainEvents(ctx, worker, out, run, &lastSeq)
			renderRunEnd(out, run, worker)
			return run, nil
		}

		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-ticker.C:
		}
	}
}

func drainEvents(ctx context.Context, worker *app, out *cliOutput, run *domain.Run, lastSeq *int64) error {
	events, err := worker.store.EventsAfter(ctx, run.ID, *lastSeq, 200)
	if err != nil {
		return err
	}
	for _, event := range events {
		*lastSeq = event.Seq
		if out.json {
			payload, err := api.EventPayload(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out.out, "%s\n", payload); err != nil {
				return err
			}
			continue
		}
		prefix := string(event.Type)
		switch event.Severity {
		case domain.SeverityWarn:
			prefix = "warn"
		case domain.SeverityError:
			prefix = "error"
		}
		out.human("[%-8s] %s\n", prefix, event.Message)
	}
	return nil
}

func renderRunEnd(out *cliOutput, run *domain.Run, worker *app) {
	out.human("run %s ended %s at %s\n", run.PublicID, run.State, run.Stage)
	if run.Branch != "" {
		out.human("  branch: %s\n", run.Branch)
	}
	if run.CommitSHA != "" {
		out.human("  commit: %s\n", run.CommitSHA)
	}
	if run.PushedAt != nil {
		out.human("  pushed: %s\n", run.PushedAt.UTC().Format(time.RFC3339))
	}
	if worker != nil && worker.store != nil {
		if report, err := worker.store.GetReport(context.Background(), run.ID); err == nil {
			if report.MergeRequestURL != "" {
				out.human("  merge request: %s\n", report.MergeRequestURL)
			}
			out.human("  report delivery: %s\n", report.DeliveryStatus)
		}
	}
	if run.FailureCode != "" {
		out.human("  failure: %s at %s\n", run.FailureCode, run.Stage)
		if run.FailureMessage != "" {
			out.human("  detail: %s\n", run.FailureMessage)
		}
		if remediation := domain.DefaultRemediation(run.FailureCode); remediation != "" {
			out.human("  next: %s\n", remediation)
		}
	}
}

// writeAmbiguousFailure names every candidate so the operator can choose with
// --repository instead of guessing (FR-015).
func writeAmbiguousFailure(out *cliOutput, ambiguous *runner.AmbiguousMappingError) error {
	candidates := make([]map[string]any, 0, len(ambiguous.Candidates))
	for _, candidate := range ambiguous.Candidates {
		candidates = append(candidates, map[string]any{
			"repositoryId": candidate.ID,
			"path":         candidate.RepositoryPath,
			"displayName":  candidate.DisplayName,
			"baseBranch":   candidate.BaseBranch,
		})
	}
	failure := runner.FailureForResolve(ambiguous)
	if out.json {
		_ = out.document(map[string]any{
			"code":        string(failure.Code),
			"message":     failure.Message,
			"stage":       string(failure.Stage),
			"remediation": failure.Remediation,
			"candidates":  candidates,
		})
	}
	out.human("refused: %s: %s\n", failure.Code, failure.Message)
	for _, candidate := range ambiguous.Candidates {
		out.human("  repository %d: %s (%s), base %s\n",
			candidate.ID, candidate.RepositoryPath, candidate.DisplayName, candidate.BaseBranch)
	}
	out.human("  next: %s\n", failure.Remediation)
	return &exitError{code: exitBlocked, err: fmt.Errorf("%s: %s", failure.Code, failure.Message)}
}

// writeFailure prints a domain failure in the selected format and returns the
// documented exit code (contracts/cli.md guarantee 4).
func writeFailure(out *cliOutput, failure domain.Failure, code int) error {
	if out.json {
		_ = out.document(map[string]any{
			"code":        string(failure.Code),
			"message":     failure.Message,
			"stage":       string(failure.Stage),
			"remediation": failure.Remediation,
		})
	}
	out.human("refused: %s at %s: %s\n", failure.Code, failure.Stage, failure.Message)
	if failure.Remediation != "" {
		out.human("  next: %s\n", failure.Remediation)
	}
	return &exitError{code: code, err: fmt.Errorf("%s: %s", failure.Code, failure.Message)}
}

func writeRunStartError(out *cliOutput, err error) error {
	var active *runner.ActiveRunError
	var failure domain.Failure
	switch {
	case errors.As(err, &active):
		payload := map[string]any{
			"code":    "active_run_exists",
			"message": active.Error(),
		}
		if active.Run != nil {
			payload["runId"] = active.Run.PublicID
			payload["stage"] = string(active.Run.Stage)
			payload["state"] = string(active.Run.State)
		}
		if out.json {
			_ = out.document(payload)
		}
		out.human("refused: issue already has an active run")
		if active.Run != nil {
			out.human(" %s in stage %s\n", active.Run.PublicID, active.Run.Stage)
		} else {
			out.human("\n")
		}
		return exitWith(exitRunFailed, "%v", err)
	case errors.Is(err, store.ErrConcurrencyLimit):
		return exitWith(exitRunFailed, "run: the configured concurrency limit is reached")
	case errors.As(err, &failure):
		return writeFailure(out, failure, exitCodeForFailure(failure.Code))
	case errors.Is(err, runner.ErrConfirmRequired):
		return exitWith(exitUsage, "run: explicit confirmation required; nothing was started")
	default:
		return exitWith(exitInternal, "run: %v", err)
	}
}
