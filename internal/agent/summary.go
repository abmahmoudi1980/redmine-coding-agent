package agent

import (
	"strings"
)

// ResultMarker is the heading the prompt requires as the last section.
const ResultMarker = "## RESULT"

// ParseResultBlock extracts the `## RESULT` block from an agent transcript.
// The last well-formed block wins, because the block is required to be the
// final section. A missing or malformed block returns ok=false and never an
// error: absence is recorded as a warning, not as a verdict (contract §6).
func ParseResultBlock(text string) (Summary, bool) {
	index := strings.LastIndex(text, ResultMarker)
	if index < 0 {
		return Summary{}, false
	}
	lines := strings.Split(text[index+len(ResultMarker):], "\n")

	var summary Summary
	seen := false
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "status":
			summary.Status = strings.ToLower(strings.TrimSpace(value))
			seen = true
		case "files":
			summary.Files = parseFileList(value)
			seen = true
		case "checks_run":
			summary.ChecksRun = cleanClaim(value)
			seen = true
		case "blockers":
			summary.Blockers = cleanClaim(value)
			seen = true
		}
	}
	switch summary.Status {
	case "done", "blocked":
	default:
		return Summary{}, false
	}
	if !seen {
		return Summary{}, false
	}
	return summary, true
}

// Blocked reports whether the agent asked for an answer instead of guessing
// (FR-030). The claim is only meaningful when the block parsed.
func (s Summary) Blocked() bool {
	return s.Status == "blocked" && strings.TrimSpace(s.Blockers) != ""
}

// ReportedStatus is the claim recorded on the invocation, empty when the block
// is absent or malformed. It must never be used as a result (FR-025).
func (s Summary) ReportedStatus() string {
	return s.Status
}

// Classify turns the observed process facts plus the parsed claim into the
// invocation status. The claim can downgrade a clean exit to blocked; it can
// never upgrade a non-zero exit to success.
func Classify(exitCode int, summary Summary, parsed bool) Status {
	if exitCode != 0 {
		return StatusFailed
	}
	if parsed && summary.Blocked() {
		return StatusBlocked
	}
	return StatusSucceeded
}

func parseFileList(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "(none)" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		cleaned := strings.TrimSpace(part)
		if cleaned == "" || cleaned == "(none)" {
			continue
		}
		files = append(files, cleaned)
	}
	if len(files) == 0 {
		return nil
	}
	return files
}

func cleanClaim(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "(none)" {
		return ""
	}
	return trimmed
}
