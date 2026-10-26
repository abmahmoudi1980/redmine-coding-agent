// Package runner holds the orchestration stages. Resolution and snapshot
// building are pure: they never touch the filesystem or a network.
package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// RepositoryLister is the narrow read the resolver needs; *store.Store
// implements it.
type RepositoryLister interface {
	ListRepositories(ctx context.Context) ([]domain.RepositoryMapping, error)
}

var (
	// ErrRepositoryAmbiguous means the project maps to several repositories and
	// no default is configured (FR-015).
	ErrRepositoryAmbiguous = errors.New("runner: repository ambiguity requires an explicit choice")
	// ErrNoRepositoryMapping means the project has no usable mapping (FR-014).
	ErrNoRepositoryMapping = errors.New("runner: no repository mapping for project")
)

// AmbiguousMappingError carries the candidates the operator must choose from.
type AmbiguousMappingError struct {
	Project    string
	Candidates []domain.RepositoryMapping
}

func (e *AmbiguousMappingError) Error() string {
	return fmt.Sprintf("runner: project %q maps to %d repositories; choose one explicitly", e.Project, len(e.Candidates))
}

func (e *AmbiguousMappingError) Is(target error) bool { return target == ErrRepositoryAmbiguous }

// NoMappingError names the project (or explicit id) that produced no mapping.
type NoMappingError struct {
	Project      string
	RepositoryID int64
}

func (e *NoMappingError) Error() string {
	if e.RepositoryID > 0 {
		return fmt.Sprintf("runner: repository id %d is not a configured mapping for project %q", e.RepositoryID, e.Project)
	}
	return fmt.Sprintf("runner: project %q has no configured repository mapping", e.Project)
}

func (e *NoMappingError) Is(target error) bool { return target == ErrNoRepositoryMapping }

// FailureForResolve maps a resolution error to the documented failure code.
func FailureForResolve(err error) domain.Failure {
	switch {
	case errors.Is(err, ErrRepositoryAmbiguous):
		failure := domain.NewFailure(domain.FailRepositoryAmbiguous, err.Error(), "")
		var ambiguous *AmbiguousMappingError
		if errors.As(err, &ambiguous) {
			failure.Message = fmt.Sprintf("Project %q maps to %d repositories; choose one explicitly.", ambiguous.Project, len(ambiguous.Candidates))
		}
		return failure
	case errors.Is(err, ErrNoRepositoryMapping):
		failure := domain.NewFailure(domain.FailNoRepositoryMapping, err.Error(), "")
		var noMapping *NoMappingError
		if errors.As(err, &noMapping) && noMapping.RepositoryID == 0 {
			failure.Message = fmt.Sprintf("Project %q has no configured repository mapping.", noMapping.Project)
		}
		return failure
	default:
		return domain.NewFailure(domain.FailNoRepositoryMapping, err.Error(), "")
	}
}

// Resolve picks the destination repository and base branch from the stored
// mappings only. It never infers, never falls back, and never guesses
// (FR-014, FR-015).
func Resolve(ctx context.Context, lister RepositoryLister, project string, explicitRepositoryID int64) (domain.RepositoryMapping, error) {
	if lister == nil {
		return domain.RepositoryMapping{}, errors.New("runner: repository source is required")
	}
	mappings, err := lister.ListRepositories(ctx)
	if err != nil {
		return domain.RepositoryMapping{}, fmt.Errorf("runner: list repositories: %w", err)
	}

	if explicitRepositoryID > 0 {
		for _, mapping := range mappings {
			if mapping.ID != explicitRepositoryID {
				continue
			}
			if !mapping.Enabled {
				return domain.RepositoryMapping{}, &NoMappingError{Project: project, RepositoryID: explicitRepositoryID}
			}
			if mapping.RedmineProject != project {
				return domain.RepositoryMapping{}, &NoMappingError{Project: project, RepositoryID: explicitRepositoryID}
			}
			return mapping, nil
		}
		return domain.RepositoryMapping{}, &NoMappingError{Project: project, RepositoryID: explicitRepositoryID}
	}

	candidates := make([]domain.RepositoryMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping.Enabled && mapping.RedmineProject == project {
			candidates = append(candidates, mapping)
		}
	}

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
