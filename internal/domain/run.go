// Package domain contains the pure types, run state machine, and failure
// vocabulary shared by the orchestrator, the store, and the HTTP API.
package domain

import (
	"strconv"
	"time"
)

// TimeLayout is the canonical timestamp format: UTC RFC 3339 with milliseconds.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders a timestamp in the canonical storage format.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// ParseTime reads any RFC 3339 timestamp, including the canonical form.
func ParseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// RunState is the coarse, persisted state of one attempt at one issue.
type RunState string

const (
	StateQueued       RunState = "queued"
	StatePreparing    RunState = "preparing"
	StateImplementing RunState = "implementing"
	StateValidating   RunState = "validating"
	StateRepairing    RunState = "repairing"
	StatePublishing   RunState = "publishing"
	StateReporting    RunState = "reporting"
	StateCompleted    RunState = "completed"
	StateFailed       RunState = "failed"
	StateBlocked      RunState = "blocked"
	StateCancelled    RunState = "cancelled"
	StateInterrupted  RunState = "interrupted"
)

// RunStates is the complete set of states, in lifecycle order.
var RunStates = []RunState{
	StateQueued, StatePreparing, StateImplementing, StateValidating,
	StateRepairing, StatePublishing, StateReporting,
	StateCompleted, StateFailed, StateBlocked, StateCancelled, StateInterrupted,
}

// ActiveRunStates are the non-terminal states. One run per issue may be in one
// of these at a time (FR-010).
var ActiveRunStates = []RunState{
	StateQueued, StatePreparing, StateImplementing, StateRepairing,
	StateValidating, StatePublishing, StateReporting,
}

// Valid reports whether s is a state this system knows about.
func (s RunState) Valid() bool {
	for _, candidate := range RunStates {
		if candidate == s {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transition may be made from s.
func (s RunState) Terminal() bool {
	return IsTerminal(s)
}

func (s RunState) String() string { return string(s) }

// Stage is the fine-grained step recorded alongside the state (FR-050).
type Stage string

const (
	StageCreated                Stage = "created"
	StageConnectivityRedmine    Stage = "connectivity.redmine"
	StageConnectivityGitlab     Stage = "connectivity.gitlab"
	StageConnectivityRepository Stage = "connectivity.repository"
	StageConnectivityAgent      Stage = "connectivity.agent"
	StageWorkspaceCreated       Stage = "workspace.created"
	StageAgentInvoked           Stage = "agent.invoked"
	StageChecksRunning          Stage = "checks.running"
	StageChecksFinished         Stage = "checks.finished"
	StageCommitCreated          Stage = "commit.created"
	StageBranchPushed           Stage = "branch.pushed"
	StageMergeRequestCreated    Stage = "mr.created"
	StageMergeRequestReused     Stage = "mr.reused"
	StageCommentPosted          Stage = "comment.posted"
	StageReportWritten          Stage = "report.written"
	StageInterrupted            Stage = "interrupted"
)

// AgentInvokedStage names the stage of a specific invovation attempt.
func AgentInvokedStage(attempt int) Stage {
	return Stage("agent.invoked:" + strconv.Itoa(attempt))
}

// CheckRunningStage names the stage while one check is executing.
func CheckRunningStage(name string) Stage {
	return Stage("checks.running:" + name)
}

func (s Stage) String() string { return string(s) }

// Outcome is the classification of one configured validation command.
type Outcome string

const (
	OutcomePassed      Outcome = "passed"
	OutcomeFailed      Outcome = "failed"
	OutcomeNotExecuted Outcome = "not_executed"
)

// Valid reports whether o is a known outcome.
func (o Outcome) Valid() bool {
	return o == OutcomePassed || o == OutcomeFailed || o == OutcomeNotExecuted
}

func (o Outcome) String() string { return string(o) }

// EventType is the kind of a run timeline entry.
type EventType string

const (
	EventState     EventType = "state"
	EventStage     EventType = "stage"
	EventActivity  EventType = "activity"
	EventCheck     EventType = "check"
	EventAgent     EventType = "agent"
	EventWorkspace EventType = "workspace"
	EventPublish   EventType = "publish"
	EventReport    EventType = "report"
	EventWarning   EventType = "warning"
	EventError     EventType = "error"
)

// Severities for run_events.
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Run is one attempt at implementing one issue.
type Run struct {
	ID                int64       `json:"-"`
	PublicID          string      `json:"runId"`
	IssueID           int         `json:"issueId"`
	IssueSubject      string      `json:"issueSubject,omitempty"`
	IssueSnapshotID   int64       `json:"-"`
	RepositoryID      *int64      `json:"-"`
	PreviousRunID     *int64      `json:"-"`
	State             RunState    `json:"state"`
	Stage             Stage       `json:"stage"`
	BaseBranch        string      `json:"baseBranch,omitempty"`
	BaseRevision      string      `json:"baseRevision,omitempty"`
	Branch            string      `json:"branch,omitempty"`
	CommitSHA         string      `json:"commitSha,omitempty"`
	PushedAt          *time.Time  `json:"pushedAt,omitempty"`
	AgentName         string      `json:"-"`
	AgentModel        string      `json:"-"`
	AgentConfigID     string      `json:"-"`
	ConfigProfileID   *int64      `json:"-"`
	RequestedBy       string      `json:"-"`
	CreatedAt         time.Time   `json:"createdAt"`
	StartedAt         *time.Time  `json:"startedAt,omitempty"`
	EndedAt           *time.Time  `json:"endedAt,omitempty"`
	CancelRequestedAt *time.Time  `json:"cancelRequestedAt,omitempty"`
	InterruptedStage  Stage       `json:"interruptedStage,omitempty"`
	FailureCode       FailureCode `json:"failureCode,omitempty"`
	FailureMessage    string      `json:"failureMessage,omitempty"`
	Summary           string      `json:"summary,omitempty"`
}

// IssueSnapshot is the immutable copy of the issue taken at run start.
type IssueSnapshot struct {
	ID                 int64         `json:"-"`
	RedmineIssueID     int           `json:"issueId"`
	Subject            string        `json:"subject"`
	Description        string        `json:"description"`
	AcceptanceCriteria string        `json:"acceptanceCriteria,omitempty"`
	CriteriaSource     string        `json:"criteriaSource"`
	ProjectID          string        `json:"-"`
	ProjectIdentifier  string        `json:"-"`
	ProjectName        string        `json:"-"`
	StatusName         string        `json:"status,omitempty"`
	TrackerName        string        `json:"tracker,omitempty"`
	PriorityName       string        `json:"priority,omitempty"`
	AssigneeName       string        `json:"assignee,omitempty"`
	ParentIssueID      *int          `json:"parentIssueId,omitempty"`
	RelatedIssueIDs    []int         `json:"relatedIssueIds,omitempty"`
	CustomFields       []CustomField `json:"customFields,omitempty"`
	SelectedComments   []Comment     `json:"comments,omitempty"`
	Attachments        []Attachment  `json:"attachments,omitempty"`
	RawJSON            string        `json:"-"`
	ContentHash        string        `json:"contentHash"`
	FetchedAt          time.Time     `json:"fetchedAt"`
}

type CustomField struct {
	ID    int    `json:"id,omitempty"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Comment struct {
	ID        int       `json:"id"`
	Author    string    `json:"author"`
	CreatedOn time.Time `json:"createdOn"`
	Notes     string    `json:"notes"`
}

type Attachment struct {
	ID         int    `json:"id"`
	Filename   string `json:"filename"`
	ContentURL string `json:"-"`
	Size       int64  `json:"size"`
	Available  bool   `json:"available"`
	Reason     string `json:"reason,omitempty"`
	Path       string `json:"-"`
}

// Workspace records the isolated working copy facts for a run.
type Workspace struct {
	ID                 int64      `json:"-"`
	RunID              int64      `json:"-"`
	Path               string     `json:"path"`
	GitDir             string     `json:"-"`
	Branch             string     `json:"branch"`
	BaseRevision       string     `json:"baseRevision"`
	CreatedAt          time.Time  `json:"createdAt"`
	CleanedAt          *time.Time `json:"cleanedAt"`
	HasUnpushedChanges bool       `json:"hasUnpushedChanges"`
	ChangedPaths       []string   `json:"changedPaths,omitempty"`
}

// AgentInvocation is one implementation or repair invocation.
type AgentInvocation struct {
	ID             int64      `json:"-"`
	RunID          int64      `json:"-"`
	AttemptNo      int        `json:"attemptNo"`
	Kind           string     `json:"kind"`
	Adapter        string     `json:"adapter"`
	Model          string     `json:"model"`
	ConfigID       string     `json:"-"`
	PromptPath     string     `json:"-"`
	PromptSHA256   string     `json:"promptSha256"`
	TranscriptPath string     `json:"-"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	EndedAt        *time.Time `json:"endedAt,omitempty"`
	ExitCode       *int       `json:"exitCode"`
	TimedOut       bool       `json:"timedOut"`
	Status         string     `json:"status"`
	ReportedStatus string     `json:"reportedStatus,omitempty"`
	ResultSummary  string     `json:"-"`
}

// ValidationResult is the worker's own verdict on one configured command
// (FR-033, FR-034). The agent can never write one of these.
type ValidationResult struct {
	ID            int64      `json:"-"`
	RunID         int64      `json:"-"`
	InvocationID  *int64     `json:"-"`
	CheckName     string     `json:"checkName"`
	Command       string     `json:"command"`
	Required      bool       `json:"required"`
	Outcome       Outcome    `json:"outcome"`
	ExitCode      *int       `json:"exitCode"`
	DurationMS    int64      `json:"durationMs"`
	OutputExcerpt string     `json:"outputExcerpt,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	ExecutedAt    *time.Time `json:"executedAt,omitempty"`
}

// CheckAttempt groups the validation rows that followed one invocation.
type CheckAttempt struct {
	AttemptNo         int                `json:"attemptNo"`
	InvocationStatus  string             `json:"invocationStatus"`
	Results           []ValidationResult `json:"results"`
	AllRequiredPassed bool               `json:"allRequiredPassed"`
	RepairPrompted    bool               `json:"repairPrompted"`
}

// Report is the final run report and its Redmine delivery state.
type Report struct {
	ID              int64      `json:"-"`
	RunID           int64      `json:"-"`
	BodyMD          string     `json:"bodyMarkdown"`
	CommitSHA       string     `json:"commitSha,omitempty"`
	MergeRequestURL string     `json:"mergeRequestUrl,omitempty"`
	DeliveryStatus  string     `json:"deliveryStatus"`
	DeliveryError   string     `json:"deliveryError,omitempty"`
	DedupeKey       string     `json:"dedupeKey"`
	CreatedAt       time.Time  `json:"createdAt"`
	DeliveredAt     *time.Time `json:"deliveredAt,omitempty"`
}

// ConfigurationProfile is the redacted, hash-identified effective config.
type ConfigurationProfile struct {
	ID            int64     `json:"-"`
	ContentHash   string    `json:"contentHash"`
	SchemaVersion int       `json:"schemaVersion"`
	SourcePath    string    `json:"sourcePath"`
	Content       string    `json:"content"`
	LoadedAt      time.Time `json:"loadedAt"`
}

// RunEvent is one append-only timeline entry.
type RunEvent struct {
	ID       int64     `json:"-"`
	RunID    int64     `json:"-"`
	Seq      int64     `json:"seq"`
	At       time.Time `json:"at"`
	Type     EventType `json:"type"`
	Severity string    `json:"severity"`
	Message  string    `json:"message"`
	Data     string    `json:"data,omitempty"`
}
