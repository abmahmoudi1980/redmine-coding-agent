package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

func mergeRequestsEnabledOnly(cfg *config.Config) {
	cfg.GitLab.PushEnabled = true
	cfg.GitLab.MergeRequestEnabled = false
}

func TestPublishPushesBranchAndOpensMergeRequest(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s), want completed", run.State, run.FailureMessage)
	}
	if run.CommitSHA == "" {
		t.Fatal("a completed run must carry its commit")
	}
	if run.PushedAt == nil {
		t.Fatal("a completed run must record pushed_at")
	}

	if tip := env.harness.Origin.Tip(t, run.Branch); tip != run.CommitSHA {
		t.Fatalf("origin %s = %q, want commit %q", run.Branch, tip, run.CommitSHA)
	}
	if tip := env.harness.Origin.Tip(t, "main"); tip != env.harness.Origin.InitialRevision {
		t.Fatalf("the base branch moved: %s", tip)
	}
	if calls := env.harness.GitLab.MergeCalls(); calls != 0 {
		t.Fatalf("the worker called a merge endpoint %d time(s)", calls)
	}

	mergeRequests := env.harness.GitLab.MergeRequests()
	if len(mergeRequests) != 1 {
		t.Fatalf("merge requests = %+v", mergeRequests)
	}
	created := mergeRequests[0]
	if created.SourceBranch != run.Branch || created.TargetBranch != "main" {
		t.Fatalf("merge request branches = %q -> %q", created.SourceBranch, created.TargetBranch)
	}
	assertSectionsInOrder(t, created.Description, []string{
		"## Issue",
		"## Implementation summary",
		"## Notable changed files",
		"## Validation",
		"## Known limitations",
		"## Manual follow-up",
	})
	if !strings.Contains(created.Description, "#4821") {
		t.Fatalf("the merge request must reference the issue:\n%s", created.Description)
	}
	if !strings.Contains(created.Description, "unit-tests") {
		t.Fatalf("the merge request must reproduce the validation results:\n%s", created.Description)
	}

	notes := env.harness.Redmine.Notes()
	if len(notes) != 1 {
		t.Fatalf("redmine notes = %+v", notes)
	}
	for _, want := range []string{"completed", run.Branch, run.CommitSHA, created.WebURL, "unit-tests"} {
		if !strings.Contains(notes[0].Notes, want) {
			t.Errorf("the comment is missing %q:\n%s", want, notes[0].Notes)
		}
	}

	report, err := env.harness.Store.GetReport(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.DeliveryStatus != "delivered" {
		t.Fatalf("delivery status = %q (%s)", report.DeliveryStatus, report.DeliveryError)
	}
	if report.DeliveredAt == nil {
		t.Fatal("a delivered report records delivered_at")
	}
	if report.MergeRequestURL != created.WebURL {
		t.Fatalf("report merge request = %q, want %q", report.MergeRequestURL, created.WebURL)
	}
	if report.CommitSHA != run.CommitSHA {
		t.Fatalf("report commit = %q", report.CommitSHA)
	}
	if len(report.DedupeKey) != 64 {
		t.Fatalf("dedupe key = %q", report.DedupeKey)
	}
	if !strings.Contains(report.BodyMD, run.CommitSHA) || !strings.Contains(report.BodyMD, created.WebURL) {
		t.Fatalf("report body omits the links:\n%s", report.BodyMD)
	}

	detail := env.detail(t, run.PublicID)
	if detail["mergeRequestUrl"] != created.WebURL {
		t.Fatalf("detail mergeRequestUrl = %v", detail["mergeRequestUrl"])
	}
	detailReport, ok := detail["report"].(map[string]any)
	if !ok {
		t.Fatalf("detail report = %v", detail["report"])
	}
	if detailReport["deliveryStatus"] != "delivered" {
		t.Fatalf("detail delivery status = %v", detailReport["deliveryStatus"])
	}

	reportDocument := env.getJSON(t, "/api/runs/"+run.PublicID+"/report")
	endpointReport, ok := reportDocument["report"].(map[string]any)
	if !ok {
		t.Fatalf("GET report = %v", reportDocument["report"])
	}
	if endpointReport["mergeRequestUrl"] != created.WebURL {
		t.Fatalf("endpoint merge request = %v", endpointReport["mergeRequestUrl"])
	}
	if body, _ := endpointReport["bodyMarkdown"].(string); !strings.Contains(body, run.Branch) {
		t.Fatalf("endpoint report body omits the branch:\n%s", body)
	}

	checksDocument := env.getJSON(t, "/api/runs/"+run.PublicID+"/checks")
	attempts, ok := checksDocument["attempts"].([]any)
	if !ok || len(attempts) != 1 {
		t.Fatalf("GET checks attempts = %v", checksDocument["attempts"])
	}
	attempt, ok := attempts[0].(map[string]any)
	if !ok || attempt["attemptNo"] != float64(1) {
		t.Fatalf("attempt = %v", attempts[0])
	}
	if results, ok := attempt["results"].([]any); !ok || len(results) != 1 {
		t.Fatalf("attempt results = %v", attempt["results"])
	}
}

func TestPublishReusesAnExistingMergeRequest(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	seeded := env.harness.GitLab.PreregisterMergeRequest("agent/issue-4821", "main", "previous title", "previous body")
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s)", run.State, run.FailureMessage)
	}

	mergeRequests := env.harness.GitLab.MergeRequests()
	if len(mergeRequests) != 1 {
		t.Fatalf("a second merge request was created: %+v", mergeRequests)
	}
	if mergeRequests[0].IID != seeded.IID {
		t.Fatalf("iid = %d, want the seeded %d", mergeRequests[0].IID, seeded.IID)
	}
	if !strings.Contains(mergeRequests[0].Description, "## Validation") {
		t.Fatalf("the reused merge request was not refreshed:\n%s", mergeRequests[0].Description)
	}
}

func TestPublishingDisabledCompletesWithSkippedPublish(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: publishDisabled})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s)", run.State, run.FailureMessage)
	}
	if run.PushedAt != nil {
		t.Fatal("publishing is disabled, so nothing may be pushed")
	}
	if branches := env.harness.Origin.Branches(t); len(branches) != 1 || branches[0] != "main" {
		t.Fatalf("origin branches = %v", branches)
	}
	if mergeRequests := env.harness.GitLab.MergeRequests(); len(mergeRequests) != 0 {
		t.Fatalf("merge requests = %+v", mergeRequests)
	}

	// Publishing is skipped, reporting is still enabled: the operator gets the
	// report, with the skipped steps recorded honestly (FR-042, FR-045).
	report, err := env.harness.Store.GetReport(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("every run must have a report: %v", err)
	}
	if report.CommitSHA != run.CommitSHA {
		t.Fatalf("report commit = %q", report.CommitSHA)
	}
	if report.MergeRequestURL != "" {
		t.Fatalf("report merge request = %q", report.MergeRequestURL)
	}
	if !strings.Contains(report.BodyMD, "not created") {
		t.Fatalf("the report must state the merge request was skipped:\n%s", report.BodyMD)
	}
	if report.DeliveryStatus != "delivered" {
		t.Fatalf("delivery status = %q (%s)", report.DeliveryStatus, report.DeliveryError)
	}
	if notes := env.harness.Redmine.Notes(); len(notes) != 1 {
		t.Fatalf("redmine notes = %+v", notes)
	}
}

func TestReportingDisabledStoresTheReportAsSkipped(t *testing.T) {
	env := newPipeline(t, pipelineOptions{
		tune: func(cfg *config.Config) {
			publishDisabled(cfg)
			cfg.Redmine.CommentOnFinish = false
		},
	})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s)", run.State, run.FailureMessage)
	}
	if notes := env.harness.Redmine.Notes(); len(notes) != 0 {
		t.Fatalf("reporting is disabled, so no comment may be posted: %+v", notes)
	}
	report, err := env.harness.Store.GetReport(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("the report must still be stored: %v", err)
	}
	if report.DeliveryStatus != "skipped" {
		t.Fatalf("delivery status = %q, want skipped", report.DeliveryStatus)
	}
	if report.BodyMD == "" {
		t.Fatal("a skipped delivery must still keep the body")
	}
}

func TestMergeRequestDisabledStillPushesTheBranch(t *testing.T) {
	env := newPipeline(t, pipelineOptions{tune: mergeRequestsEnabledOnly})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("state = %q (%s)", run.State, run.FailureMessage)
	}
	if run.PushedAt == nil {
		t.Fatal("a disabled merge request must not disable the push")
	}
	if tip := env.harness.Origin.Tip(t, run.Branch); tip != run.CommitSHA {
		t.Fatalf("origin %s = %q", run.Branch, tip)
	}
	if mergeRequests := env.harness.GitLab.MergeRequests(); len(mergeRequests) != 0 {
		t.Fatalf("merge requests = %+v", mergeRequests)
	}
	report, err := env.harness.Store.GetReport(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.MergeRequestURL != "" {
		t.Fatalf("report merge request = %q", report.MergeRequestURL)
	}
	if strings.Contains(report.BodyMD, "-/merge_requests/") {
		t.Fatalf("the report claims a merge request that was skipped:\n%s", report.BodyMD)
	}
	if !strings.Contains(report.BodyMD, "merge requests are disabled") {
		t.Fatalf("the report must state the merge request was skipped by policy:\n%s", report.BodyMD)
	}
}

func TestRejectedPushKeepsTheCommitAndFails(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	env.harness.Origin.RefusePushes(t)
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateFailed {
		t.Fatalf("state = %q, want failed", run.State)
	}
	if run.FailureCode != domain.FailPushRejected {
		t.Fatalf("failure code = %q, want push_rejected (%s)", run.FailureCode, run.FailureMessage)
	}
	if run.CommitSHA == "" {
		t.Fatal("the local commit must be preserved when the push is rejected")
	}
	if run.PushedAt != nil {
		t.Fatal("a rejected push must not record pushed_at")
	}
	if branches := env.harness.Origin.Branches(t); len(branches) != 1 || branches[0] != "main" {
		t.Fatalf("origin branches = %v", branches)
	}

	workspace, err := env.harness.Store.GetWorkspaceByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if !workspace.HasUnpushedChanges {
		t.Fatal("the workspace must record that the commit is unpushed")
	}

	report, err := env.harness.Store.GetReport(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("a failed publish must still leave a report: %v", err)
	}
	if !strings.Contains(report.BodyMD, run.CommitSHA) {
		t.Fatalf("the report must name the preserved commit:\n%s", report.BodyMD)
	}
	if !strings.Contains(report.BodyMD, string(domain.FailPushRejected)) {
		t.Fatalf("the report must name the failure:\n%s", report.BodyMD)
	}
}

func TestFailedCommentKeepsTheReportBody(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	env.harness.Redmine.SetNoteFailure(true)
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("a failed comment must not undo the work (state %q, %s)", run.State, run.FailureMessage)
	}
	if run.PushedAt == nil {
		t.Fatal("the push happened and must be recorded")
	}
	if mergeRequests := env.harness.GitLab.MergeRequests(); len(mergeRequests) != 1 {
		t.Fatalf("merge requests = %+v", mergeRequests)
	}

	report, err := env.harness.Store.GetReport(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.DeliveryStatus != "failed" {
		t.Fatalf("delivery status = %q, want failed", report.DeliveryStatus)
	}
	if report.DeliveryError == "" {
		t.Fatal("a failed delivery must record its error")
	}
	if report.BodyMD == "" {
		t.Fatal("the report body must be retained when delivery fails")
	}
	if !strings.Contains(report.BodyMD, run.CommitSHA) {
		t.Fatalf("the retained body must contain the links:\n%s", report.BodyMD)
	}
}

func TestMergeRequestFailureKeepsThePushAndFails(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	env.harness.GitLab.SetMergeRequestFailure(true)
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateFailed {
		t.Fatalf("state = %q, want failed", run.State)
	}
	if run.FailureCode != domain.FailMergeRequestFailed {
		t.Fatalf("failure code = %q, want merge_request_failed (%s)", run.FailureCode, run.FailureMessage)
	}
	if run.PushedAt == nil {
		t.Fatal("the push already happened and must remain recorded")
	}
	if tip := env.harness.Origin.Tip(t, run.Branch); tip != run.CommitSHA {
		t.Fatalf("the pushed branch must survive the merge request failure: %q != %q", tip, run.CommitSHA)
	}
	if mergeRequests := env.harness.GitLab.MergeRequests(); len(mergeRequests) != 0 {
		t.Fatalf("merge requests = %+v", mergeRequests)
	}
}

func (e *pipelineEnvironment) getJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	response, err := http.Get(e.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, response.StatusCode, payload)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, payload)
	}
	return document
}

func assertSectionsInOrder(t *testing.T, body string, sections []string) {
	t.Helper()
	previous := -1
	for _, section := range sections {
		index := strings.Index(body, section)
		if index < 0 {
			t.Fatalf("missing section %q:\n%s", section, body)
		}
		if index <= previous {
			t.Fatalf("section %q is out of order:\n%s", section, body)
		}
		previous = index
	}
}
