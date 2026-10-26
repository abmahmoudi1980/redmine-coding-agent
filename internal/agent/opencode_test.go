package agent_test

import (
	"os"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
)

func TestParseGoldenTranscript(t *testing.T) {
	raw, err := os.ReadFile("testdata/transcript-golden.jsonl")
	if err != nil {
		t.Fatalf("read golden transcript: %v", err)
	}

	transcript := agent.ParseTranscript(string(raw))

	if len(transcript.PolicyViolations) != 1 {
		t.Fatalf("policy violations = %+v", transcript.PolicyViolations)
	}
	if !strings.Contains(transcript.PolicyViolations[0], "/etc/shadow") {
		t.Fatalf("violation does not name the path: %q", transcript.PolicyViolations[0])
	}

	messages := make([]string, 0, len(transcript.Activity))
	for _, event := range transcript.Activity {
		messages = append(messages, event.Message)
	}
	joined := strings.Join(messages, "\n")
	for _, want := range []string{
		"Reading the invoice code before changing it.",
		"read: internal/invoice/total.go",
		"bash: go test ./internal/invoice/...",
		"Rounded half up and kept the existing cases.",
		"edit: internal/invoice/total.go",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("activity is missing %q\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "/etc/shadow") {
		t.Error("a denied operation must not appear as ordinary activity")
	}
	if strings.Contains(joined, "future_shape") || strings.Contains(joined, "not json at all") {
		t.Errorf("unknown shapes leaked into activity: %s", joined)
	}
	if len(transcript.Activity) != 5 {
		t.Fatalf("activity count = %d, want 5: %s", len(transcript.Activity), joined)
	}

	summary, ok := agent.ParseResultBlock(string(raw))
	if !ok {
		t.Fatal("the golden transcript ends with a parsable RESULT block")
	}
	if summary.Status != "done" || len(summary.Files) != 2 || summary.Blockers != "" {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestParseTranscriptToleratesGarbageAndEmptyInput(t *testing.T) {
	for _, input := range []string{"", "\n\n", "{", "[]", `{"type":"tool"}`, strings.Repeat("x", 10000)} {
		transcript := agent.ParseTranscript(input)
		if len(transcript.PolicyViolations) != 0 {
			t.Fatalf("%q produced violations: %+v", input, transcript.PolicyViolations)
		}
	}
}

func TestParseTranscriptStopsAtTheActivityCap(t *testing.T) {
	line := `{"type":"tool","name":"read","path":"a.go"}` + "\n"
	transcript := agent.ParseTranscript(strings.Repeat(line, 700))
	if len(transcript.Activity) > 500 {
		t.Fatalf("activity count = %d, want <= 500", len(transcript.Activity))
	}
}
