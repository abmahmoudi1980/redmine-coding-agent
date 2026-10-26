package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/runner"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// RunService is the narrow orchestration surface the HTTP layer needs.
type RunService interface {
	Create(ctx context.Context, req runner.CreateRequest) (*domain.Run, error)
	Cancel(ctx context.Context, publicID string) (run *domain.Run, changed bool, err error)
	Retry(ctx context.Context, req runner.RetryRequest) (*domain.Run, error)
}

type createRunRequest struct {
	IssueID      int   `json:"issueId"`
	RepositoryID int64 `json:"repositoryId"`
	Confirm      bool  `json:"confirm"`
	CommentIDs   []int `json:"commentIds"`
}

type runDocument struct {
	RunID              string            `json:"runId"`
	IssueID            int               `json:"issueId"`
	IssueSubject       string            `json:"issueSubject,omitempty"`
	PreviousRunID      *string           `json:"previousRunId"`
	AttemptNo          int               `json:"attemptNo"`
	State              string            `json:"state"`
	Stage              string            `json:"stage"`
	Terminal           bool              `json:"terminal"`
	Repository         *RepositoryOption `json:"repository"`
	BaseBranch         string            `json:"baseBranch,omitempty"`
	BaseRevision       string            `json:"baseRevision,omitempty"`
	Branch             string            `json:"branch,omitempty"`
	CommitSHA          *string           `json:"commitSha"`
	PushedAt           *time.Time        `json:"pushedAt"`
	MergeRequestURL    *string           `json:"mergeRequestUrl"`
	Agent              agentDocument     `json:"agent"`
	CreatedAt          time.Time         `json:"createdAt"`
	StartedAt          *time.Time        `json:"startedAt"`
	EndedAt            *time.Time        `json:"endedAt"`
	ElapsedMS          int64             `json:"elapsedMs"`
	CancelRequestedAt  *time.Time        `json:"cancelRequestedAt"`
	InterruptedStage   string            `json:"interruptedStage,omitempty"`
	FailureCode        string            `json:"failureCode,omitempty"`
	FailureMessage     string            `json:"failureMessage,omitempty"`
	FailureStage       string            `json:"failureStage,omitempty"`
	FailureRemediation string            `json:"failureRemediation,omitempty"`
	Summary            string            `json:"summary,omitempty"`
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

type runPageDocument struct {
	Items      []runDocument `json:"items"`
	NextCursor *string       `json:"nextCursor"`
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

// handleListRuns returns the durable, newest-first run history (FR-049).
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The database is not available in this process.")
		return
	}

	query := r.URL.Query()
	filter := store.RunFilter{}

	if raw := strings.TrimSpace(query.Get("state")); raw != "" {
		state := domain.RunState(raw)
		if !state.Valid() {
			s.writeProblem(w, http.StatusBadRequest, "invalid_state", "The state filter must be a known run state.")
			return
		}
		filter.State = state
	}
	if raw := strings.TrimSpace(query.Get("issueId")); raw != "" {
		issueID, err := strconv.Atoi(raw)
		if err != nil || issueID < 1 {
			s.writeProblem(w, http.StatusBadRequest, "invalid_issue_id", "The issue number must be a positive integer.")
			return
		}
		filter.IssueID = issueID
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			s.writeProblem(w, http.StatusBadRequest, "invalid_limit", "The page size must be between 1 and 100.")
			return
		}
		filter.Limit = limit
	}
	filter.Cursor = strings.TrimSpace(query.Get("cursor"))

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	runs, next, err := s.store.ListRuns(ctx, filter)
	if err != nil {
		if strings.Contains(err.Error(), "invalid cursor") {
			s.writeProblem(w, http.StatusBadRequest, "invalid_cursor", "The cursor is not a value this worker issued.")
			return
		}
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The run history could not be read.")
		return
	}

	items := make([]runDocument, 0, len(runs))
	for index := range runs {
		items = append(items, s.runDocumentFor(ctx, &runs[index]))
	}
	page := runPageDocument{Items: items}
	if next != "" {
		page.NextCursor = &next
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, page)
}

// handleRetryRun starts a new attempt linked to a finished one (FR-052).
func (s *Server) handleRetryRun(w http.ResponseWriter, r *http.Request) {
	publicID := strings.TrimSpace(r.PathValue("runId"))
	if publicID == "" {
		s.writeProblem(w, http.StatusNotFound, "run_not_found", "No run matches that identifier.")
		return
	}
	if s.runs == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The run pipeline is not wired in this process.")
		return
	}
	if _, ok := s.runForRequest(w, r); !ok {
		return
	}

	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	created, err := s.runs.Retry(ctx, runner.RetryRequest{
		PublicID:    publicID,
		RequestedBy: strings.TrimSpace(r.Header.Get("X-Operator")),
	})
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		s.writeProblem(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("Run %s does not exist.", publicID))
		return
	default:
		s.writeRunError(w, err)
		return
	}

	w.Header().Set("Location", "/api/runs/"+created.PublicID)
	s.writeJSON(w, http.StatusCreated, s.runDocumentFor(ctx, created))
}

// handleCancelRun asks the pipeline to stop and returns at once. The stream
// reports the outcome; already-created side effects are never rolled back
// (FR-012, FR-013, US4-AC4).
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	publicID := strings.TrimSpace(r.PathValue("runId"))
	if publicID == "" {
		s.writeProblem(w, http.StatusNotFound, "run_not_found", "No run matches that identifier.")
		return
	}
	if s.runs == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The run pipeline is not wired in this process.")
		return
	}
	if _, ok := s.runForRequest(w, r); !ok {
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	run, changed, err := s.runs.Cancel(ctx, publicID)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		s.writeProblem(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("Run %s does not exist.", publicID))
		return
	case errors.Is(err, runner.ErrRunNotCancellable):
		state := ""
		if run != nil {
			state = string(run.State)
		}
		detail := "The run has already finished, so there is nothing to cancel."
		if state != "" {
			detail = fmt.Sprintf("Run %s already finished in state %s.", publicID, state)
		}
		s.writeProblem(w, http.StatusConflict, "run_not_cancellable", detail)
		return
	default:
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The cancellation could not be recorded.")
		return
	}
	if run == nil {
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The cancellation could not be recorded.")
		return
	}

	status := http.StatusAccepted
	if !changed {
		status = http.StatusOK
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, status, s.runDocumentFor(ctx, run))
}

// handleGetRunLogs tails the per-run log file. The file is redacted as it is
// written, so the tail needs no second scrub (T103).
func (s *Server) handleGetRunLogs(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runForRequest(w, r)
	if !ok {
		return
	}
	if s.logger == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "Log files are not configured in this process.")
		return
	}

	tail := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			s.writeProblem(w, http.StatusBadRequest, "invalid_tail", "The tail size must be a positive integer.")
			return
		}
		if parsed > 5000 {
			parsed = 5000
		}
		tail = parsed
	}

	lines, err := tailFile(s.logger.RunLogPath(run.PublicID), tail)
	if errors.Is(err, os.ErrNotExist) {
		s.writeProblem(w, http.StatusGone, "logs_pruned", "The log file for this run was pruned; the timeline is still available.")
		return
	}
	if err != nil {
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The log file could not be read.")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// maxLogTailBytes bounds how much of a log file is read from the end.
const maxLogTailBytes = 2 << 20

// tailFile returns the last n lines, oldest first.
func tailFile(path string, n int) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, os.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := int64(0)
	if size > maxLogTailBytes {
		offset = size - maxLogTailBytes
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	text := strings.TrimRight(string(content), "\r\n")
	if text == "" {
		return []string{}, nil
	}
	all := strings.Split(text, "\n")
	if offset > 0 && len(all) > 0 {
		// The first line of a partial read is very likely cut in half.
		all = all[1:]
	}
	for index := range all {
		all[index] = strings.TrimRight(all[index], "\r")
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// handleGetRunReport returns the stored report even when its Redmine delivery
// failed, so the text is never lost (FR-044, FR-047).
func (s *Server) handleGetRunReport(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runForRequest(w, r)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	report, err := s.store.GetReport(ctx, run.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.writeJSON(w, http.StatusOK, map[string]any{"report": nil})
		return
	}
	if err != nil {
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The report could not be read.")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"report": report})
}

// handleGetRunChecks returns the per-attempt check grouping (FR-034).
func (s *Server) handleGetRunChecks(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runForRequest(w, r)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	attempts, err := s.store.CheckAttempts(ctx, run.ID)
	if err != nil {
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The check results could not be read.")
		return
	}
	if attempts == nil {
		attempts = []domain.CheckAttempt{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"attempts": attempts})
}

// runForRequest resolves the {runId} path value and writes a problem when the
// run does not exist or the database is unavailable.
func (s *Server) runForRequest(w http.ResponseWriter, r *http.Request) (*domain.Run, bool) {
	publicID := strings.TrimSpace(r.PathValue("runId"))
	if publicID == "" {
		s.writeProblem(w, http.StatusNotFound, "run_not_found", "No run matches that identifier.")
		return nil, false
	}
	if s.store == nil {
		s.writeProblem(w, http.StatusServiceUnavailable, "worker_not_ready", "The database is not available in this process.")
		return nil, false
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	run, err := s.store.GetRunByPublicID(ctx, publicID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeProblem(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("Run %s does not exist.", publicID))
			return nil, false
		}
		s.writeProblem(w, http.StatusInternalServerError, "internal_error", "The run could not be read.")
		return nil, false
	}
	return run, true
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

	detail := s.buildRunDetail(ctx, run)
	s.writeJSON(w, http.StatusOK, detail)
}

// buildRunDetail assembles the full audit view of one run. The HTTP handler and
// the CLI share it so `runs show --json` cannot drift from `/api/runs/{id}`.
func (s *Server) buildRunDetail(ctx context.Context, run *domain.Run) runDetailDocument {
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
	return detail
}

// RunDocumentFor renders one run exactly as GET /api/runs/{id} does.
func (s *Server) RunDocumentFor(ctx context.Context, run *domain.Run) any {
	return s.runDocumentFor(ctx, run)
}

// RunDetailFor renders the full audit document the API returns.
func (s *Server) RunDetailFor(ctx context.Context, run *domain.Run) any {
	return s.buildRunDetail(ctx, run)
}

// RunPageFor renders a history page exactly as GET /api/runs does.
func (s *Server) RunPageFor(ctx context.Context, runs []domain.Run, next string) any {
	items := make([]runDocument, 0, len(runs))
	for index := range runs {
		items = append(items, s.runDocumentFor(ctx, &runs[index]))
	}
	page := runPageDocument{Items: items}
	if next != "" {
		cursor := next
		page.NextCursor = &cursor
	}
	return page
}

func (s *Server) runDocumentFor(ctx context.Context, run *domain.Run) runDocument {
	document := runDocument{
		RunID:              run.PublicID,
		IssueID:            run.IssueID,
		IssueSubject:       run.IssueSubject,
		AttemptNo:          s.attemptNumber(ctx, run),
		State:              string(run.State),
		Stage:              string(run.Stage),
		Terminal:           domain.IsTerminal(run.State),
		BaseBranch:         run.BaseBranch,
		BaseRevision:       run.BaseRevision,
		Branch:             run.Branch,
		PushedAt:           run.PushedAt,
		Agent:              agentDocument{Name: run.AgentName, Model: run.AgentModel},
		CreatedAt:          run.CreatedAt,
		StartedAt:          run.StartedAt,
		EndedAt:            run.EndedAt,
		ElapsedMS:          durationMS(run.StartedAt, run.EndedAt),
		CancelRequestedAt:  run.CancelRequestedAt,
		InterruptedStage:   string(run.InterruptedStage),
		FailureCode:        string(run.FailureCode),
		FailureMessage:     run.FailureMessage,
		FailureStage:       string(run.Stage),
		FailureRemediation: domain.DefaultRemediation(run.FailureCode),
		Summary:            run.Summary,
	}
	if run.FailureCode == "" {
		document.FailureStage = ""
		document.FailureRemediation = ""
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

// attemptNumber walks the retry chain so the UI can distinguish a retry from
// the original attempt without reading every linked run (FR-052).
func (s *Server) attemptNumber(ctx context.Context, run *domain.Run) int {
	if run == nil {
		return 1
	}
	number := 1
	current := run
	for steps := 0; steps < 50 && current.PreviousRunID != nil; steps++ {
		if s.store == nil {
			break
		}
		previous, err := s.store.GetRun(ctx, *current.PreviousRunID)
		if err != nil {
			break
		}
		number++
		current = previous
	}
	return number
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
