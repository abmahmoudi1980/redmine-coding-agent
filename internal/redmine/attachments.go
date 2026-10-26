package redmine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// MaxAttachmentBytes is the largest single attachment the worker will fetch.
const MaxAttachmentBytes int64 = 25 << 20

// AttachmentResult records what happened to one attachment. A failure is
// recorded per file instead of silently omitting it (FR-007).
type AttachmentResult struct {
	Attachment Attachment
	Path       string
	Available  bool
	Reason     string
}

// DownloadAttachments fetches every attachment into dir. A single file's
// failure never aborts the rest; the caller persists the unavailability.
func (c *Client) DownloadAttachments(ctx context.Context, attachments []Attachment, dir string) ([]AttachmentResult, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("redmine: attachment destination is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("redmine: create attachment dir: %w", err)
	}

	results := make([]AttachmentResult, 0, len(attachments))
	for _, attachment := range attachments {
		result := AttachmentResult{Attachment: attachment}
		path, err := c.DownloadAttachment(ctx, attachment, dir)
		if err != nil {
			result.Reason = err.Error()
		} else {
			result.Path = path
			result.Available = true
		}
		results = append(results, result)
	}
	return results, nil
}

// DownloadAttachment fetches one attachment and returns the written path.
func (c *Client) DownloadAttachment(ctx context.Context, attachment Attachment, dir string) (string, error) {
	if strings.TrimSpace(attachment.ContentURL) == "" {
		return "", errors.New("attachment has no content URL")
	}
	if attachment.Filesize > MaxAttachmentBytes {
		return "", fmt.Errorf("attachment is larger than the %d byte limit", MaxAttachmentBytes)
	}

	endpoint, err := c.resolveURL(attachment.ContentURL)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	key, err := c.apiKey.Resolve()
	if err != nil {
		return "", fmt.Errorf("resolve api key: %w", err)
	}
	if c.redactor != nil {
		c.redactor.Register(key)
	}
	request.Header.Set("X-Redmine-API-Key", key)

	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch attachment: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch attachment: HTTP %d", response.StatusCode)
	}

	name := safeFilename(attachment)
	destination := filepath.Join(dir, name)
	temporary, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		return "", fmt.Errorf("create attachment file: %w", err)
	}
	temporaryPath := temporary.Name()
	written, copyErr := io.Copy(temporary, io.LimitReader(response.Body, MaxAttachmentBytes+1))
	closeErr := temporary.Close()
	if copyErr != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("write attachment: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("close attachment: %w", closeErr)
	}
	if written > MaxAttachmentBytes {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("attachment exceeds the %d byte limit", MaxAttachmentBytes)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("store attachment: %w", err)
	}
	return destination, nil
}

func (c *Client) resolveURL(reference string) (string, error) {
	base, err := url.Parse(c.baseURL + "/")
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	relative, err := url.Parse(reference)
	if err != nil {
		return "", fmt.Errorf("invalid attachment URL: %w", err)
	}
	return base.ResolveReference(relative).String(), nil
}

func safeFilename(attachment Attachment) string {
	name := strings.ReplaceAll(strings.TrimSpace(attachment.Filename), "\\", "/")
	name = filepath.Base(name)
	if name == "" || name == "." || name == ".." {
		name = fmt.Sprintf("attachment-%d", attachment.ID)
	}
	return name
}
