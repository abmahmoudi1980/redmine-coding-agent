package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretRef is an indirection to a secret value: `env:NAME` or `file:/path`.
// The resolved value is never stored on the struct, and every formatting verb
// renders only the reference, so a SecretRef can be logged safely.
type SecretRef struct {
	reference string
}

// NewSecretRef builds a reference from its textual form.
func NewSecretRef(reference string) SecretRef {
	return SecretRef{reference: strings.TrimSpace(reference)}
}

// Reference returns the textual reference (e.g. "env:REDMINE_API_KEY").
func (s SecretRef) Reference() string { return s.reference }

// IsSet reports whether a reference was configured at all.
func (s SecretRef) IsSet() bool { return s.reference != "" }

// String returns the reference, never the value. This is what makes %v, %s,
// %+v, slog, and encoding/json safe.
func (s SecretRef) String() string { return s.reference }

// GoString keeps %#v from exposing anything but the reference.
func (s SecretRef) GoString() string { return "config.SecretRef(" + s.reference + ")" }

// MarshalYAML writes the reference back, so config round-trips are lossless
// and a persisted profile never contains a value.
func (s SecretRef) MarshalYAML() (any, error) {
	if s.reference == "" {
		return "", nil
	}
	return s.reference, nil
}

// UnmarshalYAML accepts only a string reference.
func (s *SecretRef) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("secret reference must be a string: %w", err)
	}
	s.reference = strings.TrimSpace(raw)
	return nil
}

// MarshalJSON writes the reference, never the value.
func (s SecretRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.reference)
}

// UnmarshalJSON accepts only a string reference.
func (s *SecretRef) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("secret reference must be a string: %w", err)
	}
	s.reference = strings.TrimSpace(raw)
	return nil
}

// Validate checks the shape of the reference without resolving it, so config
// can be validated without the secret being present.
func (s SecretRef) Validate() error {
	if s.reference == "" {
		return fmt.Errorf("secret reference is required")
	}
	scheme, rest, ok := strings.Cut(s.reference, ":")
	if !ok || strings.TrimSpace(rest) == "" {
		return fmt.Errorf("secret reference %q must be env:NAME or file:/path", s.reference)
	}
	switch scheme {
	case "env", "file":
		return nil
	default:
		return fmt.Errorf("secret reference %q uses unsupported scheme %q (want env: or file:)", s.reference, scheme)
	}
}

// Resolve reads the secret lazily. Errors name the reference only, never a
// value or a partially-read value.
func (s SecretRef) Resolve() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	scheme, rest, _ := strings.Cut(s.reference, ":")
	switch scheme {
	case "env":
		value, ok := os.LookupEnv(rest)
		if !ok || value == "" {
			return "", fmt.Errorf("secret reference %q is not set", s.reference)
		}
		return value, nil
	case "file":
		raw, err := os.ReadFile(rest)
		if err != nil {
			return "", fmt.Errorf("secret reference %q could not be read: %w", s.reference, err)
		}
		value := strings.TrimRight(string(raw), "\r\n")
		if value == "" {
			return "", fmt.Errorf("secret reference %q is empty", s.reference)
		}
		return value, nil
	default:
		return "", fmt.Errorf("secret reference %q uses unsupported scheme %q", s.reference, scheme)
	}
}
