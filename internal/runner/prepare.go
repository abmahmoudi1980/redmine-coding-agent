package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// prepare proves every dependency is reachable before touching the filesystem,
// then creates the isolated worktree (FR-016 – FR-020).
func (r *Runner) prepare(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, log *logging.RunLog) (*domain.Workspace, *outcome) {
	if out := r.connectivityRedmine(ctx, run, log); out != nil {
		return nil, out
	}
	project, out := r.connectivityGitLab(ctx, run, mapping, log)
	if out != nil {
		return nil, out
	}
	if out := r.connectivityRepository(ctx, run, mapping, project, log); out != nil {
		return nil, out
	}
	if out := r.connectivityAgent(ctx, run, log); out != nil {
		return nil, out
	}
	return r.createWorkspace(ctx, run, mapping, log)
}

func (r *Runner) connectivityRedmine(ctx context.Context, run *domain.Run, log *logging.RunLog) *outcome {
	_ = r.stage(ctx, run, domain.StageConnectivityRedmine, domain.RunEvent{
		Type:    domain.EventStage,
		Message: "checking that Redmine answers with the configured credential",
	}, log)

	if r.redmine == nil {
		out := failureOutcome(domain.FailConnectivityRedmine, "No Redmine client is configured.", domain.StageConnectivityRedmine)
		return &out
	}
	if _, err := r.redmine.GetIssue(ctx, run.IssueID); err != nil {
		failure := redmine.FailureFor(err, run.IssueID)
		out := outcome{State: failure.Code.TerminalState(), Stage: domain.StageConnectivityRedmine, Failure: &failure}
		return &out
	}
	return nil
}

func (r *Runner) connectivityGitLab(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, log *logging.RunLog) (*gitlab.Project, *outcome) {
	_ = r.stage(ctx, run, domain.StageConnectivityGitlab, domain.RunEvent{
		Type:    domain.EventStage,
		Message: "checking that GitLab answers for " + mapping.RepositoryPath,
	}, log)

	if r.gitlab == nil {
		out := failureOutcome(domain.FailConnectivityGitlab, "No GitLab client is configured.", domain.StageConnectivityGitlab)
		return nil, &out
	}
	project, err := r.gitlab.GetProject(ctx, mapping.RepositoryPath)
	if err != nil {
		failure := gitlab.FailureFor(err)
		out := outcome{State: failure.Code.TerminalState(), Stage: domain.StageConnectivityGitlab, Failure: &failure}
		return nil, &out
	}
	return project, nil
}

func (r *Runner) connectivityRepository(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, project *gitlab.Project, log *logging.RunLog) *outcome {
	_ = r.stage(ctx, run, domain.StageConnectivityRepository, domain.RunEvent{
		Type:    domain.EventStage,
		Message: "checking that the repository can be fetched with the configured credential",
	}, log)

	remoteURL := strings.TrimSpace(project.HTTPURL)
	if remoteURL == "" {
		out := failureOutcome(domain.FailConnectivityRepository,
			"GitLab did not report an HTTP clone URL for "+mapping.RepositoryPath+".",
			domain.StageConnectivityRepository)
		return &out
	}

	r.configureGitAuth()
	if _, err := r.git.EnsureMirror(ctx, mapping.ID, remoteURL); err != nil {
		failure := domain.ConnectivityFailure(&domain.UpstreamError{
			Kind: domain.UpstreamRepository,
			Op:   "fetch " + mapping.RepositoryPath,
			Err:  err,
		})
		failure.Stage = domain.StageConnectivityRepository
		out := outcome{State: failure.Code.TerminalState(), Stage: domain.StageConnectivityRepository, Failure: &failure}
		return &out
	}
	return nil
}

func (r *Runner) connectivityAgent(ctx context.Context, run *domain.Run, log *logging.RunLog) *outcome {
	_ = r.stage(ctx, run, domain.StageConnectivityAgent, domain.RunEvent{
		Type:    domain.EventStage,
		Message: "checking that the coding agent is available",
	}, log)

	adapter, err := r.adapters.Lookup(r.agent.Adapter)
	if err != nil {
		out := failureOutcome(domain.FailAgentUnavailable,
			fmt.Sprintf("The configured agent adapter %q is not registered.", r.agent.Adapter),
			domain.StageConnectivityAgent)
		return &out
	}
	probe, err := adapter.Probe(ctx, agent.Config{
		Command:       r.config.Agent.Command,
		Model:         r.agent.Model,
		AgentName:     r.agent.Profile,
		WorkspaceRoot: r.config.Data.WorkspaceRoot,
		Redactor:      r.redactorOrNil(),
	})
	if err != nil {
		out := failureOutcome(domain.FailAgentUnavailable, err.Error(), domain.StageConnectivityAgent)
		return &out
	}
	if !probe.Available {
		message := probe.Detail
		if message == "" {
			message = fmt.Sprintf("%s is not usable", r.config.Agent.Command)
		}
		out := failureOutcome(domain.FailAgentUnavailable, message, domain.StageConnectivityAgent)
		return &out
	}
	return nil
}

func (r *Runner) createWorkspace(ctx context.Context, run *domain.Run, mapping domain.RepositoryMapping, log *logging.RunLog) (*domain.Workspace, *outcome) {
	workspacePath, err := git.ResolveWorkspacePath(r.config.Data.WorkspaceRoot, run.PublicID)
	if err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageWorkspaceCreated)
		return nil, &out
	}
	if err := r.git.AddWorktree(ctx, mapping.ID, workspacePath, run.Branch, mapping.BaseBranch); err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageWorkspaceCreated)
		return nil, &out
	}

	baseRevision, err := r.git.Head(ctx, workspacePath)
	if err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageWorkspaceCreated)
		return nil, &out
	}
	gitDir, err := r.git.WorktreeGitDir(ctx, workspacePath)
	if err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageWorkspaceCreated)
		return nil, &out
	}

	workspace := &domain.Workspace{
		RunID:        run.ID,
		Path:         workspacePath,
		GitDir:       gitDir,
		Branch:       run.Branch,
		BaseRevision: baseRevision,
		ChangedPaths: nil,
		CreatedAt:    run.CreatedAt,
	}
	if _, err := r.store.CreateWorkspace(ctx, workspace); err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageWorkspaceCreated)
		return nil, &out
	}

	branch := run.Branch
	base := mapping.BaseBranch
	if err := r.store.UpdateRun(ctx, run.ID, store.RunPatch{
		BaseBranch:   &base,
		BaseRevision: &baseRevision,
		Branch:       &branch,
	}); err != nil {
		if log != nil {
			log.Warn("could not record the base revision", "error", err)
		}
	}
	run.BaseBranch = base
	run.BaseRevision = baseRevision
	run.Branch = branch

	if err := r.stage(ctx, run, domain.StageWorkspaceCreated, domain.RunEvent{
		Type:    domain.EventWorkspace,
		Message: fmt.Sprintf("workspace %s on branch %s from %s", workspacePath, branch, baseRevision),
		Data:    `{"baseRevision":"` + baseRevision + `"}`,
	}, log); err != nil {
		out := failureOutcome(domain.FailWorkspaceFailed, err.Error(), domain.StageWorkspaceCreated)
		return nil, &out
	}
	return workspace, nil
}

// configureGitAuth supplies the GitLab token to git over the environment, so it
// never appears in argv or in a repository config file (FR-056).
func (r *Runner) configureGitAuth() {
	if r.config == nil || r.git == nil {
		return
	}
	token, err := r.config.GitLab.Token.Resolve()
	if err != nil {
		return
	}
	r.registerSecret(token)
	r.git.SetHTTPAuth("oauth2", token)
}

func (r *Runner) registerSecret(value string) {
	if r.logger == nil || r.logger.Redactor() == nil || value == "" {
		return
	}
	r.logger.Redactor().Register(value)
}

func (r *Runner) redactorOrNil() agent.Redactor {
	if r.logger == nil || r.logger.Redactor() == nil {
		return nil
	}
	return r.logger.Redactor()
}

// collectAttachments downloads the issue's files before the snapshot is
// persisted, recording each failure instead of silently omitting it (FR-007).
func (r *Runner) collectAttachments(ctx context.Context, snapshot *domain.IssueSnapshot, runDir string) {
	if r.redmine == nil || len(snapshot.Attachments) == 0 {
		return
	}
	attachments := make([]redmine.Attachment, 0, len(snapshot.Attachments))
	for _, attachment := range snapshot.Attachments {
		attachments = append(attachments, redmine.Attachment{
			ID:         attachment.ID,
			Filename:   attachment.Filename,
			ContentURL: attachment.ContentURL,
			Filesize:   attachment.Size,
		})
	}

	results, err := r.redmine.DownloadAttachments(ctx, attachments, filepath.Join(runDir, "attachments"))
	if err != nil {
		for index := range snapshot.Attachments {
			snapshot.Attachments[index].Available = false
			snapshot.Attachments[index].Reason = err.Error()
		}
		return
	}
	for index := range snapshot.Attachments {
		if index >= len(results) {
			break
		}
		snapshot.Attachments[index].Available = results[index].Available
		snapshot.Attachments[index].Path = results[index].Path
		if !results[index].Available {
			snapshot.Attachments[index].Reason = results[index].Reason
		}
		if results[index].Available {
			snapshot.Attachments[index].Filename = filepath.Base(results[index].Path)
		}
	}
}
