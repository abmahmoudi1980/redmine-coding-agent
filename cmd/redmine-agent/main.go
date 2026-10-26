package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "redmine-agent: %v\n", err)
		var code *exitError
		if errors.As(err, &code) {
			os.Exit(code.code)
		}
		os.Exit(exitInternal)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return &exitError{code: exitUsage, err: errUsage}
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "run":
		return runRun(args[1:])
	case "runs":
		return runRuns(args[1:])
	case "cancel":
		return runCancel(args[1:])
	case "retry":
		return runRetry(args[1:])
	case "prune":
		return runPrune(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "version":
		return runVersion(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usageText)
		return nil
	default:
		return &exitError{code: exitUsage, err: fmt.Errorf("%w: %q", errUnknownCommand, args[0])}
	}
}

var (
	errUsage          = errors.New("no command given, expected one of: serve, run, runs, cancel, retry, prune, doctor, version, help")
	errUnknownCommand = errors.New("unknown command")
)

const usageText = `Usage: redmine-agent <command> [flags]

Commands:
  serve               Start the HTTP API and the run pipeline (long-running mode)
  run                 Start one issue headlessly (requires --issue and --yes)
  runs                list | show | checks | logs | report one run
  cancel              Ask an active run to stop
  retry               Start a new attempt linked to a finished run
  prune               Delete run directories and rows older than --older-than
  doctor              Verify configuration, credentials, and dependencies
  version             Print the build identity (--json for machines)
  help                Print this message

Global flags: --config <path> --json --log-level <level> --no-color
`

func buildVersion() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s/%s)", version, commit, builtAt, runtime.GOOS, runtime.GOARCH)
}
