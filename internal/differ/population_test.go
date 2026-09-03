package differ

import (
	"testing"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// These tests are the TGD-BL-33/TGD-BL-06 baselining prerequisite: every
// UNATTRIBUTABLE verdict must carry a machine-readable population the tool
// computes itself, replacing the free-text-reason / ad hoc SQL-grepping used
// to derive TGD-BL-33's one-off session analysis. ClassifyUnattributable
// takes only data the tool already has by the time it is called (the SQL
// text, and the row-level/structural-only relation split runOracleGate
// already computes) — no new capture, no database read.

func rowLevelInvoices() []schema.Relation {
	return []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
	}
}

func structuralOnlyProjects() []schema.Relation {
	return []schema.Relation{
		{Schema: "public", Name: "projects", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "org_id"},
	}
}

func TestClassifyUnattributable_NoDeclaredTable(t *testing.T) {
	got := ClassifyUnattributable("SELECT now()", rowLevelInvoices(), structuralOnlyProjects())
	if got != PopulationNoDeclaredTable {
		t.Errorf("got %q, want %q", got, PopulationNoDeclaredTable)
	}
}

func TestClassifyUnattributable_StructuralOnly(t *testing.T) {
	got := ClassifyUnattributable("SELECT * FROM projects WHERE org_id = 'acme'",
		rowLevelInvoices(), structuralOnlyProjects())
	if got != PopulationStructuralOnly {
		t.Errorf("got %q, want %q", got, PopulationStructuralOnly)
	}
}

func TestClassifyUnattributable_RowLevelUnattributed(t *testing.T) {
	got := ClassifyUnattributable("SELECT * FROM invoices_view AS invoices WHERE 1=1",
		rowLevelInvoices(), structuralOnlyProjects())
	if got != PopulationRowLevelUnattributed {
		t.Errorf("got %q, want %q", got, PopulationRowLevelUnattributed)
	}
}

// TestClassifyUnattributable_RowLevelTakesPriorityOverStructural: a query
// touching BOTH a row-level and a structural-only table is
// row_level_unattributed, not structural_only — the row-level table is the
// one whose coverage the tool can actually reason about, and the reason
// attribution failed is not explained by the structural gap alone.
func TestClassifyUnattributable_RowLevelTakesPriorityOverStructural(t *testing.T) {
	got := ClassifyUnattributable(
		"SELECT * FROM invoices_view AS invoices JOIN projects ON projects.id = invoices.project_id",
		rowLevelInvoices(), structuralOnlyProjects())
	if got != PopulationRowLevelUnattributed {
		t.Errorf("got %q, want %q", got, PopulationRowLevelUnattributed)
	}
}

// TestClassifyUnattributable_NonQuery covers every prefix TGD-BL-33's
// analysis found in real coder traffic: cursor protocol (DECLARE/FETCH/
// CLOSE/LOCK) and session bookkeeping (BEGIN/COMMIT/SET/RESET/PREPARE/
// EXECUTE). This check runs BEFORE table-reference matching — a pg_dump
// DECLARE CURSOR naming a real scoped table by name is still non_query, not
// row_level_unattributed, since it is diagnostic-dump traffic, not
// application SQL.
func TestClassifyUnattributable_NonQuery(t *testing.T) {
	cases := []string{
		`DECLARE _pg_dump_cursor CURSOR FOR SELECT * FROM ONLY public.invoices`,
		`FETCH 100 FROM _pg_dump_cursor`,
		`CLOSE _pg_dump_cursor`,
		`LOCK TABLE public.invoices, public.projects IN ACCESS SHARE MODE`,
		`BEGIN READ WRITE`,
		`COMMIT`,
		`SET statement_timeout = 0`,
		`RESET ALL`,
		`PREPARE stmt AS SELECT * FROM invoices`,
		`EXECUTE stmt`,
	}
	for _, sql := range cases {
		got := ClassifyUnattributable(sql, rowLevelInvoices(), structuralOnlyProjects())
		if got != PopulationNonQuery {
			t.Errorf("ClassifyUnattributable(%q) = %q, want %q", sql, got, PopulationNonQuery)
		}
	}
}

// TestClassifyUnattributable_NonQueryIgnoresLeadingComment: sqlc-generated
// coder queries carry a "-- name: X :one" header comment (TGD-BL-31); a
// non-query statement issued by a driver never carries one in practice, but
// the check must stay consistent with isWriteStatement's own comment-
// skipping rather than silently diverging from it.
func TestClassifyUnattributable_NonQueryIgnoresLeadingComment(t *testing.T) {
	got := ClassifyUnattributable("-- some comment\nBEGIN", rowLevelInvoices(), structuralOnlyProjects())
	if got != PopulationNonQuery {
		t.Errorf("got %q, want %q", got, PopulationNonQuery)
	}
}

// TestClassifyUnattributable_KeywordPrefixIsWordBounded: "SET" must not
// match a real query that merely starts with a similarly-spelled identifier
// — there is no SQL statement type this could realistically collide with,
// but the check is boundary-aware on principle, the same discipline
// isWriteStatement's own prefix check follows.
func TestClassifyUnattributable_KeywordPrefixIsWordBounded(t *testing.T) {
	got := ClassifyUnattributable("SELECT * FROM invoices WHERE tenant_id = $1",
		rowLevelInvoices(), structuralOnlyProjects())
	if got == PopulationNonQuery {
		t.Errorf("a SELECT was misclassified as non_query")
	}
}
