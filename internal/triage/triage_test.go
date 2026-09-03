package triage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is this file's own tiny fixture helper: a file at repoRoot/rel
// with the given content, creating parent directories as needed.
func writeFile(t *testing.T, repoRoot, rel, content string) {
	t.Helper()
	full := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// TestRun_FlagsWriteWithNoTenantColumnMention is the core positive case:
// a table with real evidence elsewhere in the corpus that it carries
// organization_id, and one INSERT into it that never mentions that column
// at all — the exact shape TGD-BL-42's own investigation found real leaks
// hiding behind (an unscoped write into a table everything else scopes).
func TestRun_FlagsWriteWithNoTenantColumnMention(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "queries/widgets.sql", `
-- name: GetWidget :one
SELECT * FROM widgets WHERE organization_id = $1 AND id = $2;

-- name: InsertWidgetMissingTenant :exec
INSERT INTO widgets (id, name, created_at) VALUES ($1, $2, $3);
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Suspicions) != 1 {
		t.Fatalf("got %d suspicions, want 1: %+v", len(rep.Suspicions), rep.Suspicions)
	}
	s := rep.Suspicions[0]
	if s.Table != "widgets" || s.QueryName != "InsertWidgetMissingTenant" || s.StatementKind != "WRITE" {
		t.Errorf("got %+v, want the unscoped INSERT flagged", s)
	}
}

// TestRun_DoesNotFlagQueryMentioningTenantColumn: the companion negative —
// a query against the same known-scoped table that DOES mention the
// column must never appear in Suspicions.
func TestRun_DoesNotFlagQueryMentioningTenantColumn(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "queries/widgets.sql", `
-- name: GetWidget :one
SELECT * FROM widgets WHERE organization_id = $1 AND id = $2;

-- name: ListWidgets :many
SELECT * FROM widgets WHERE organization_id = $1;
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Suspicions) != 0 {
		t.Fatalf("got %d suspicions, want 0: %+v", len(rep.Suspicions), rep.Suspicions)
	}
}

// TestRun_L6TautologyNotFlagged reproduces design §7's L6 fixture verbatim
// (internal/differ/corpus_test.go's own SQL text) inside a corpus that
// otherwise establishes invoices as known-scoped. TGD-US-08 AC-3: Tier 0
// must NOT flag this — a naive presence check sees "tenant_id" mentioned
// and stops there, which is the acceptance criterion, not a bug.
func TestRun_L6TautologyNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "queries/invoices.sql", `
-- name: RealQuery :many
SELECT * FROM invoices WHERE tenant_id = $1;

-- name: L6Tautology :many
SELECT * FROM invoices WHERE tenant_id = tenant_id;
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, s := range rep.Suspicions {
		if s.QueryName == "L6Tautology" {
			t.Fatalf("L6 was flagged, want AC-3's documented non-detection: %+v", s)
		}
	}
}

// TestRun_L10ORDefeatNotFlagged: design §7's L10 fixture (internal/differ/
// corpus_test.go's own SQL text), same AC-3 requirement.
func TestRun_L10ORDefeatNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "queries/invoices.sql", `
-- name: RealQuery :many
SELECT * FROM invoices WHERE tenant_id = $1;

-- name: L10ORDefeat :many
SELECT * FROM invoices WHERE tenant_id = $1 OR status = $2;
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, s := range rep.Suspicions {
		if s.QueryName == "L10ORDefeat" {
			t.Fatalf("L10 was flagged, want AC-3's documented non-detection: %+v", s)
		}
	}
}

// TestRun_UnknownTableNeverFlagged: a table with zero evidence anywhere in
// the corpus of carrying a tenant-shaped column is never "known scoped"
// and produces no suspicion even though it has no predicate at all —
// undercounting is the safe direction (package doc).
func TestRun_UnknownTableNeverFlagged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "queries/settings.sql", `
-- name: GetSetting :one
SELECT * FROM app_settings WHERE key = $1;
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Suspicions) != 0 {
		t.Fatalf("got %d suspicions, want 0 — app_settings has no tenant-column evidence anywhere: %+v",
			len(rep.Suspicions), rep.Suspicions)
	}
	if len(rep.KnownScopedTables) != 0 {
		t.Fatalf("known_scoped_tables = %v, want empty", rep.KnownScopedTables)
	}
}

// TestRun_OutputNeverContainsForbiddenWords is TGD-US-08 AC-2, checked
// against every string this package actually emits — not merely the ones
// this test's author remembered to avoid.
func TestRun_OutputNeverContainsForbiddenWords(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "queries/widgets.sql", `
-- name: GetWidget :one
SELECT * FROM widgets WHERE organization_id = $1;

-- name: InsertWidgetMissingTenant :exec
INSERT INTO widgets (id, name) VALUES ($1, $2);
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Suspicions) == 0 {
		t.Fatal("expected at least one suspicion to check its text against")
	}
	forbidden := []string{"leak", "proven", "violation"}
	check := func(field, s string) {
		lower := strings.ToLower(s)
		for _, f := range forbidden {
			if strings.Contains(lower, f) {
				t.Errorf("%s contains forbidden word %q: %q", field, f, s)
			}
		}
	}
	check("Report.Label", rep.Label)
	for _, s := range rep.Suspicions {
		check("Suspicion.Reason", s.Reason)
	}
	if !rep.Unverified {
		t.Error("Report.Unverified must always be true")
	}
}

// TestRun_SkipsGeneratedGoFiles: a *.go file carrying the standard "Code
// generated ... DO NOT EDIT" header is never scanned — TGD-BL-42-adjacent
// reasoning: sqlc emits the same SQL text twice (once in *.sql, once in a
// generated *.sql.go), and scanning both would double-count every
// statement in a sqlc-shaped repo (coder among them).
func TestRun_SkipsGeneratedGoFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "db/queries.sql.go", "// Code generated by sqlc. DO NOT EDIT.\n"+
		"package db\n\nconst insertWidget = `INSERT INTO widgets (id, name) VALUES ($1, $2)`\n")
	writeFile(t, dir, "queries/widgets.sql", `
-- name: GetWidget :one
SELECT * FROM widgets WHERE organization_id = $1;
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Suspicions) != 0 {
		t.Fatalf("got %d suspicions, want 0 — the generated file's INSERT must not have been scanned: %+v",
			len(rep.Suspicions), rep.Suspicions)
	}
}

// TestRun_ScansHandWrittenGoStringLiterals: a repo with no *.sql files at
// all, SQL embedded directly in Go source (database/sql style, not sqlc) —
// Tier 0 must still find it, since "any Go repository" (TGD-US-08 AC-1)
// includes repos that never adopted sqlc's file convention.
func TestRun_ScansHandWrittenGoStringLiterals(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "store/widgets.go", `package store

import "database/sql"

func GetWidget(db *sql.DB, id string) {
	db.Query("SELECT * FROM widgets WHERE organization_id = $1 AND id = $2", id)
}

func InsertWidgetMissingTenant(db *sql.DB, id, name string) {
	db.Exec("INSERT INTO widgets (id, name) VALUES ($1, $2)", id, name)
}
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Suspicions) != 1 {
		t.Fatalf("got %d suspicions, want 1: %+v", len(rep.Suspicions), rep.Suspicions)
	}
	if rep.Suspicions[0].Table != "widgets" {
		t.Errorf("got table %q, want widgets", rep.Suspicions[0].Table)
	}
}

// TestRun_RanksWritesBeforeReads: given both a flagged write and a flagged
// read against equally-confident tables, the write must rank first.
func TestRun_RanksWritesBeforeReads(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "queries/a.sql", `
-- name: ScopedReadA :many
SELECT * FROM widgets_a WHERE organization_id = $1;

-- name: UnscopedReadA :many
SELECT * FROM widgets_a WHERE id = $1;
`)
	writeFile(t, dir, "queries/b.sql", `
-- name: ScopedReadB :many
SELECT * FROM widgets_b WHERE organization_id = $1;

-- name: UnscopedWriteB :exec
INSERT INTO widgets_b (id) VALUES ($1);
`)
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Suspicions) != 2 {
		t.Fatalf("got %d suspicions, want 2: %+v", len(rep.Suspicions), rep.Suspicions)
	}
	if rep.Suspicions[0].StatementKind != "WRITE" || rep.Suspicions[0].Rank != 1 {
		t.Errorf("suspicions[0] = %+v, want the WRITE ranked first", rep.Suspicions[0])
	}
	if rep.Suspicions[1].StatementKind != "READ" || rep.Suspicions[1].Rank != 2 {
		t.Errorf("suspicions[1] = %+v, want the READ ranked second", rep.Suspicions[1])
	}
}

// TestRun_PlainEnglishCLIHelpTextNotMisclassifiedAsSQL reproduces a real
// defect found running this package against real coder source in the same
// session it was written: "Update the coder section in %s" and "Delete all
// encrypted data from the database" (real coder CLI help strings) both
// start with a SQL verb keyword and were misclassified as SQL, with "the"
// extracted as a table name via the UPDATE/DELETE regex, before
// looksLikeSQL's structural-keyword gate (SET for UPDATE, FROM for
// DELETE/SELECT, INTO for INSERT) was added.
func TestRun_PlainEnglishCLIHelpTextNotMisclassifiedAsSQL(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cli/help.go", "package cli\n\n"+
		"const updateHelp = \"Update the coder section in %s\"\n"+
		"const deleteHelp = \"Delete all encrypted data from the database. THIS IS A DESTRUCTIVE OPERATION.\"\n"+
		"const selectHelp = \"Select an option from the menu below\"\n")
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.StatementsFound != 0 {
		t.Fatalf("statements_found = %d, want 0 — none of these are SQL: %+v",
			rep.StatementsFound, rep.Suspicions)
	}
	for _, s := range rep.Suspicions {
		if s.Table == "the" {
			t.Fatalf("english prose misclassified as SQL, table=%q: %+v", s.Table, s)
		}
	}
}

// TestRun_NonexistentRepoRootReturnsError: Run reports a usage-shaped
// error for a bad path rather than a fabricated empty report — the CLI
// layer, not this package, decides what exit code that becomes (package
// doc: TGD-US-08 AC-1's "exits zero regardless of findings" is a promise
// about the analysis, not about a nonsense invocation).
func TestRun_NonexistentRepoRootReturnsError(t *testing.T) {
	_, err := Run(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("got nil error, want one for a nonexistent repo root")
	}
}

// TestRun_MigrationsDirectorySkipped: a *.sql file under a migrations/
// directory is schema (DDL), not an application query — must not
// contribute a false "table" from an ALTER/CREATE TABLE statement.
func TestRun_MigrationsDirectorySkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "migrations/0001_create_widgets.sql",
		"CREATE TABLE widgets (id uuid PRIMARY KEY, organization_id uuid NOT NULL);\n")
	rep, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.StatementsFound != 0 {
		t.Fatalf("statements_found = %d, want 0 — migrations must be skipped", rep.StatementsFound)
	}
}
