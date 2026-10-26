package api

import (
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
)

type projectResponse struct {
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
}

type issueSnapshotResponse struct {
	IssueID            int                  `json:"issueId"`
	Subject            string               `json:"subject"`
	Description        string               `json:"description"`
	AcceptanceCriteria string               `json:"acceptanceCriteria,omitempty"`
	CriteriaSource     string               `json:"criteriaSource"`
	Project            projectResponse      `json:"project"`
	Status             string               `json:"status,omitempty"`
	Tracker            string               `json:"tracker,omitempty"`
	Priority           string               `json:"priority,omitempty"`
	Assignee           string               `json:"assignee,omitempty"`
	ParentIssueID      *int                 `json:"parentIssueId,omitempty"`
	RelatedIssueIDs    []int                `json:"relatedIssueIds,omitempty"`
	CustomFields       []domain.CustomField `json:"customFields,omitempty"`
	Comments           []domain.Comment     `json:"comments,omitempty"`
	Attachments        []domain.Attachment  `json:"attachments,omitempty"`
	ContentHash        string               `json:"contentHash"`
	FetchedAt          time.Time            `json:"fetchedAt"`
}

type issuePreviewResponse struct {
	Issue      issueSnapshotResponse `json:"issue"`
	Repository RepositoryOption      `json:"repository"`
	BaseBranch string                `json:"baseBranch"`
	Warnings   []string              `json:"warnings"`
	Candidates []RepositoryOption    `json:"candidates,omitempty"`
	StartedAt  time.Time             `json:"startedAt"`
}

type checkDefinitionResponse struct {
	Name           string `json:"name"`
	Command        string `json:"command"`
	Required       bool   `json:"required"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Position       int    `json:"position"`
}

type repositoryResponse struct {
	RepositoryOption
	RedmineProject      string                    `json:"redmineProject"`
	Enabled             bool                      `json:"enabled"`
	PublishEnabled      bool                      `json:"publishEnabled"`
	MergeRequestEnabled bool                      `json:"mergeRequestEnabled"`
	Checks              []checkDefinitionResponse `json:"checks"`
}

// handleListRepositories returns the stored mappings and their checks so the
// operator can see what the worker believes before starting anything.
func (s *Server) handleListRepositories(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The repository table is not available in this process.")
		return
	}

	project := strings.TrimSpace(r.URL.Query().Get("project"))
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	mappings, err := s.store.ListRepositories(ctx)
	if err != nil {
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "Could not read repository mappings.")
		return
	}

	items := make([]repositoryResponse, 0, len(mappings))
	for _, mapping := range mappings {
		// Project-less mappings apply to every issue and are always listed.
		if project != "" && mapping.RedmineProject != "" && mapping.RedmineProject != project {
			continue
		}
		items = append(items, repositoryFromMapping(mapping))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handlePreviewIssue retrieves one issue and resolves its destination without
// any side effect (US1).
func (s *Server) handlePreviewIssue(w http.ResponseWriter, r *http.Request) {
	started := time.Now().UTC()

	issueID, err := ParseIssueID(r.PathValue("issueId"))
	if err != nil {
		s.writeProblem(w, http.StatusBadRequest, "invalid_issue_id", "The issue number must be a positive integer.")
		return
	}

	var explicitRepositoryID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("repositoryId")); raw != "" {
		explicitRepositoryID, err = ParseRepositoryID(raw)
		if err != nil {
			s.writeProblem(w, http.StatusBadRequest, "invalid_repository_id", "The repository id must be a positive integer.")
			return
		}
	}
	repositoryPath := strings.TrimSpace(r.URL.Query().Get("repositoryPath"))

	if s.redmine == nil || s.store == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The worker is not wired to Redmine and its database yet.")
		return
	}

	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	issue, err := s.redmine.GetIssue(ctx, issueID)
	if err != nil {
		s.writeProblemDocument(w, ProblemForFailure(redmine.FailureFor(err, issueID)))
		return
	}

	snapshot := runner.BuildSnapshot(issue, started)
	warnings := previewWarnings(snapshot)

	mapping, err := runner.ResolveSelection(ctx, s.store, runner.Selection{
		Project:        issue.Project.Identifier,
		RepositoryID:   explicitRepositoryID,
		RepositoryPath: repositoryPath,
	})
	if err != nil {
		problem := ProblemForFailure(runner.FailureForResolve(err))
		var ambiguous *runner.AmbiguousMappingError
		if errors.As(err, &ambiguous) {
			problem.Candidates = repositoryOptions(ambiguous.Candidates)
		}
		s.writeProblemDocument(w, problem)
		return
	}

	s.writeJSON(w, http.StatusOK, issuePreviewResponse{
		Issue:      snapshotResponse(snapshot),
		Repository: optionFromMapping(mapping),
		BaseBranch: mapping.BaseBranch,
		Warnings:   warnings,
		StartedAt:  started,
	})
}

func previewWarnings(snapshot domain.IssueSnapshot) []string {
	warnings := make([]string, 0, 2)
	if snapshot.CriteriaSource != "heading" || strings.TrimSpace(snapshot.AcceptanceCriteria) == "" {
		warnings = append(warnings, "no_acceptance_criteria")
	}
	for _, attachment := range snapshot.Attachments {
		if !attachment.Available {
			warnings = append(warnings, "attachments_unavailable")
			break
		}
	}
	return warnings
}

func snapshotResponse(snapshot domain.IssueSnapshot) issueSnapshotResponse {
	return issueSnapshotResponse{
		IssueID:            snapshot.RedmineIssueID,
		Subject:            snapshot.Subject,
		Description:        snapshot.Description,
		AcceptanceCriteria: snapshot.AcceptanceCriteria,
		CriteriaSource:     snapshot.CriteriaSource,
		Project: projectResponse{
			Identifier: snapshot.ProjectIdentifier,
			Name:       snapshot.ProjectName,
		},
		Status:          snapshot.StatusName,
		Tracker:         snapshot.TrackerName,
		Priority:        snapshot.PriorityName,
		Assignee:        snapshot.AssigneeName,
		ParentIssueID:   snapshot.ParentIssueID,
		RelatedIssueIDs: snapshot.RelatedIssueIDs,
		CustomFields:    snapshot.CustomFields,
		Comments:        snapshot.SelectedComments,
		Attachments:     snapshot.Attachments,
		ContentHash:     snapshot.ContentHash,
		FetchedAt:       snapshot.FetchedAt,
	}
}

func repositoryFromMapping(mapping domain.RepositoryMapping) repositoryResponse {
	checks := make([]checkDefinitionResponse, 0, len(mapping.Checks))
	for _, check := range mapping.Checks {
		checks = append(checks, checkDefinitionResponse{
			Name:           check.Name,
			Command:        check.CommandFor(runtime.GOOS),
			Required:       check.Required,
			TimeoutSeconds: check.TimeoutSeconds,
			Position:       check.Position,
		})
	}
	return repositoryResponse{
		RepositoryOption:    optionFromMapping(mapping),
		RedmineProject:      mapping.RedmineProject,
		Enabled:             mapping.Enabled,
		PublishEnabled:      mapping.PublishEnabled,
		MergeRequestEnabled: mapping.MergeRequestEnabled,
		Checks:              checks,
	}
}

func optionFromMapping(mapping domain.RepositoryMapping) RepositoryOption {
	return RepositoryOption{
		RepositoryID: mapping.ID,
		Path:         mapping.RepositoryPath,
		DisplayName:  mapping.DisplayName,
		BaseBranch:   mapping.BaseBranch,
		IsDefault:    mapping.IsDefault,
	}
}

func repositoryOptions(mappings []domain.RepositoryMapping) []RepositoryOption {
	options := make([]RepositoryOption, 0, len(mappings))
	for _, mapping := range mappings {
		options = append(options, optionFromMapping(mapping))
	}
	return options
}
