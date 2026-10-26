package api

import (
	"errors"
	"strconv"
	"strings"
)

// ErrInvalidIssueID is returned before any upstream call (FR-002).
var ErrInvalidIssueID = errors.New("invalid issue id")

// ErrInvalidRepositoryID covers a malformed explicit repository choice.
var ErrInvalidRepositoryID = errors.New("invalid repository id")

// ParseIssueID accepts only the decimal form of a positive integer. A blank,
// signed, fractional, or non-numeric value is rejected without network access.
func ParseIssueID(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !allDigits(trimmed) {
		return 0, ErrInvalidIssueID
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value < 1 {
		return 0, ErrInvalidIssueID
	}
	return value, nil
}

// ParseRepositoryID accepts only a positive decimal repository id.
func ParseRepositoryID(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !allDigits(trimmed) {
		return 0, ErrInvalidRepositoryID
	}
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || value < 1 {
		return 0, ErrInvalidRepositoryID
	}
	return value, nil
}

func allDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return value != ""
}
