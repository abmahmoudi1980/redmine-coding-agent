// Package gitlab is the minimal REST client for the paths the worker needs:
// one project lookup for connectivity, and (in the publishing story) merge
// requests. It deliberately exposes no merge or branch-delete operation.
package gitlab

import (
	"bytes"
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

// DefaultTimeout bounds one GitLab request when the caller sets none.
const DefaultTimeout = 30 * time.Second

const maxResponseBytes = 8 << 20

// ErrProjectNotFound is a mapping mistake rather than a connectivity failure.
var ErrProjectNotFound = errors.New("gitlab: project not found")

// Options configures a Client.
type Options struct {
	BaseURL    string
	Token      config.SecretRef
	HTTPClient *http.Client
	Timeout    time.Duration
	Redactor   *logging.Redactor
	Logger     *slog.Logger
}

// Client talks to one GitLab instance with one credential.
type Client struct {
	baseURL  string
	token    config.SecretRef
	http     *http.Client
	redactor *logging.Redactor
	logger   *slog.Logger
}

// Project is the subset of a GitLab project the worker records.
type Project struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	HTTPURL           string `json:"http_url_to_repo"`
	SSHURL            string `json:"ssh_url_to_repo"`
	DefaultBranch     string `json:"default_branch"`
	WebURL            string `json:"web_url"`
}

// MergeRequest is the subset of a GitLab merge request the worker records.
type MergeRequest struct {
	ID           int    `json:"id"`
	IID          int    `json:"iid"`
	ProjectID    int    `json:"project_id"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	WebURL       string `json:"web_url"`
}

// MergeRequestInput is the create/update payload. It deliberately exposes no
// merge or approval field: merging belongs to a human (FR-041).
type MergeRequestInput struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
}

func (in MergeRequestInput) payload() map[string]any {
	payload := map[string]any{"title": in.Title, "description": in.Description}
	if strings.TrimSpace(in.SourceBranch) != "" {
		payload["source_branch"] = in.SourceBranch
	}
	if strings.TrimSpace(in.TargetBranch) != "" {
		payload["target_branch"] = in.TargetBranch
	}
	return payload
}

// New validates the shape of its inputs; it resolves no secret and makes no
// network call.
func New(opts Options) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, errors.New("gitlab: base URL is required")
	}
	if err := opts.Token.Validate(); err != nil {
		return nil, fmt.Errorf("gitlab: token: %w", err)
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
		token:    opts.Token,
		http:     httpClient,
		redactor: opts.Redactor,
		logger:   logger,
	}, nil
}

// BaseURL is the configured endpoint without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// GetProject proves reachability and credential validity for one repository
// (FR-016).
func (c *Client) GetProject(ctx context.Context, repositoryPath string) (*Project, error) {
	path := strings.Trim(strings.TrimSpace(repositoryPath), "/")
	if path == "" {
		return nil, errors.New("gitlab: repository path is required")
	}

	endpoint := fmt.Sprintf("%s/api/v4/projects/%s", c.baseURL, url.PathEscape(path))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build request: %w", err)
	}
	if err := c.authorize(request); err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: "get project " + path, Err: err}
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	c.logRequest(http.MethodGet, "/projects/"+path, response.StatusCode, time.Since(started))
	if readErr != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: "read project " + path, Err: readErr}
	}

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, path)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, &domain.UpstreamError{
			Kind: domain.UpstreamGitlab,
			Op:   "get project " + path,
			Err:  fmt.Errorf("the configured token cannot read this project (HTTP %d)", response.StatusCode),
		}
	default:
		return nil, &domain.UpstreamError{
			Kind: domain.UpstreamGitlab,
			Op:   "get project " + path,
			Err:  fmt.Errorf("unexpected HTTP status %d", response.StatusCode),
		}
	}

	var project Project
	if err := json.Unmarshal(body, &project); err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: "decode project " + path, Err: err}
	}
	if project.ID == 0 {
		return nil, &domain.UpstreamError{
			Kind: domain.UpstreamGitlab,
			Op:   "decode project " + path,
			Err:  errors.New("response has no project id"),
		}
	}
	if project.PathWithNamespace == "" {
		project.PathWithNamespace = path
	}
	return &project, nil
}

// FailureFor maps a client error to the connectivity vocabulary (FR-017).
func FailureFor(err error) domain.Failure {
	failure := domain.ConnectivityFailure(err)
	if errors.Is(err, ErrProjectNotFound) {
		failure.Code = domain.FailConnectivityRepository
		failure.Remediation = domain.DefaultRemediation(domain.FailConnectivityRepository)
	}
	if failure.Message == "operation failed" {
		failure.Message = "GitLab could not be reached."
	}
	failure.Stage = domain.StageConnectivityGitlab
	return failure
}

// FindOpenMergeRequest returns the open merge request for one branch pair, or
// nil when none exists. Publishing calls this first so a repeated attempt
// reuses the existing request instead of duplicating it (FR-043).
func (c *Client) FindOpenMergeRequest(ctx context.Context, repositoryPath, sourceBranch, targetBranch string) (*MergeRequest, error) {
	endpoint, err := c.mergeRequestsEndpoint(repositoryPath)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("source_branch", sourceBranch)
	query.Set("target_branch", targetBranch)
	query.Set("state", "opened")

	op := "list merge requests " + repositoryPath
	payload, err := c.do(ctx, http.MethodGet, endpoint+"?"+query.Encode(), op, nil)
	if err != nil {
		return nil, err
	}
	var mergeRequests []MergeRequest
	if err := json.Unmarshal(payload, &mergeRequests); err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: "decode " + op, Err: err}
	}
	for index := range mergeRequests {
		candidate := mergeRequests[index]
		if candidate.SourceBranch == sourceBranch && candidate.TargetBranch == targetBranch {
			return &candidate, nil
		}
	}
	return nil, nil
}

// CreateMergeRequest opens one merge request against the configured base
// branch (FR-040).
func (c *Client) CreateMergeRequest(ctx context.Context, repositoryPath string, input MergeRequestInput) (*MergeRequest, error) {
	if strings.TrimSpace(input.SourceBranch) == "" || strings.TrimSpace(input.TargetBranch) == "" {
		return nil, errors.New("gitlab: source and target branches are required")
	}
	endpoint, err := c.mergeRequestsEndpoint(repositoryPath)
	if err != nil {
		return nil, err
	}
	return c.writeMergeRequest(ctx, http.MethodPost, endpoint, "create merge request "+repositoryPath, input)
}

// UpdateMergeRequest refreshes the title and description of an existing merge
// request so a reused request never keeps a stale report (FR-043).
func (c *Client) UpdateMergeRequest(ctx context.Context, repositoryPath string, iid int, input MergeRequestInput) (*MergeRequest, error) {
	if iid <= 0 {
		return nil, fmt.Errorf("gitlab: invalid merge request iid %d", iid)
	}
	endpoint, err := c.mergeRequestsEndpoint(repositoryPath)
	if err != nil {
		return nil, err
	}
	endpoint = fmt.Sprintf("%s/%d", endpoint, iid)
	return c.writeMergeRequest(ctx, http.MethodPut, endpoint, "update merge request "+repositoryPath, input)
}

func (c *Client) writeMergeRequest(ctx context.Context, method, endpoint, op string, input MergeRequestInput) (*MergeRequest, error) {
	payload, err := c.do(ctx, method, endpoint, op, input.payload())
	if err != nil {
		return nil, err
	}
	var mergeRequest MergeRequest
	if err := json.Unmarshal(payload, &mergeRequest); err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: "decode " + op, Err: err}
	}
	if mergeRequest.IID == 0 {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: op, Err: errors.New("response has no merge request iid")}
	}
	return &mergeRequest, nil
}

func (c *Client) mergeRequestsEndpoint(repositoryPath string) (string, error) {
	path := strings.Trim(strings.TrimSpace(repositoryPath), "/")
	if path == "" {
		return "", errors.New("gitlab: repository path is required")
	}
	return fmt.Sprintf("%s/api/v4/projects/%s/merge_requests", c.baseURL, url.PathEscape(path)), nil
}

// do performs one authorized JSON request. Only 2xx responses are returned;
// every other outcome becomes an UpstreamError so publishing can map it to a
// single failure code.
func (c *Client) do(ctx context.Context, method, endpoint, op string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("gitlab: encode %s: %w", op, err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build request for %s: %w", op, err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if err := c.authorize(request); err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")

	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: op, Err: err}
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	c.logRequest(method, request.URL.Path, response.StatusCode, time.Since(started))
	if readErr != nil {
		return nil, &domain.UpstreamError{Kind: domain.UpstreamGitlab, Op: op, Err: readErr}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		detail := strings.TrimSpace(string(data))
		if len(detail) > 512 {
			detail = detail[:512]
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return nil, &domain.UpstreamError{
			Kind: domain.UpstreamGitlab,
			Op:   op,
			Err:  fmt.Errorf("HTTP %d: %s", response.StatusCode, detail),
		}
	}
	return data, nil
}

func (c *Client) authorize(request *http.Request) error {
	token, err := c.token.Resolve()
	if err != nil {
		return fmt.Errorf("gitlab: %w", err)
	}
	if c.redactor != nil {
		c.redactor.Register(token)
	}
	request.Header.Set("PRIVATE-TOKEN", token)
	return nil
}

func (c *Client) logRequest(method, path string, status int, elapsed time.Duration) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.Debug("gitlab request",
		"method", method,
		"path", path,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
	)
}
