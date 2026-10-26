package main

import (
	"flag"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

func newTestFlagSet() *flag.FlagSet {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.Bool("json", false, "")
	flags.Int("tail", 10, "")
	return flags
}

func TestExitCodeForRunMirrorsTerminalState(t *testing.T) {
	cases := []struct {
		name string
		run  domain.Run
		want int
	}{
		{"completed", domain.Run{State: domain.StateCompleted}, exitOK},
		{"validation", domain.Run{State: domain.StateFailed, FailureCode: domain.FailValidationFailed}, exitValidation},
		{"dependency redmine", domain.Run{State: domain.StateFailed, FailureCode: domain.FailConnectivityRedmine}, exitDependency},
		{"dependency gitlab", domain.Run{State: domain.StateFailed, FailureCode: domain.FailConnectivityGitlab}, exitDependency},
		{"dependency repository", domain.Run{State: domain.StateFailed, FailureCode: domain.FailConnectivityRepository}, exitDependency},
		{"agent unavailable", domain.Run{State: domain.StateFailed, FailureCode: domain.FailAgentUnavailable}, exitDependency},
		{"other failure", domain.Run{State: domain.StateFailed, FailureCode: domain.FailNoChanges}, exitRunFailed},
		{"blocked", domain.Run{State: domain.StateBlocked}, exitBlocked},
		{"cancelled", domain.Run{State: domain.StateCancelled}, exitCancelled},
		{"interrupted", domain.Run{State: domain.StateInterrupted}, exitInterrupted},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := exitCodeForRun(&testCase.run); got != testCase.want {
				t.Fatalf("exitCodeForRun(%s) = %d, want %d", testCase.name, got, testCase.want)
			}
		})
	}
	if exitCodeForRun(nil) != exitInternal {
		t.Fatal("a nil run must be an internal error")
	}
}

func TestParseRetention(t *testing.T) {
	cases := []struct {
		value      string
		fallback   int
		want       time.Duration
		shouldFail bool
	}{
		{value: "", fallback: 7, want: 7 * 24 * time.Hour},
		{value: "", fallback: 0, want: 7 * 24 * time.Hour},
		{value: "30d", want: 30 * 24 * time.Hour},
		{value: "2w", want: 14 * 24 * time.Hour},
		{value: "12h", want: 12 * time.Hour},
		{value: "45m", want: 45 * time.Minute},
		{value: "30", want: 30 * 24 * time.Hour},
		{value: "0d", shouldFail: true},
		{value: "-3d", shouldFail: true},
		{value: "soon", shouldFail: true},
	}
	for _, testCase := range cases {
		got, err := parseRetention(testCase.value, testCase.fallback)
		if testCase.shouldFail {
			if err == nil {
				t.Fatalf("parseRetention(%q) = %s, want an error", testCase.value, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseRetention(%q): %v", testCase.value, err)
		}
		if got != testCase.want {
			t.Fatalf("parseRetention(%q) = %s, want %s", testCase.value, got, testCase.want)
		}
	}
}

func TestSplitArgsAcceptsFlagsAfterPositionals(t *testing.T) {
	flags := newTestFlagSet()
	flagArgs, positional := splitArgs(flags, []string{"run-id", "--json", "--tail", "50"})
	if len(positional) != 1 || positional[0] != "run-id" {
		t.Fatalf("positional = %v", positional)
	}
	joined := ""
	for _, arg := range flagArgs {
		joined += arg + " "
	}
	if joined != "--json --tail 50 " {
		t.Fatalf("flagArgs = %q", joined)
	}
}
