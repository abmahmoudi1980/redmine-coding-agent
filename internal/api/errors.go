// Package api wires the HTTP surface described by contracts/openapi.yaml.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// Problem is the RFC 9457 body used for every error response.
type Problem struct {
	Type       string             `json:"type"`
	Title      string             `json:"title"`
	Status     int                `json:"status"`
	Code       string             `json:"code"`
	Detail     string             `json:"detail"`
	Stage      string             `json:"stage,omitempty"`
	Candidates []RepositoryOption `json:"candidates,omitempty"`
	ActiveRun  *ActiveRunRef      `json:"activeRun,omitempty"`
}

// RepositoryOption is the candidate shape returned on ambiguity.
type RepositoryOption struct {
	RepositoryID int64  `json:"repositoryId"`
	Path         string `json:"path"`
	DisplayName  string `json:"displayName,omitempty"`
	BaseBranch   string `json:"baseBranch"`
	IsDefault    bool   `json:"isDefault"`
}

// ActiveRunRef lets the UI jump straight to the run that blocks a request.
type ActiveRunRef struct {
	RunID string `json:"runId"`
	Stage string `json:"stage"`
	State string `json:"state"`
}

// StatusForFailure maps a domain failure to the documented HTTP status.
func StatusForFailure(failure domain.Failure) int {
	switch failure.Code {
	case domain.FailIssueInputInvalid:
		return http.StatusBadRequest
	case domain.FailIssueNotFound, domain.FailIssueInaccessible:
		return http.StatusNotFound
	case domain.FailNoRepositoryMapping, domain.FailRepositoryAmbiguous:
		return http.StatusConflict
	case domain.FailConnectivityRedmine, domain.FailConnectivityGitlab,
		domain.FailConnectivityRepository, domain.FailAgentUnavailable,
		domain.FailMergeRequestFailed, domain.FailPushRejected:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// ProblemForFailure builds a problem document from a domain failure.
func ProblemForFailure(failure domain.Failure) Problem {
	status := StatusForFailure(failure)
	return Problem{
		Title:  TitleFor(string(failure.Code)),
		Status: status,
		Code:   string(failure.Code),
		Detail: failure.Message,
		Stage:  string(failure.Stage),
	}
}

// TitleFor is the human title used for a problem code.
func TitleFor(code string) string {
	switch code {
	case "invalid_issue_id":
		return "Invalid issue number"
	case "invalid_repository_id":
		return "Invalid repository choice"
	case "invalid_body":
		return "Invalid request body"
	case "confirmation_required":
		return "Confirmation required"
	case "worker_not_ready":
		return "Worker not ready"
	case "issue_not_found", "issue_inaccessible":
		return "Issue not available"
	case "repository_ambiguous":
		return "Repository selection required"
	case "no_repository_mapping":
		return "No repository mapping"
	case "active_run_exists":
		return "An active run already exists"
	case "concurrency_limit":
		return "Concurrency limit reached"
	case "run_not_found":
		return "Run not found"
	case "run_not_cancellable":
		return "Run cannot be cancelled"
	case "logs_pruned":
		return "Logs were pruned"
	case "invalid_tail":
		return "Invalid log tail size"
	case "invalid_state":
		return "Invalid run state"
	case "invalid_limit":
		return "Invalid page size"
	case "invalid_cursor":
		return "Invalid cursor"
	case "streaming_unsupported":
		return "Streaming unsupported"
	case "connectivity_redmine":
		return "Redmine unreachable"
	case "connectivity_gitlab":
		return "GitLab unreachable"
	case "connectivity_repository":
		return "Repository unreachable"
	case "agent_unavailable":
		return "Coding agent unavailable"
	case "not_implemented":
		return "Not implemented"
	case "internal_error":
		return "Internal error"
	default:
		if strings.TrimSpace(code) == "" {
			return "Request failed"
		}
		return strings.ReplaceAll(code, "_", " ")
	}
}

// writeProblem emits application/problem+json with a redacted detail.
func (s *Server) writeProblem(w http.ResponseWriter, status int, code, detail string) {
	s.writeProblemDocument(w, Problem{
		Title:  TitleFor(code),
		Status: status,
		Code:   code,
		Detail: detail,
	})
}

// writeProblemDocument fills the derived fields and writes the body.
func (s *Server) writeProblemDocument(w http.ResponseWriter, problem Problem) {
	if problem.Title == "" {
		problem.Title = TitleFor(problem.Code)
	}
	if problem.Status == 0 {
		problem.Status = http.StatusInternalServerError
	}
	problem.Type = "https://redmine-agent.local/problems/" + problem.Code
	problem.Detail = s.redact(problem.Detail)

	body, err := json.Marshal(problem)
	if err != nil {
		http.Error(w, http.StatusText(problem.Status), problem.Status)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(problem.Status)
	_, _ = w.Write(body)
}
