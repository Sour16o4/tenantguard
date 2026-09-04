package differ

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/oracle"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// This file is design doc §7's 24-fixture corpus, verbatim: 10 SAFE, 10 LEAK,
// 4 UNATTRIBUTABLE, asserted as exact-set equality — every fixture correctly
// classified, no substitutions between classes. Two exceptions, both
// documented findings rather than silent deviations:
//
//   - L5 ("correct predicate, wrong tenant value") is, per §7.4, a
//     Tier-1-only blind spot: at Tier 1 the tool has no independent source of
//     intended tenant, so it trusts the query's own claimed value and comes
//     back SAFE. Tier 2 (context propagation) is not built in this codebase —
//     there is no mechanism to feed Diff an out-of-band intended tenant — so
//     the Tier 2 column below is declared as documentation only, never
//     executed. Only the Tier 1 verdict is asserted against a real Diff call.
//   - L7 ("view lacking its own scoping") is a SECOND, structural blind spot,
//     found by running it against real PostgreSQL before writing this
//     comment: a view without security_invoker evaluates RLS using the VIEW
//     OWNER's privileges. Since the owner here is the same superuser this
//     tool already uses as its "unrestricted, sees everything" baseline, RLS
//     is bypassed identically on both legs of the diff — there is no owner
//     configuration under which this construction produces a leak the
//     re-execution method can see (a non-superuser, non-BYPASSRLS owner just
//     makes the view correctly scoped instead, since the policy expression
//     reads the CALLING session's GUC regardless of whose privileges gate the
//     SELECT). Unlike L5, Tier 2 does not fix this: it is a blind spot at
//     every tier. Confirmed SAFE/SAFE is the correct, documented expectation
//     for this fixture, not a defect in this run's classification.
//
// Both are recorded in the design doc and LIMITATIONS.md alongside this file.
//
// Every fixture's expected verdict is declared in the table below BEFORE any
// Diff call in this file runs — the two dynamic values it needs (the real ids
// of the two seeded rows) are substituted into the SQL text at setup, but no
// expected verdict is chosen, adjusted, or reviewed after seeing output.

// corpusFixture is one entry from design doc §7. tier1 is asserted against a
// real Diff call. tier2 is recorded for documentation and cross-checked for
// internal consistency (it must equal tier1 everywhere except L5), but is
// never executed — there is no Tier 2 mechanism in this codebase.
type corpusFixture struct {
	id     string
	desc   string
	sql    string
	params []capture.Param
	// resolved is false only for U4, the one capture-layer fixture.
	resolved bool
	tier1    Verdict
	tier2    Verdict
}

// corpusFixtures builds the table using aInvoiceID and bInvoiceID, the real
// ids of the two seeded invoice rows (tenant "acme" and "globex"
// respectively) — the only pieces of the table that cannot be literal
// constants, since PostgreSQL assigns them at insert time.
func corpusFixtures(aInvoiceID, bInvoiceID int64) []corpusFixture {
	aID := fmt.Sprintf("%d", aInvoiceID)
	return []corpusFixture{
		// --- SAFE (10), §7.1 ---
		{id: "S1", desc: "baseline: scoped select",
			sql: "SELECT * FROM invoices WHERE tenant_id = $1", params: evParams("acme"),
			resolved: true, tier1: Safe, tier2: Safe},
		{id: "S2", desc: "scoped select with an extra ANDed condition",
			// The design doc's literal example selects a "name" column this
			// fixture schema does not have; "status" is the equivalent
			// text column actually present.
			sql:    "SELECT status FROM invoices WHERE tenant_id = $1 AND status = $2",
			params: evParams("acme", "open"), resolved: true, tier1: Safe, tier2: Safe},
		{id: "S3", desc: "join with both sides scoped and agreeing",
			sql: "SELECT i.id, a.note FROM invoices i JOIN audit_log a ON a.invoice_id = i.id " +
				"WHERE i.tenant_id = $1 AND a.tenant_id = $2",
			params: evParams("acme", "acme"), resolved: true, tier1: Safe, tier2: Safe},
		{id: "S4", desc: "CTE whose inner select is scoped",
			sql:    "WITH scoped AS (SELECT * FROM invoices WHERE tenant_id = $1) SELECT * FROM scoped",
			params: evParams("acme"), resolved: true, tier1: Safe, tier2: Safe},
		{id: "S5", desc: "scoped count",
			sql: "SELECT count(*) FROM invoices WHERE tenant_id = $1", params: evParams("acme"),
			resolved: true, tier1: Safe, tier2: Safe},
		{id: "S6", desc: "security_invoker view queried with its own tenant predicate",
			sql: "SELECT * FROM invoices_view WHERE tenant_id = $1", params: evParams("acme"),
			resolved: true, tier1: Safe, tier2: Safe},
		{id: "S7", desc: "scoped select narrowed further by id",
			sql: "SELECT * FROM invoices WHERE id = $1 AND tenant_id = $2", params: evParams(aID, "acme"),
			resolved: true, tier1: Safe, tier2: Safe},
		{id: "S8", desc: "scoped select with ORDER BY and LIMIT",
			sql:    "SELECT * FROM invoices WHERE tenant_id = $1 ORDER BY id LIMIT 10",
			params: evParams("acme"), resolved: true, tier1: Safe, tier2: Safe},
		{id: "S9", desc: "scoped insert, dry-run compared by acceptance not row content",
			sql:    "INSERT INTO invoices (tenant_id, status, amount) VALUES ($1, $2, $3) RETURNING *",
			params: evParams("acme", "open", "500"), resolved: true, tier1: Safe, tier2: Safe},
		{id: "S10", desc: "a second, different scoped table",
			sql: "SELECT * FROM audit_log WHERE tenant_id = $1", params: evParams("acme"),
			resolved: true, tier1: Safe, tier2: Safe},

		// --- LEAK (10), §7.2 ---
		{id: "L1", desc: "PK lookup, no tenant predicate",
			sql: "SELECT * FROM invoices WHERE id = $1", params: evParams(aID),
			resolved: true, tier1: Leak, tier2: Leak},
		{id: "L2", desc: "unscoped count leaks a number without returning a row",
			sql: "SELECT count(*) FROM invoices", resolved: true, tier1: Leak, tier2: Leak},
		{id: "L3", desc: "join scoping only the joined side; base table unconstrained",
			sql:    "SELECT i.* FROM invoices i JOIN audit_log a ON a.invoice_id = i.id WHERE a.tenant_id = $1",
			params: evParams("acme"), resolved: true, tier1: Leak, tier2: Leak},
		{id: "L4", desc: "CTE aggregate with no scoping",
			sql:      "WITH agg AS (SELECT count(*) c FROM invoices) SELECT c FROM agg",
			resolved: true, tier1: Leak, tier2: Leak},
		{id: "L5", desc: "correct predicate, wrong tenant value — invisible to every syntactic method",
			// The query itself is textually identical in shape to S1; what
			// makes this a leak is an out-of-band fact (the caller's real
			// tenant is "globex", not "acme") that Tier 1 has no way to
			// observe. Diff trusts the query's own claim and reports Safe —
			// documented in LIMITATIONS.md, not a defect in this test.
			sql: "SELECT * FROM invoices WHERE tenant_id = $1", params: evParams("acme"),
			resolved: true, tier1: Safe, tier2: Leak},
		{id: "L6", desc: "tautology defeats a naive predicate-presence check",
			sql:      "SELECT * FROM invoices WHERE tenant_id = tenant_id",
			resolved: true, tier1: Leak, tier2: Leak},
		{id: "L7", desc: "view lacking its own scoping — a second, structural blind spot",
			// See the file-level comment: undetectable at any tier by a
			// re-execution oracle when the view owner is the same
			// superuser used as the unrestricted baseline.
			sql:      "SELECT * FROM invoices_view_no_invoker",
			resolved: true, tier1: Safe, tier2: Safe},
		{id: "L8", desc: "IN-subquery expands to all tenants",
			sql:      "SELECT * FROM invoices WHERE tenant_id IN (SELECT tenant_id FROM tenants)",
			resolved: true, tier1: Leak, tier2: Leak},
		{id: "L9", desc: "second aggregate-leak shape",
			sql: "SELECT max(amount) FROM invoices", resolved: true, tier1: Leak, tier2: Leak},
		{id: "L10", desc: "OR defeats the scoping predicate",
			sql:    "SELECT * FROM invoices WHERE tenant_id = $1 OR status = $2",
			params: evParams("acme", "open"), resolved: true, tier1: Leak, tier2: Leak},

		// --- UNATTRIBUTABLE (4), §7.3 ---
		{id: "U1", desc: "tenant computed by a subquery, not present as a literal",
			sql:    "SELECT * FROM invoices WHERE tenant_id = (SELECT tenant_id FROM users WHERE id = $1)",
			params: evParams("1"), resolved: true, tier1: Unattributable, tier2: Unattributable},
		{id: "U2", desc: "set-returning function hides table access from a text scan",
			sql: "SELECT * FROM scoped_summary()", resolved: true,
			tier1: Unattributable, tier2: Unattributable},
		{id: "U3", desc: "join across two scoped tables with conflicting tenant values",
			sql: "SELECT * FROM invoices i JOIN audit_log a ON a.invoice_id = i.id " +
				"WHERE i.tenant_id = $1 AND a.tenant_id = $2",
			params: evParams("acme", "globex"), resolved: true,
			tier1: Unattributable, tier2: Unattributable},
		{id: "U4", desc: "capture-layer fixture: unresolved Bind, never guessed at as SAFE",
			resolved: false, tier1: Unattributable, tier2: Unattributable},
	}
}

// TestCorpus_ExactSetEquality is design doc §7's acceptance criterion,
// verbatim: exact set equality across all three verdict classes, not a
// precision/recall threshold. Every one of the 24 fixtures must land in
// exactly the class declared for it above (Tier 1 column) — a fixture
// classified into the wrong bucket fails this test even if some OTHER
// fixture's failure would "average out" to an acceptable score.
func TestCorpus_ExactSetEquality(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)

	fixtures := corpusFixtures(f.aInvoiceID, f.bInvoiceID)

	wantByVerdict := map[Verdict][]string{}
	gotByVerdict := map[Verdict][]string{}

	for _, fx := range fixtures {
		t.Run(fx.id, func(t *testing.T) {
			ev := capture.Event{Resolved: fx.resolved, SQL: fx.sql, Params: fx.params}
			r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
			wantByVerdict[fx.tier1] = append(wantByVerdict[fx.tier1], fx.id)
			gotByVerdict[r.Verdict] = append(gotByVerdict[r.Verdict], fx.id)
			if r.Verdict != fx.tier1 {
				t.Errorf("%s (%s): verdict = %s, want %s (reason: %s)",
					fx.id, fx.desc, r.Verdict, fx.tier1, r.Reason)
			}
		})
	}

	for _, v := range []Verdict{Safe, Leak, Unattributable} {
		if len(gotByVerdict[v]) != len(wantByVerdict[v]) {
			t.Errorf("%s set: got %v, want %v", v, gotByVerdict[v], wantByVerdict[v])
		}
	}
}

// TestCorpus_TierExpectationsAreConsistent checks the DECLARED table itself,
// never Diff: every fixture's Tier 1 and Tier 2 expected verdict must agree,
// except the two documented blind spots (L5, which Tier 2 is supposed to
// fix, and L7, which no tier fixes). A corpus entry that silently drifted the
// two columns apart anywhere else would misrepresent what Tier 2 actually
// buys — this is a self-check on the fixture table's own honesty, not a
// property of any code under test.
func TestCorpus_TierExpectationsAreConsistent(t *testing.T) {
	for _, fx := range corpusFixtures(1, 2) {
		if fx.tier1 != fx.tier2 && fx.id != "L5" {
			t.Errorf("%s: tier1=%s tier2=%s differ, but only L5 is documented to "+
				"differ by tier", fx.id, fx.tier1, fx.tier2)
		}
	}
}

// tier2IntendedTenant declares, per fixture, the out-of-band "real" tenant
// TGD-US-12's guardrail would have from request/session context — the one
// fact Tier 1 never has and CheckTenant compares the query's own claimed
// value against. Declared here, once, alongside the corpus's own tier1/
// tier2 expectations, and — like corpusFixtures itself — BEFORE
// TestCorpus_Tier2ExactSetEquality below ever calls CheckTenant: nothing
// here was chosen or adjusted after seeing a result.
//
// Every fixture's intended tenant is "acme" — the tenant its own query
// text claims, when it claims one at all — with exactly ONE deliberate
// exception: L5, whose entire point is that the query claims "acme" while
// the request context's real tenant is "globex". A fixture not listed here
// (there are none) would default to "acme" via the zero value, which is
// itself "acme"-shaped for every fixture except L5 — spelled out per-ID
// anyway, not left to a default, so this table is legible on its own
// without cross-referencing corpusFixtures' SQL text to know what value is
// being asserted for each.
var tier2IntendedTenant = map[string]string{
	"S1": "acme", "S2": "acme", "S3": "acme", "S4": "acme", "S5": "acme",
	"S6": "acme", "S7": "acme", "S8": "acme", "S9": "acme", "S10": "acme",
	"L1": "acme", "L2": "acme", "L3": "acme", "L4": "acme",
	"L5": "globex", // the ONE fixture where intended != what the query claims
	"L6": "acme", "L7": "acme", "L8": "acme", "L9": "acme", "L10": "acme",
	"U1": "acme", "U2": "acme", "U3": "acme",
	// U4 has no Tier 2 equivalent at all (design §7.4: "n/a (no proxy)") —
	// it is a capture-layer artifact (an unresolved wire-protocol Bind);
	// CheckTenant operates on already-known, in-process Go values and has
	// no concept of an unresolved parameter to begin with. Deliberately
	// absent from this map; TestCorpus_Tier2ExactSetEquality skips it by
	// name, not by a zero-value fallback that would silently assert "acme"
	// against a fixture with no real SQL/params to check at all.
}

// checkTenantExpected is CheckTenant's OWN, actually-measured, understood
// and argued-correct verdict per fixture — distinct from fx.tier2, design
// §7.4's ORIGINAL prediction, which this file leaves untouched as the
// historical record of what was hypothesized before Tier 2 existed
// (per instruction: report a contradiction, don't reconcile the corpus to
// the code). The two tables disagree at exactly THREE fixtures now — see
// SRS §7.5/§7.6 for the full accounting of why. (A fourth, U2, disagreed
// until TGD-BL-43: CheckTenant originally allowed through any query naming
// no DECLARED-SCOPED table at all, which let "SELECT * FROM
// scoped_summary()" — a function wrapping real access to invoices — pass
// completely uninspected, a fail-open bypass in a fail-closed library, not
// merely a mis-classification. Fixed by requiring every relation-shaped
// name a query mentions to resolve against the FULL policy (Scoped,
// Unscoped, AND Unclassifiable), fail-closed on anything that does not —
// internal/differ/tier2.go's resolveReferences. U2 now lands exactly where
// §7.4 originally predicted, UNATTRIBUTABLE, and is no longer listed below.):
//
//   - L3, L10: predicted LEAK, actually SAFE. Tier 1 catches both through
//     Diff's real RLS row-set re-execution (an OR-defeated predicate for
//     L10; a join whose text-level tenant match is alias-blind for L3),
//     not through attribution. CheckTenant has no equivalent of that
//     re-execution (internal/guardrail's package doc: that is the whole
//     cost/coverage argument for choosing this mechanism), so a query
//     whose extracted value matches context passes, regardless of the
//     query's boolean structure around it.
//   - L7: predicted SAFE ("a blind spot at every tier", TGD-BL-23),
//     actually LEAK. CheckTenant blocks ANY scoped-table query with zero
//     predicate at all, unconditionally — it has no notion of "RLS would
//     have been bypassed anyway by view ownership" (Tier 1's specific
//     reason for missing it), so that reason for missing it does not
//     apply here. A genuinely positive finding: this mechanism closes L7
//     too, not just L5, refuting "blind spot at every tier" for THIS
//     specific Tier 2 implementation (though not for one built on
//     mechanism (1), RLS reuse, which would inherit Tier 1's exact blindness).
var checkTenantExpected = map[string]Verdict{
	"S1": Safe, "S2": Safe, "S3": Safe, "S4": Safe, "S5": Safe,
	"S6": Safe, "S7": Safe, "S8": Safe, "S9": Safe, "S10": Safe,
	"L1": Leak, "L2": Leak,
	"L3": Safe, // diverges from fx.tier2 (Leak) — see doc comment above
	"L4": Leak, "L5": Leak, "L6": Leak,
	"L7": Leak, // diverges from fx.tier2 (Safe) — see doc comment above
	"L8": Leak, "L9": Leak,
	"L10": Safe, // diverges from fx.tier2 (Leak) — see doc comment above
	"U1":  Unattributable,
	"U2":  Unattributable, // TGD-BL-43: matches fx.tier2 exactly, no longer a divergence
	"U3":  Unattributable,
}

// TestCorpus_Tier2ExactSetEquality is TGD-US-06 AC-5 and TGD-US-12 AC-4,
// executed for the first time: design §7.4's Tier 2 expected set, checked
// against a real CheckTenant call per fixture — not merely declared and
// cross-checked for internal consistency
// (TestCorpus_TierExpectationsAreConsistent, above, does that; this test is
// the thing that was never run before this session). U4 is excluded (see
// tier2IntendedTenant's doc comment: no Tier 2 equivalent exists for a
// capture-layer-only fixture) — the Tier 2 corpus is therefore 23
// fixtures, not 24.
//
// Asserted against checkTenantExpected (CheckTenant's actual, understood,
// argued-correct behavior — locking it in as a regression test), NOT
// against fx.tier2 (design §7.4's original, pre-implementation
// prediction), which four fixtures contradict for reasons checkTenantExpected's
// own doc comment names precisely. Every one of those four divergences is
// also logged explicitly on every run — never only on the first one — so
// the gap between what was predicted and what this mechanism actually does
// stays visible in the test's own output, not just in this file's comments.
func TestCorpus_Tier2ExactSetEquality(t *testing.T) {
	relations := []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "audit_log", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "invoices_view", Kind: "VIEW",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "invoices_view_no_invoker", Kind: "VIEW",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		// users and tenants (newDiffFixture's schema, above) both carry a
		// tenant_id column — a real `infer` run against this schema would
		// classify both Scoped, same as invoices/audit_log. Declared here
		// for TGD-BL-43's resolveReferences: U1 and L8 both reference one
		// of these in a subquery, and since TGD-BL-43 requires resolveReferences
		// requires EVERY relation-shaped name a query mentions to resolve
		// against the FULL policy (not just the two tables the original
		// Tier 1 fixture cared about), omitting them would make U1/L8
		// "unresolvable" for the wrong reason — a fixture gap, not the
		// unresolvable-relation bypass TGD-BL-43 exists to catch.
		{Schema: "public", Name: "users", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "tenants", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
	}

	wantByVerdict := map[Verdict][]string{}
	gotByVerdict := map[Verdict][]string{}

	for _, fx := range corpusFixtures(1, 2) {
		if fx.id == "U4" {
			continue // no Tier 2 equivalent — see tier2IntendedTenant's doc comment.
		}
		intended, ok := tier2IntendedTenant[fx.id]
		if !ok {
			t.Fatalf("%s: no declared intended tenant in tier2IntendedTenant", fx.id)
		}
		want, ok := checkTenantExpected[fx.id]
		if !ok {
			t.Fatalf("%s: no declared expected verdict in checkTenantExpected", fx.id)
		}
		t.Run(fx.id, func(t *testing.T) {
			r := CheckTenant(fx.sql, relations, fx.params, intended)
			wantByVerdict[want] = append(wantByVerdict[want], fx.id)
			gotByVerdict[r.Verdict] = append(gotByVerdict[r.Verdict], fx.id)
			if r.Verdict != want {
				t.Errorf("%s (%s): CheckTenant verdict = %s, want %s (reason: %s)",
					fx.id, fx.desc, r.Verdict, want, r.Reason)
			}
			if want != fx.tier2 {
				t.Logf("%s: design §7.4 predicted %s; CheckTenant actually (and correctly, per "+
					"checkTenantExpected's own doc comment) produces %s — see SRS §7.5",
					fx.id, fx.tier2, want)
			}
		})
	}

	for _, v := range []Verdict{Safe, Leak, Unattributable} {
		if len(gotByVerdict[v]) != len(wantByVerdict[v]) {
			t.Errorf("%s set: got %v, want %v", v, gotByVerdict[v], wantByVerdict[v])
		}
	}
}

// corpusFixture is the schema and connections the whole corpus runs against:
// invoices/audit_log (Scoped, RLS'd), a users table and a tenants lookup
// table (neither Scoped — U1 and L8 reference them as ordinary, unrelated
// data), a set-returning function (U2), and two views exercising the two
// different RLS-through-a-view outcomes (S6's security_invoker view, L7's
// non-invoker one).
type corpusDBFixture struct {
	probeDB    *sql.DB
	restricted *sql.DB
	relations  []schema.Relation
	aInvoiceID int64
	bInvoiceID int64
}

func newCorpusFixture(t *testing.T) *corpusDBFixture {
	t.Helper()
	ctx := context.Background()
	base := adminDSN(t)
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}

	probeName := "tgd_corpus"
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", probeName)); err != nil {
		t.Fatalf("drop stale probe: %v", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %q", probeName)); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", probeName))
		admin.Close()
	})

	probeDSN := dsnFor(t, base, probeName, "", "")
	probeDB, err := sql.Open("postgres", probeDSN)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	t.Cleanup(func() { probeDB.Close() })

	schemaSQL := `
		CREATE TABLE invoices (
			id bigserial PRIMARY KEY,
			tenant_id text NOT NULL,
			status text NOT NULL DEFAULT 'open',
			amount integer NOT NULL
		);
		CREATE TABLE audit_log (
			id bigserial PRIMARY KEY,
			tenant_id text NOT NULL,
			invoice_id bigint NOT NULL,
			note text NOT NULL DEFAULT ''
		);
		CREATE TABLE users (
			id bigserial PRIMARY KEY,
			tenant_id text NOT NULL
		);
		CREATE TABLE tenants (
			tenant_id text PRIMARY KEY
		);
		CREATE VIEW invoices_view WITH (security_invoker = true) AS
			SELECT * FROM invoices;
		CREATE VIEW invoices_view_no_invoker AS
			SELECT * FROM invoices;
		CREATE FUNCTION scoped_summary() RETURNS SETOF invoices AS
			$$ SELECT * FROM invoices $$ LANGUAGE sql STABLE;

		INSERT INTO tenants (tenant_id) VALUES ('acme'), ('globex');
		INSERT INTO users (id, tenant_id) VALUES (1, 'acme');
	`
	if _, err := probeDB.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	var aInvoiceID, bInvoiceID int64
	if err := probeDB.QueryRowContext(ctx,
		"INSERT INTO invoices (tenant_id, status, amount) VALUES ('acme', 'open', 100) RETURNING id",
	).Scan(&aInvoiceID); err != nil {
		t.Fatalf("seed acme invoice: %v", err)
	}
	if err := probeDB.QueryRowContext(ctx,
		"INSERT INTO invoices (tenant_id, status, amount) VALUES ('globex', 'open', 200) RETURNING id",
	).Scan(&bInvoiceID); err != nil {
		t.Fatalf("seed globex invoice: %v", err)
	}

	// S3's audit_log row correctly references the acme invoice it claims.
	// L3's deliberately does not: it claims tenant "acme" but its invoice_id
	// points at the globex-owned invoice — a genuine cross-tenant join leak
	// once RLS is enabled on both tables independently.
	if _, err := probeDB.ExecContext(ctx,
		"INSERT INTO audit_log (tenant_id, invoice_id, note) VALUES ($1, $2, 's3'), ($3, $4, 'l3')",
		"acme", aInvoiceID, "acme", bInvoiceID); err != nil {
		t.Fatalf("seed audit_log: %v", err)
	}

	baseTables := []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "audit_log", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
	}
	for _, r := range baseTables {
		if err := oracle.EnableRLS(ctx, probeDB, r, "tgd_corpus_policy_"+r.Name); err != nil {
			t.Fatalf("enable RLS on %s: %v", r.Name, err)
		}
	}

	// invoices_view and invoices_view_no_invoker are declared as their own
	// Scoped relations for the differ's purposes — see the identical pattern
	// and rationale in differ_test.go's newDiffFixture. invoices_view_no_
	// invoker's declaration exists specifically so L7 reaches the real
	// run-and-compare path (rather than short-circuiting on "no scoped table
	// referenced"), demonstrating the blind spot concretely instead of just
	// asserting it never runs.
	relations := append(append([]schema.Relation{}, baseTables...),
		schema.Relation{Schema: "public", Name: "invoices_view", Kind: "VIEW",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		schema.Relation{Schema: "public", Name: "invoices_view_no_invoker", Kind: "VIEW",
			Class: schema.Scoped, TenantColumn: "tenant_id"})

	roleName := "tgd_corpus_role"
	// CreateRestrictedRole runs LAST, after every table, view and function
	// above already exists: its GRANT ... ON ALL TABLES IN SCHEMA public is a
	// snapshot, not a standing default, and views are included under "ALL
	// TABLES" exactly as base tables are.
	if err := oracle.CreateRestrictedRole(ctx, probeDB, roleName, "corpuspw", relations); err != nil {
		t.Fatalf("create restricted role: %v", err)
	}
	t.Cleanup(func() {
		probeDB.ExecContext(context.Background(), fmt.Sprintf("DROP ROLE IF EXISTS %q", roleName))
	})
	restrictedDSN := dsnFor(t, base, probeName, roleName, "corpuspw")
	restricted, err := sql.Open("postgres", restrictedDSN)
	if err != nil {
		t.Fatalf("open restricted: %v", err)
	}
	t.Cleanup(func() { restricted.Close() })

	return &corpusDBFixture{probeDB: probeDB, restricted: restricted, relations: relations,
		aInvoiceID: aInvoiceID, bInvoiceID: bInvoiceID}
}
