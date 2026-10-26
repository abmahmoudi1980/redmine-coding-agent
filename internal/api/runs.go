package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// RunService is the narrow orchestration surface the HTTP layer needs.
type RunService interface {
	Create(ctx context.Context, req runner.CreateRequest) (*domain.Run, error)
}

type createRunRequest struct {
	IssueID      int   `json:"issueId"`
	RepositoryID int64 `json:"repositoryId"`
	Confirm      bool  `json:"confirm"`
	CommentIDs   []int `json:"commentIds"`
}

type runDocument struct {
	RunID             string            `json:"runId"`
	IssueID           int               `json:"issueId"`
	IssueSubject      string            `json:"issueSubject,omitempty"`
	PreviousRunID     *string           `json:"previousRunId"`
	State             string            `json:"state"`
	Stage             string            `json:"stage"`
	Terminal          bool              `json:"terminal"`
	Repository        *RepositoryOption `json:"repository"`
	BaseBranch        string            `json:"baseBranch,omitempty"`
	BaseRevision      string            `json:"baseRevision,omitempty"`
	Branch            string            `json:"branch,omitempty"`
	CommitSHA         *string           `json:"commitSha"`
	PushedAt          *time.Time        `json:"pushedAt"`
	MergeRequestURL   *string           `json:"mergeRequestUrl"`
	Agent             agentDocument     `json:"agent"`
	CreatedAt         time.Time         `json:"createdAt"`
	StartedAt         *time.Time        `json:"startedAt"`
	EndedAt           *time.Time        `json:"endedAt"`
	ElapsedMS         int64             `json:"elapsedMs"`
	CancelRequestedAt *time.Time        `json:"cancelRequestedAt"`
	FailureCode       string            `json:"failureCode,omitempty"`
	FailureMessage    string            `json:"failureMessage,omitempty"`
	Summary           string            `json:"summary,omitempty"`
}

type agentDocument struct {
	Name  string `json:"name,omitempty"`
	Model string `json:"model,omitempty"`
}

type workspaceDocument struct {
	Path               string     `json:"path,omitempty"`
	Branch             string     `json:"branch"`
	BaseRevision       string     `json:"baseRevision"`
	CreatedAt          time.Time  `json:"createdAt"`
	CleanedAt          *time.Time `json:"cleanedAt"`
	HasUnpushedChanges bool       `json:"hasUnpushedChanges"`
	ChangedPaths       []string   `json:"changedPaths,omitempty"`
}

type invocationDocument struct {
	AttemptNo           int        `json:"attemptNo"`
	Kind                string     `json:"kind"`
	Adapter             string     `json:"adapter"`
	Model               string     `json:"model"`
	Status              string     `json:"status"`
	ReportedStatus      string     `json:"reportedStatus"`
	ExitCode            *int       `json:"exitCode"`
	TimedOut            bool       `json:"timedOut"`
	StartedAt           *time.Time `json:"startedAt,omitempty"`
	EndedAt             *time.Time `json:"endedAt,omitempty"`
	DurationMS          int64      `json:"durationMs"`
	PromptSHA256        string     `json:"promptSha256"`
	TranscriptAvailable bool       `json:"transcriptAvailable"`
}

type eventDocument struct {
	Seq      int64           `json:"seq"`
	At       time.Time       `json:"at"`
	Type     string          `json:"type"`
	Severity string          `json:"severity"`
	Message  string          `json:"message"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type runDetailDocument struct {
	runDocument
	Issue             *issueSnapshotResponse    `json:"issue,omitempty"`
	Workspace         *workspaceDocument        `json:"workspace"`
	Invocations       []invocationDocument      `json:"invocations"`
	Checks            []domain.ValidationResult `json:"checks"`
	Report            *domain.Report            `json:"report"`
	ConfigProfileHash string                    `json:"configProfileHash,omitempty"`
	Timeline          []eventDocument           `json:"timeline"`
}

// handleCreateRun is the only way a run starts (FR-008).
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var body createRunRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		s.writeProblem(w, http.StatusBadRequest, "invalid_body", "The request body must be a JSON object with issueId, confirm, and optionally repositoryId.")
		return
	}
	if body.IssueID < 1 {
		s.writeProblem(w, http.StatusBadRequest, "invalid_issue_id", "The issue number must be a positive integer.")
		return
	}
	if !body.Confirm {
		s.writeProblem(w, http.StatusBadRequest, "confirmation_required", "The run starts only after the operator confirms the issue, repository, and base branch.")
		return
	}
	if s.runs == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The run pipeline is not wired in this process.")
		return
	}

	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	created, err := s.runs.Create(ctx, runner.CreateRequest{
		IssueID:      body.IssueID,
		RepositoryID: body.RepositoryID,
		Confirm:      body.Confirm,
		CommentIDs:   body.CommentIDs,
		RequestedBy:  strings.TrimSpace(r.Header.Get("X-Operator")),
	})
	if err != nil {
		s.writeRunError(w, err)
		return
	}

	w.Header().Set("Location", "/api/runs/"+created.PublicID)
	s.writeJSON(w, http.StatusCreated, s.runDocumentFor(ctx, created))
}

// handleGetRun returns the full audit view of one run (FR-048, SC-011).
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	publicID := strings.TrimSpace(r.PathValue("runId"))
	if publicID == "" {
		s.writeProblem(w, http.StatusNotFound, "run_not_found", "No run matches that identifier.")
		return
	}
	if s.store == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The database is not available in this process.")
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	run, err := s.store.GetRunByPublicID(ctx, publicID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeProblem(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("Run %s does not exist.", publicID))
			return
		}
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The run could not be read.")
		return
	}

	detail := runDetailDocument{
		runDocument: s.runDocumentFor(ctx, run),
		Invocations: []invocationDocument{},
		Checks:      []domain.ValidationResult{},
		Timeline:    []eventDocument{},
	}

	if snapshot, err := s.store.GetIssueSnapshot(ctx, run.IssueSnapshotID); err == nil {
		response := snapshotResponse(*snapshot)
		detail.Issue = &response
	}
	if workspace, err := s.store.GetWorkspaceByRun(ctx, run.ID); err == nil {
		detail.Workspace = &workspaceDocument{
			Path:               workspace.Path,
			Branch:             workspace.Branch,
			BaseRevision:       workspace.BaseRevision,
			CreatedAt:          workspace.CreatedAt,
			CleanedAt:          workspace.CleanedAt,
			HasUnpushedChanges: workspace.HasUnpushedChanges,
			ChangedPaths:       workspace.ChangedPaths,
		}
	}
	if invocations, err := s.store.ListInvocations(ctx, run.ID); err == nil {
		for _, invocation := range invocations {
			detail.Invocations = append(detail.Invocations, invocationDocument{
				AttemptNo:           invocation.AttemptNo,
				Kind:                invocation.Kind,
				Adapter:             invocation.Adapter,
				Model:               invocation.Model,
				Status:              invocation.Status,
				ReportedStatus:      invocation.ReportedStatus,
				ExitCode:            invocation.ExitCode,
				TimedOut:            invocation.TimedOut,
				StartedAt:           invocation.StartedAt,
				EndedAt:             invocation.EndedAt,
				DurationMS:          durationMS(invocation.StartedAt, invocation.EndedAt),
				PromptSHA256:        invocation.PromptSHA256,
				TranscriptAvailable: invocation.TranscriptPath != "",
			})
		}
	}
	if checks, err := s.store.ListValidationResults(ctx, run.ID); err == nil {
		detail.Checks = append(detail.Checks, checks...)
	}
	if report, err := s.store.GetReport(ctx, run.ID); err == nil {
		detail.Report = report
		if report.MergeRequestURL != "" {
			url := report.MergeRequestURL
			detail.MergeRequestURL = &url
		}
	}
	if run.ConfigProfileID != nil {
		if profile, err := s.store.GetConfigurationProfile(ctx, *run.ConfigProfileID); err == nil {
			detail.ConfigProfileHash = profile.ContentHash
		}
	}
	if events, err := s.store.LatestEvents(ctx, run.ID, 200); err == nil {
		for _, event := range events {
			detail.Timeline = append(detail.Timeline, eventDocument{
				Seq:      event.Seq,
				At:       event.At,
				Type:     string(event.Type),
				Severity: event.Severity,
				Message:  event.Message,
				Data:     eventData(event.Data),
			})
		}
	}

	s.writeJSON(w, http.StatusOK, detail)
}

func (s *Server) runDocumentFor(ctx context.Context, run *domain.Run) runDocument {
	document := runDocument{
		RunID:             run.PublicID,
		IssueID:           run.IssueID,
		IssueSubject:      run.IssueSubject,
		State:             string(run.State),
		Stage:             string(run.Stage),
		Terminal:          domain.IsTerminal(run.State),
		BaseBranch:        run.BaseBranch,
		BaseRevision:      run.BaseRevision,
		Branch:            run.Branch,
		PushedAt:          run.PushedAt,
		Agent:             agentDocument{Name: run.AgentName, Model: run.AgentModel},
		CreatedAt:         run.CreatedAt,
		StartedAt:         run.StartedAt,
		EndedAt:           run.EndedAt,
		ElapsedMS:         durationMS(run.StartedAt, run.EndedAt),
		CancelRequestedAt: run.CancelRequestedAt,
		FailureCode:       string(run.FailureCode),
		FailureMessage:    run.FailureMessage,
		Summary:           run.Summary,
	}
	if run.CommitSHA != "" {
		commit := run.CommitSHA
		document.CommitSHA = &commit
	}
	if run.RepositoryID != nil && s.store != nil {
		if mapping, err := s.store.GetRepositoryByID(ctx, *run.RepositoryID); err == nil {
			option := optionFromMapping(*mapping)
			document.Repository = &option
		}
	}
	if run.PreviousRunID != nil && s.store != nil {
		if previous, err := s.store.GetRun(ctx, *run.PreviousRunID); err == nil {
			id := previous.PublicID
			document.PreviousRunID = &id
		}
	}
	return document
}

func (s *Server) writeRunError(w http.ResponseWriter, err error) {
	var active *runner.ActiveRunError
	var failure domain.Failure

	switch {
	case errors.Is(err, runner.ErrConfirmRequired):
		s.writeProblem(w, http.StatusBadRequest, "confirmation_required", "The run starts only after the operator confirms the issue, repository, and base branch.")
	case errors.As(err, &active):
		problem := Problem{
			Title:  TitleFor("active_run_exists"),
			Status: http.StatusConflict,
			Code:   "active_run_exists",
			Detail: "This issue already has an active run. Wait for it to finish, or cancel it.",
		}
		if active.Run != nil {
			problem.Detail = fmt.Sprintf("Issue %d already has run %s in stage %s.", active.Run.IssueID, active.Run.PublicID, active.Run.Stage)
			problem.ActiveRun = &ActiveRunRef{
				RunID: active.Run.PublicID,
				Stage: string(active.Run.Stage),
				State: string(active.Run.State),
			}
		}
		s.writeProblemDocument(w, problem)
	case errors.Is(err, store.ErrConcurrencyLimit):
		s.writeProblem(w, http.StatusTooManyRequests, "concurrency_limit", "The configured limit of active runs is reached. Wait for a run to finish.")
	case errors.As(err, &failure):
		s.writeProblemDocument(w, ProblemForFailure(failure))
	default:
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The run could not be started.")
	}
}

func durationMS(started, ended *time.Time) int64 {
	if started == nil {
		return 0
	}
	finish := time.Now().UTC()
	if ended != nil {
		finish = *ended
	}
	if finish.Before(*started) {
		return 0
	}
	return finish.Sub(*started).Milliseconds()
}

func eventData(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if json.Valid([]byte(trimmed)) && strings.HasPrefix(trimmed, "{") {
		return json.RawMessage(trimmed)
	}
	encoded, err := json.Marshal(map[string]string{"text": trimmed})
	if err != nil {
		return nil
	}
	return encoded
}
