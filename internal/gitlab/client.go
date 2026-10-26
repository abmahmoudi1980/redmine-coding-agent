// Package gitlab is the minimal REST client for the paths the worker needs:
// one project lookup for connectivity, and (in the publishing story) merge
// requests. It deliberately exposes no merge or branch-delete operation.
package gitlab

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
	c.logRequest("/projects/"+path, response.StatusCode, time.Since(started))
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

func (c *Client) logRequest(path string, status int, elapsed time.Duration) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.Debug("gitlab request",
		"method", http.MethodGet,
		"path", path,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
	)
}
