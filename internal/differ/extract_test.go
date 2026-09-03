package differ

import (
	"strings"
	"testing"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// These tests are deliberately database-free: ExtractTenant is a pure text
// heuristic over (SQL, declared scoped relations, resolved parameters). It is
// not a SQL parser — Postgres remains the authority on what a query means.
// Its only job is to find A PLAUSIBLE tenant value to bind the restricted
// re-execution to; Diff's own RLS-enforced re-execution is what actually
// proves a leak, regardless of how the extraction got there. That division of
// labour is why L3/L8/L9/L10 (below, in the full corpus) need no special
// handling here: RLS enforces correctly on re-execution no matter how the
// query's own WHERE clause is written.

func invoicesScoped() []schema.Relation {
	return []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
	}
}

func twoScopedTables() []schema.Relation {
	return []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "audit_log", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
	}
}

func textParam(v string) capture.Param { return capture.Param{Known: true, Text: v} }
func binaryParam() capture.Param {
	return capture.Param{Known: false, Reason: "binary parameter; type OID not captured"}
}

func TestExtractTenant_ResolvedFromParameter(t *testing.T) {
	a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1",
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme)", a)
	}
}

func TestExtractTenant_AliasPrefixAndCast(t *testing.T) {
	a := ExtractTenant(`SELECT i.* FROM invoices i WHERE i.tenant_id::text = $1`,
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) despite alias+cast", a)
	}
}

func TestExtractTenant_NoPredicateAtAll(t *testing.T) {
	// L1's shape: a scoped table is referenced, but nothing compares its
	// tenant column to anything. This must NOT be Unattributable — it falls
	// through to "no tenant claimed," which Diff turns into a LEAK verdict by
	// comparing against an RLS-denies-everything re-execution.
	a := ExtractTenant("SELECT * FROM invoices WHERE id = $1",
		invoicesScoped(), []capture.Param{textParam("42")})
	if a.Kind != AttrNoPredicate {
		t.Fatalf("got %+v, want AttrNoPredicate", a)
	}
}

func TestExtractTenant_TautologyFallsToNoPredicate(t *testing.T) {
	// L6's shape. tenant_id = tenant_id provides no discriminating value —
	// it is syntactically a comparison but carries no usable tenant value, so
	// it must be treated the same as "no predicate," not guessed at and not
	// reported Unattributable.
	a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = tenant_id",
		invoicesScoped(), nil)
	if a.Kind != AttrNoPredicate {
		t.Fatalf("got %+v, want AttrNoPredicate for a tautology", a)
	}
}

func TestExtractTenant_ORClauseStillResolvesTheRealParam(t *testing.T) {
	// L10's shape: the OR defeats the application's OWN scoping intent, but
	// extraction only needs to find the $1 reference — RLS's re-execution is
	// what actually reveals the leak from the extra OR-matched rows.
	a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1 OR status = 'public'",
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme)", a)
	}
}

func TestExtractTenant_SubqueryIsUnattributable(t *testing.T) {
	// U1's shape: the tenant is computed by a subquery, not present as a
	// value extraction can read off directly.
	a := ExtractTenant(
		"SELECT * FROM invoices WHERE tenant_id = (SELECT tenant_id FROM users WHERE id = $1)",
		invoicesScoped(), []capture.Param{textParam("7")})
	if a.Kind != AttrUnattributable {
		t.Fatalf("got %+v, want AttrUnattributable (subquery)", a)
	}
	if a.Reason == "" {
		t.Errorf("Unattributable result must carry a reason")
	}
}

func TestExtractTenant_NoScopedTableReferencedIsUnattributable(t *testing.T) {
	// U2's shape: the query never names a declared scoped table at all — a
	// set-returning function hides the real table access from a text scan.
	a := ExtractTenant("SELECT * FROM scoped_summary()", invoicesScoped(), nil)
	if a.Kind != AttrUnattributable {
		t.Fatalf("got %+v, want AttrUnattributable (no scoped table referenced)", a)
	}
}

// TestReferencesScopedTable_TrueWhenReferenced/_FalseWhenNot are
// internal/guardrail's (TGD-US-12) load-bearing distinction: a query
// referencing no scoped table at all must be allowed through a fail-closed
// guardrail (nothing to enforce), which AttrUnattributable's single Kind
// cannot distinguish from a scoped table whose tenant genuinely could not
// be determined — see ReferencesScopedTable's own doc comment.
func TestReferencesScopedTable_TrueWhenReferenced(t *testing.T) {
	if !ReferencesScopedTable("SELECT * FROM invoices WHERE id = $1", invoicesScoped()) {
		t.Fatal("got false, want true — invoices is declared Scoped and is referenced")
	}
}

func TestReferencesScopedTable_FalseWhenNotReferenced(t *testing.T) {
	if ReferencesScopedTable("SELECT now()", invoicesScoped()) {
		t.Fatal("got true, want false — no table is referenced at all")
	}
	if ReferencesScopedTable("SELECT * FROM scoped_summary()", invoicesScoped()) {
		t.Fatal("got true, want false — U2's shape: the real table is hidden inside a function call")
	}
}

// TestReferencesScopedTable_FalseForUnscopedTable: a table the policy
// declares Unscoped (or doesn't declare at all) must not count, even if
// referenced by name — only Scoped relations are enforcement-relevant.
func TestReferencesScopedTable_FalseForUnscopedTable(t *testing.T) {
	rels := []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "countries", Kind: "BASE TABLE", Class: schema.Unscoped},
	}
	if ReferencesScopedTable("SELECT * FROM countries", rels) {
		t.Fatal("got true, want false — countries is declared Unscoped")
	}
}

func TestExtractTenant_ConflictingValuesAcrossTablesIsUnattributable(t *testing.T) {
	// U3's shape: two scoped tables, two DIFFERENT resolved tenant values.
	a := ExtractTenant(
		"SELECT * FROM invoices i JOIN audit_log a ON a.invoice_id = i.id "+
			"WHERE i.tenant_id = $1 AND a.tenant_id = $2",
		twoScopedTables(), []capture.Param{textParam("acme"), textParam("globex")})
	if a.Kind != AttrUnattributable {
		t.Fatalf("got %+v, want AttrUnattributable (conflicting values)", a)
	}
}

func TestExtractTenant_AgreeingValuesAcrossTablesResolve(t *testing.T) {
	// S3's shape: both sides scoped, both correctly bound to the SAME tenant.
	a := ExtractTenant(
		"SELECT * FROM invoices i JOIN audit_log a ON a.invoice_id = i.id "+
			"WHERE i.tenant_id = $1 AND a.tenant_id = $2",
		twoScopedTables(), []capture.Param{textParam("acme"), textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) when both sides agree", a)
	}
}

// TestExtractTenant_BinaryParameterIsUnattributableNeverSafe is this turn's
// explicit requirement: a parameter that could not be decoded must never
// produce a resolved value, and must never let a query read as SAFE by
// accident. It is checked twice — once when the undecoded parameter IS the
// tenant column, once when it is a DIFFERENT parameter entirely — because a
// differ that only checks the tenant param's decodability would happily
// substitute garbage for some OTHER unresolved parameter and produce an
// untrustworthy re-execution of a query that never actually ran that way.
func TestExtractTenant_BinaryParameterIsUnattributableNeverSafe(t *testing.T) {
	t.Run("undecoded tenant parameter itself", func(t *testing.T) {
		a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1",
			invoicesScoped(), []capture.Param{binaryParam()})
		if a.Kind != AttrUnattributable {
			t.Fatalf("got %+v, want AttrUnattributable", a)
		}
	})
	t.Run("undecoded OTHER parameter, tenant param resolvable", func(t *testing.T) {
		a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1 AND amount > $2",
			invoicesScoped(), []capture.Param{textParam("acme"), binaryParam()})
		if a.Kind != AttrUnattributable {
			t.Fatalf("got %+v, want AttrUnattributable even though only the SECOND "+
				"parameter is undecoded — the query cannot be safely re-executed at all "+
				"without knowing every parameter's real value", a)
		}
	})
}

// TestExtractTenant_UnattributableReasonNamesParameterPositions is TGD-US-05
// AC-2: the report must state WHICH parameter positions were unrecoverable,
// not just that "at least one" was — an operator debugging a real capture
// needs to know which bind value to go look at.
func TestExtractTenant_UnattributableReasonNamesParameterPositions(t *testing.T) {
	t.Run("single unrecoverable position", func(t *testing.T) {
		a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1",
			invoicesScoped(), []capture.Param{binaryParam()})
		if !strings.Contains(a.Reason, "$1") {
			t.Errorf("reason does not name position $1: %q", a.Reason)
		}
	})
	t.Run("multiple unrecoverable positions are all named", func(t *testing.T) {
		a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1 AND amount > $2 AND status = $3",
			invoicesScoped(), []capture.Param{binaryParam(), textParam("acme"), binaryParam()})
		if !strings.Contains(a.Reason, "$1") || !strings.Contains(a.Reason, "$3") {
			t.Errorf("reason does not name both unrecoverable positions $1 and $3: %q", a.Reason)
		}
		if strings.Contains(a.Reason, "$2") {
			t.Errorf("reason wrongly names $2, which WAS recoverable: %q", a.Reason)
		}
	})
}

// TestExtractTenant_InsertPositionalValue covers S9's shape: an INSERT names
// its tenant column in the column list and the corresponding VALUE
// positionally, not via a WHERE-clause comparison — the SAME comparison
// syntax the rest of this file assumes does not exist in INSERT statements at
// all. Found by running the differ against a real insert fixture: extraction
// fell through to AttrNoPredicate (no comparison found anywhere), causing
// Diff to bind the restricted re-execution to no tenant, which RLS's implicit
// WITH CHECK then correctly — but unhelpfully — rejected as a policy
// violation on a query that was actually correctly scoped.
func TestExtractTenant_InsertPositionalValue(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (tenant_id, status, amount) VALUES ($1, $2, $3) RETURNING *",
		invoicesScoped(), []capture.Param{textParam("acme"), textParam("open"), textParam("999")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) from the INSERT's column/VALUES position", a)
	}
}

// TestExtractTenant_InsertWithoutTenantColumnFallsToNoPredicate: an insert
// that never mentions the tenant column at all (e.g. it has a default) must
// not be guessed at — same AttrNoPredicate fallback as a SELECT missing its
// tenant predicate.
func TestExtractTenant_InsertWithoutTenantColumnFallsToNoPredicate(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (status, amount) VALUES ($1, $2) RETURNING *",
		invoicesScoped(), []capture.Param{textParam("open"), textParam("50")})
	if a.Kind != AttrNoPredicate {
		t.Fatalf("got %+v, want AttrNoPredicate", a)
	}
}

// TestExtractTenant_InsertWithFunctionCallValueResolves reproduces a real
// defect found running against captured coder traffic (TGD-BL-34): the old
// insertShape/VALUES regex used `\(([^)]*)\)`, which stops at the FIRST `)`
// it sees. When the first value is a function call with its own parens —
// gen_random_uuid(), as coder's UpsertProvisionerDaemon issues — the capture
// truncated at that inner paren, split as one field against a 10-column
// list, and produced a false "column list and VALUES list have different
// lengths" Unattributable verdict for a query that actually resolves cleanly
// (organization_id is column 8, value $7). This must resolve, not error.
func TestExtractTenant_InsertWithFunctionCallValueResolves(t *testing.T) {
	a := ExtractTenant(
		`INSERT INTO invoices (id, status, tenant_id, amount) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING *`,
		invoicesScoped(), []capture.Param{textParam("open"), textParam("acme"), textParam("999")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — a function-call value before the tenant "+
			"column must not break column/VALUES alignment", a)
	}
}

// TestExtractTenant_InsertGenuineLengthMismatchStillUnattributable keeps the
// original protective intent once the parser is fixed: a REAL mismatch
// (fewer values than columns) must still be reported, not silently misread
// as something else now that nested parens are handled correctly.
func TestExtractTenant_InsertGenuineLengthMismatchStillUnattributable(t *testing.T) {
	a := ExtractTenant(
		`INSERT INTO invoices (tenant_id, status, amount) VALUES ($1, $2) RETURNING *`,
		invoicesScoped(), []capture.Param{textParam("acme"), textParam("open")})
	if a.Kind != AttrUnattributable || !strings.Contains(a.Reason, "different lengths") {
		t.Fatalf("got %+v, want AttrUnattributable naming the length mismatch", a)
	}
}

// TestExtractTenant_InsertCastPositionalValueResolves reproduces TGD-BL-42: a
// real defect found running against captured coder traffic. sqlc's default
// codegen casts every positional VALUES parameter ($N::type) — coder's
// InsertChatModelConfig binds organization_id at $11::uuid — but
// extractFromInsert's old matcher was `^\$(\d+)$`, anchored at both ends, so
// the trailing `::uuid` defeated it and a real, present tenant value was
// silently discarded as AttrNoPredicate (the same bucket a genuinely
// unscoped INSERT produces), which downstream fed diffWrite an empty tenant
// and produced a false LEAK verdict — not from real cross-tenant exposure,
// but from this tool losing a value it had already captured correctly. Must
// resolve, not fall through.
func TestExtractTenant_InsertCastPositionalValueResolves(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (tenant_id, status, amount) VALUES ($1::uuid, $2::text, $3::integer)",
		invoicesScoped(), []capture.Param{textParam("acme"), textParam("open"), textParam("999")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — a ::type cast on the positional value "+
			"must not defeat extraction", a)
	}
}

// TestExtractTenant_InsertSpacedCastResolves: the same defect with whitespace
// around the `::` — coder's own capture contains both `$N::type` and
// `$N :: type` spacings; a fix anchored to the compact form only would still
// be incomplete.
func TestExtractTenant_InsertSpacedCastResolves(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (tenant_id, status) VALUES ($1 :: uuid, $2)",
		invoicesScoped(), []capture.Param{textParam("acme"), textParam("open")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — whitespace around :: must not defeat extraction", a)
	}
}

// TestExtractTenant_InsertArrayCastResolves: a cast to an array type
// ($N::bigint[]), seen in coder's own batch-insert queries for other
// columns; the tenant column itself is not usually array-typed, but the
// matcher must not special-case away from this shape.
func TestExtractTenant_InsertArrayCastResolves(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (tenant_id, status) VALUES ($1::text[], $2)",
		invoicesScoped(), []capture.Param{textParam("acme"), textParam("open")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — an array cast must not defeat extraction", a)
	}
}

// TestExtractTenant_InsertMultiWordTypeCastResolves: PostgreSQL type names
// are not always one word (timestamp with time zone, double precision); the
// tenant column itself won't usually be cast to one of these, but a sibling
// column in the same VALUES list using one must not corrupt the whole
// column/value alignment the tenant lookup depends on.
func TestExtractTenant_InsertMultiWordTypeCastResolves(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (status, tenant_id) VALUES ($1::timestamp with time zone, $2::uuid)",
		invoicesScoped(), []capture.Param{textParam("2026-01-01"), textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — a multi-word cast on a sibling column "+
			"must not defeat the tenant column's own resolution", a)
	}
}

// TestExtractTenant_InsertNestedCastResolves: a chained cast ($1::uuid::text)
// — two casts stacked, seen nowhere in this project's coder capture but a
// legal PostgreSQL expression a future capture could contain.
func TestExtractTenant_InsertNestedCastResolves(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (tenant_id, status) VALUES ($1::uuid::text, $2)",
		invoicesScoped(), []capture.Param{textParam("acme"), textParam("open")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — a chained cast must not defeat extraction", a)
	}
}

// TestExtractTenant_InsertCastFuncFormResolves: the SQL-standard CAST($N AS
// type) form, an alternative spelling to `::type` that TGD-BL-42's fix
// covers alongside the far more common shorthand — same defect class, same
// silent-AttrNoPredicate failure mode, before the fix.
func TestExtractTenant_InsertCastFuncFormResolves(t *testing.T) {
	a := ExtractTenant(
		"INSERT INTO invoices (tenant_id, status) VALUES (CAST($1 AS uuid), $2)",
		invoicesScoped(), []capture.Param{textParam("acme"), textParam("open")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — CAST($N AS type) must resolve like ::type does", a)
	}
}

// TestExtractTenant_WhereClauseCastRHSResolves documents (and locks in) that
// the WHERE-clause comparison path was already immune to TGD-BL-42: unlike
// extractFromInsert's anchored `^\$(\d+)$`, tenantCompareRegex's
// right-hand-side alternation captures only the `$N` prefix of `$1::uuid`
// (the `::uuid` is left unconsumed, outside the match), so paramRef already
// matched cleanly. This was true before the fix and remains true after it —
// recorded as an explicit test because TGD-BL-42's own investigation found
// this path was clean only by inspection, never by a test naming the cast
// case, which is exactly the gap this closes.
func TestExtractTenant_WhereClauseCastRHSResolves(t *testing.T) {
	a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1::uuid",
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme)", a)
	}
}

// TestExtractTenant_WhereClauseSpacedCastRHSResolves: the same, with
// whitespace around `::`.
func TestExtractTenant_WhereClauseSpacedCastRHSResolves(t *testing.T) {
	a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1 :: uuid",
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme)", a)
	}
}

// TestExtractTenant_WhereClauseArrayCastRHSResolves: an array-cast RHS.
func TestExtractTenant_WhereClauseArrayCastRHSResolves(t *testing.T) {
	a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = $1::text[]",
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme)", a)
	}
}

// TestExtractTenant_WhereClauseCastFuncRHSResolves: TGD-BL-42 also covers
// CAST($N AS type) on a WHERE-clause right-hand side — before the fix,
// tenantCompareRegex's RHS alternation captured only the bare word "CAST"
// (its \w+ fallback), losing the parameter reference inside entirely; the
// fix adds an explicit CAST(...) alternative captured whole.
func TestExtractTenant_WhereClauseCastFuncRHSResolves(t *testing.T) {
	a := ExtractTenant("SELECT * FROM invoices WHERE tenant_id = CAST($1 AS uuid)",
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme)", a)
	}
}

// TestExtractTenant_ViewAliasedToBaseTableNameResolves reproduces a second
// real defect found the same session (TGD-BL-34): coder's
// GetWorkspaceBuildByID queries `FROM workspace_build_with_user AS
// workspace_builds` — a view, aliased to the exact name of the real scoped
// table. The old tableRef regex captured only the token immediately after
// FROM (the view name), never the alias, so the query was reported as
// touching no declared scoped table at all, even though it names the real
// table as its own alias. referencedTables must also recognise the alias.
func TestExtractTenant_ViewAliasedToBaseTableNameResolves(t *testing.T) {
	a := ExtractTenant(
		`SELECT * FROM invoices_with_extra_columns AS invoices WHERE tenant_id = $1`,
		invoicesScoped(), []capture.Param{textParam("acme")})
	if a.Kind != AttrResolved || a.Value != "acme" {
		t.Fatalf("got %+v, want AttrResolved(acme) — the alias names a declared scoped table", a)
	}
}
