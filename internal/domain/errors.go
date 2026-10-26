package domain

import (
	"errors"
	"fmt"
)

// FailureCode is the machine-readable reason a run stopped (data-model §10).
type FailureCode string

const (
	FailIssueNotFound          FailureCode = "issue_not_found"
	FailIssueInaccessible      FailureCode = "issue_inaccessible"
	FailIssueInputInvalid      FailureCode = "issue_input_invalid"
	FailNoRepositoryMapping    FailureCode = "no_repository_mapping"
	FailRepositoryAmbiguous    FailureCode = "repository_ambiguous"
	FailConfigInvalid          FailureCode = "config_invalid"
	FailConnectivityRedmine    FailureCode = "connectivity_redmine"
	FailConnectivityGitlab     FailureCode = "connectivity_gitlab"
	FailConnectivityRepository FailureCode = "connectivity_repository"
	FailAgentUnavailable       FailureCode = "agent_unavailable"
	FailWorkspaceFailed        FailureCode = "workspace_failed"
	FailAgentError             FailureCode = "agent_error"
	FailAgentTimeout           FailureCode = "agent_timeout"
	FailAgentBlocked           FailureCode = "agent_blocked"
	FailPolicyViolation        FailureCode = "policy_violation"
	FailNoChanges              FailureCode = "no_changes"
	FailValidationFailed       FailureCode = "validation_failed"
	FailNoChecksConfigured     FailureCode = "no_checks_configured"
	FailPushRejected           FailureCode = "push_rejected"
	FailMergeRequestFailed     FailureCode = "merge_request_failed"
	FailCancelled              FailureCode = "cancelled"
	FailInterrupted            FailureCode = "interrupted"
)

// FailureCodes is every code in data-model §10.
var FailureCodes = []FailureCode{
	FailIssueNotFound, FailIssueInaccessible, FailIssueInputInvalid,
	FailNoRepositoryMapping, FailRepositoryAmbiguous, FailConfigInvalid,
	FailConnectivityRedmine, FailConnectivityGitlab, FailConnectivityRepository,
	FailAgentUnavailable, FailWorkspaceFailed, FailAgentError, FailAgentTimeout,
	FailAgentBlocked, FailPolicyViolation, FailNoChanges, FailValidationFailed,
	FailNoChecksConfigured, FailPushRejected, FailMergeRequestFailed,
	FailCancelled, FailInterrupted,
}

// Valid reports whether c is a known failure code.
func (c FailureCode) Valid() bool {
	for _, candidate := range FailureCodes {
		if candidate == c {
			return true
		}
	}
	return false
}

// TerminalState is the state a run ends in when it fails with code c.
func (c FailureCode) TerminalState() RunState {
	switch c {
	case FailNoRepositoryMapping, FailRepositoryAmbiguous, FailAgentBlocked:
		return StateBlocked
	case FailCancelled:
		return StateCancelled
	case FailInterrupted:
		return StateInterrupted
	default:
		return StateFailed
	}
}

// Failure is a diagnosable run stop: what happened, where, and what to do.
type Failure struct {
	Code        FailureCode
	Message     string
	Stage       Stage
	Remediation string
}

func (f Failure) Error() string {
	if f.Stage != "" {
		return fmt.Sprintf("%s at %s: %s", f.Code, f.Stage, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.Code, f.Message)
}

// NewFailure builds a failure, filling the standard remediation when the
// caller has nothing more specific to say.
func NewFailure(code FailureCode, message string, stage Stage) Failure {
	return Failure{Code: code, Message: message, Stage: stage, Remediation: DefaultRemediation(code)}
}

// DefaultRemediation is operator-facing guidance rendered by the UI so a
// failed run is diagnosable without opening logs (SC-011).
func DefaultRemediation(code FailureCode) string {
	switch code {
	case FailIssueNotFound, FailIssueInaccessible:
		return "Check the issue number and that the configured Redmine credential can read that project."
	case FailIssueInputInvalid:
		return "Enter a positive integer issue number."
	case FailNoRepositoryMapping:
		return "Add the source-code path to repositories in config.yaml and restart, or enter a configured path in the panel."
	case FailRepositoryAmbiguous:
		return "Set is_default: true on one repository, or choose a source-code path explicitly in the panel."
	case FailConfigInvalid:
		return "Run `redmine-agent doctor` to see which configuration key is invalid."
	case FailConnectivityRedmine:
		return "Verify redmine.base_url, the VPN path, and that the api_key reference resolves."
	case FailConnectivityGitlab:
		return "Verify gitlab.base_url, the VPN path, and that the token reference resolves."
	case FailConnectivityRepository:
		return "Confirm the token may fetch the mapped repository path in GitLab."
	case FailAgentUnavailable:
		return "Install the agent CLI, or point agent.command at an executable on PATH."
	case FailWorkspaceFailed:
		return "Check workspace_root permissions, disk space, and that git is installed."
	case FailAgentError:
		return "Open the run transcript; the agent exited without producing a usable result."
	case FailAgentTimeout:
		return "Raise agent.timeout_minutes, or narrow the issue."
	case FailAgentBlocked:
		return "Answer the agent's blockers on the issue, then retry."
	case FailPolicyViolation:
		return "Inspect the transcript: the agent tried to leave its workspace. Review the policy and the issue text."
	case FailNoChanges:
		return "The agent made no change. Refine the issue or retry with more context."
	case FailValidationFailed:
		return "Open the failing check output, fix the cause, and retry."
	case FailNoChecksConfigured:
		return "Configure at least one required check for this repository, or disable publishing for it."
	case FailPushRejected:
		return "Confirm the token may push branches, and that branch protection allows agent/* branches."
	case FailMergeRequestFailed:
		return "The branch was pushed but the merge request failed; retry to reuse the branch, or open it by hand."
	case FailCancelled:
		return "Start a new run when you are ready."
	case FailInterrupted:
		return "The worker stopped mid-run. Review the workspace, then retry explicitly."
	default:
		return ""
	}
}

// UpstreamKind identifies which external system a connectivity failure is about.
type UpstreamKind string

const (
	UpstreamRedmine    UpstreamKind = "redmine"
	UpstreamGitlab     UpstreamKind = "gitlab"
	UpstreamRepository UpstreamKind = "repository"
)

// UpstreamError wraps an error from an external system.
type UpstreamError struct {
	Kind UpstreamKind
	Op   string
	Err  error
}

func (e *UpstreamError) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("%s %s: %v", e.Kind, e.Op, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// ConnectivityFailure maps an upstream error to the matching connectivity code.
func ConnectivityFailure(err error) Failure {
	code := FailConnectivityRepository
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		switch upstream.Kind {
		case UpstreamRedmine:
			code = FailConnectivityRedmine
		case UpstreamGitlab:
			code = FailConnectivityGitlab
		case UpstreamRepository:
			code = FailConnectivityRepository
		}
	}
	return OwnershipFailure(code, err)
}

// PushRejectedFailure maps a rejected git push. The local commit stays.
func PushRejectedFailure(err error) Failure {
	return OwnershipFailure(FailPushRejected, err)
}

// MergeRequestFailure maps a failed merge-request creation or reuse.
func MergeRequestFailure(err error) Failure {
	return OwnershipFailure(FailMergeRequestFailed, err)
}

func OwnershipFailure(code FailureCode, err error) Failure {
	msg := "operation failed"
	if err != nil {
		msg = err.Error()
	}
	return Failure{Code: code, Message: msg, Remediation: DefaultRemediation(code)}
}
