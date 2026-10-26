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
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errUsage
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "version":
		fmt.Fprintln(os.Stdout, buildVersion())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usageText)
		return nil
	default:
		return fmt.Errorf("%w: %q", errUnknownCommand, args[0])
	}
}

var (
	errUsage          = errors.New("no command given, expected one of: serve, version, help")
	errUnknownCommand = errors.New("unknown command")
)

const usageText = `Usage: redmine-agent <command>

Commands:
  serve      Start the HTTP API and the run pipeline (long-running mode)
  version    Print the build version
  help       Print this message
`

func buildVersion() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s/%s)", version, commit, builtAt, runtime.GOOS, runtime.GOARCH)
}
