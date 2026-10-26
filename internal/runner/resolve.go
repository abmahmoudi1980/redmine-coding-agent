// Package runner holds the orchestration stages. Resolution and snapshot
// building are pure: they never touch the filesystem or a network.
package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// RepositoryLister is the narrow read the resolver needs; *store.Store
// implements it.
type RepositoryLister interface {
	ListRepositories(ctx context.Context) ([]domain.RepositoryMapping, error)
}

var (
	// ErrRepositoryAmbiguous means several repositories match and no default is
	// configured (FR-015).
	ErrRepositoryAmbiguous = errors.New("runner: repository ambiguity requires an explicit choice")
	// ErrNoRepositoryMapping means no usable repository matched (FR-014).
	ErrNoRepositoryMapping = errors.New("runner: no repository mapping matches the selection")
)

// AmbiguousMappingError carries the candidates the operator must choose from.
type AmbiguousMappingError struct {
	Project    string
	Candidates []domain.RepositoryMapping
}

func (e *AmbiguousMappingError) Error() string {
	if e.Project == "" {
		return fmt.Sprintf("runner: %d repositories are configured; choose one explicitly", len(e.Candidates))
	}
	return fmt.Sprintf("runner: project %q maps to %d repositories; choose one explicitly", e.Project, len(e.Candidates))
}

func (e *AmbiguousMappingError) Is(target error) bool { return target == ErrRepositoryAmbiguous }

// NoMappingError names the selection that produced no mapping.
type NoMappingError struct {
	Project        string
	RepositoryID   int64
	RepositoryPath string
}

func (e *NoMappingError) Error() string {
	if e.RepositoryID > 0 {
		return fmt.Sprintf("runner: repository id %d is not a configured, enabled mapping", e.RepositoryID)
	}
	if e.RepositoryPath != "" {
		return fmt.Sprintf("runner: no configured repository matches path %q", e.RepositoryPath)
	}
	if e.Project == "" {
		return "runner: no repository is configured"
	}
	return fmt.Sprintf("runner: no repository mapping is configured for project %q", e.Project)
}

func (e *NoMappingError) Is(target error) bool { return target == ErrNoRepositoryMapping }

// FailureForResolve maps a resolution error to the documented failure code.
func FailureForResolve(err error) domain.Failure {
	switch {
	case errors.Is(err, ErrRepositoryAmbiguous):
		failure := domain.NewFailure(domain.FailRepositoryAmbiguous, err.Error(), "")
		var ambiguous *AmbiguousMappingError
		if errors.As(err, &ambiguous) {
			if ambiguous.Project == "" {
				failure.Message = fmt.Sprintf("%d repositories are configured; choose one explicitly.", len(ambiguous.Candidates))
			} else {
				failure.Message = fmt.Sprintf("Project %q maps to %d repositories; choose one explicitly.", ambiguous.Project, len(ambiguous.Candidates))
			}
		}
		return failure
	case errors.Is(err, ErrNoRepositoryMapping):
		failure := domain.NewFailure(domain.FailNoRepositoryMapping, err.Error(), "")
		var noMapping *NoMappingError
		if errors.As(err, &noMapping) && noMapping.RepositoryID == 0 {
			switch {
			case noMapping.RepositoryPath != "":
				failure.Message = fmt.Sprintf("No configured repository matches path %q.", noMapping.RepositoryPath)
			case noMapping.Project != "":
				failure.Message = fmt.Sprintf("Project %q has no configured repository mapping.", noMapping.Project)
			}
		}
		return failure
	default:
		return domain.NewFailure(domain.FailNoRepositoryMapping, err.Error(), "")
	}
}

// Selection is what the operator asked for. RepositoryID and RepositoryPath
// are explicit choices from the web panel; Project is the issue's project and
// is only used as a fallback for configurations that still map by project.
type Selection struct {
	Project        string
	RepositoryID   int64
	RepositoryPath string
}

// Resolve is the project-only entry point kept for callers that do not have a
// repository path. It is equivalent to ResolveSelection.
func Resolve(ctx context.Context, lister RepositoryLister, project string, explicitRepositoryID int64) (domain.RepositoryMapping, error) {
	return ResolveSelection(ctx, lister, Selection{Project: project, RepositoryID: explicitRepositoryID})
}

// ResolveSelection picks the destination repository and base branch from the
// stored mappings only. It never infers, never falls back to the filesystem,
// and never guesses (FR-014, FR-015).
func ResolveSelection(ctx context.Context, lister RepositoryLister, selection Selection) (domain.RepositoryMapping, error) {
	if lister == nil {
		return domain.RepositoryMapping{}, errors.New("runner: repository source is required")
	}
	mappings, err := lister.ListRepositories(ctx)
	if err != nil {
		return domain.RepositoryMapping{}, fmt.Errorf("runner: list repositories: %w", err)
	}

	// An explicit repository id is the strongest signal: the operator picked
	// the destination in the panel, so the issue's project is irrelevant.
	if selection.RepositoryID > 0 {
		for _, mapping := range mappings {
			if mapping.ID == selection.RepositoryID && mapping.Enabled {
				return mapping, nil
			}
		}
		return domain.RepositoryMapping{}, &NoMappingError{
			Project:        selection.Project,
			RepositoryID:   selection.RepositoryID,
			RepositoryPath: selection.RepositoryPath,
		}
	}

	// An explicit source-code path selects the mapping by path. This is the
	// common case for the web panel and needs no Redmine project id.
	if path := strings.TrimSpace(selection.RepositoryPath); path != "" {
		return resolveByPath(selection, mappings, path)
	}

	// Fallback for configurations that still map repositories to projects.
	if project := strings.TrimSpace(selection.Project); project != "" {
		projectCandidates := filterMappings(mappings, func(mapping domain.RepositoryMapping) bool {
			return mapping.RedmineProject == project
		})
		if len(projectCandidates) > 0 {
			return chooseMapping(selection.Project, projectCandidates)
		}
	}

	// Project-less mappings apply to every issue.
	globalCandidates := filterMappings(mappings, func(mapping domain.RepositoryMapping) bool {
		return mapping.RedmineProject == ""
	})
	if len(globalCandidates) > 0 {
		return chooseMapping(selection.Project, globalCandidates)
	}

	return domain.RepositoryMapping{}, &NoMappingError{
		Project:        selection.Project,
		RepositoryPath: selection.RepositoryPath,
	}
}

func resolveByPath(selection Selection, mappings []domain.RepositoryMapping, path string) (domain.RepositoryMapping, error) {
	matches := filterMappings(mappings, func(mapping domain.RepositoryMapping) bool {
		return sameRepositoryPath(mapping.RepositoryPath, path)
	})
	if len(matches) == 0 {
		return domain.RepositoryMapping{}, &NoMappingError{
			Project:        selection.Project,
			RepositoryPath: path,
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	// Legacy configurations may repeat the same path under several projects.
	// Prefer the entry that belongs to the issue's project, then the default.
	if project := strings.TrimSpace(selection.Project); project != "" {
		for _, mapping := range matches {
			if mapping.RedmineProject == project {
				return mapping, nil
			}
		}
	}
	for _, mapping := range matches {
		if mapping.IsDefault {
			return mapping, nil
		}
	}
	return domain.RepositoryMapping{}, &AmbiguousMappingError{Project: selection.Project, Candidates: matches}
}

func chooseMapping(project string, candidates []domain.RepositoryMapping) (domain.RepositoryMapping, error) {
	switch len(candidates) {
	case 0:
		return domain.RepositoryMapping{}, &NoMappingError{Project: project}
	case 1:
		return candidates[0], nil
	default:
		for _, candidate := range candidates {
			if candidate.IsDefault {
				return candidate, nil
			}
		}
		return domain.RepositoryMapping{}, &AmbiguousMappingError{Project: project, Candidates: candidates}
	}
}

func filterMappings(mappings []domain.RepositoryMapping, keep func(domain.RepositoryMapping) bool) []domain.RepositoryMapping {
	filtered := make([]domain.RepositoryMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping.Enabled && keep(mapping) {
			filtered = append(filtered, mapping)
		}
	}
	return filtered
}

func sameRepositoryPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if domain.IsLocalRepositoryPath(a) || domain.IsLocalRepositoryPath(b) {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}
