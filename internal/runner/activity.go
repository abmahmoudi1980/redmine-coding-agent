package runner

import (
	"fmt"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
)

// maxActivityLineLength bounds one feed line so a pathological tool argument
// cannot flood the timeline.
const maxActivityLineLength = 240

// summariseActivity turns raw agent tool events into a short, readable feed.
// Consecutive repeats collapse into one line with a count, which is what keeps
// a twenty-file read burst from becoming twenty identical entries (T102).
func summariseActivity(events []agent.ActivityEvent, limit int) []string {
	if limit <= 0 {
		limit = 50
	}

	type entry struct {
		message string
		count   int
	}
	entries := make([]entry, 0, len(events))
	for _, event := range events {
		line := activityLine(event)
		if line == "" {
			continue
		}
		if len(entries) > 0 && entries[len(entries)-1].message == line {
			entries[len(entries)-1].count++
			continue
		}
		entries = append(entries, entry{message: line, count: 1})
	}

	lines := make([]string, 0, limit+1)
	for _, item := range entries {
		if len(lines) >= limit {
			lines = append(lines, "further agent activity is in the transcript")
			return lines
		}
		if item.count > 1 {
			lines = append(lines, fmt.Sprintf("%s (×%d)", item.message, item.count))
			continue
		}
		lines = append(lines, item.message)
	}
	return lines
}

// activityLine renders one event: a verb for known tools, the message
// otherwise, truncated to a readable width.
func activityLine(event agent.ActivityEvent) string {
	message := strings.TrimSpace(event.Message)
	if message == "" {
		return ""
	}

	verb, subject, found := strings.Cut(message, ": ")
	if !found {
		return truncateActivity(message)
	}
	subject = strings.TrimSpace(subject)

	switch strings.ToLower(strings.TrimSpace(verb)) {
	case "read", "cat", "open", "view":
		return truncateActivity("Reading " + subject)
	case "edit", "write", "patch", "apply", "create", "update":
		return truncateActivity("Editing " + subject)
	case "bash", "shell", "command", "exec", "run", "terminal":
		return truncateActivity("Running " + subject)
	case "grep", "search", "glob", "find", "list", "ls":
		return truncateActivity("Searching " + subject)
	case "webfetch", "fetch", "http":
		return truncateActivity("Fetching " + subject)
	case "permission_denied", "permission error", "policy_violation", "sandbox_violation":
		return truncateActivity("Blocked by policy: " + subject)
	default:
		return truncateActivity(message)
	}
}

func truncateActivity(text string) string {
	if len(text) <= maxActivityLineLength {
		return text
	}
	return text[:maxActivityLineLength-1] + "…"
}
