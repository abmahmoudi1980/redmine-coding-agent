package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// runRuns implements `runs list|show|checks|logs|report` from
// contracts/cli.md §runs. Every JSON document is the same one the API returns.
func runRuns(args []string) error {
	if len(args) == 0 {
		return exitWith(exitUsage, "runs: expected one of list, show, checks, logs, report")
	}
	switch args[0] {
	case "list":
		return runsList(args[1:])
	case "show":
		return runsShow(args[1:])
	case "checks":
		return runsChecks(args[1:])
	case "logs":
		return runsLogs(args[1:])
	case "report":
		return runsReport(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, runsUsage)
		return nil
	default:
		return exitWith(exitUsage, "runs: unknown subcommand %q", args[0])
	}
}

const runsUsage = `Usage: redmine-agent runs <subcommand> [flags]

Subcommands:
  list      history table (newest first)
  show      full audit view of one run
  checks    per-attempt validation results
  logs      redacted log tail (--tail N)
  report    the persisted report body
`

// runsSession opens the worker for read-only history commands.
func runsSession(globals *globalFlags, jsonMode bool) (*app, *cliOutput, error) {
	level, err := parseLogLevel(globals.logLevel)
	if err != nil {
		return nil, nil, exitWith(exitUsage, "runs: %v", err)
	}
	ctx := context.Background()
	worker, err := newApp(ctx, appOptions{
		configPath: globals.configPath,
		stderr:     os.Stderr,
		logLevel:   level,
		withRunner: true,
	})
	if err != nil {
		return nil, nil, err
	}
	return worker, newCLIOutput(worker.redactor, jsonMode), nil
}

func runsList(args []string) error {
	flags := flag.NewFlagSet("runs list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	state := flags.String("state", "", "filter by run state")
	issue := flags.Int("issue", 0, "filter by Redmine issue number")
	limit := flags.Int("limit", 25, "page size (1-100)")
	cursor := flags.String("cursor", "", "opaque cursor from the previous page")
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "runs list: %v", err)
	}
	if len(positional) > 0 {
		return exitWith(exitUsage, "runs list: unexpected argument %q", positional[0])
	}
	if *limit < 1 || *limit > 100 {
		return exitWith(exitUsage, "runs list: --limit must be between 1 and 100")
	}
	filter := store.RunFilter{IssueID: *issue, Limit: *limit, Cursor: strings.TrimSpace(*cursor)}
	if *state != "" {
		candidate := domain.RunState(*state)
		if !candidate.Valid() {
			return exitWith(exitUsage, "runs list: --state must be a known run state")
		}
		filter.State = candidate
	}

	worker, output, err := runsSession(globals, globals.json)
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()
	defer func() { _ = output.Close() }()

	runs, next, err := worker.store.ListRuns(context.Background(), filter)
	if err != nil {
		return exitWith(exitInternal, "runs list: %v", err)
	}
	if globals.json {
		return output.document(worker.documents().RunPageFor(context.Background(), runs, next))
	}
	renderRunTable(output, runs, next)
	return nil
}

func renderRunTable(out *cliOutput, runs []domain.Run, next string) {
	out.human("%-28s %-8s %-13s %-22s %-8s %s\n", "RUN", "ISSUE", "STATE", "STAGE", "ELAPSED", "STARTED")
	for index := range runs {
		run := runs[index]
		out.human("%-28s %-8d %-13s %-22s %-8s %s\n",
			run.PublicID,
			run.IssueID,
			run.State,
			run.Stage,
			durationLabel(run),
			run.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
		)
	}
	if len(runs) == 0 {
		out.human("no runs match that filter\n")
	}
	if next != "" {
		out.human("next cursor: %s\n", next)
	}
}

func durationLabel(run domain.Run) string {
	if run.StartedAt == nil {
		return "-"
	}
	end := run.EndedAt
	if end == nil {
		return "running"
	}
	if end.Before(*run.StartedAt) {
		return "-"
	}
	return end.Sub(*run.StartedAt).Round(time.Second).String()
}

func runsShow(args []string) error {
	flags := flag.NewFlagSet("runs show", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "runs show: %v", err)
	}
	if len(positional) != 1 {
		return exitWith(exitUsage, "runs show: expected exactly one run id")
	}

	worker, output, err := runsSession(globals, globals.json)
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()
	defer func() { _ = output.Close() }()

	ctx := context.Background()
	run, err := worker.store.GetRunByPublicID(ctx, positional[0])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return exitWith(exitRunFailed, "runs show: run %s does not exist", positional[0])
		}
		return exitWith(exitInternal, "runs show: %v", err)
	}
	if globals.json {
		return output.document(worker.documents().RunDetailFor(ctx, run))
	}
	renderRunDetail(output, worker, run)
	return nil
}

func renderRunDetail(out *cliOutput, worker *app, run *domain.Run) {
	out.human("run:      %s (attempt %d)\n", run.PublicID, attemptLabel(worker, run))
	out.human("issue:    #%d %s\n", run.IssueID, run.IssueSubject)
	out.human("state:    %s at %s\n", run.State, run.Stage)
	if run.Branch != "" {
		out.human("branch:   %s from %s\n", run.Branch, run.BaseBranch)
	}
	if run.BaseRevision != "" {
		out.human("base rev: %s\n", run.BaseRevision)
	}
	out.human("created:  %s\n", run.CreatedAt.UTC().Format("2006-01-02 15:04:05Z"))
	if run.EndedAt != nil {
		out.human("ended:    %s\n", run.EndedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if run.CommitSHA != "" {
		out.human("commit:   %s\n", run.CommitSHA)
	}
	if run.PushedAt != nil {
		out.human("pushed:   %s\n", run.PushedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if run.FailureCode != "" {
		out.human("failure:  %s at %s: %s\n", run.FailureCode, run.Stage, run.FailureMessage)
		if remediation := domain.DefaultRemediation(run.FailureCode); remediation != "" {
			out.human("next:     %s\n", remediation)
		}
	}

	ctx := context.Background()
	if workspace, err := worker.store.GetWorkspaceByRun(ctx, run.ID); err == nil {
		out.human("\nworkspace: %s\n", workspace.Path)
		if workspace.HasUnpushedChanges {
			out.human("  unpushed changes remain; the workspace will not be pruned\n")
		}
		for _, path := range workspace.ChangedPaths {
			out.human("  %s\n", path)
		}
	}
	if invocations, err := worker.store.ListInvocations(ctx, run.ID); err == nil && len(invocations) > 0 {
		out.human("\ninvocations:\n")
		for _, invocation := range invocations {
			exitCode := "-"
			if invocation.ExitCode != nil {
				exitCode = fmt.Sprint(*invocation.ExitCode)
			}
			out.human("  #%d %-8s %-10s exit=%s timed_out=%t reported=%s\n",
				invocation.AttemptNo, invocation.Kind, invocation.Status, exitCode,
				invocation.TimedOut, invocation.ReportedStatus)
		}
	}
	if attempts, err := worker.store.CheckAttempts(ctx, run.ID); err == nil && len(attempts) > 0 {
		out.human("\nchecks:\n")
		for _, attempt := range attempts {
			out.human("  attempt %d (invocation %s)\n", attempt.AttemptNo, attempt.InvocationStatus)
			for _, result := range attempt.Results {
				required := "optional"
				if result.Required {
					required = "required"
				}
				exitCode := "-"
				if result.ExitCode != nil {
					exitCode = fmt.Sprint(*result.ExitCode)
				}
				out.human("    %-9s %-16s %-8s exit=%-4s %dms\n",
					result.Outcome, result.CheckName, required, exitCode, result.DurationMS)
				if result.Outcome != domain.OutcomePassed && result.OutputExcerpt != "" {
					out.human("      %s\n", firstLines(result.OutputExcerpt, 3))
				}
			}
		}
	}
	if report, err := worker.store.GetReport(ctx, run.ID); err == nil {
		out.human("\nreport delivery: %s\n", report.DeliveryStatus)
		if report.MergeRequestURL != "" {
			out.human("merge request:   %s\n", report.MergeRequestURL)
		}
		if report.DeliveryError != "" {
			out.human("delivery error:  %s\n", report.DeliveryError)
		}
	}
}

func attemptLabel(worker *app, run *domain.Run) int {
	number := 1
	current := run
	for steps := 0; steps < 50 && current.PreviousRunID != nil; steps++ {
		previous, err := worker.store.GetRun(context.Background(), *current.PreviousRunID)
		if err != nil {
			break
		}
		number++
		current = previous
	}
	return number
}

func firstLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, " | ")
}

func runsChecks(args []string) error {
	flags := flag.NewFlagSet("runs checks", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "runs checks: %v", err)
	}
	if len(positional) != 1 {
		return exitWith(exitUsage, "runs checks: expected exactly one run id")
	}

	worker, output, err := runsSession(globals, globals.json)
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()
	defer func() { _ = output.Close() }()

	ctx := context.Background()
	run, err := worker.store.GetRunByPublicID(ctx, positional[0])
	if err != nil {
		return exitWith(exitRunFailed, "runs checks: run %s does not exist", positional[0])
	}
	attempts, err := worker.store.CheckAttempts(ctx, run.ID)
	if err != nil {
		return exitWith(exitInternal, "runs checks: %v", err)
	}
	if attempts == nil {
		attempts = []domain.CheckAttempt{}
	}
	if globals.json {
		return output.document(map[string]any{"attempts": attempts})
	}
	for _, attempt := range attempts {
		output.human("attempt %d (invocation %s, all required passed: %t)\n",
			attempt.AttemptNo, attempt.InvocationStatus, attempt.AllRequiredPassed)
		for _, result := range attempt.Results {
			exitCode := "-"
			if result.ExitCode != nil {
				exitCode = fmt.Sprint(*result.ExitCode)
			}
			output.human("  %-13s %-16s exit=%-4s %dms\n", result.Outcome, result.CheckName, exitCode, result.DurationMS)
		}
	}
	if len(attempts) == 0 {
		output.human("no checks have run yet\n")
	}
	return nil
}

func runsLogs(args []string) error {
	flags := flag.NewFlagSet("runs logs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	tail := flags.Int("tail", 500, "number of trailing lines")
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "runs logs: %v", err)
	}
	if len(positional) != 1 {
		return exitWith(exitUsage, "runs logs: expected exactly one run id")
	}
	if *tail < 1 {
		return exitWith(exitUsage, "runs logs: --tail must be a positive integer")
	}
	if *tail > 5000 {
		*tail = 5000
	}

	worker, output, err := runsSession(globals, globals.json)
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()
	defer func() { _ = output.Close() }()

	path := worker.logger.RunLogPath(positional[0])
	// #nosec G703 -- RunLogPath sanitizes the run id into one file name under log_dir.
	if _, statErr := os.Stat(path); statErr != nil {
		return exitWith(exitRunFailed, "runs logs: the log for %s is unavailable (pruned or never written)", positional[0])
	}
	lines, err := tailFileLines(path, *tail)
	if err != nil {
		return exitWith(exitInternal, "runs logs: %v", err)
	}
	if globals.json {
		return output.document(map[string]any{"lines": lines})
	}
	for _, line := range lines {
		fmt.Fprintln(output.out, line)
	}
	return nil
}

// tailFileLines reads at most n trailing lines from a file.
func tailFileLines(path string, n int) ([]string, error) {
	data, err := os.ReadFile(path) // #nosec G304 G703 -- path is the configured log dir plus a sanitized run id.
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func runsReport(args []string) error {
	flags := flag.NewFlagSet("runs report", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "runs report: %v", err)
	}
	if len(positional) != 1 {
		return exitWith(exitUsage, "runs report: expected exactly one run id")
	}

	worker, output, err := runsSession(globals, globals.json)
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()
	defer func() { _ = output.Close() }()

	ctx := context.Background()
	run, err := worker.store.GetRunByPublicID(ctx, positional[0])
	if err != nil {
		return exitWith(exitRunFailed, "runs report: run %s does not exist", positional[0])
	}
	report, err := worker.store.GetReport(ctx, run.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return exitWith(exitRunFailed, "runs report: run %s has no report yet", positional[0])
		}
		return exitWith(exitInternal, "runs report: %v", err)
	}
	if globals.json {
		return output.document(map[string]any{"report": report})
	}
	if _, err := io.WriteString(output.out, report.BodyMD); err != nil {
		return exitWith(exitInternal, "runs report: %v", err)
	}
	if !strings.HasSuffix(report.BodyMD, "\n") {
		fmt.Fprintln(output.out)
	}
	return nil
}
