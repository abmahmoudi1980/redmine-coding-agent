package runner

import (
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
)

func TestSummariseActivityTurnsToolsIntoReadableLines(t *testing.T) {
	lines := summariseActivity([]agent.ActivityEvent{
		{Kind: "tool", Message: "read: src/orders/service.go"},
		{Kind: "tool", Message: "edit: src/orders/service.go"},
		{Kind: "tool", Message: "bash: go test ./..."},
		{Kind: "assistant", Message: "Looking at the rounding helper."},
	}, 50)

	want := []string{
		"Reading src/orders/service.go",
		"Editing src/orders/service.go",
		"Running go test ./...",
		"Looking at the rounding helper.",
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v", lines)
	}
	for index := range want {
		if lines[index] != want[index] {
			t.Errorf("line %d = %q, want %q", index, lines[index], want[index])
		}
	}
}

func TestSummariseActivityCoalescesRepeatsAndCapsTheFeed(t *testing.T) {
	events := make([]agent.ActivityEvent, 0, 30)
	for index := 0; index < 3; index++ {
		events = append(events, agent.ActivityEvent{Message: "read: a.go"})
	}
	for index := 0; index < 30; index++ {
		events = append(events, agent.ActivityEvent{Message: "read: file-" + string(rune('a'+index)) + ".go"})
	}

	lines := summariseActivity(events, 5)
	if lines[0] != "Reading a.go (×3)" {
		t.Fatalf("first line = %q, want the coalesced repeat", lines[0])
	}
	if len(lines) != 6 {
		t.Fatalf("lines = %d, want the cap plus the truncation marker: %v", len(lines), lines)
	}
	if lines[len(lines)-1] != "further agent activity is in the transcript" {
		t.Fatalf("last line = %q", lines[len(lines)-1])
	}
}

func TestSummariseActivitySkipsEmptyMessages(t *testing.T) {
	if lines := summariseActivity([]agent.ActivityEvent{{}, {Message: "   "}}, 10); len(lines) != 0 {
		t.Fatalf("lines = %v, want none", lines)
	}
}
