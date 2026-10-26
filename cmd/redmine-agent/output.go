package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

// Exit codes for terminal run states mirror contracts/cli.md §Exit codes.
const (
	exitValidation  = 5
	exitRunFailed   = 6
	exitBlocked     = 7
	exitCancelled   = 8
	exitInterrupted = 9
)

// globalFlags are documented as valid on every command.
type globalFlags struct {
	configPath string
	logLevel   string
	json       bool
	noColor    bool
}

func registerGlobalFlags(flags *flag.FlagSet) *globalFlags {
	globals := &globalFlags{}
	flags.StringVar(&globals.configPath, "config", "", "path to config.yaml (defaults to $"+config.EnvConfigPath+" or config.yaml)")
	flags.BoolVar(&globals.json, "json", false, "machine-readable output on stdout")
	flags.StringVar(&globals.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	flags.BoolVar(&globals.noColor, "no-color", false, "disable ANSI styling")
	return globals
}

// cliOutput keeps human prose off stdout when a machine is reading stdout, and
// puts every byte through the redactor (contracts/cli.md guarantee 3).
type cliOutput struct {
	out      io.Writer
	err      io.Writer
	json     bool
	flushOut func() error
	flushErr func() error
}

func newCLIOutput(redactor *logging.Redactor, jsonMode bool) *cliOutput {
	if redactor == nil {
		redactor = logging.NewRedactor()
	}
	stdout := redactor.Writer(os.Stdout)
	stderr := redactor.Writer(os.Stderr)
	return &cliOutput{
		out:      stdout,
		err:      stderr,
		json:     jsonMode,
		flushOut: flusherFor(stdout),
		flushErr: flusherFor(stderr),
	}
}

func flusherFor(writer io.Writer) func() error {
	if flusher, ok := writer.(interface{ Flush() error }); ok {
		return flusher.Flush
	}
	return func() error { return nil }
}

func (c *cliOutput) Close() error {
	if c == nil {
		return nil
	}
	if c.flushOut != nil {
		_ = c.flushOut()
	}
	if c.flushErr != nil {
		return c.flushErr()
	}
	return nil
}

// human writes operator-facing prose to stderr when --json owns stdout.
func (c *cliOutput) human(format string, args ...any) {
	writer := c.out
	if c.json {
		writer = c.err
	}
	fmt.Fprintf(writer, format, args...)
}

// document writes exactly one JSON document per line on stdout.
func (c *cliOutput) document(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.out, "%s\n", encoded)
	return err
}

// exitCodeForRun mirrors a terminal state onto the documented process status.
func exitCodeForRun(run *domain.Run) int {
	if run == nil {
		return exitInternal
	}
	switch run.State {
	case domain.StateCompleted:
		return exitOK
	case domain.StateBlocked:
		return exitBlocked
	case domain.StateCancelled:
		return exitCancelled
	case domain.StateInterrupted:
		return exitInterrupted
	case domain.StateFailed:
		return exitCodeForFailure(run.FailureCode)
	default:
		return exitOK
	}
}

// exitCodeForFailure maps a failure code onto a process status even before a
// run exists (a preview or a refused start).
func exitCodeForFailure(code domain.FailureCode) int {
	switch code {
	case domain.FailConnectivityRedmine, domain.FailConnectivityGitlab,
		domain.FailConnectivityRepository, domain.FailAgentUnavailable:
		return exitDependency
	case domain.FailValidationFailed:
		return exitValidation
	case domain.FailNoRepositoryMapping, domain.FailRepositoryAmbiguous, domain.FailAgentBlocked:
		return exitBlocked
	case domain.FailCancelled:
		return exitCancelled
	case domain.FailInterrupted:
		return exitInterrupted
	default:
		return exitRunFailed
	}
}

// exitForRun turns a terminal run into the process error that carries its
// status, keeping the message useful for a human reading stderr.
func exitForRun(run *domain.Run) error {
	code := exitCodeForRun(run)
	if code == exitOK {
		return nil
	}
	reason := run.FailureMessage
	if reason == "" {
		reason = string(run.State)
	}
	return &exitError{
		code: code,
		err:  fmt.Errorf("run %s ended %s%s", run.PublicID, run.State, reasonSuffix(run.FailureCode, reason)),
	}
}

func reasonSuffix(code domain.FailureCode, reason string) string {
	if code == "" {
		return ": " + reason
	}
	return fmt.Sprintf(": %s (%s)", reason, code)
}

// splitArgs separates flags from positionals so documented forms like
// `runs show <run-id> --json` work with the stdlib flag package, which stops at
// the first non-flag argument.
func splitArgs(flags *flag.FlagSet, args []string) (flagArgs, positional []string) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			return flagArgs, positional
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if equals := strings.Index(name, "="); equals >= 0 {
			continue
		}
		defined := flags.Lookup(name)
		if defined == nil {
			continue
		}
		if boolean, ok := defined.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if index+1 < len(args) {
			index++
			flagArgs = append(flagArgs, args[index])
		}
	}
	return flagArgs, positional
}
