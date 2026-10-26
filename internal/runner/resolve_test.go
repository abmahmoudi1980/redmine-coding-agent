package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

type staticRepositories []domain.RepositoryMapping

func (s staticRepositories) ListRepositories(context.Context) ([]domain.RepositoryMapping, error) {
	return s, nil
}

func mapping(id int64, project, path string, isDefault bool) domain.RepositoryMapping {
	return domain.RepositoryMapping{
		ID:             id,
		RedmineProject: project,
		RepositoryPath: path,
		BaseBranch:     "main",
		IsDefault:      isDefault,
		PublishEnabled: true,
		Enabled:        true,
	}
}

func TestResolveSingleMapping(t *testing.T) {
	source := staticRepositories{mapping(3, "billing", "group/billing-api", false)}

	resolved, err := Resolve(context.Background(), source, "billing", 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != 3 || resolved.BaseBranch != "main" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestResolveAmbiguousWithoutDefaultListsCandidates(t *testing.T) {
	source := staticRepositories{
		mapping(3, "billing", "group/billing-api", false),
		mapping(4, "billing", "group/billing-ui", false),
	}

	_, err := Resolve(context.Background(), source, "billing", 0)
	if !errors.Is(err, ErrRepositoryAmbiguous) {
		t.Fatalf("error = %v, want ErrRepositoryAmbiguous", err)
	}

	var ambiguous *AmbiguousMappingError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error %v does not carry candidates", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Fatalf("candidates = %+v", ambiguous.Candidates)
	}
	if ambiguous.Candidates[0].RepositoryPath != "group/billing-api" ||
		ambiguous.Candidates[1].RepositoryPath != "group/billing-ui" {
		t.Fatalf("candidate order = %+v", ambiguous.Candidates)
	}
}

func TestResolveNoMapping(t *testing.T) {
	source := staticRepositories{mapping(3, "other", "group/other-api", false)}

	_, err := Resolve(context.Background(), source, "billing", 0)
	if !errors.Is(err, ErrNoRepositoryMapping) {
		t.Fatalf("error = %v, want ErrNoRepositoryMapping", err)
	}
	if FailureForResolve(err).Code != domain.FailNoRepositoryMapping {
		t.Fatalf("failure = %+v", FailureForResolve(err))
	}
}

func TestResolveExplicitRepositoryOverridesAmbiguity(t *testing.T) {
	source := staticRepositories{
		mapping(3, "billing", "group/billing-api", false),
		mapping(4, "billing", "group/billing-ui", false),
	}

	resolved, err := Resolve(context.Background(), source, "billing", 4)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != 4 || resolved.RepositoryPath != "group/billing-ui" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestResolveExplicitRepositoryMustBeConfigured(t *testing.T) {
	source := staticRepositories{mapping(3, "billing", "group/billing-api", true)}

	_, err := Resolve(context.Background(), source, "billing", 99)
	if !errors.Is(err, ErrNoRepositoryMapping) {
		t.Fatalf("error = %v, want ErrNoRepositoryMapping", err)
	}
}

func TestResolvePrefersDefault(t *testing.T) {
	source := staticRepositories{
		mapping(3, "billing", "group/billing-api", false),
		mapping(4, "billing", "group/billing-ui", true),
	}

	resolved, err := Resolve(context.Background(), source, "billing", 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != 4 {
		t.Fatalf("resolved = %+v, want the default mapping", resolved)
	}
}

func TestResolveIgnoresDisabledMappings(t *testing.T) {
	disabled := mapping(4, "billing", "group/billing-ui", false)
	disabled.Enabled = false
	source := staticRepositories{
		mapping(3, "billing", "group/billing-api", false),
		disabled,
	}

	resolved, err := Resolve(context.Background(), source, "billing", 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != 3 {
		t.Fatalf("resolved = %+v, want the enabled mapping", resolved)
	}
}

func TestResolveDoesNotInferFromRepositoryPath(t *testing.T) {
	source := staticRepositories{mapping(3, "billing", "group/billing-api", false)}

	_, err := Resolve(context.Background(), source, "billing-api", 0)
	if !errors.Is(err, ErrNoRepositoryMapping) {
		t.Fatalf("error = %v, want ErrNoRepositoryMapping", err)
	}
}
