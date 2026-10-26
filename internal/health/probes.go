package health

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/git"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// MinFreeBytes is the floor below which the disk probe fails: a worker that
// cannot write a workspace cannot finish a run either.
const MinFreeBytes = 512 << 20

// Remediation strings are the actionable next step FR-017 requires.
const (
	remediationDatabase = "Check data.database, that its directory is writable, and that the worker can apply migrations."
	remediationRedmine  = "Check redmine.base_url and that the credential resolves and can read a project."
	remediationGitLab   = "Check gitlab.base_url and that the token can see the mapped project."
	remediationRepo     = "Check that the repository path is mapped, exists in GitLab, and that git can fetch it with the configured credential."
	remediationAgent    = "Install the configured agent command or fix agent.command, then restart the worker."
	remediationDisk     = "Free space on the volume that holds data.workspace_root and data.database."
)

// Deps are the collaborators the default probes use. A nil collaborator turns
// its probe into "unknown" rather than a failure.
type Deps struct {
	Store    *store.Store
	Config   *config.Config
	Redmine  *redmine.Client
	GitLab   *gitlab.Client
	Git      *git.Manager
	Adapters *agent.Registry
	Redactor Redactor

	// FreeSpace and AgentVersion exist so tests can probe without a real disk
	// or a real coding agent.
	FreeSpace    func(path string) (uint64, error)
	AgentVersion func(ctx context.Context) (string, error)
}

// DefaultProbes returns the components the operator sees, in the documented
// order: database, redmine, gitlab, repository, agent, disk.
func DefaultProbes(deps Deps) []Probe {
	return []Probe{
		databaseProbe(deps),
		redmineProbe(deps),
		gitlabProbe(deps),
		repositoryProbe(deps),
		agentProbe(deps),
		diskProbe(deps),
	}
}

// New builds the production checker from its collaborators.
func New(deps Deps) *Checker {
	return NewChecker(Options{Probes: DefaultProbes(deps), Redactor: deps.Redactor})
}

// ByName finds one probe in a list, which is how the doctor runs them one at a
// time in its documented order.
func ByName(probes []Probe, name string) (Probe, bool) {
	for _, probe := range probes {
		if probe.Name == name {
			return probe, true
		}
	}
	return Probe{}, false
}

func databaseProbe(deps Deps) Probe {
	return Probe{
		Name:        "database",
		Remediation: remediationDatabase,
		Check: func(ctx context.Context) (string, error) {
			if deps.Store == nil {
				return "", fmt.Errorf("%w: no database is attached to this worker", ErrNotConfigured)
			}
			version, err := deps.Store.SchemaVersion(ctx)
			if err != nil {
				return "", fmt.Errorf("the database could not be read: %w", err)
			}
			want := store.LatestSchemaVersion()
			if version != want {
				return "", fmt.Errorf("migration state is %d but this build expects %d", version, want)
			}
			rollback := errors.New("health: probe rollback")
			if err := deps.Store.InTx(ctx, func(*store.Tx) error { return rollback }); err != nil && !errors.Is(err, rollback) {
				return "", fmt.Errorf("the database is not writable: %w", err)
			}
			return fmt.Sprintf("schema version %d applied; the database is writable", version), nil
		},
	}
}

func redmineProbe(deps Deps) Probe {
	return Probe{
		Name:        "redmine",
		Deep:        true,
		Remediation: remediationRedmine,
		Check: func(ctx context.Context) (string, error) {
			if deps.Redmine == nil {
				return "", fmt.Errorf("%w: no Redmine client is attached to this worker", ErrNotConfigured)
			}
			mapping, mappingErr := firstMapping(ctx, deps.Store)
			if mappingErr == nil {
				project, err := deps.Redmine.GetProject(ctx, mapping.RedmineProject)
				if err != nil {
					return "", err
				}
				identifier := project.Identifier
				if identifier == "" {
					identifier = mapping.RedmineProject
				}
				return fmt.Sprintf("the credential read project %s", identifier), nil
			}
			if !errors.Is(mappingErr, ErrNotConfigured) {
				return "", mappingErr
			}
			login, err := deps.Redmine.Probe(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("the credential authenticated as %s", login), nil
		},
	}
}

func gitlabProbe(deps Deps) Probe {
	return Probe{
		Name:        "gitlab",
		Deep:        true,
		Remediation: remediationGitLab,
		Check: func(ctx context.Context) (string, error) {
			if deps.GitLab == nil {
				return "", fmt.Errorf("%w: no GitLab client is attached to this worker", ErrNotConfigured)
			}
			mapping, err := firstMapping(ctx, deps.Store)
			if err != nil {
				return "", err
			}
			project, err := deps.GitLab.GetProject(ctx, mapping.RepositoryPath)
			if err != nil {
				return "", errors.New(gitlab.FailureFor(err).Message)
			}
			return fmt.Sprintf("%s answered as project %d", mapping.RepositoryPath, project.ID), nil
		},
	}
}

func repositoryProbe(deps Deps) Probe {
	return Probe{
		Name:        "repository",
		Deep:        true,
		Remediation: remediationRepo,
		Check: func(ctx context.Context) (string, error) {
			if deps.Git == nil {
				return "", fmt.Errorf("%w: no git manager is attached to this worker", ErrNotConfigured)
			}
			if deps.GitLab == nil {
				return "", fmt.Errorf("%w: no GitLab client is attached to this worker", ErrNotConfigured)
			}
			mapping, err := firstMapping(ctx, deps.Store)
			if err != nil {
				return "", err
			}
			project, err := deps.GitLab.GetProject(ctx, mapping.RepositoryPath)
			if err != nil {
				return "", errors.New(gitlab.FailureFor(err).Message)
			}
			branches, err := deps.Git.CheckRemote(ctx, project.HTTPURL)
			if err != nil {
				return "", fmt.Errorf("git cannot read %s: %w", mapping.RepositoryPath, err)
			}
			return fmt.Sprintf("%d branch heads are readable from %s", branches, mapping.RepositoryPath), nil
		},
	}
}

func agentProbe(deps Deps) Probe {
	return Probe{
		Name:        "agent",
		Remediation: remediationAgent,
		Check: func(ctx context.Context) (string, error) {
			if deps.AgentVersion != nil {
				version, err := deps.AgentVersion(ctx)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("the agent answered %s", version), nil
			}
			if deps.Adapters == nil || deps.Config == nil {
				return "", fmt.Errorf("%w: no coding agent is attached to this worker", ErrNotConfigured)
			}
			adapter, err := deps.Adapters.Lookup(deps.Config.Agent.Adapter)
			if err != nil {
				return "", fmt.Errorf("the configured adapter %q is not registered", deps.Config.Agent.Adapter)
			}
			probe, err := adapter.Probe(ctx, agent.Config{
				Command:       deps.Config.Agent.Command,
				Model:         deps.Config.Agent.Model,
				AgentName:     deps.Config.Agent.Profile,
				WorkspaceRoot: deps.Config.Data.WorkspaceRoot,
				Redactor:      deps.Redactor,
			})
			if err != nil {
				return "", err
			}
			if !probe.Available {
				if probe.Detail == "" {
					return "", fmt.Errorf("%s is not usable", deps.Config.Agent.Command)
				}
				return "", errors.New(probe.Detail)
			}
			version := strings.TrimSpace(probe.Version)
			if version == "" {
				version = "a version"
			}
			return fmt.Sprintf("%s answered %s", deps.Config.Agent.Command, version), nil
		},
	}
}

func diskProbe(deps Deps) Probe {
	return Probe{
		Name:        "disk",
		Remediation: remediationDisk,
		Check: func(ctx context.Context) (string, error) {
			path := diskProbePath(deps.Config)
			free := deps.FreeSpace
			if free == nil {
				free = freeSpace
			}
			available, err := free(path)
			if err != nil {
				return "", fmt.Errorf("free space at %s could not be read: %w", path, err)
			}
			if available < MinFreeBytes {
				return "", fmt.Errorf("only %s is free at %s", FormatBytes(available), path)
			}
			return fmt.Sprintf("%s free at %s", FormatBytes(available), path), nil
		},
	}
}

func diskProbePath(cfg *config.Config) string {
	if cfg == nil {
		return "."
	}
	if root := strings.TrimSpace(cfg.Data.WorkspaceRoot); root != "" {
		return root
	}
	if database := strings.TrimSpace(cfg.Data.Database); database != "" {
		return filepath.Dir(database)
	}
	return "."
}

func firstMapping(ctx context.Context, st *store.Store) (domain.RepositoryMapping, error) {
	if st == nil {
		return domain.RepositoryMapping{}, fmt.Errorf("%w: no repository table is attached to this worker", ErrNotConfigured)
	}
	mappings, err := st.ListRepositories(ctx)
	if err != nil {
		return domain.RepositoryMapping{}, fmt.Errorf("repository mappings could not be read: %w", err)
	}
	for _, mapping := range mappings {
		if mapping.Enabled {
			return mapping, nil
		}
	}
	return domain.RepositoryMapping{}, fmt.Errorf("%w: no repository mapping is enabled in config.yaml", ErrNotConfigured)
}
