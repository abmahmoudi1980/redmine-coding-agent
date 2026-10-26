package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/redmine"
)

// SnapshotPolicy is the heading vocabulary the acceptance-criteria heuristic
// recognises. It is deliberately narrow: an honest `none` beats a guess.
var acceptanceHeadings = map[string]bool{
	"acceptance criteria":     true,
	"acceptance criterion":    true,
	"criteria for acceptance": true,
	"ac":                      true,
	"معیار پذیرش":             true,
	"معیارهای پذیرش":          true,
	"پذیرش":                   true,
}

var (
	markdownHeading = regexp.MustCompile(`^(\s{0,3})(#{1,6})\s+(.+?)\s*#*\s*$`)
	inlineHeading   = regexp.MustCompile(`(?i)^\s*(?:\*\*)?(acceptance criteria|acceptance criterion|criteria for acceptance|ac|معیار پذیرش|معیارهای پذیرش|پذیرش)(?:\*\*)?\s*:\s*(.*)$`)
	whitespace      = regexp.MustCompile(`\s+`)
)

// BuildSnapshot turns one Redmine payload into the immutable snapshot a run
// records. It is pure: no filesystem, no network, no clock beyond fetchedAt.
func BuildSnapshot(issue *redmine.Issue, fetchedAt time.Time) domain.IssueSnapshot {
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	if issue == nil {
		return domain.IssueSnapshot{CriteriaSource: "none", FetchedAt: fetchedAt, RawJSON: "{}", ContentHash: hashSnapshot("", "", nil)}
	}

	comments := selectComments(issue.Journals)
	criteria, criteriaSource := ExtractAcceptanceCriteria(issue.Description)

	attachments := make([]domain.Attachment, 0, len(issue.Attachments))
	for _, attachment := range issue.Attachments {
		attachments = append(attachments, domain.Attachment{
			ID:         attachment.ID,
			Filename:   attachment.Filename,
			ContentURL: attachment.ContentURL,
			Size:       attachment.Filesize,
			Available:  strings.TrimSpace(attachment.ContentURL) != "",
		})
	}

	customFields := make([]domain.CustomField, 0, len(issue.CustomFields))
	for _, field := range issue.CustomFields {
		customFields = append(customFields, domain.CustomField{
			ID:    field.ID,
			Name:  field.Name,
			Value: field.Value.String(),
		})
	}

	snapshot := domain.IssueSnapshot{
		RedmineIssueID:     issue.ID,
		Subject:            issue.Subject,
		Description:        issue.Description,
		AcceptanceCriteria: criteria,
		CriteriaSource:     criteriaSource,
		ProjectID:          issue.Project.ID.String(),
		ProjectIdentifier:  issue.Project.Identifier,
		ProjectName:        issue.Project.Name,
		StatusName:         issue.Status.Name,
		TrackerName:        issue.Tracker.Name,
		PriorityName:       issue.Priority.Name,
		RelatedIssueIDs:    relatedIssueIDs(issue),
		CustomFields:       customFields,
		SelectedComments:   comments,
		Attachments:        attachments,
		RawJSON:            issue.RawJSON,
		ContentHash:        hashSnapshot(issue.Subject, issue.Description, comments),
		FetchedAt:          fetchedAt.UTC(),
	}
	if issue.AssignedTo != nil {
		snapshot.AssigneeName = issue.AssignedTo.Name
	}
	if issue.Parent != nil {
		parent := issue.Parent.ID
		snapshot.ParentIssueID = &parent
	}
	if snapshot.RawJSON == "" {
		snapshot.RawJSON = "{}"
	}
	return snapshot
}

func selectComments(journals []redmine.Journal) []domain.Comment {
	comments := make([]domain.Comment, 0, len(journals))
	for _, journal := range journals {
		notes := strings.TrimSpace(journal.Notes)
		if notes == "" {
			continue
		}
		comments = append(comments, domain.Comment{
			ID:        journal.ID,
			Author:    journal.User.Name,
			CreatedOn: journal.CreatedOn.UTC(),
			Notes:     notes,
		})
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].CreatedOn.Equal(comments[j].CreatedOn) {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedOn.Before(comments[j].CreatedOn)
	})
	return comments
}

func relatedIssueIDs(issue *redmine.Issue) []int {
	seen := make(map[int]bool)
	add := func(id int) {
		if id > 0 && id != issue.ID {
			seen[id] = true
		}
	}
	for _, relation := range issue.Relations {
		if relation.IssueID == issue.ID {
			add(relation.IssueToID)
		}
		if relation.IssueToID == issue.ID {
			add(relation.IssueID)
		}
	}
	for _, child := range issue.Children {
		add(child.ID)
	}
	if len(seen) == 0 {
		return nil
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func hashSnapshot(subject, description string, comments []domain.Comment) string {
	payload, _ := json.Marshal(comments)
	sum := sha256.Sum256([]byte(subject + "\n" + description + "\n" + string(payload)))
	return hex.EncodeToString(sum[:])
}

// ExtractAcceptanceCriteria finds a recognised heading or inline label and
// returns the text beneath it plus its provenance (data-model §4).
func ExtractAcceptanceCriteria(description string) (string, string) {
	if strings.TrimSpace(description) == "" {
		return "", "none"
	}
	lines := strings.Split(description, "\n")

	for index, line := range lines {
		if level, title := headingOf(line); level > 0 {
			if !acceptanceHeadings[normalizeTitle(title)] {
				continue
			}
			body := collectSection(lines[index+1:], level)
			if body != "" {
				return body, "heading"
			}
			continue
		}
		if match := inlineHeading.FindStringSubmatch(line); match != nil {
			first := strings.TrimSpace(match[2])
			rest := collectSection(lines[index+1:], 1)
			body := strings.TrimSpace(joinNonEmpty(first, rest))
			if body != "" {
				return body, "heading"
			}
		}
	}
	return "", "none"
}

func headingOf(line string) (int, string) {
	match := markdownHeading.FindStringSubmatch(line)
	if match == nil {
		return 0, ""
	}
	return len(match[2]), match[3]
}

func normalizeTitle(title string) string {
	normalized := strings.ToLower(strings.TrimSpace(title))
	normalized = strings.TrimSuffix(normalized, ":")
	return whitespace.ReplaceAllString(normalized, " ")
}

func collectSection(lines []string, level int) string {
	collected := make([]string, 0, len(lines))
	for _, line := range lines {
		if nextLevel, _ := headingOf(line); nextLevel > 0 && nextLevel <= level {
			break
		}
		collected = append(collected, line)
	}
	return strings.TrimSpace(strings.Join(collected, "\n"))
}

func joinNonEmpty(first, rest string) string {
	if first == "" {
		return rest
	}
	if rest == "" {
		return first
	}
	return first + "\n" + rest
}
