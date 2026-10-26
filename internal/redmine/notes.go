package redmine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// PostNote adds one journal note to an issue (FR-045).
func (c *Client) PostNote(ctx context.Context, issueID int, notes string) error {
	if issueID <= 0 {
		return fmt.Errorf("redmine: issue id %d is not a positive integer", issueID)
	}
	if strings.TrimSpace(notes) == "" {
		return errors.New("redmine: a note body is required")
	}

	payload, err := json.Marshal(map[string]any{"issue": map[string]string{"notes": notes}})
	if err != nil {
		return fmt.Errorf("redmine: encode note: %w", err)
	}
	endpoint := fmt.Sprintf("%s/issues/%d.json", c.baseURL, issueID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("redmine: build note request: %w", err)
	}

	key, err := c.apiKey.Resolve()
	if err != nil {
		return fmt.Errorf("redmine: %w", err)
	}
	if c.redactor != nil {
		c.redactor.Register(key)
	}
	request.Header.Set("X-Redmine-API-Key", key)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: fmt.Sprintf("post note on issue %d", issueID), Err: err}
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	c.logRequest(fmt.Sprintf("PUT /issues/%d.json", issueID), response.StatusCode, time.Since(started))
	if readErr != nil {
		return &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: fmt.Sprintf("read note response for issue %d", issueID), Err: readErr}
	}

	switch response.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: issue %d", ErrIssueNotFound, issueID)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: posting a note on issue %d was refused (HTTP %d)", ErrIssueInaccessible, issueID, response.StatusCode)
	default:
		detail := strings.TrimSpace(string(body))
		if len(detail) > 512 {
			detail = detail[:512]
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   fmt.Sprintf("post note on issue %d", issueID),
			Err:  fmt.Errorf("HTTP %d: %s", response.StatusCode, detail),
		}
	}
}

// NoteContains reports whether any journal note on the issue already carries
// marker. It is how a repeated report for the same outcome is skipped instead
// of commented twice (FR-045).
func (c *Client) NoteContains(ctx context.Context, issueID int, marker string) (bool, error) {
	if strings.TrimSpace(marker) == "" {
		return false, errors.New("redmine: a marker is required")
	}
	issue, err := c.GetIssue(ctx, issueID)
	if err != nil {
		return false, err
	}
	for _, journal := range issue.Journals {
		if strings.Contains(journal.Notes, marker) {
			return true, nil
		}
	}
	return false, nil
}
