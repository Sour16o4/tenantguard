package guardrail

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// --- pure, database-free tests ---

func TestWithTenant_RoundTrips(t *testing.T) {
	ctx := WithTenant(context.Background(), "acme")
	got, ok := TenantFromContext(ctx)
	if !ok || got != "acme" {
		t.Fatalf("got (%q, %v), want (\"acme\", true)", got, ok)
	}
}

func TestTenantFromContext_AbsentIsNotOK(t *testing.T) {
	_, ok := TenantFromContext(context.Background())
	if ok {
		t.Fatal("got ok=true on a context with no tenant ever set")
	}
}

// TestTenantFromContext_EmptyStringTreatedAsAbsent is TGD-US-12 AC-3's own
// fail-closed rule, checked at the propagation layer: a present-but-empty
// tenant must not be usable to defeat "context absent -> block."
func TestTenantFromContext_EmptyStringTreatedAsAbsent(t *testing.T) {
	ctx := WithTenant(context.Background(), "")
	_, ok := TenantFromContext(ctx)
	if ok {
		t.Fatal("got ok=true for an empty-string tenant, want false (AC-3)")
	}
}

// --- DB-backed tests: TGD_TEST_DSN required, same convention as the rest
// of this project (internal/differ, internal/oracle) ---

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TGD_TEST_DSN")
	if dsn == "" {
		t.Skip("TGD_TEST_DSN not set; the guardrail wrapper was NOT exercised against real Postgres in this run")
	}
	return dsn
}

// guardrailFixture is a real Postgres database with one scoped table
// (widgets) — no RLS, no probe, no restricted role: TGD-US-12's whole
// argument (internal/guardrail's package doc) is that this mechanism never
// touches any of that machinery, so the test fixture doesn't build it
// either. Two real rows exist, tenants "acme" and "globex".
type guardrailFixture struct {
	rawDB  *sql.DB
	policy *schema.Policy
}

func newGuardrailFixture(t *testing.T) *guardrailFixture {
	t.Helper()
	ctx := context.Background()
	base := adminDSN(t)
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}

	dbName := "tgd_guardrail_fixture"
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`); err != nil {
		t.Fatalf("drop stale: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
		admin.Close()
	})

	u, err := parseAndSetDB(base, dbName)
	if err != nil {
		t.Fatalf("build dsn: %v", err)
	}
	rawDB, err := sql.Open("postgres", u)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	t.Cleanup(func() { rawDB.Close() })

	if _, err := rawDB.ExecContext(ctx, `
		CREATE TABLE widgets (
			id bigserial PRIMARY KEY,
			tenant_id text NOT NULL,
			name text NOT NULL
		);
		INSERT INTO widgets (tenant_id, name) VALUES ('acme', 'acme-widget'), ('globex', 'globex-widget');
	`); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	policy := &schema.Policy{Relations: []schema.Relation{
		{Schema: "public", Name: "widgets", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
	}}
	return &guardrailFixture{rawDB: rawDB, policy: policy}
}

func parseAndSetDB(base, dbName string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

func TestDB_AllowsCorrectlyScopedQuery(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)
	ctx := WithTenant(context.Background(), "acme")

	rows, err := g.QueryContext(ctx, "SELECT name FROM widgets WHERE tenant_id = $1", "acme")
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	if len(got) != 1 || got[0] != "acme-widget" {
		t.Fatalf("got %v, want [acme-widget]", got)
	}
}

// TestDB_BlocksUnscopedQuery is TGD-US-12 AC-1, against a real wrapped
// connection: a query on a scoped table with no tenant predicate returns an
// error and no rows — and never reaches the database at all, proven by
// pointing the wrapped DB at an address nothing listens on: if the query
// were actually attempted, Postgres's absence would surface as a
// connection error, not ErrUnscopedQuery.
func TestDB_BlocksUnscopedQuery(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)
	ctx := WithTenant(context.Background(), "acme")

	rows, err := g.QueryContext(ctx, "SELECT name FROM widgets")
	if rows != nil {
		rows.Close()
		t.Fatal("got non-nil rows on a blocked query")
	}
	if !errors.Is(err, ErrUnscopedQuery) {
		t.Fatalf("got %v, want ErrUnscopedQuery", err)
	}
}

// TestDB_BlockedQueryNeverReachesDatabase proves the "zero DB contact"
// claim concretely, not just by absence of a returned row: a DB pointed at
// an address nothing listens on still returns ErrUnscopedQuery/
// ErrNoTenantContext rather than a connection error, because the check
// runs and fails before g.inner is ever touched.
func TestDB_BlockedQueryNeverReachesDatabase(t *testing.T) {
	unreachable, err := sql.Open("postgres", "postgres://nobody:nobody@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open (lazy, should not dial yet): %v", err)
	}
	defer unreachable.Close()

	policy := &schema.Policy{Relations: []schema.Relation{
		{Schema: "public", Name: "widgets", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
	}}
	g := Wrap(unreachable, policy)

	// No tenant in context at all (AC-3) — must fail closed without dialing.
	if _, err := g.ExecContext(context.Background(), "UPDATE widgets SET name = $1 WHERE tenant_id = $2", "x", "acme"); !errors.Is(err, ErrNoTenantContext) {
		t.Fatalf("got %v, want ErrNoTenantContext (and no dial attempt)", err)
	}

	// Unscoped query on a scoped table — must fail closed without dialing.
	ctx := WithTenant(context.Background(), "acme")
	if _, err := g.ExecContext(ctx, "UPDATE widgets SET name = $1", "x"); !errors.Is(err, ErrUnscopedQuery) {
		t.Fatalf("got %v, want ErrUnscopedQuery (and no dial attempt)", err)
	}
}

// TestDB_BlocksWhenContextTenantAbsent is TGD-US-12 AC-3, directly.
func TestDB_BlocksWhenContextTenantAbsent(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)

	rows, err := g.QueryContext(context.Background(), "SELECT name FROM widgets WHERE tenant_id = $1", "acme")
	if rows != nil {
		rows.Close()
		t.Fatal("got non-nil rows with no tenant in context")
	}
	if !errors.Is(err, ErrNoTenantContext) {
		t.Fatalf("got %v, want ErrNoTenantContext", err)
	}
}

// TestDB_BlocksWrongTenant is TGD-US-12 AC-2 — L5 detection, the whole
// reason this package exists: a query whose OWN predicate is syntactically
// perfect but names a different tenant than the request context's.
func TestDB_BlocksWrongTenant(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)
	// Context says "globex"; the query itself claims "acme".
	ctx := WithTenant(context.Background(), "globex")

	rows, err := g.QueryContext(ctx, "SELECT name FROM widgets WHERE tenant_id = $1", "acme")
	if rows != nil {
		rows.Close()
		t.Fatal("got non-nil rows on a wrong-tenant query")
	}
	if !errors.Is(err, ErrWrongTenant) {
		t.Fatalf("got %v, want ErrWrongTenant", err)
	}
}

func TestDB_AllowsCorrectlyScopedExec(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)
	ctx := WithTenant(context.Background(), "acme")

	res, err := g.ExecContext(ctx, "UPDATE widgets SET name = $1 WHERE tenant_id = $2", "renamed", "acme")
	if err != nil {
		t.Fatalf("ExecContext: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}
}

func TestDB_BlocksUnscopedExec(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)
	ctx := WithTenant(context.Background(), "acme")

	_, err := g.ExecContext(ctx, "UPDATE widgets SET name = $1", "renamed-everyone")
	if !errors.Is(err, ErrUnscopedQuery) {
		t.Fatalf("got %v, want ErrUnscopedQuery", err)
	}

	// Confirm the blocked write really never ran: both rows still have
	// their original names.
	var count int
	if err := f.rawDB.QueryRowContext(context.Background(),
		"SELECT count(*) FROM widgets WHERE name = 'renamed-everyone'").Scan(&count); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if count != 0 {
		t.Fatalf("got %d rows renamed, want 0 — the blocked write must never have executed", count)
	}
}

func TestDB_QueryRowContext_AllowsAndBlocks(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)

	// Allowed: correctly scoped, returns the real row.
	ctx := WithTenant(context.Background(), "acme")
	var name string
	if err := g.QueryRowContext(ctx, "SELECT name FROM widgets WHERE tenant_id = $1 AND id = $2",
		"acme", 1).Scan(&name); err != nil {
		t.Fatalf("Scan on an allowed row: %v", err)
	}

	// Blocked: no tenant in context — Scan must return the guardrail's own
	// error, not attempt the query.
	err := g.QueryRowContext(context.Background(), "SELECT name FROM widgets WHERE tenant_id = $1", "acme").Scan(&name)
	if !errors.Is(err, ErrNoTenantContext) {
		t.Fatalf("got %v, want ErrNoTenantContext from Scan", err)
	}
}

// TestDB_BlocksFunctionWrappingScopedTable is TGD-BL-43, proven against a
// real wrapped connection with U2's own exact shape: a PostgreSQL function
// that internally reads the scoped widgets table, called the way U2's
// fixture is (SELECT * FROM <function>()). infer's own relationQuery
// selects only from pg_class (BASE TABLE/VIEW/MATERIALIZED VIEW/FOREIGN
// TABLE/PARTITIONED TABLE), never pg_proc, so a function can never appear
// in ANY policy this tool generates — before TGD-BL-43, that meant
// CheckTenant's "does this name a declared Scoped table" gate saw nothing
// to enforce and returned Safe, letting a query allow through data from a
// real scoped table it never inspected at all. This test would have PASSED
// nonsense before the fix (rows returned, no error) and must FAIL now if
// the fix is ever reverted — see the mutation proof in this function's own
// history (SRS §7.6): reverting resolveReferences to the old
// ReferencesScopedTable-only gate reproduces exactly this failure.
func TestDB_BlocksFunctionWrappingScopedTable(t *testing.T) {
	f := newGuardrailFixture(t)
	ctx := context.Background()
	if _, err := f.rawDB.ExecContext(ctx, `
		CREATE FUNCTION all_widgets() RETURNS SETOF widgets AS
		$$ SELECT * FROM widgets $$ LANGUAGE sql STABLE;
	`); err != nil {
		t.Fatalf("create function: %v", err)
	}

	g := Wrap(f.rawDB, f.policy)
	rctx := WithTenant(context.Background(), "acme")

	rows, err := g.QueryContext(rctx, "SELECT * FROM all_widgets()")
	if rows != nil {
		rows.Close()
		t.Fatal("got non-nil rows — a function wrapping a scoped table must be blocked, not passed through uninspected")
	}
	if !errors.Is(err, ErrUnattributable) {
		t.Fatalf("got %v, want ErrUnattributable — an unresolvable relation must fail closed", err)
	}
}

// TestDB_AllowsCTEOverScopedTable proves the fix's own load-bearing
// exclusion (differ.cteNames): a query's own CTE name is never a real
// relation and must not be treated as an unresolvable one — a fix for
// TGD-BL-43 with no notion of this would block ordinary WITH-shaped
// queries wholesale, not just the function/view bypass it exists to catch.
func TestDB_AllowsCTEOverScopedTable(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)
	ctx := WithTenant(context.Background(), "acme")

	var name string
	err := g.QueryRowContext(ctx,
		"WITH scoped AS (SELECT * FROM widgets WHERE tenant_id = $1) SELECT name FROM scoped WHERE id = $2",
		"acme", 1).Scan(&name)
	if err != nil {
		t.Fatalf("got %v, want the CTE's own name to resolve without touching the policy", err)
	}
	if name != "acme-widget" {
		t.Fatalf("got %q, want acme-widget", name)
	}
}

// TestDB_AllowsUnrelatedQuery: a query touching no declared scoped table at
// all must be allowed through unconditionally — nothing to enforce
// (ReferencesScopedTable's own doc comment, differ.CheckTenant's first
// check).
func TestDB_AllowsUnrelatedQuery(t *testing.T) {
	f := newGuardrailFixture(t)
	g := Wrap(f.rawDB, f.policy)
	ctx := WithTenant(context.Background(), "acme")

	var one int
	if err := g.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("got %v, want a query with no scoped table to pass through", err)
	}
}
