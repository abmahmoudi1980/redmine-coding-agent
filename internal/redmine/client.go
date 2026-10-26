// Package redmine is the minimal REST client for the read paths the worker
// needs: one issue with its journals, relations, children, and attachments,
// plus attachment downloads.
package redmine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

// DefaultTimeout bounds one Redmine request when the caller sets none.
const DefaultTimeout = 30 * time.Second

// maxResponseBytes keeps a misbehaving upstream from exhausting memory.
const maxResponseBytes = 16 << 20

// ErrIssueNotFound and ErrIssueInaccessible distinguish the two 4xx cases the
// operator must see differently (FR-005).
var (
	ErrIssueNotFound     = errors.New("redmine: issue not found")
	ErrIssueInaccessible = errors.New("redmine: issue is not accessible with the configured credential")
)

// Options configures a Client.
type Options struct {
	BaseURL    string
	APIKey     config.SecretRef
	HTTPClient *http.Client
	Timeout    time.Duration
	Redactor   *logging.Redactor
	Logger     *slog.Logger
}

// Client talks to one Redmine instance with one credential.
type Client struct {
	baseURL  string
	apiKey   config.SecretRef
	http     *http.Client
	redactor *logging.Redactor
	logger   *slog.Logger
}

// New validates the shape of its inputs; it resolves no secret and makes no
// network call.
func New(opts Options) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, errors.New("redmine: base URL is required")
	}
	if err := opts.APIKey.Validate(); err != nil {
		return nil, fmt.Errorf("redmine: api_key: %w", err)
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL:  base,
		apiKey:   opts.APIKey,
		http:     httpClient,
		redactor: opts.Redactor,
		logger:   logger,
	}, nil
}

// BaseURL is the configured endpoint without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// GetIssue fetches one issue with everything the snapshot needs (FR-003).
func (c *Client) GetIssue(ctx context.Context, id int) (*Issue, error) {
	if id <= 0 {
		return nil, fmt.Errorf("redmine: issue id %d is not a positive integer", id)
	}

	endpoint := fmt.Sprintf("%s/issues/%d.json?include=journals,relations,children,attachments", c.baseURL, id)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("redmine: build request: %w", err)
	}

	key, err := c.apiKey.Resolve()
	if err != nil {
		return nil, fmt.Errorf("redmine: %w", err)
	}
	if c.redactor != nil {
		c.redactor.Register(key)
	}
	request.Header.Set("X-Redmine-API-Key", key)
	request.Header.Set("Accept", "application/json")

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: fmt.Sprintf("get issue %d", id), Err: err}
	}
	defer response.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	c.logRequest(fmt.Sprintf("/issues/%d.json", id), response.StatusCode, time.Since(started))
	if readErr != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: fmt.Sprintf("get issue %d", id), Err: readErr}
	}

	switch response.StatusCode {
	case http.StatusOK:
		// Continue below.
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: issue %d", ErrIssueNotFound, id)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: issue %d (HTTP %d)", ErrIssueInaccessible, id, response.StatusCode)
	default:
		return nil, &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   fmt.Sprintf("get issue %d", id),
			Err:  fmt.Errorf("unexpected HTTP status %d", response.StatusCode),
		}
	}

	var envelope struct {
		Issue json.RawMessage `json:"issue"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: fmt.Sprintf("decode issue %d", id), Err: err}
	}
	if len(envelope.Issue) == 0 {
		return nil, &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   fmt.Sprintf("decode issue %d", id),
			Err:  errors.New("response has no issue object"),
		}
	}

	var issue Issue
	if err := json.Unmarshal(envelope.Issue, &issue); err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: fmt.Sprintf("decode issue %d", id), Err: err}
	}
	if issue.ID == 0 {
		issue.ID = id
	}
	if err := c.resolveProjectIdentifier(ctx, &issue); err != nil {
		return nil, err
	}
	issue.RawJSON = string(envelope.Issue)
	return &issue, nil
}

// resolveProjectIdentifier fills issue.Project.Identifier when the issue
// payload omits it. Redmine's issue representation ships only the project id
// and name, so the identifier that maps to a repository is read with one extra
// GET rather than guessed from the id or the name.
func (c *Client) resolveProjectIdentifier(ctx context.Context, issue *Issue) error {
	if issue == nil {
		return errors.New("redmine: issue is required")
	}
	if strings.TrimSpace(issue.Project.Identifier) != "" {
		return nil
	}
	projectID := strings.TrimSpace(issue.Project.ID.String())
	if projectID == "" {
		return &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   fmt.Sprintf("resolve project for issue %d", issue.ID),
			Err:  errors.New("issue has no project id and no project identifier"),
		}
	}
	project, err := c.GetProject(ctx, projectID)
	if err != nil {
		return &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   fmt.Sprintf("resolve project %s for issue %d", projectID, issue.ID),
			Err:  err,
		}
	}
	identifier := strings.TrimSpace(project.Identifier)
	if identifier == "" {
		return &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   fmt.Sprintf("resolve project %s for issue %d", projectID, issue.ID),
			Err:  errors.New("project has no identifier"),
		}
	}
	issue.Project.Identifier = identifier
	if strings.TrimSpace(issue.Project.Name) == "" {
		issue.Project.Name = project.Name
	}
	return nil
}

// Probe proves the configured credential can read Redmine. It performs one
// authenticated GET and mutates nothing, which is what the health and doctor
// checks need (FR-058).
func (c *Client) Probe(ctx context.Context) (string, error) {
	endpoint := c.baseURL + "/users/current.json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("redmine: build request: %w", err)
	}
	key, err := c.apiKey.Resolve()
	if err != nil {
		return "", fmt.Errorf("redmine: %w", err)
	}
	if c.redactor != nil {
		c.redactor.Register(key)
	}
	request.Header.Set("X-Redmine-API-Key", key)
	request.Header.Set("Accept", "application/json")

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return "", &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: "probe credential", Err: err}
	}
	defer response.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	c.logRequest("/users/current.json", response.StatusCode, time.Since(started))
	if readErr != nil {
		return "", &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: "probe credential", Err: readErr}
	}

	switch response.StatusCode {
	case http.StatusOK:
		// Continue below.
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("redmine: the configured credential was rejected (HTTP %d)", response.StatusCode)
	default:
		return "", &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   "probe credential",
			Err:  fmt.Errorf("unexpected HTTP status %d", response.StatusCode),
		}
	}

	var envelope struct {
		User struct {
			Login     string `json:"login"`
			FirstName string `json:"firstname"`
			LastName  string `json:"lastname"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: "decode current user", Err: err}
	}
	login := strings.TrimSpace(envelope.User.Login)
	if login == "" {
		login = strings.TrimSpace(envelope.User.FirstName + " " + envelope.User.LastName)
	}
	if login == "" {
		login = "the configured credential"
	}
	return login, nil
}

// GetProject proves the configured credential can read one configured project.
// It is a single authenticated GET and mutates nothing (FR-058).
func (c *Client) GetProject(ctx context.Context, identifier string) (*Project, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, errors.New("redmine: project identifier is required")
	}
	endpoint := c.baseURL + "/projects/" + url.PathEscape(identifier) + ".json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("redmine: build request: %w", err)
	}
	key, err := c.apiKey.Resolve()
	if err != nil {
		return nil, fmt.Errorf("redmine: %w", err)
	}
	if c.redactor != nil {
		c.redactor.Register(key)
	}
	request.Header.Set("X-Redmine-API-Key", key)
	request.Header.Set("Accept", "application/json")

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: "read project " + identifier, Err: err}
	}
	defer response.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	c.logRequest("/projects/"+identifier+".json", response.StatusCode, time.Since(started))
	if readErr != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: "read project " + identifier, Err: readErr}
	}

	switch response.StatusCode {
	case http.StatusOK:
		// Continue below.
	case http.StatusNotFound:
		return nil, fmt.Errorf("redmine: project %q was not found or is not visible to the credential", identifier)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("redmine: the configured credential cannot read project %q (HTTP %d)", identifier, response.StatusCode)
	default:
		return nil, &domain.UpstreamError{
			Kind: domain.UpstreamRedmine,
			Op:   "read project " + identifier,
			Err:  fmt.Errorf("unexpected HTTP status %d", response.StatusCode),
		}
	}

	var envelope struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamRedmine, Op: "decode project " + identifier, Err: err}
	}
	return &envelope.Project, nil
}

// FailureFor maps a client error to the failure vocabulary (FR-005).
func FailureFor(err error, issueID int) domain.Failure {
	switch {
	case errors.Is(err, ErrIssueNotFound):
		return domain.Failure{
			Code:        domain.FailIssueNotFound,
			Message:     fmt.Sprintf("Issue %d was not found, or is not visible to the configured Redmine credential.", issueID),
			Stage:       domain.StageConnectivityRedmine,
			Remediation: domain.DefaultRemediation(domain.FailIssueNotFound),
		}
	case errors.Is(err, ErrIssueInaccessible):
		return domain.Failure{
			Code:        domain.FailIssueInaccessible,
			Message:     fmt.Sprintf("Issue %d exists but the configured Redmine credential cannot read it.", issueID),
			Stage:       domain.StageConnectivityRedmine,
			Remediation: domain.DefaultRemediation(domain.FailIssueInaccessible),
		}
	default:
		failure := domain.ConnectivityFailure(err)
		if failure.Message == "operation failed" {
			failure.Message = "Redmine could not be reached."
		}
		failure.Stage = domain.StageConnectivityRedmine
		return failure
	}
}

func (c *Client) logRequest(path string, status int, elapsed time.Duration) {
	if c == nil || c.logger == nil {
		return
	}
	// Only the path and status: never headers, never the resolved credential.
	c.logger.Debug("redmine request",
		"method", http.MethodGet,
		"path", path,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
	)
}
