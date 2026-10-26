package runner

import (
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"strings"

	"github.com/systemgroup/redmine-coding-agent/internal/agent"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/gitlab"
)

// The stored mapping is the policy the runner must obey: a per-repository
// switch can only narrow the global one (FR-042, T129).
func TestRepositoryPolicyOverridesTheGlobalSwitches(t *testing.T) {
	cfg := &config.Config{
		GitLab: config.GitLab{PushEnabled: true, MergeRequestEnabled: true},
	}
	runner := &Runner{config: cfg, gitlab: &gitlab.Client{}}

	cases := []struct {
		name          string
		mapping       domain.RepositoryMapping
		wantPublish   bool
		wantMergeRepo bool
	}{
		{
			name:          "table disables both",
			mapping:       domain.RepositoryMapping{PublishEnabled: false, MergeRequestEnabled: false},
			wantPublish:   false,
			wantMergeRepo: false,
		},
		{
			name:          "table enables both",
			mapping:       domain.RepositoryMapping{PublishEnabled: true, MergeRequestEnabled: true},
			wantPublish:   true,
			wantMergeRepo: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := runner.publishEnabled(testCase.mapping); got != testCase.wantPublish {
				t.Errorf("publishEnabled = %v, want %v", got, testCase.wantPublish)
			}
			if got := runner.mergeRequestEnabled(testCase.mapping); got != testCase.wantMergeRepo {
				t.Errorf("mergeRequestEnabled = %v, want %v", got, testCase.wantMergeRepo)
			}
		})
	}
}

func TestGlobalPolicyStillGatesTheRepositorySwitch(t *testing.T) {
	cfg := &config.Config{
		GitLab: config.GitLab{PushEnabled: false, MergeRequestEnabled: false},
	}
	runner := &Runner{config: cfg, gitlab: &gitlab.Client{}}
	mapping := domain.RepositoryMapping{PublishEnabled: true, MergeRequestEnabled: true}

	if runner.publishEnabled(mapping) {
		t.Error("a globally disabled push must not be overridden by the repository")
	}
	if runner.mergeRequestEnabled(mapping) {
		t.Error("a globally disabled merge request must not be overridden by the repository")
	}
}

// A local repository has no merge-request endpoint, but pushing the dedicated
// branch into it is still allowed and needs no GitLab client.
func TestLocalRepositoryPushesButNeverOpensAMergeRequest(t *testing.T) {
	cfg := &config.Config{
		GitLab: config.GitLab{PushEnabled: true, MergeRequestEnabled: true},
	}
	runner := &Runner{config: cfg}
	mapping := domain.RepositoryMapping{
		RepositoryPath:      t.TempDir(),
		PublishEnabled:      true,
		MergeRequestEnabled: true,
	}

	if !mapping.IsLocal() {
		t.Fatalf("%q was not recognised as a local path", mapping.RepositoryPath)
	}
	if !runner.publishEnabled(mapping) {
		t.Error("pushing the branch into a local repository must not require GitLab")
	}
	if runner.mergeRequestEnabled(mapping) {
		t.Error("a local repository must never open a merge request")
	}
	if got := runner.remoteLabel(mapping); !strings.Contains(got, "local repository") {
		t.Errorf("remoteLabel = %q", got)
	}
}

// A failed invocation must name the agent's own error, not only its exit code.
func TestAgentFailureMessageCarriesTheAgentError(t *testing.T) {
	runner := &Runner{}
	out := runner.outcomeForInvocation(agent.Result{
		Status:   agent.StatusFailed,
		ExitCode: 1,
		Error:    `Agent not found: "restricted"`,
	}, nil, 1)

	if out == nil || out.Failure == nil {
		t.Fatal("a failed invocation must produce a failure outcome")
	}
	if out.Failure.Code != domain.FailAgentError {
		t.Fatalf("code = %q", out.Failure.Code)
	}
	if !strings.Contains(out.Failure.Message, `Agent not found: "restricted"`) {
		t.Fatalf("message = %q", out.Failure.Message)
	}
	if !strings.Contains(out.Failure.Message, "code 1") {
		t.Fatalf("message must keep the exit code: %q", out.Failure.Message)
	}
}
