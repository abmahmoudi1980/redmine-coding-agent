package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// CommandRunner executes the configured checks. *validate.Runner implements it.
type CommandRunner interface {
	RunCheck(ctx context.Context, check domain.CheckDefinition, workspace string) domain.ValidationResult
}

// Options are the collaborators the pipeline needs.
type Options struct {
	Config    *config.Config
	Store     *store.Store
	Git       *git.Manager
	Adapters  *agent.Registry
	Redmine   *redmine.Client
	GitLab    *gitlab.Client
	Logger    *logging.Logger
	Validator CommandRunner
	NewID     func() (string, error)
}

// Runner owns the run lifecycle: one goroutine per accepted run, one shared
// store, and one git manager with per-repository locking.
type Runner struct {
	config    *config.Config
	store     *store.Store
	git       *git.Manager
	adapters  *agent.Registry
	redmine   *redmine.Client
	gitlab    *gitlab.Client
	logger    *logging.Logger
	validator CommandRunner
	newID     func() (string, error)
	agent     agentSettings

	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc

	wg sync.WaitGroup
}

// agentSettings is the resolved, hash-identified agent policy for this process
// (T081, FR-027).
type agentSettings struct {
	Adapter    string
	Model      string
	Profile    string
	Timeout    time.Duration
	MaxRepairs int
	ConfigID   string
}

// Errors the HTTP layer maps to specific problem documents.
var (
	// ErrConfirmRequired means the caller did not confirm the preview.
	ErrConfirmRequired = errors.New("runner: explicit confirmation is required")
)

// ActiveRunError blocks a second start for an issue and names the run in the
// way so the operator can jump to it (FR-010).
type ActiveRunError struct {
	Run *domain.Run
}

func (e *ActiveRunError) Error() string {
	if e == nil || e.Run == nil {
		return "runner: an active run already exists for this issue"
	}
	return fmt.Sprintf("runner: issue %d already has run %s in stage %s", e.Run.IssueID, e.Run.PublicID, e.Run.Stage)
}

// New validates its collaborators and returns a ready runner. It never
// contacts an external system.
func New(opts Options) (*Runner, error) {
	if opts.Config == nil {
		return nil, errors.New("runner: configuration is required")
	}
	if opts.Store == nil {
		return nil, errors.New("runner: store is required")
	}
	if opts.Git == nil {
		return nil, errors.New("runner: git manager is required")
	}
	if opts.Adapters == nil {
		return nil, errors.New("runner: adapter registry is required")
	}
	newID := opts.NewID
	if newID == nil {
		newID = func() (string, error) {
			id, err := uuid.NewV7()
			if err != nil {
				return "", err
			}
			return id.String(), nil
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.New(logging.Options{LogDir: opts.Config.Data.LogDir})
	}
	return &Runner{
		config:    opts.Config,
		store:     opts.Store,
		git:       opts.Git,
		adapters:  opts.Adapters,
		redmine:   opts.Redmine,
		gitlab:    opts.GitLab,
		logger:    logger,
		validator: opts.Validator,
		newID:     newID,
		agent:     resolveAgentSettings(opts.Config),
	}, nil
}

// CreateRequest is one confirmed start.
type CreateRequest struct {
	IssueID      int
	RepositoryID int64
	Confirm      bool
	CommentIDs   []int
	RequestedBy  string
}

// Create validates, reserves a concurrency slot, persists the snapshot and run,
// and starts the pipeline. It returns as soon as the run is queued.
func (r *Runner) Create(ctx context.Context, req CreateRequest) (*domain.Run, error) {
	return r.create(ctx, req, createOptions{})
}

// createOptions carries the retry lineage that only Retry sets.
type createOptions struct {
	previous *domain.Run
	branch   string
}

func (r *Runner) create(ctx context.Context, req CreateRequest, opts createOptions) (*domain.Run, error) {
	if !req.Confirm {
		return nil, ErrConfirmRequired
	}
	if req.IssueID < 1 {
		return nil, domain.NewFailure(domain.FailIssueInputInvalid, "The issue number must be a positive integer.", domain.StageCreated)
	}
	if r.redmine == nil {
		return nil, errors.New("runner: the Redmine client is not configured")
	}

	active, err := r.store.ActiveRunByIssue(ctx, req.IssueID)
	if err != nil {
		return nil, fmt.Errorf("runner: active run lookup: %w", err)
	}
	if active != nil {
		return nil, &ActiveRunError{Run: active}
	}

	issue, err := r.redmine.GetIssue(ctx, req.IssueID)
	if err != nil {
		return nil, redmine.FailureFor(err, req.IssueID)
	}

	snapshot := BuildSnapshot(issue, time.Now().UTC())
	snapshot.SelectedComments = filterComments(snapshot.SelectedComments, req.CommentIDs)

	mapping, err := Resolve(ctx, r.store, issue.Project.Identifier, req.RepositoryID)
	if err != nil {
		return nil, FailureForResolve(err)
	}

	publicID, err := r.newID()
	if err != nil {
		return nil, fmt.Errorf("runner: allocate run id: %w", err)
	}
	runDir, err := r.prepareRunDir(publicID)
	if err != nil {
		return nil, fmt.Errorf("runner: prepare run directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(runDir)
		}
	}()

	r.collectAttachments(ctx, &snapshot, runDir)

	profileID, err := r.saveConfigProfile(ctx)
	if err != nil {
		return nil, err
	}

	branch := opts.branch
	if branch == "" {
		branch = BranchName(req.IssueID)
		if opts.previous != nil && opts.previous.Branch != "" {
			branch = opts.previous.Branch
		}
	}

	repositoryID := mapping.ID
	run := &domain.Run{
		PublicID:        publicID,
		IssueID:         req.IssueID,
		IssueSubject:    snapshot.Subject,
		RepositoryID:    &repositoryID,
		State:           domain.StateQueued,
		Stage:           domain.StageCreated,
		BaseBranch:      mapping.BaseBranch,
		Branch:          branch,
		AgentName:       r.agent.Adapter,
		AgentModel:      r.agent.Model,
		AgentConfigID:   r.agent.ConfigID,
		ConfigProfileID: profileID,
		RequestedBy:     req.RequestedBy,
		CreatedAt:       time.Now().UTC(),
	}
	if opts.previous != nil {
		previousID := opts.previous.ID
		run.PreviousRunID = &previousID
	}
	if err := r.store.ReserveRun(ctx, &snapshot, run, r.config.Concurrency.MaxActiveRuns); err != nil {
		switch {
		case errors.Is(err, store.ErrActiveRunExists):
			existing, lookupErr := r.store.ActiveRunByIssue(ctx, req.IssueID)
			if lookupErr == nil && existing != nil {
				return nil, &ActiveRunError{Run: existing}
			}
			return nil, &ActiveRunError{}
		case errors.Is(err, store.ErrConcurrencyLimit):
			return nil, store.ErrConcurrencyLimit
		default:
			return nil, err
		}
	}
	cleanup = false

	r.wg.Add(1)
	// #nosec G118 -- the run outlives the HTTP request that started it; its
	// lifetime is owned by the pipeline, not by the caller's context.
	go r.execute(run, snapshot, mapping, runDir)

	return run, nil
}

// Wait blocks until every in-flight pipeline finishes. Used by tests and by a
// graceful shutdown.
func (r *Runner) Wait() { r.wg.Wait() }

// execute runs one pipeline on its own context and settles the terminal state.
// The context is cancellable by the operator and is the only thing standing
// between a long agent run and a pushed branch (FR-012).
func (r *Runner) execute(run *domain.Run, snapshot domain.IssueSnapshot, mapping domain.RepositoryMapping, runDir string) {
	defer r.wg.Done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	untrack := r.trackCancel(run.PublicID, cancel)
	defer untrack()

	runLog, err := r.logger.WithRun(run.PublicID, string(run.Stage))
	if err != nil {
		r.logger.Base().Error("run log unavailable", "run_id", run.PublicID, "error", err)
	}
	if runLog != nil {
		defer runLog.Close()
	}

	out := r.pipeline(ctx, run, snapshot, mapping, runLog)
	r.settle(ctx, run, out, runLog)
}

// pipeline drives the state machine from preparing to the last implemented
// stage. Every returned outcome is a terminal state unless State is empty.
func (r *Runner) pipeline(ctx context.Context, run *domain.Run, snapshot domain.IssueSnapshot, mapping domain.RepositoryMapping, log *logging.RunLog) outcome {
	if out := r.stopRequested(ctx, run); out != nil {
		return *out
	}
	if out := r.advance(ctx, run, domain.StatePreparing, domain.StageCreated, domain.Conditions{}, nil, log, "connectivity checks are starting"); out != nil {
		return *out
	}

	workspace, out := r.prepare(ctx, run, mapping, log)
	if out != nil {
		return *out
	}

	if out := r.stopRequested(ctx, run); out != nil {
		return *out
	}
	if out := r.advance(ctx, run, domain.StateImplementing, domain.StageWorkspaceCreated, domain.Conditions{}, nil, log, "the agent is being invoked"); out != nil {
		return *out
	}

	invocation, out := r.invoke(ctx, run, snapshot, mapping, workspace, 1, "implement", "", log)
	if out != nil {
		return *out
	}

	if out := r.stopRequested(ctx, run); out != nil {
		return *out
	}
	if out := r.advance(ctx, run, domain.StateValidating, domain.AgentInvokedStage(1), domain.Conditions{}, nil, log, "the worker is inspecting the change"); out != nil {
		return *out
	}

	return r.validateAndCommit(ctx, run, snapshot, mapping, workspace, invocation, 1, log)
}

// settle writes the terminal state. A parked outcome (State empty) leaves the
// run where it is and records why.
func (r *Runner) settle(ctx context.Context, run *domain.Run, out outcome, log *logging.RunLog) {
	// The terminal write must land even though the run's context is cancelled.
	writeCtx, cancel := durableContext(ctx)
	defer cancel()
	ctx = writeCtx

	if out.State == "" {
		r.logger.Base().Warn("run parked before publishing",
			"run_id", run.PublicID, "stage", run.Stage, "state", run.State)
		_, _ = r.store.AppendEvent(ctx, run.ID, domain.RunEvent{
			Type:     domain.EventWarning,
			Severity: domain.SeverityWarn,
			Message:  "publishing is not implemented in this build: the commit is local and nothing was pushed",
		})
		return
	}

	// reporting may not move to cancelled; if the cancel arrived that late, the
	// report is already written and completed is the truthful outcome.
	if out.State == domain.StateCancelled && run.State == domain.StateReporting {
		out.State = domain.StateCompleted
		out.Failure = nil
		r.event(ctx, run, domain.RunEvent{
			Type:     domain.EventWarning,
			Severity: domain.SeverityWarn,
			Message:  "the cancel request arrived after the report was written; the run completed and is recorded as such",
		})
	}

	stage := out.Stage
	if stage == "" {
		stage = run.Stage
	}
	event := domain.RunEvent{
		Type:     domain.EventState,
		Severity: domain.SeverityInfo,
		Message:  fmt.Sprintf("run ended %s at %s", out.State, stage),
		Data:     fmt.Sprintf(`{"state":%q,"stage":%q}`, out.State, stage),
	}
	transition := store.Transition{
		To:         out.State,
		Stage:      stage,
		Conditions: out.Conditions,
		Failure:    out.Failure,
		Event:      event,
	}
	if out.State == domain.StateCancelled {
		transition.Conditions.CancelRequested = true
	}
	if out.Failure != nil {
		transition.Event.Severity = domain.SeverityWarn
		if out.Failure.Message != "" {
			transition.Event.Message = fmt.Sprintf("run ended %s at %s: %s", out.State, stage, out.Failure.Message)
		}
	}
	// The report is persisted before the terminal state becomes observable, so
	// a reader that sees a finished run can always read its report (FR-044).
	r.persistFallbackReport(ctx, run, out)
	if err := r.store.TransitionRun(ctx, run.ID, transition); err != nil {
		r.logger.Base().Error("could not record the terminal state",
			"run_id", run.PublicID, "state", out.State, "error", err)
		return
	}
	run.State = out.State
	run.Stage = stage
	if out.Failure != nil {
		r.logger.Base().Info("run failed",
			"run_id", run.PublicID, "code", out.Failure.Code, "stage", stage,
			"message", out.Failure.Message)
	}
}

// outcome is the result of one pipeline phase: a terminal state, or empty when
// the run stops at an unimplemented seam.
type outcome struct {
	State      domain.RunState
	Stage      domain.Stage
	Failure    *domain.Failure
	Conditions domain.Conditions
}

func failureOutcome(code domain.FailureCode, message string, stage domain.Stage) outcome {
	failure := domain.NewFailure(code, message, stage)
	return outcome{State: failure.Code.TerminalState(), Stage: stage, Failure: &failure}
}

// advance moves the run to a new state and records the transition. A nil
// return means the move succeeded.
func (r *Runner) advance(ctx context.Context, run *domain.Run, to domain.RunState, stage domain.Stage, conditions domain.Conditions, failure *domain.Failure, log *logging.RunLog, message string) *outcome {
	writeCtx, cancel := durableContext(ctx)
	defer cancel()

	event := domain.RunEvent{
		Type:     domain.EventState,
		Severity: domain.SeverityInfo,
		Message:  message,
		Data:     fmt.Sprintf(`{"state":%q,"stage":%q}`, to, stage),
	}
	if err := r.store.TransitionRun(writeCtx, run.ID, store.Transition{
		To:         to,
		Stage:      stage,
		Conditions: conditions,
		Failure:    failure,
		Event:      event,
	}); err != nil {
		if log != nil {
			log.Error("state transition failed", "from", run.State, "to", to, "error", err)
		}
		out := failureOutcome(domain.FailAgentError, fmt.Sprintf("The worker could not record the transition to %s: %v", to, err), stage)
		return &out
	}
	run.State = to
	run.Stage = stage
	if log != nil {
		log.WithStage(string(stage)).Info("state transition", "state", string(to))
	}
	return nil
}

// stage records progress inside the current state.
func (r *Runner) stage(ctx context.Context, run *domain.Run, stage domain.Stage, event domain.RunEvent, log *logging.RunLog) error {
	writeCtx, cancel := durableContext(ctx)
	defer cancel()

	run.Stage = stage
	if event.Type == "" {
		event.Type = domain.EventStage
	}
	if !strings.Contains(event.Data, `"stage"`) {
		event.Data = fmt.Sprintf(`{"stage":%q}`, stage)
	}
	if err := r.store.SetStage(writeCtx, run.ID, stage, event); err != nil {
		if log != nil {
			log.Warn("could not record the stage", "stage", stage, "error", err)
		}
		return err
	}
	if log != nil {
		log.WithStage(string(stage)).Info("stage", "message", event.Message)
	}
	return nil
}

func (r *Runner) event(ctx context.Context, run *domain.Run, event domain.RunEvent) {
	writeCtx, cancel := durableContext(ctx)
	defer cancel()
	if _, err := r.store.AppendEvent(writeCtx, run.ID, event); err != nil {
		r.logger.Base().Warn("could not append a run event",
			"run_id", run.PublicID, "error", err)
	}
}

// prepareRunDir is <workspace_root>/.runs/<run_id>/: run artifacts live
// outside the worktree so they can never be committed.
func (r *Runner) prepareRunDir(publicID string) (string, error) {
	root := r.config.Data.WorkspaceRoot
	dir := filepath.Join(root, ".runs", publicID)
	if err := git.EnsureWithin(root, dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

func (r *Runner) saveConfigProfile(ctx context.Context) (*int64, error) {
	hash := r.config.ContentHash()
	if hash == "" {
		return nil, nil
	}
	content, err := json.Marshal(r.config.RedactedDocument())
	if err != nil {
		return nil, fmt.Errorf("runner: encode configuration profile: %w", err)
	}
	profile := &domain.ConfigurationProfile{
		ContentHash:   hash,
		SchemaVersion: r.config.SchemaVersion,
		SourcePath:    r.config.SourcePath,
		Content:       string(content),
		LoadedAt:      r.config.LoadedAt,
	}
	id, err := r.store.SaveConfigurationProfile(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("runner: save configuration profile: %w", err)
	}
	return &id, nil
}

func (r *Runner) redact(text string) string {
	if r.logger == nil || r.logger.Redactor() == nil {
		return text
	}
	return r.logger.Redactor().Redact(text)
}

func (r *Runner) baseLogger() *slog.Logger {
	if r.logger == nil {
		return slog.Default()
	}
	return r.logger.Base()
}

// BranchName is the dedicated branch for one issue (FR-018).
func BranchName(issueID int) string {
	return fmt.Sprintf("agent/issue-%d", issueID)
}

// resolveAgentSettings freezes the agent policy for this process and gives it
// a stable identifier (T081).
func resolveAgentSettings(cfg *config.Config) agentSettings {
	settings := agentSettings{
		Adapter:    strings.TrimSpace(cfg.Agent.Adapter),
		Model:      strings.TrimSpace(cfg.Agent.Model),
		Profile:    strings.TrimSpace(cfg.Agent.Profile),
		Timeout:    time.Duration(cfg.Agent.TimeoutMinutes) * time.Minute,
		MaxRepairs: cfg.Agent.MaxRepairs,
	}
	if settings.Timeout <= 0 {
		settings.Timeout = 45 * time.Minute
	}
	if settings.MaxRepairs < 0 {
		settings.MaxRepairs = 0
	}
	payload, _ := json.Marshal(map[string]any{
		"adapter":         settings.Adapter,
		"model":           settings.Model,
		"profile":         settings.Profile,
		"timeout_minutes": cfg.Agent.TimeoutMinutes,
		"max_repairs":     settings.MaxRepairs,
		"policy_version":  agent.PolicyVersion,
	})
	sum := sha256.Sum256(payload)
	settings.ConfigID = hex.EncodeToString(sum[:])
	return settings
}

// filterComments narrows the comments sent to the agent when the operator
// selected specific journal entries. An empty selection keeps them all.
func filterComments(comments []domain.Comment, ids []int) []domain.Comment {
	if len(ids) == 0 {
		return comments
	}
	keep := make(map[int]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	filtered := make([]domain.Comment, 0, len(comments))
	for _, comment := range comments {
		if keep[comment.ID] {
			filtered = append(filtered, comment)
		}
	}
	return filtered
}
