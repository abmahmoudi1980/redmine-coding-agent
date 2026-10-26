// Package logging provides structured logs with mandatory secret redaction.
package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

const replacement = "***"

// Redactor holds resolved secret values and scrubs them from any text or
// stream. Registering a value more than once is harmless.
type Redactor struct {
	mu       sync.RWMutex
	variants []string
	seen     map[string]struct{}
}

// NewRedactor returns an empty registry.
func NewRedactor() *Redactor {
	return &Redactor{seen: make(map[string]struct{})}
}

// Register adds secret values. Every textual variant an encoder might produce
// is registered too: raw, URL-encoded, path-encoded, and JSON-escaped.
func (r *Redactor) Register(values ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]struct{})
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		for _, variant := range variantsOf(value) {
			if _, ok := r.seen[variant]; ok {
				continue
			}
			r.seen[variant] = struct{}{}
			r.variants = append(r.variants, variant)
		}
	}
	// Longest first, so a secret that contains another is scrubbed whole.
	sort.Slice(r.variants, func(i, j int) bool {
		if len(r.variants[i]) != len(r.variants[j]) {
			return len(r.variants[i]) > len(r.variants[j])
		}
		return r.variants[i] < r.variants[j]
	})
}

// Count reports how many distinct variants are registered.
func (r *Redactor) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.variants)
}

// Redact replaces every registered variant with ***.
func (r *Redactor) Redact(text string) string {
	if r == nil || text == "" {
		return text
	}
	r.mu.RLock()
	variants := r.variants
	r.mu.RUnlock()
	for _, variant := range variants {
		if strings.Contains(text, variant) {
			text = strings.ReplaceAll(text, variant, replacement)
		}
	}
	return text
}

// Writer returns an io.Writer that scrubs complete lines before they reach dst.
// A trailing partial line is held until Flush/Close, so a secret split across
// two writes cannot slip through.
func (r *Redactor) Writer(dst io.Writer) io.Writer {
	if dst == nil {
		return nil
	}
	return &redactingWriter{redactor: r, dst: dst}
}

func variantsOf(value string) []string {
	out := []string{value}
	if escaped := url.QueryEscape(value); escaped != value {
		out = append(out, escaped)
	}
	if escaped := url.PathEscape(value); escaped != value {
		out = append(out, escaped)
	}
	if encoded, err := json.Marshal(value); err == nil {
		trimmed := strings.Trim(string(encoded), `"`)
		if trimmed != value {
			out = append(out, trimmed)
		}
	}
	return out
}

type redactingWriter struct {
	redactor *Redactor
	dst      io.Writer
	mu       sync.Mutex
	buf      []byte
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		index := bytes.IndexByte(w.buf, '\n')
		if index < 0 {
			break
		}
		line := w.buf[:index+1]
		if _, err := io.WriteString(w.dst, w.redactor.Redact(string(line))); err != nil {
			return 0, err
		}
		w.buf = w.buf[index+1:]
	}
	return len(p), nil
}

// Flush writes any buffered partial line, redacted.
func (w *redactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(w.dst, w.redactor.Redact(string(w.buf)))
	w.buf = nil
	return err
}

// Close flushes buffered text and closes dst when it is a closer.
func (w *redactingWriter) Close() error {
	err := w.Flush()
	if closer, ok := w.dst.(io.Closer); ok {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}
