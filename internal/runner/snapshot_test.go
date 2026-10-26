package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
)

func TestExtractAcceptanceCriteria(t *testing.T) {
	cases := []struct {
		name        string
		description string
		want        string
		source      string
	}{
		{
			name:        "markdown heading",
			description: "Totals are wrong.\n\n## Acceptance Criteria\n- totals round half up\n- invoices still load\n\n## Notes\nignored",
			want:        "- totals round half up\n- invoices still load",
			source:      "heading",
		},
		{
			name:        "inline label",
			description: "Please fix.\n\nAC: totals round half up\nand invoices load",
			want:        "totals round half up\nand invoices load",
			source:      "heading",
		},
		{
			name:        "persian heading",
			description: "صورتحساب اشتباه است.\n\n## معیار پذیرش\n- جمع\u200cها به بالا رند شوند",
			want:        "- جمع\u200cها به بالا رند شوند",
			source:      "heading",
		},
		{
			name:        "no recognisable heading",
			description: "Totals are wrong. Please fix soon.",
			want:        "",
			source:      "none",
		},
		{
			name:        "empty heading is not criteria",
			description: "## Acceptance Criteria\n\n## Notes\nnothing",
			want:        "",
			source:      "none",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, source := ExtractAcceptanceCriteria(testCase.description)
			if got != testCase.want {
				t.Fatalf("criteria = %q, want %q", got, testCase.want)
			}
			if source != testCase.source {
				t.Fatalf("source = %q, want %q", source, testCase.source)
			}
		})
	}
}

func TestBuildSnapshotSelectsCommentsAndHashesContent(t *testing.T) {
	fetchedAt := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	issue := &redmine.Issue{
		ID:          4821,
		Subject:     "Invoice totals round incorrectly",
		Description: "## Acceptance Criteria\n- totals round half up\n",
		Project:     redmine.Project{ID: redmine.FlexibleString("7"), Identifier: "billing", Name: "Billing"},
		Status:      redmine.Named{Name: "New"},
		Tracker:     redmine.Named{Name: "Feature"},
		Priority:    redmine.Named{Name: "Normal"},
		AssignedTo:  &redmine.Named{Name: "Operator"},
		Parent:      &redmine.IDRef{ID: 4800},
		Journals: []redmine.Journal{
			{ID: 12, User: redmine.Named{Name: "Reporter"}, CreatedOn: fetchedAt, Notes: "later"},
			{ID: 11, User: redmine.Named{Name: "Reporter"}, CreatedOn: fetchedAt.Add(-time.Hour), Notes: "  first  "},
			{ID: 13, User: redmine.Named{Name: "Reporter"}, CreatedOn: fetchedAt, Notes: "   "},
		},
		Relations: []redmine.Relation{
			{IssueID: 4821, IssueToID: 4900},
			{IssueID: 4901, IssueToID: 4821},
			{IssueID: 4821, IssueToID: 4900},
		},
		Children: []redmine.IDRef{{ID: 4902}},
		CustomFields: []redmine.CustomField{
			{ID: 3, Name: "Team", Value: redmine.FlexibleString("Payments")},
		},
		Attachments: []redmine.Attachment{
			{ID: 55, Filename: "screen.txt", Filesize: 12, ContentURL: "/attachments/download/55/screen.txt"},
			{ID: 56, Filename: "missing.txt", Filesize: 4},
		},
		RawJSON: `{"id":4821}`,
	}

	snapshot := BuildSnapshot(issue, fetchedAt)

	if snapshot.CriteriaSource != "heading" || !strings.Contains(snapshot.AcceptanceCriteria, "totals round half up") {
		t.Fatalf("criteria = %q/%q", snapshot.AcceptanceCriteria, snapshot.CriteriaSource)
	}
	if len(snapshot.SelectedComments) != 2 {
		t.Fatalf("comments = %+v", snapshot.SelectedComments)
	}
	if snapshot.SelectedComments[0].ID != 11 || snapshot.SelectedComments[0].Notes != "first" {
		t.Fatalf("first comment = %+v", snapshot.SelectedComments[0])
	}
	if len(snapshot.RelatedIssueIDs) != 3 {
		t.Fatalf("related ids = %+v", snapshot.RelatedIssueIDs)
	}
	if snapshot.ParentIssueID == nil || *snapshot.ParentIssueID != 4800 {
		t.Fatalf("parent = %+v", snapshot.ParentIssueID)
	}
	if snapshot.AssigneeName != "Operator" || snapshot.ProjectID != "7" || snapshot.ProjectIdentifier != "billing" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(snapshot.Attachments) != 2 || !snapshot.Attachments[0].Available || snapshot.Attachments[1].Available {
		t.Fatalf("attachments = %+v", snapshot.Attachments)
	}
	if snapshot.RawJSON != `{"id":4821}` {
		t.Fatalf("raw json = %q", snapshot.RawJSON)
	}
	if len(snapshot.ContentHash) != 64 {
		t.Fatalf("content hash = %q", snapshot.ContentHash)
	}

	second := BuildSnapshot(issue, fetchedAt)
	if second.ContentHash != snapshot.ContentHash {
		t.Fatalf("content hash is not stable: %q vs %q", snapshot.ContentHash, second.ContentHash)
	}

	changed := *issue
	changed.Subject = "Different subject"
	if BuildSnapshot(&changed, fetchedAt).ContentHash == snapshot.ContentHash {
		t.Fatal("content hash ignored a changed subject")
	}
}
