package differ

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/oracle"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// These tests need real PostgreSQL, for the same reason internal/oracle's do:
// RLS is the thing under test. Set TGD_TEST_DSN; without it they skip loudly.
func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TGD_TEST_DSN")
	if dsn == "" {
		t.Skip("TGD_TEST_DSN not set; the differ was NOT exercised against real Postgres in this run")
	}
	return dsn
}

func dsnFor(t *testing.T, base, dbName, role, pass string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	if role != "" {
		u.User = url.UserPassword(role, pass)
	}
	u.Path = "/" + dbName
	return u.String()
}

// diffFixture builds a probe database directly (no separate source database —
// the differ's own tests don't need the oracle package's full probe-from-a-
// live-target machinery, only a schema with RLS already synthesised), with a
// canary role that Diff exercises per query.
type diffFixture struct {
	admin      *sql.DB
	probeName  string
	probeDB    *sql.DB
	restricted *sql.DB
	roleName   string
	relations  []schema.Relation
}

func newDiffFixture(t *testing.T, name string) *diffFixture {
	t.Helper()
	ctx := context.Background()
	base := adminDSN(t)
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}

	probeName := "tgd_differ_" + name
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
		CREATE VIEW invoices_view WITH (security_invoker = true) AS
			SELECT * FROM invoices;
		CREATE VIEW invoices_view_unscoped WITH (security_invoker = true) AS
			SELECT * FROM invoices;

		INSERT INTO invoices (tenant_id, status, amount) VALUES
			('acme', 'paid', 100),
			('acme', 'open', 50),
			('globex', 'paid', 200),
			('globex', 'open', 75);
		INSERT INTO audit_log (tenant_id, invoice_id, note)
			SELECT tenant_id, id, 'seeded' FROM invoices;
	`
	if _, err := probeDB.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	baseTables := []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "audit_log", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
	}
	for _, r := range baseTables {
		if err := oracle.EnableRLS(ctx, probeDB, r, "tgd_policy_"+r.Name); err != nil {
			t.Fatalf("enable RLS on %s: %v", r.Name, err)
		}
	}

	// invoices_view is declared as its own scoped relation for the DIFFER's
	// purposes, separately from schema.Classify's automatic-inference rule
	// (which correctly marks views Unclassifiable, since RLS cannot attach to
	// a view object directly). A view running WITH security_invoker=true
	// inherits RLS from the underlying table transparently — the policy
	// author declaring "this view also carries tenant_id scoping" is exactly
	// how ExtractTenant recognises a query against the view by name at all;
	// without an explicit relation entry the view would never be recognised
	// as scoped, since it doesn't share the base table's name.
	relations := append(append([]schema.Relation{}, baseTables...),
		schema.Relation{Schema: "public", Name: "invoices_view", Kind: "VIEW",
			Class: schema.Scoped, TenantColumn: "tenant_id"})

	roleName := "tgd_differ_role_" + name
	if err := oracle.CreateRestrictedRole(ctx, probeDB, roleName, "differpw", relations); err != nil {
		t.Fatalf("create restricted role: %v", err)
	}
	t.Cleanup(func() {
		probeDB.ExecContext(context.Background(), fmt.Sprintf("DROP ROLE IF EXISTS %q", roleName))
	})
	restrictedDSN := dsnFor(t, base, probeName, roleName, "differpw")
	restricted, err := sql.Open("postgres", restrictedDSN)
	if err != nil {
		t.Fatalf("open restricted: %v", err)
	}
	t.Cleanup(func() { restricted.Close() })

	return &diffFixture{admin: admin, probeName: probeName, probeDB: probeDB,
		restricted: restricted, roleName: roleName, relations: relations}
}

func evParams(vals ...string) []capture.Param {
	out := make([]capture.Param, len(vals))
	for i, v := range vals {
		out[i] = capture.Param{Known: true, Text: v}
	}
	return out
}

// --- the core mechanism ---

func TestDiff_CorrectlyScopedQueryIsSafe(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "safe")
	ev := capture.Event{Resolved: true,
		SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
		Params: evParams("acme")}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe", r.Verdict, r.Reason)
	}
	if r.Vacuous {
		t.Errorf("Vacuous = true for a real, non-empty match — acme has 2 real invoices; "+
			"this SAFE demonstrated something, it must not be flagged vacuous (reason: %s)", r.Reason)
	}
}

// TestDiff_VacuousSafeWhenBothLegsMatchNothing is TGD-BL-39's decision,
// implemented: a comparison where BOTH the unrestricted and restricted
// re-executions match zero rows is still SAFE (∅=∅ literally holds), but it
// demonstrated nothing about whether RLS would have withheld anything had
// the query matched something — Vacuous distinguishes it from a real pass
// without inventing a fourth verdict (§5's three-outcome model is
// unchanged). "nonexistent-tenant" has no rows in the fixture's invoices
// table at all, so both legs return empty regardless of tenant scoping.
func TestDiff_VacuousSafeWhenBothLegsMatchNothing(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "vacuous")
	ev := capture.Event{Resolved: true,
		SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
		Params: evParams("nonexistent-tenant")}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe — both legs matched zero rows, so ∅=∅ holds", r.Verdict, r.Reason)
	}
	if !r.Vacuous {
		t.Errorf("Vacuous = false, want true — neither leg matched a single row; this SAFE "+
			"proves nothing about RLS withholding anything (reason: %s)", r.Reason)
	}
}

func TestDiff_MissingPredicateIsLeak(t *testing.T) {
	// L1's shape: no tenant predicate at all. Diff must treat "no tenant
	// claimed" as an RLS-denies-everything comparison, not skip judging it.
	ctx := context.Background()
	f := newDiffFixture(t, "nopredicate")
	ev := capture.Event{Resolved: true,
		SQL:    "SELECT * FROM invoices WHERE id = $1",
		Params: evParams("1")}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Leak {
		t.Fatalf("verdict = %v (%s), want Leak — a query with no tenant scoping "+
			"returned real rows to an unproven audience", r.Verdict, r.Reason)
	}
	if r.WithheldRows == 0 {
		t.Errorf("Leak reported with WithheldRows = 0")
	}
}

func TestDiff_AggregateWithNoPredicateIsLeakOnDifferingScalar(t *testing.T) {
	// TGD-US-06 AC-3, verbatim: an aggregate with no tenant predicate is a
	// LEAK when the scalar differs, even though no row crossed the boundary.
	ctx := context.Background()
	f := newDiffFixture(t, "aggregate")
	ev := capture.Event{Resolved: true, SQL: "SELECT count(*) FROM invoices"}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Leak {
		t.Fatalf("verdict = %v (%s), want Leak (differing count)", r.Verdict, r.Reason)
	}
}

func TestDiff_ResolvedTenantIsTrustedAsGroundTruth(t *testing.T) {
	// L5's Tier 2 shape (context propagation supplies the correct tenant
	// independently). At the Diff-function level (not yet wired to a proxy
	// tier), this exercises the mechanism directly: a resolved-but-wrong
	// tenant value produces a genuine, provable divergence.
	ctx := context.Background()
	f := newDiffFixture(t, "wrongtenant")
	ev := capture.Event{Resolved: true,
		SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
		Params: evParams("acme")}

	// Diff itself only ever uses the query's OWN claimed tenant (acme here),
	// so on its own this fixture is SAFE — proving L5's Tier 1 blind spot is
	// a property of WHICH tenant gets asserted as ground truth, not of Diff's
	// mechanism. Documented explicitly; the full L5 corpus case is in
	// corpus_test.go where the tier distinction is asserted directly.
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe: Diff has no independent ground truth "+
			"to know 'acme' is the wrong tenant — that is exactly L5's documented "+
			"Tier 1 blind spot, not a bug in Diff itself", r.Verdict, r.Reason)
	}
}

func TestDiff_UnattributableNeverBecomesSafe(t *testing.T) {
	// A binary/undecoded parameter must produce Unattributable, and Diff must
	// not touch the database attempting to re-execute with a guessed value.
	ctx := context.Background()
	f := newDiffFixture(t, "unattr")
	ev := capture.Event{Resolved: true,
		SQL: "SELECT * FROM invoices WHERE tenant_id = $1",
		Params: []capture.Param{
			{Known: false, Reason: "binary parameter; type OID not captured"},
		}}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Unattributable {
		t.Fatalf("verdict = %v (%s), want Unattributable", r.Verdict, r.Reason)
	}
}

func TestDiff_UnresolvedBindIsUnattributable(t *testing.T) {
	// U4's shape: the capture layer itself never matched this Bind to a
	// Parse. SQL is empty; Diff must not treat empty SQL as "nothing to leak."
	ctx := context.Background()
	f := newDiffFixture(t, "unresolvedbind")
	ev := capture.Event{Resolved: false, SQL: ""}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Unattributable {
		t.Fatalf("verdict = %v (%s), want Unattributable", r.Verdict, r.Reason)
	}
}

// --- the trust boundary: no write occurs, for SELECT or for INSERT ---

func TestDiff_InsertReturningNeverWrites(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "insertwrite")

	var before int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&before)

	ev := capture.Event{Resolved: true,
		SQL:    "INSERT INTO invoices (tenant_id, status, amount) VALUES ($1, $2, $3) RETURNING *",
		Params: evParams("acme", "open", "999")}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe for a correctly-scoped insert", r.Verdict, r.Reason)
	}

	var after int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&after)
	if after != before {
		t.Fatalf("row count changed %d -> %d; Diff wrote to the database "+
			"re-executing a captured INSERT — every re-execution must roll back", before, after)
	}
}

// TestDiff_WriteRejectedByRLSIsLeak exercises diffWrite's rejection branch
// directly.
//
// By construction, a simple single-table INSERT can never trip this branch
// through Diff's own public API: the "claimed tenant" IS the value being
// written, so RLS's WITH CHECK is always comparing that value to itself and
// can never disagree. Genuinely reaching this path in production would need a
// multi-table write (a trigger inserting into a second scoped table under a
// different tenant) or a hand-modified policy inconsistent with what
// synthesis produced — neither is this deliverable's fixture set. This test
// instead proves the MECHANISM directly: the underlying policy is
// deliberately weakened to compare against a column that will never match
// (id, not tenant_id), so ANY insert — including one whose own values are
// perfectly self-consistent — is rejected by RLS, and confirms diffWrite
// correctly reports that as Leak rather than as an opaque re-execution error.
func TestDiff_WriteRejectedByRLSIsLeak(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "writerejected")

	// Replace invoices' policy with one that can never match any insert.
	exec := func(q string) {
		if _, err := f.probeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`DROP POLICY tgd_policy_invoices ON invoices`)
	exec(`CREATE POLICY tgd_policy_invoices ON invoices
		USING (id::text = current_setting('tenantguard.tenant', true))`)

	var before int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&before)

	ev := capture.Event{Resolved: true,
		SQL:    "INSERT INTO invoices (tenant_id, status, amount) VALUES ($1, $2, $3) RETURNING *",
		Params: evParams("acme", "open", "5")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)

	if r.Verdict != Leak {
		t.Fatalf("verdict = %v (%s), want Leak: the write succeeded unrestricted "+
			"but RLS rejects it for the claimed tenant", r.Verdict, r.Reason)
	}

	var after int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&after)
	if after != before {
		t.Fatalf("row count changed %d -> %d; a rejected, rolled-back insert must never persist", before, after)
	}
}

// TestDiff_WriteAcceptedByRLSIsSafe is the converse: both unrestricted and
// restricted succeed, and no rollback-related side effect leaks through as a
// false positive.
func TestDiff_WriteAcceptedByRLSIsSafe(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "writeaccepted")
	var before int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&before)

	ev := capture.Event{Resolved: true,
		SQL:    "INSERT INTO invoices (tenant_id, status, amount) VALUES ($1, $2, $3) RETURNING *",
		Params: evParams("globex", "open", "5")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe", r.Verdict, r.Reason)
	}
	// TGD-BL-40: this insert affects exactly 1 row on both legs, so it must
	// not be flagged Vacuous — it demonstrated a real, non-empty write RLS
	// genuinely accepted, the write-path analogue of
	// TestDiff_CorrectlyScopedQueryIsSafe's non-vacuous read-path assertion.
	if r.Vacuous {
		t.Errorf("Vacuous = true, want false — this write affected a real row on both legs")
	}

	var after int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&after)
	if after != before {
		t.Fatalf("row count changed %d -> %d; a rolled-back insert must never persist", before, after)
	}
}

// --- TGD-BL-35: a captured write colliding with the probe's own history ---
//
// newConflictFixture builds tables shaped for TGD-BL-35's own two measured
// real-target collision shapes (SRS §7.15/§7.16): a single-column literal
// key (coder's own InsertOrganizationMember/InsertTemplate/InsertTemplateVersion
// shape) and a composite key (zitadel's own
// eventstore.unique_constraints/projections.current_states shape, both
// (instance_id, ...) primary keys) — each pre-seeded with one real row a
// captured INSERT can collide with, exactly the way a captured write
// colliding with the probe's own TEMPLATE-copied history does for real.
func newConflictFixture(t *testing.T, name string) *diffFixture {
	t.Helper()
	ctx := context.Background()
	base := adminDSN(t)
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}

	probeName := "tgd_conflict_" + name
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
		CREATE TABLE org_members (
			id text PRIMARY KEY,
			tenant_id text NOT NULL,
			user_id text NOT NULL
		);
		INSERT INTO org_members (id, tenant_id, user_id) VALUES
			('member-1', 'acme', 'user-1');

		CREATE TABLE unique_constraints (
			instance_id text NOT NULL,
			unique_type text NOT NULL,
			unique_field text NOT NULL,
			PRIMARY KEY (instance_id, unique_type, unique_field)
		);
		INSERT INTO unique_constraints (instance_id, unique_type, unique_field) VALUES
			('acme', 'migration_started', '14_events_push');
	`
	if _, err := probeDB.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	relations := []schema.Relation{
		{Schema: "public", Name: "org_members", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "unique_constraints", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "instance_id"},
	}
	for _, r := range relations {
		if err := oracle.EnableRLS(ctx, probeDB, r, "tgd_policy_"+r.Name); err != nil {
			t.Fatalf("enable RLS on %s: %v", r.Name, err)
		}
	}

	roleName := "tgd_conflict_role_" + name
	if err := oracle.CreateRestrictedRole(ctx, probeDB, roleName, "conflictpw", relations); err != nil {
		t.Fatalf("create restricted role: %v", err)
	}
	t.Cleanup(func() {
		probeDB.ExecContext(context.Background(), fmt.Sprintf("DROP ROLE IF EXISTS %q", roleName))
	})
	restrictedDSN := dsnFor(t, base, probeName, roleName, "conflictpw")
	restricted, err := sql.Open("postgres", restrictedDSN)
	if err != nil {
		t.Fatalf("open restricted: %v", err)
	}
	t.Cleanup(func() { restricted.Close() })

	return &diffFixture{admin: admin, probeName: probeName, probeDB: probeDB,
		restricted: restricted, roleName: roleName, relations: relations}
}

// TestDiff_DuplicateKeyOnReplay_SingleColumnKey_Resolves is TGD-BL-35's
// regression test for coder's own measured shape: a captured INSERT
// literally replaying a row the probe's own history already contains, on a
// single-column primary key. Before the fix, this failed with a raw
// PostgreSQL 23505 and came back Unattributable — not because tenant
// scoping was wrong, but because the probe already held this exact row.
// After the fix, the colliding row is deleted (and restored — see the
// postcondition below) inside the same rolled-back transaction, and the
// write is genuinely compared: the claimed tenant matches the row that was
// there, so this must resolve to a definite verdict, not Unattributable.
func TestDiff_DuplicateKeyOnReplay_SingleColumnKey_Resolves(t *testing.T) {
	ctx := context.Background()
	f := newConflictFixture(t, "singlecol")

	ev := capture.Event{Resolved: true,
		SQL:    "INSERT INTO org_members (id, tenant_id, user_id) VALUES ($1, $2, $3)",
		Params: evParams("member-1", "acme", "user-1")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)

	if r.Verdict == Unattributable {
		t.Fatalf("verdict = Unattributable (%s), want a definite verdict — "+
			"the duplicate-key collision should have been resolved, not left unattributable", r.Reason)
	}
	if r.Verdict != Safe {
		t.Errorf("verdict = %v (%s), want Safe — the claimed tenant (acme) matches the "+
			"pre-existing row's own tenant, so a correctly-scoped policy admits it", r.Verdict, r.Reason)
	}

	// TGD-BL-35's own resolution mechanism deletes-then-retries inside a
	// transaction that is always rolled back — the pre-existing row must
	// still be there afterward, unchanged, the same "never a durable write"
	// guarantee every other re-execution in this package already gives
	// (§3.3).
	var tenant string
	if err := f.probeDB.QueryRowContext(ctx,
		"SELECT tenant_id FROM org_members WHERE id = 'member-1'").Scan(&tenant); err != nil {
		t.Fatalf("pre-existing row missing after Diff — the conflict resolution's delete must be "+
			"rolled back, never durable: %v", err)
	}
	if tenant != "acme" {
		t.Errorf("pre-existing row's tenant_id = %q, want unchanged %q", tenant, "acme")
	}
	var count int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM org_members").Scan(&count)
	if count != 1 {
		t.Errorf("org_members has %d rows, want exactly 1 (the original) — no durable insert either", count)
	}
}

// TestDiff_DuplicateKeyOnReplay_CompositeKey_Resolves is TGD-BL-35's
// regression test for zitadel's own measured shape: the identical mechanism,
// on a 3-column composite primary key — the shape that accounted for 93.5%
// of zitadel's own row_level_unattributed population (SRS §7.16) before
// this fix.
func TestDiff_DuplicateKeyOnReplay_CompositeKey_Resolves(t *testing.T) {
	ctx := context.Background()
	f := newConflictFixture(t, "compositekey")

	ev := capture.Event{Resolved: true,
		SQL:    "INSERT INTO unique_constraints (instance_id, unique_type, unique_field) VALUES ($1, $2, $3)",
		Params: evParams("acme", "migration_started", "14_events_push")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)

	if r.Verdict == Unattributable {
		t.Fatalf("verdict = Unattributable (%s), want a definite verdict on this composite-key collision", r.Reason)
	}
	if r.Verdict != Safe {
		t.Errorf("verdict = %v (%s), want Safe", r.Verdict, r.Reason)
	}

	var count int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM unique_constraints").Scan(&count)
	if count != 1 {
		t.Errorf("unique_constraints has %d rows, want exactly 1 (the original, restored) — no durable insert", count)
	}
}

// TestDiff_DuplicateKeyOnReplay_RLSBlocksResolution_NeverSafe is the safety
// constraint this fix must uphold in the one direction that matters most:
// when the pre-existing colliding row belongs to a DIFFERENT tenant than
// the captured write claims, the restricted role's own attempt to clear it
// is correctly blocked by row-level security (it is not that tenant's row
// to delete) — the collision must stay genuinely unresolved for that leg.
// This must fail toward Unattributable, never be treated as license to
// report Safe, and — equally — must not be misreported as Leak either: the
// restricted leg's real INSERT was never even attempted, so claiming a
// row-security *rejection* would fabricate a finding this run never tested.
func TestDiff_DuplicateKeyOnReplay_RLSBlocksResolution_NeverSafe(t *testing.T) {
	ctx := context.Background()
	f := newConflictFixture(t, "blocked")

	// The pre-existing row belongs to "acme" (from the fixture). This event
	// claims a DIFFERENT tenant, "globex", for the identical (id) key —
	// the shape a genuine cross-tenant key collision or a misattributed
	// capture would produce.
	ev := capture.Event{Resolved: true,
		SQL:    "INSERT INTO org_members (id, tenant_id, user_id) VALUES ($1, $2, $3)",
		Params: evParams("member-1", "globex", "user-2")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)

	if r.Verdict == Safe {
		t.Fatalf("verdict = Safe (%s) — must NEVER report Safe when the restricted role's own "+
			"conflict resolution was blocked by row-level security", r.Reason)
	}
	if r.Verdict == Leak {
		t.Fatalf("verdict = Leak (%s) — must not fabricate a row-security rejection either: "+
			"the restricted leg's real write was never attempted here", r.Reason)
	}
	if r.Verdict != Unattributable {
		t.Errorf("verdict = %v (%s), want Unattributable", r.Verdict, r.Reason)
	}

	// Same durability guarantee: the original row is untouched.
	var tenant string
	f.probeDB.QueryRowContext(ctx, "SELECT tenant_id FROM org_members WHERE id = 'member-1'").Scan(&tenant)
	if tenant != "acme" {
		t.Errorf("pre-existing row's tenant_id = %q, want unchanged %q", tenant, "acme")
	}
}

// TestDiff_CTEWrappedWriteRoutesToDiffWrite is TGD-BL-41 proven end-to-end
// against real Postgres: a CTE-wrapped INSERT (the same shape as
// DeleteOldWorkspaceBuildOrchestrations/ExpirePrebuildsAPIKeys — one or more
// leading CTEs, then the real DML) must be routed through diffWrite's
// success/rejection comparison, not the read-path row-set comparison.
//
// Reuses TestDiff_WriteRejectedByRLSIsLeak's mechanism (the policy is
// replaced with one that can never match, so WITH CHECK rejects any write)
// to prove routing directly, not just infer it from isWriteStatement's own
// unit tests: only diffWrite's error-based comparison can turn a RLS
// rejection into Leak with this specific reason text. Under the read-path
// bug this fixes, the identical INSERT would instead be compared by row-set
// content, which cannot even observe a WITH CHECK rejection the same way
// (the restricted re-execution would simply fail outright as a query error,
// misreported as Unattributable rather than Leak).
func TestDiff_CTEWrappedWriteRoutesToDiffWrite(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "ctewrite")

	exec := func(q string) {
		if _, err := f.probeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`DROP POLICY tgd_policy_invoices ON invoices`)
	exec(`CREATE POLICY tgd_policy_invoices ON invoices
		USING (id::text = current_setting('tenantguard.tenant', true))`)

	var before int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&before)

	ev := capture.Event{Resolved: true,
		SQL: `WITH src AS (
				SELECT $2::text AS status, $3::integer AS amount
			)
			INSERT INTO invoices (tenant_id, status, amount)
			SELECT $1, status, amount FROM src
			RETURNING *`,
		Params: evParams("acme", "open", "5")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)

	if r.Verdict != Leak {
		t.Fatalf("verdict = %v (%s), want Leak: a CTE-wrapped INSERT routed to diffWrite "+
			"must report RLS's WITH CHECK rejection, not a read-path misclassification", r.Verdict, r.Reason)
	}
	if !strings.Contains(r.Reason, "row-level security") {
		t.Errorf("reason = %q, want diffWrite's row-level-security rejection message — "+
			"anything else suggests this still went through the read path", r.Reason)
	}

	var after int
	f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&after)
	if after != before {
		t.Fatalf("row count changed %d -> %d; a rejected, rolled-back insert must never persist", before, after)
	}
}

// TestDiff_WriteVacuousWhenBothLegsAffectZeroRows is TGD-BL-40: a write whose
// predicate matches nothing succeeds on both legs (no RLS involvement at
// all, since there is nothing to check), which must be flagged Vacuous —
// the write-path analogue of TestDiff_VacuousSafeWhenBothLegsMatchNothing.
// No RETURNING clause, so this is exactly the shape runRolledBack's old
// QueryContext-based comparison could never observe (it reports zero rows
// for ANY no-RETURNING write, matched or not) — proving execRolledBack's
// RowsAffected count, not row content, is what makes this detectable.
func TestDiff_WriteVacuousWhenBothLegsAffectZeroRows(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "writevacuous")

	ev := capture.Event{Resolved: true,
		SQL:    "DELETE FROM invoices WHERE tenant_id = $1 AND status = 'no-such-status'",
		Params: evParams("acme")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe", r.Verdict, r.Reason)
	}
	if !r.Vacuous {
		t.Errorf("Vacuous = false, want true — this delete matched zero rows on both legs, "+
			"proving nothing about RLS (reason: %s)", r.Reason)
	}
}

// TestDiff_WriteVacuousWithReturningClauseHandledCorrectly is TGD-BL-40's
// other named requirement: a write WITH a RETURNING clause already carried
// row data even before this fix (runRolledBack's QueryContext-based path
// could see it), so the fix must not regress that case while adding
// detection for the no-RETURNING case above. Same zero-match delete, with
// RETURNING added.
func TestDiff_WriteVacuousWithReturningClauseHandledCorrectly(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "writevacuousreturning")

	ev := capture.Event{Resolved: true,
		SQL:    "DELETE FROM invoices WHERE tenant_id = $1 AND status = 'no-such-status' RETURNING *",
		Params: evParams("acme")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe", r.Verdict, r.Reason)
	}
	if !r.Vacuous {
		t.Errorf("Vacuous = false, want true — this delete matched zero rows on both legs "+
			"even with RETURNING present (reason: %s)", r.Reason)
	}
}

// TestDiff_WriteNotVacuousWhenRowsAffected pins the negative case using a
// RETURNING clause, alongside TestDiff_WriteAcceptedByRLSIsSafe's
// no-RETURNING negative case, so both shapes have both signs covered.
func TestDiff_WriteNotVacuousWhenRowsAffected(t *testing.T) {
	ctx := context.Background()
	f := newDiffFixture(t, "writenotvacuous")

	ev := capture.Event{Resolved: true,
		SQL:    "DELETE FROM invoices WHERE tenant_id = $1 AND status = 'paid' RETURNING *",
		Params: evParams("acme")}
	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe", r.Verdict, r.Reason)
	}
	if r.Vacuous {
		t.Errorf("Vacuous = true, want false — acme has a real 'paid' invoice, "+
			"this delete affected a real row on both legs (reason: %s)", r.Reason)
	}
}

// --- views: security_invoker is required for RLS to apply as the caller ---

func TestDiff_ViewWithSecurityInvokerInheritsRLS(t *testing.T) {
	// S6's shape. Without security_invoker=true, a view runs with the
	// privileges of its OWNER, and RLS on the underlying table would be
	// evaluated as the owner, not the caller — silently defeating the whole
	// mechanism. The fixture creates the view WITH security_invoker
	// explicitly; this test exists to prove that choice is load-bearing.
	ctx := context.Background()
	f := newDiffFixture(t, "viewinvoker")
	ev := capture.Event{Resolved: true,
		SQL:    "SELECT * FROM invoices_view WHERE tenant_id = $1",
		Params: evParams("acme")}

	r := Diff(ctx, f.probeDB, f.restricted, f.relations, ev)
	if r.Verdict != Safe {
		t.Fatalf("verdict = %v (%s), want Safe", r.Verdict, r.Reason)
	}
}

// TestIsWriteStatement_IgnoresLeadingComment reproduces a real defect found
// during the first differential run against captured coder traffic
// (2026-08-31): every coder query is sqlc-generated with a leading
// "-- name: X :one" style comment. isWriteStatement's naive HasPrefix check
// on the trimmed, upper-cased text never matches past that comment, so real
// UPDATE/INSERT/DELETE statements were routed through the SELECT-style
// row-set comparison instead of diffWrite — exactly the non-deterministic-
// column false-positive diffWrite's own docstring says it exists to avoid
// (an UPDATE ... RETURNING with `updated_at = now()` differs between the two
// re-executions on that column alone, misclassified as LEAK). This test must
// fail before the fix and pass after.
func TestIsWriteStatement_IgnoresLeadingComment(t *testing.T) {
	sqlText := "-- name: UpdateCustomRole :one\nUPDATE\n\tcustom_roles\nSET\n\tdisplay_name = $1\nWHERE\n\tname = $2\nRETURNING id"
	if !isWriteStatement(sqlText) {
		t.Fatalf("isWriteStatement returned false for a comment-prefixed UPDATE: %q", sqlText)
	}
}

// TestIsWriteStatement_CTEWrappedWrite is TGD-BL-41: a leading-keyword check
// alone sees "WITH" and never looks past the CTE definition(s) to the
// statement's real verb, misrouting a CTE-wrapped write through the
// read-path row-set comparison instead of diffWrite. Reproduces the two real
// coder shapes found (DeleteOldWorkspaceBuildOrchestrations,
// ExpirePrebuildsAPIKeys): one or more leading CTEs followed by the actual
// DELETE/UPDATE/INSERT.
func TestIsWriteStatement_CTEWrappedWrite(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"single CTE then DELETE", `WITH orchestrations AS (
				SELECT id FROM provisioner_jobs WHERE completed_at < $1
			)
			DELETE FROM workspace_build_orchestrations
			WHERE job_id IN (SELECT id FROM orchestrations)`},
		{"single CTE then UPDATE", `WITH expired AS (
				SELECT id FROM prebuilds_api_keys WHERE expires_at < $1
			)
			UPDATE prebuilds_api_keys SET revoked = true
			WHERE id IN (SELECT id FROM expired)`},
		{"single CTE then INSERT", `WITH src AS (
				SELECT id, name FROM templates WHERE org_id = $1
			)
			INSERT INTO template_audit (template_id, name)
			SELECT id, name FROM src`},
		{"multiple CTEs, comma-separated, then DELETE", `WITH a AS (SELECT 1), b AS (
				SELECT id FROM jobs WHERE done
			)
			DELETE FROM jobs WHERE id IN (SELECT id FROM b)`},
		{"RECURSIVE CTE then UPDATE", `WITH RECURSIVE tree AS (
				SELECT id FROM nodes WHERE parent_id IS NULL
				UNION ALL
				SELECT n.id FROM nodes n JOIN tree t ON n.parent_id = t.id
			)
			UPDATE nodes SET visited = true WHERE id IN (SELECT id FROM tree)`},
		{"CTE with column list then DELETE", `WITH ids (id) AS (
				SELECT id FROM stale
			)
			DELETE FROM stale WHERE id IN (SELECT id FROM ids)`},
		{"MATERIALIZED CTE then DELETE", `WITH ids AS MATERIALIZED (
				SELECT id FROM stale
			)
			DELETE FROM stale WHERE id IN (SELECT id FROM ids)`},
		{"NOT MATERIALIZED CTE then UPDATE", `WITH ids AS NOT MATERIALIZED (
				SELECT id FROM stale
			)
			UPDATE stale SET touched = true WHERE id IN (SELECT id FROM ids)`},
		{"leading comment then CTE-wrapped DELETE", "-- name: DeleteOldWorkspaceBuildOrchestrations :exec\n" +
			`WITH orchestrations AS (SELECT id FROM jobs WHERE done)
			DELETE FROM workspace_build_orchestrations WHERE job_id IN (SELECT id FROM orchestrations)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !isWriteStatement(c.sql) {
				t.Fatalf("isWriteStatement returned false for a CTE-wrapped write: %q", c.sql)
			}
		})
	}
}

// TestIsWriteStatement_WriteNestedInsideCTE is TGD-BL-41's other named shape:
// the write can sit in a nested CTE rather than the outer statement (e.g. a
// data-modifying CTE whose result the outer SELECT merely reads). The outer
// statement's own leading verb is SELECT, but executing it still performs
// the write, so it must route to diffWrite exactly as a top-level write
// would.
func TestIsWriteStatement_WriteNestedInsideCTE(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"DELETE nested in CTE, outer SELECT", `WITH deleted AS (
				DELETE FROM sessions WHERE expires_at < $1 RETURNING id
			)
			SELECT count(*) FROM deleted`},
		{"UPDATE nested in CTE, outer SELECT", `WITH touched AS (
				UPDATE workspaces SET last_used_at = now() WHERE id = $1 RETURNING id
			)
			SELECT id FROM touched`},
		{"doubly-nested WITH, inner CTE is a write", `WITH outer_cte AS (
				WITH inner_cte AS (
					DELETE FROM audit_logs WHERE id = $1 RETURNING id
				)
				SELECT * FROM inner_cte
			)
			SELECT * FROM outer_cte`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !isWriteStatement(c.sql) {
				t.Fatalf("isWriteStatement returned false for a write nested inside a CTE: %q", c.sql)
			}
		})
	}
}

// TestIsWriteStatement_PlainCTEReadIsNotAWrite guards against the routing
// fix over-matching: a ordinary read-only CTE (no write anywhere in it or
// its outer statement) must still be classified as a read, or every
// CTE-shaped SELECT in the corpus would be wrongly routed through diffWrite's
// success/rejection comparison instead of row-set comparison.
func TestIsWriteStatement_PlainCTEReadIsNotAWrite(t *testing.T) {
	sqlText := `WITH scoped AS (
			SELECT * FROM invoices WHERE tenant_id = $1
		)
		SELECT * FROM scoped WHERE status = 'open'`
	if isWriteStatement(sqlText) {
		t.Fatalf("isWriteStatement returned true for a read-only CTE: %q", sqlText)
	}
}
