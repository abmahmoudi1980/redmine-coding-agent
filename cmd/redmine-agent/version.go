package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
)

// runVersion prints build identity. The JSON shape is the machine-readable
// counterpart of `GET /api/meta` (contracts/cli.md §version).
func runVersion(args []string) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	globals := registerGlobalFlags(flags)
	if err := flags.Parse(args); err != nil {
		return exitWith(exitUsage, "version: %v", err)
	}
	if flags.NArg() > 0 {
		return exitWith(exitUsage, "version: unexpected argument %q", flags.Arg(0))
	}

	document := map[string]any{
		"version":       version,
		"commit":        commit,
		"builtAt":       builtAt,
		"goVersion":     runtime.Version(),
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"schemaVersion": config.CurrentSchemaVersion,
	}
	if globals.json {
		output := newCLIOutput(nil, true)
		defer func() { _ = output.Close() }()
		return output.document(document)
	}
	fmt.Fprintln(os.Stdout, buildVersion())
	fmt.Fprintf(os.Stdout, "schema_version: %d\n", config.CurrentSchemaVersion)
	return nil
}
