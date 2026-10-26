package agent_test

import (
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
)

func TestParseResultBlockDone(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"assistant","text":"Writing the fix."}`,
		"",
		"## RESULT",
		"status: done",
		"files: internal/invoice/total.go, internal/invoice/total_test.go",
		"checks_run: go test ./internal/invoice/... - exit 0 observed by me",
		"blockers: (none)",
	}, "\n")

	summary, ok := agent.ParseResultBlock(transcript)
	if !ok {
		t.Fatal("a well-formed block must parse")
	}
	if summary.Status != "done" {
		t.Fatalf("status = %q", summary.Status)
	}
	if len(summary.Files) != 2 || summary.Files[0] != "internal/invoice/total.go" {
		t.Fatalf("files = %+v", summary.Files)
	}
	if !strings.Contains(summary.ChecksRun, "exit 0 observed by me") {
		t.Fatalf("checks_run = %q", summary.ChecksRun)
	}
	if summary.Blockers != "" {
		t.Fatalf("blockers = %q", summary.Blockers)
	}
	if got := agent.Classify(0, summary, ok); got != agent.StatusSucceeded {
		t.Fatalf("Classify(done) = %q, want succeeded", got)
	}
	if summary.ReportedStatus() != "done" {
		t.Fatalf("reported status = %q", summary.ReportedStatus())
	}
}

func TestParseResultBlockBlocked(t *testing.T) {
	transcript := strings.Join([]string{
		"## RESULT",
		"status: blocked",
		"files: (none)",
		"checks_run: (none)",
		"blockers: Which database should the migration target?",
	}, "\n")

	summary, ok := agent.ParseResultBlock(transcript)
	if !ok {
		t.Fatal("a blocked block must parse")
	}
	if !summary.Blocked() {
		t.Fatal("blocked with blockers must report Blocked")
	}
	if got := agent.Classify(0, summary, ok); got != agent.StatusBlocked {
		t.Fatalf("Classify(blocked) = %q, want blocked", got)
	}
}

func TestParseResultBlockBlockedWithoutBlockersIsNotBlocked(t *testing.T) {
	transcript := "## RESULT\nstatus: blocked\nblockers: (none)\n"
	summary, ok := agent.ParseResultBlock(transcript)
	if !ok {
		t.Fatal("the block is well-formed")
	}
	if summary.Blocked() {
		t.Fatal("blocked without blockers cannot be treated as blocked")
	}
	if got := agent.Classify(0, summary, ok); got != agent.StatusSucceeded {
		t.Fatalf("Classify = %q, want succeeded", got)
	}
}

func TestParseResultBlockMissingOrMalformedIsNotAFailure(t *testing.T) {
	cases := map[string]string{
		"missing":    "the agent said nothing in the required shape",
		"no status":  "## RESULT\nfiles: a.go\n",
		"bad status": "## RESULT\nstatus: maybe\n",
	}
	for name, transcript := range cases {
		t.Run(name, func(t *testing.T) {
			summary, ok := agent.ParseResultBlock(transcript)
			if ok {
				t.Fatalf("block must not parse: %+v", summary)
			}
			if summary.ReportedStatus() != "" {
				t.Fatalf("reported status = %q, want empty", summary.ReportedStatus())
			}
			if got := agent.Classify(0, summary, ok); got != agent.StatusSucceeded {
				t.Fatalf("Classify = %q, want succeeded (validation, not the claim, decides)", got)
			}
		})
	}
}

func TestClassifyNeverUpgradesANonZeroExit(t *testing.T) {
	summary := agent.Summary{Status: "done"}
	if got := agent.Classify(1, summary, true); got != agent.StatusFailed {
		t.Fatalf("Classify(exit 1, done) = %q, want failed", got)
	}
}

func TestParseResultBlockKeepsTheLastBlock(t *testing.T) {
	transcript := "## RESULT\nstatus: done\n\nlater\n\n## RESULT\nstatus: blocked\nblockers: need an answer\n"
	summary, ok := agent.ParseResultBlock(transcript)
	if !ok || !summary.Blocked() {
		t.Fatalf("summary = %+v, ok = %v", summary, ok)
	}
}
