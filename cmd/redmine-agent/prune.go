package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/runner"
)

// runPrune is manual retention (contracts/cli.md §version / prune). Nothing
// schedules it; a scheduler or an operator runs it deliberately.
func runPrune(args []string) error {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	olderThan := flags.String("older-than", "", "delete runs older than this window (30d, 12h, 2w); defaults to data.retention_days")
	dryRun := flags.Bool("dry-run", false, "report what would be deleted without deleting it")
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "prune: %v", err)
	}
	if len(positional) > 0 {
		return exitWith(exitUsage, "prune: unexpected argument %q", positional[0])
	}

	level, err := parseLogLevel(globals.logLevel)
	if err != nil {
		return exitWith(exitUsage, "prune: %v", err)
	}
	ctx := context.Background()
	worker, err := newApp(ctx, appOptions{
		configPath: globals.configPath,
		stderr:     os.Stderr,
		logLevel:   level,
		withRunner: true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()

	output := newCLIOutput(worker.redactor, globals.json)
	defer func() { _ = output.Close() }()

	window, err := parseRetention(*olderThan, worker.config.Data.RetentionDays)
	if err != nil {
		return exitWith(exitUsage, "prune: %v", err)
	}

	report, err := worker.runner.Prune(ctx, runner.CleanupOptions{
		OlderThan: window,
		DryRun:    *dryRun,
	})
	if err != nil {
		return exitWith(exitInternal, "prune: %v", err)
	}

	if globals.json {
		return output.document(report)
	}
	mode := "deleted"
	count := report.Deleted
	if *dryRun {
		mode = "would delete"
		count = report.Candidates
	}
	output.human("%s %d of %d candidate run(s) older than %s\n",
		mode, count, report.Candidates, window)
	for _, skip := range report.Skipped {
		output.human("  kept %s: %s\n", skip.RunID, skip.Reason)
	}
	output.human("no branch and no merge request was deleted\n")
	return nil
}

// parseRetention accepts the documented day/week/hour forms plus a bare number
// of days. Go's duration parser has no day unit, so this is deliberate.
func parseRetention(value string, fallbackDays int) (time.Duration, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		if fallbackDays <= 0 {
			fallbackDays = 7
		}
		return time.Duration(fallbackDays) * 24 * time.Hour, nil
	}
	unit := trimmed[len(trimmed)-1]
	amount := trimmed
	multiplier := 24 * time.Hour
	switch unit {
	case 'd':
		amount = trimmed[:len(trimmed)-1]
	case 'w':
		amount = trimmed[:len(trimmed)-1]
		multiplier = 7 * 24 * time.Hour
	case 'h':
		amount = trimmed[:len(trimmed)-1]
		multiplier = time.Hour
	case 'm':
		amount = trimmed[:len(trimmed)-1]
		multiplier = time.Minute
	default:
		if _, err := strconv.Atoi(trimmed); err != nil {
			return 0, fmt.Errorf("--older-than must look like 30d, 2w, 12h, or a number of days")
		}
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(amount))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("--older-than must be a positive number")
	}
	return time.Duration(parsed) * multiplier, nil
}
