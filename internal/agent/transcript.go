package agent

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// maxActivityEvents bounds the activity list handed to the UI.
	maxActivityEvents = 500
	// maxTranscriptParseBytes bounds how much of a transcript is parsed.
	maxTranscriptParseBytes = 4 << 20
)

// Transcript is the best-effort reading of one raw stdout capture. The file is
// always the source of truth; this is a courtesy for the activity feed.
type Transcript struct {
	Activity         []ActivityEvent
	PolicyViolations []string
	Warnings         []string
}

var policyViolationTypes = map[string]bool{
	"permission_denied":  true,
	"permission.denied":  true,
	"permission_error":   true,
	"policy_violation":   true,
	"policy.violation":   true,
	"sandbox_violation":  true,
	"security_violation": true,
}

// ParseTranscript reads newline-delimited JSON tolerantly: unknown shapes are
// skipped, and a malformed line never aborts a run (contract §5).
func ParseTranscript(text string) Transcript {
	if len(text) > maxTranscriptParseBytes {
		text = text[len(text)-maxTranscriptParseBytes:]
	}

	var transcript Transcript
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event == nil {
			continue
		}

		kind := stringField(event, "type")
		if kind == "" {
			kind = stringField(event, "event")
		}
		message := firstNonEmpty(
			stringField(event, "message"),
			stringField(event, "text"),
			stringField(event, "summary"),
			describeToolEvent(event),
		)

		if policyViolationTypes[strings.ToLower(kind)] || truthy(event["permission_denied"]) {
			violation := strings.TrimSpace(message)
			if violation == "" {
				violation = "an event reported a denied permission"
			}
			if path := stringField(event, "path"); path != "" && !strings.Contains(violation, path) {
				violation += " (" + path + ")"
			}
			transcript.PolicyViolations = append(transcript.PolicyViolations, violation)
			continue
		}

		if message == "" {
			continue
		}
		if len(transcript.Activity) >= maxActivityEvents {
			transcript.Activity = transcript.Activity[:maxActivityEvents-1]
		}
		transcript.Activity = append(transcript.Activity, ActivityEvent{
			At:      parseEventTime(stringField(event, "at"), stringField(event, "timestamp")),
			Kind:    kind,
			Message: message,
			Data:    event,
		})
	}
	return transcript
}

func describeToolEvent(event map[string]any) string {
	name := stringField(event, "name")
	if name == "" {
		return ""
	}
	path := stringField(event, "path")
	switch {
	case path != "":
		return name + ": " + path
	case stringField(event, "command") != "":
		return name + ": " + stringField(event, "command")
	default:
		return name
	}
}

func stringField(event map[string]any, key string) string {
	value, ok := event[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func truthy(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

func parseEventTime(candidates ...string) time.Time {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339Nano, candidate); err == nil {
			return parsed.UTC()
		}
	}
	return time.Now().UTC()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// tailBuffer is a bounded writer that keeps the last n bytes. Agent output is
// captured for parsing and excerpts without unbounded memory growth.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newTailBuffer(limit int) *tailBuffer {
	if limit <= 0 {
		limit = 64 << 10
	}
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(p)
	b.data = append(b.data, p...)
	if overflow := len(b.data) - b.limit; overflow > 0 {
		b.data = append(b.data[:0], b.data[overflow:]...)
	}
	return written, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// redactingWriter scrubs every chunk before it reaches a durable sink (A9).
type redactingWriter struct {
	target io.Writer
	redact func(string) string
}

func newRedactingWriter(target io.Writer, redact func(string) string) io.Writer {
	if redact == nil {
		return target
	}
	return &redactingWriter{target: target, redact: redact}
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	scrubbed := w.redact(string(p))
	if _, err := io.WriteString(w.target, scrubbed); err != nil {
		return 0, err
	}
	return len(p), nil
}
