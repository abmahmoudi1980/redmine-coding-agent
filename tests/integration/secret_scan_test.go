package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
	"github.com/systemgroup/redmine-coding-agent/internal/store"
)

// TestNoSecretValueSurvivesAFullPipeline walks every durable sink and every
// response after a real run, then fails on the first fixture credential found
// anywhere (SC-013, FR-056).
func TestNoSecretValueSurvivesAFullPipeline(t *testing.T) {
	env := newPipeline(t, pipelineOptions{})
	env.harness.AddIssue(issueWithMode("ok"))

	run := env.start(t, 4821)
	if run.State != domain.StateCompleted {
		t.Fatalf("run state = %s, want completed (failure: %s %s)", run.State, run.FailureCode, run.FailureMessage)
	}

	secrets := []string{fixtureRedmineKey, fixtureGitLabToken}
	assertClean := func(where, text string) {
		t.Helper()
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s contains a resolved credential", where)
			}
		}
	}

	assertClean("database dump", dumpDatabase(t, env.harness.Store))

	files := 0
	err := filepath.WalkDir(env.harness.Root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		base := entry.Name()
		if strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".db-wal") || strings.HasSuffix(base, ".db-shm") {
			return nil // The SQL dump above already covers the database.
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Size() > 8<<20 {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files++
		assertClean("file "+strings.TrimPrefix(path, env.harness.Root), string(content))
		return nil
	})
	if err != nil {
		t.Fatalf("walk run artifacts: %v", err)
	}
	if files == 0 {
		t.Fatal("no run artifacts were scanned; the test would pass vacuously")
	}

	paths := []string{
		"/api/config",
		"/api/meta",
		"/api/health",
		"/api/health?deep=true",
		"/api/repositories",
		"/api/runs",
		"/api/runs/" + run.PublicID,
		"/api/runs/" + run.PublicID + "/checks",
		"/api/runs/" + run.PublicID + "/report",
		"/api/runs/" + run.PublicID + "/logs",
	}
	for _, path := range paths {
		response, getErr := http.Get(env.server.URL + path)
		if getErr != nil {
			t.Fatalf("GET %s: %v", path, getErr)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		assertClean("response "+path, string(body))
	}

	for _, note := range env.harness.Redmine.Notes() {
		assertClean("Redmine note", note.Notes)
	}
	for _, mergeRequest := range env.harness.GitLab.MergeRequests() {
		assertClean("merge request", mergeRequest.Title+"\n"+mergeRequest.Description)
	}
	if len(env.harness.GitLab.MergeRequests()) == 0 {
		t.Fatal("the run published no merge request; the scan did not cover that path")
	}
}

// dumpDatabase renders every table and row as text, which is what an operator
// would see if they opened the file with a SQLite browser.
func dumpDatabase(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()

	rows, err := st.Reader().QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close table list: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}

	var dump strings.Builder
	for _, table := range tables {
		tableRows, err := st.Reader().QueryContext(ctx, fmt.Sprintf(`SELECT * FROM %q`, table))
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		columns, err := tableRows.Columns()
		if err != nil {
			_ = tableRows.Close()
			t.Fatalf("columns of %s: %v", table, err)
		}
		for tableRows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := tableRows.Scan(pointers...); err != nil {
				_ = tableRows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			for index, value := range values {
				fmt.Fprintf(&dump, "%s.%s=%v\n", table, columns[index], value)
			}
		}
		if err := tableRows.Close(); err != nil {
			t.Fatalf("close %s: %v", table, err)
		}
		if err := tableRows.Err(); err != nil {
			t.Fatalf("iterate %s: %v", table, err)
		}
	}
	return dump.String()
}
