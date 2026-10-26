package redmine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FlexibleString accepts the number-or-string identifiers and custom-field
// values Redmine returns without losing their text form.
type FlexibleString string

func (f FlexibleString) String() string { return string(f) }

func (f *FlexibleString) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*f = FlexibleString(text)
		return nil
	}

	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("flexible string: %w", err)
	}
	switch typed := value.(type) {
	case nil:
		*f = ""
	case float64:
		*f = FlexibleString(strconv.FormatFloat(typed, 'f', -1, 64))
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, fmt.Sprint(item))
		}
		*f = FlexibleString(strings.Join(parts, ", "))
	default:
		*f = FlexibleString(fmt.Sprint(typed))
	}
	return nil
}

// Named is a Redmine id/name pair (status, tracker, priority, user, project).
type Named struct {
	ID   FlexibleString `json:"id"`
	Name string         `json:"name"`
}

// Project is the issue's Redmine project.
type Project struct {
	ID         FlexibleString `json:"id"`
	Identifier string         `json:"identifier"`
	Name       string         `json:"name"`
}

// IDRef is a bare issue reference from `parent` or `children`.
type IDRef struct {
	ID int `json:"id"`
}

// CustomField is one configured custom field on the issue.
type CustomField struct {
	ID    int            `json:"id"`
	Name  string         `json:"name"`
	Value FlexibleString `json:"value"`
}

// Journal is one issue update. Only entries with notes become comments.
type Journal struct {
	ID        int       `json:"id"`
	User      Named     `json:"user"`
	CreatedOn time.Time `json:"created_on"`
	Notes     string    `json:"notes"`
}

// Relation is one issue relation.
type Relation struct {
	ID           int    `json:"id"`
	IssueID      int    `json:"issue_id"`
	IssueToID    int    `json:"issue_to_id"`
	RelationType string `json:"relation_type"`
}

// Attachment is one file attached to the issue.
type Attachment struct {
	ID          int    `json:"id"`
	Filename    string `json:"filename"`
	Filesize    int64  `json:"filesize"`
	ContentURL  string `json:"content_url"`
	Description string `json:"description"`
}

// Issue is the Redmine issue payload the preview and the run snapshot need.
type Issue struct {
	ID           int           `json:"id"`
	Subject      string        `json:"subject"`
	Description  string        `json:"description"`
	Project      Project       `json:"project"`
	Status       Named         `json:"status"`
	Tracker      Named         `json:"tracker"`
	Priority     Named         `json:"priority"`
	AssignedTo   *Named        `json:"assigned_to"`
	Parent       *IDRef        `json:"parent"`
	Relations    []Relation    `json:"relations"`
	Children     []IDRef       `json:"children"`
	CustomFields []CustomField `json:"custom_fields"`
	Journals     []Journal     `json:"journals"`
	Attachments  []Attachment  `json:"attachments"`
	CreatedOn    time.Time     `json:"created_on"`
	UpdatedOn    time.Time     `json:"updated_on"`

	// RawJSON is the exact issue object returned by Redmine, kept for audit.
	RawJSON string `json:"-"`
}
