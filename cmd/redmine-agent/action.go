package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/url"

	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// runCancel records a cancellation and, with --wait, follows the run until the
// process tree and the terminal state are durable (contracts/cli.md §cancel).
func runCancel(args []string) error {
	flags := flag.NewFlagSet("cancel", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	wait := flags.Bool("wait", false, "wait until the process tree is gone and the run is terminal")
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "cancel: %v", err)
	}
	if len(positional) != 1 {
		return exitWith(exitUsage, "cancel: expected exactly one run id")
	}

	worker, output, err := runsSession(globals, globals.json)
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()
	defer func() { _ = output.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, changed, err := worker.runner.Cancel(ctx, positional[0])
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return exitWith(exitRunFailed, "cancel: run %s does not exist", positional[0])
		case errors.Is(err, runner.ErrRunNotCancellable):
			return exitWith(exitRunFailed, "cancel: run %s has already finished", positional[0])
		default:
			return exitWith(exitInternal, "cancel: %v", err)
		}
	}
	if globals.json {
		_ = output.document(worker.documents().RunDocumentFor(ctx, run))
	}
	if changed {
		output.human("cancel requested for %s\n", run.PublicID)
	} else {
		output.human("cancellation was already recorded for %s\n", run.PublicID)
	}

	if !*wait {
		return nil
	}
	final, followErr := followRun(ctx, worker, output, run)
	if followErr != nil && !errors.Is(followErr, context.Canceled) {
		return exitWith(exitInternal, "cancel: %v", followErr)
	}
	if globals.json {
		if err := output.document(worker.documents().RunDocumentFor(ctx, final)); err != nil {
			return exitWith(exitInternal, "cancel: %v", err)
		}
	}
	return exitForRun(final)
}

// runRetry starts a new attempt linked to a finished one. A retry still needs
// an explicit --yes: the previous attempt may have been cancelled for a reason.
func runRetry(args []string) error {
	flags := flag.NewFlagSet("retry", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	yes := flags.Bool("yes", false, "confirm the new attempt")
	wait := flags.Bool("wait", true, "stream progress until the run reaches a terminal state")
	format := flags.String("format", "human", "human or json")
	flagArgs, positional := splitArgs(flags, args)
	if err := flags.Parse(flagArgs); err != nil {
		return exitWith(exitUsage, "retry: %v", err)
	}
	if len(positional) != 1 {
		return exitWith(exitUsage, "retry: expected exactly one run id")
	}
	if !*yes {
		return exitWith(exitUsage, "retry: explicit confirmation required (--yes); nothing was started")
	}
	jsonMode, err := outputMode(*format, globals.json)
	if err != nil {
		return exitWith(exitUsage, "retry: %v", err)
	}

	worker, output, err := runsSession(globals, jsonMode)
	if err != nil {
		return err
	}
	defer func() { _ = worker.close() }()
	defer func() { _ = output.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !*wait {
		return submitRun(ctx, worker.config, output, "/api/runs/"+url.PathEscape(positional[0])+"/retry", map[string]any{})
	}

	created, err := worker.runner.Retry(ctx, runner.RetryRequest{
		PublicID:    positional[0],
		RequestedBy: operatorName(),
	})
	if err != nil {
		return writeRunStartError(output, err)
	}

	output.human("retry %s started for issue #%d (previous %s)\n", created.PublicID, created.IssueID, positional[0])

	final, followErr := followRun(ctx, worker, output, created)
	if followErr != nil && !errors.Is(followErr, context.Canceled) {
		return exitWith(exitInternal, "retry: %v", followErr)
	}
	if jsonMode {
		if err := output.document(worker.documents().RunDocumentFor(ctx, final)); err != nil {
			return exitWith(exitInternal, "retry: %v", err)
		}
	}
	return exitForRun(final)
}
