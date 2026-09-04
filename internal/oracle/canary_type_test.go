package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// --- pure, DB-free tests of the type dispatch itself ---

// TestCanaryLiteralUUID closes TGD-BL-27: SeedCanaries must not insert the
// bare CanaryA/CanaryB string constants into a uuid-typed tenant column —
// "tgd-canary-aaaa" is not valid uuid syntax. canaryLiteral must produce a
// valid, ::uuid-cast literal instead, mirroring sampleValue's own
// type-dispatch pattern (a switch on the normalized PostgreSQL type name).
func TestCanaryLiteralUUID(t *testing.T) {
	litA, okA := canaryLiteral("uuid", CanaryA)
	if !okA {
		t.Fatalf("canaryLiteral(%q, CanaryA) ok = false, want a valid uuid literal", "uuid")
	}
	if !strings.Contains(litA, "::uuid") {
		t.Errorf("canaryLiteral(%q, CanaryA) = %q, want it cast ::uuid", "uuid", litA)
	}
	if strings.Contains(litA, CanaryA) {
		t.Errorf("canaryLiteral(%q, CanaryA) = %q, still contains the raw non-uuid text %q", "uuid", litA, CanaryA)
	}

	litB, okB := canaryLiteral("uuid", CanaryB)
	if !okB {
		t.Fatalf("canaryLiteral(%q, CanaryB) ok = false, want a valid uuid literal", "uuid")
	}
	if litA == litB {
		t.Fatalf("canaryLiteral produced the SAME literal for CanaryA and CanaryB (%q); "+
			"A1 needs two distinguishable canary rows or it cannot demonstrate withholding", litA)
	}

	// format_type() reports a size modifier for some types but never for uuid;
	// exercise the modifier-stripping path anyway so a regression there is caught.
	if lit, ok := canaryLiteral("uuid(16)", CanaryA); !ok || lit != litA {
		t.Errorf("canaryLiteral(%q, CanaryA) = (%q, %v), want the same as canaryLiteral(%q, CanaryA)", "uuid(16)", lit, ok, "uuid")
	}
}

// TestCanaryLiteralTextUnchanged: every fixture and integration test in this
// project's history used a text tenant column — the fix must not change that
// behaviour at all.
func TestCanaryLiteralTextUnchanged(t *testing.T) {
	for _, pgType := range []string{"text", "character varying", "character varying(255)", "citext", "name"} {
		lit, ok := canaryLiteral(pgType, CanaryA)
		if !ok {
			t.Fatalf("canaryLiteral(%q, CanaryA) ok = false, want the existing text-literal path", pgType)
		}
		want := "'" + CanaryA + "'"
		if lit != want {
			t.Errorf("canaryLiteral(%q, CanaryA) = %q, want %q (unchanged bare-quoted literal)", pgType, lit, want)
		}
	}
}

// TestCanaryLiteralUnsupportedType: a tenant column of a type the dispatch
// does not know must be reported honestly (ok=false), not guessed at —
// exactly sampleValue's own contract for O5.
func TestCanaryLiteralUnsupportedType(t *testing.T) {
	for _, pgType := range []string{"point", "tsvector", "integer[]"} {
		if _, ok := canaryLiteral(pgType, CanaryA); ok {
			t.Errorf("canaryLiteral(%q, CanaryA) ok = true, want false: unsupported tenant column type", pgType)
		}
	}
}

// TestCanaryTextMatchesLiteral: EnableRLS's synthesised policy compares
// tenant_column::text to current_setting() as plain text equality
// (oracle.go's CREATE POLICY). Whatever canaryLiteral stores in the column
// must, once cast to text by Postgres, equal canaryText's return exactly —
// otherwise CheckA1 sets a session variable that can never match the row it
// just seeded, and A1 stops proving anything for that column type.
func TestCanaryTextMatchesLiteral(t *testing.T) {
	for _, pgType := range []string{"uuid", "text", "character varying(255)"} {
		text, ok := canaryText(pgType, CanaryA)
		if !ok {
			t.Fatalf("canaryText(%q, CanaryA) ok = false", pgType)
		}
		lit, ok := canaryLiteral(pgType, CanaryA)
		if !ok {
			t.Fatalf("canaryLiteral(%q, CanaryA) ok = false", pgType)
		}
		if !strings.Contains(lit, "'"+text+"'") {
			t.Errorf("canaryLiteral(%q, CanaryA) = %q, does not embed canaryText's value %q", pgType, lit, text)
		}
	}
}

// TestSampleValueArray closes the second gap: sampleValue must cover
// array-typed NOT NULL columns (found blocking 8 of coder's 22 scoped
// tables). When the element type is itself known, the array wraps a
// per-row-distinct single element (TGD-BL-28: two rows sharing a value can
// collide against a uniqueness constraint, so an always-empty array would
// have the same defect the tenant-column fix and this one both closed
// elsewhere). An unknown element type still falls back to an empty array —
// valid for anything, just not distinguishable per row.
func TestSampleValueArray(t *testing.T) {
	cases := []struct {
		pgType string
		want   string
	}{
		{"text[]", "ARRAY['tgd-0']::text[]"},
		{"integer[]", "ARRAY[1000]::integer[]"},
		{"character varying(50)[]", "ARRAY['tgd-0']::character varying[]"},
	}
	for _, c := range cases {
		got, ok := sampleValue(c.pgType, 0)
		if !ok {
			t.Fatalf("sampleValue(%q, 0) ok = false, want array support", c.pgType)
		}
		if got != c.want {
			t.Errorf("sampleValue(%q, 0) = %q, want %q", c.pgType, got, c.want)
		}
	}
}

// TestSampleValueArrayUnknownElementFallsBackToEmpty: an array of a type
// sampleValue itself does not recognise still satisfies NOT NULL, honestly
// not distinguishable per row (mirrors the same fallback the array branch
// used unconditionally before this session's per-row fix).
func TestSampleValueArrayUnknownElementFallsBackToEmpty(t *testing.T) {
	got, ok := sampleValue("point[]", 0)
	if !ok {
		t.Fatalf(`sampleValue("point[]", 0) ok = false, want the empty-array fallback`)
	}
	if got != "'{}'::point[]" {
		t.Errorf(`sampleValue("point[]", 0) = %q, want "'{}'::point[]"`, got)
	}
}

// TestSampleValueArrayDistinctAcrossRows: the two indices SeedCanaries uses
// for the two canary rows on the same array column must produce different
// elements, or the fix is cosmetic.
func TestSampleValueArrayDistinctAcrossRows(t *testing.T) {
	a, okA := sampleValue("integer[]", 0)
	b, okB := sampleValue("integer[]", 1)
	if !okA || !okB {
		t.Fatalf("sampleValue ok = (%v, %v), want both true", okA, okB)
	}
	if a == b {
		t.Fatalf("sampleValue(%q, 0) and sampleValue(%q, 1) produced the SAME array literal %q; "+
			"two canary rows sharing a value collides with any uniqueness constraint on the column", "integer[]", "integer[]", a)
	}
}

// --- integration tests requiring a real PostgreSQL ---

// uuidFixture builds a source/probe pair like newFixture, but with a
// uuid-typed tenant column — coder's real, universal shape (22/22 scoped
// tables), never previously exercised by any fixture in this project.
func uuidFixture(t *testing.T, name string) *fixture {
	t.Helper()
	ctx := context.Background()
	admin := openAdmin(t)

	f := &fixture{
		admin:    admin,
		source:   "tgd_src_" + name,
		probe:    "tgd_probe_" + name,
		roleName: "tgd_role_" + name,
		rolePass: "tgdpass",
		rel: schema.Relation{
			Schema: "public", Name: "orgs", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "organization_id",
		},
	}

	run := func(q string) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	run(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.probe))
	run(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.source))
	run(fmt.Sprintf("CREATE DATABASE %q", f.source))
	t.Cleanup(func() {
		admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.probe))
		admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.source))
	})

	src := openDB(t, dsnFor(t, f.source, "", ""))
	mustExec(t, ctx, src, `CREATE TABLE orgs (
		id bigserial PRIMARY KEY,
		organization_id uuid NOT NULL,
		amount integer NOT NULL)`)
	// At least 2 real rows must exist for SeedCanaries's sampling mechanism
	// (TGD-BL-29) to have anything to copy.
	mustExec(t, ctx, src, `INSERT INTO orgs (organization_id, amount) VALUES
		('11111111-1111-4111-8111-111111111111', 1),
		('22222222-2222-4222-8222-222222222222', 2)`)
	src.Close()

	if err := CreateProbeDatabase(ctx, admin, f.source, f.probe); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	f.probeDB = openDB(t, dsnFor(t, f.probe, "", ""))

	if err := CreateRestrictedRole(ctx, f.probeDB, f.roleName, f.rolePass, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		f.probeDB.ExecContext(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %q", f.roleName))
	})

	f.restricted = openDB(t, dsnFor(t, f.probe, f.roleName, f.rolePass))
	return f
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestSeedCanariesUUIDTenantColumn is TGD-BL-27's red/green case: before the
// fix this fails with pq's 22P02 "invalid input syntax for type uuid", the
// exact error the real coder run reproduced.
func TestSeedCanariesUUIDTenantColumn(t *testing.T) {
	ctx := context.Background()
	f := uuidFixture(t, "uuidseed")

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the uuid tenant column seeded, not skipped", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %v, want the one table seeded", seeded)
	}

	var count int
	if err := f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM orgs").Scan(&count); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if count != 4 {
		t.Fatalf("orgs row count = %d, want 4 (2 inherited + one row per canary)", count)
	}

	var distinctTenants int
	if err := f.probeDB.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT count(DISTINCT organization_id) FROM orgs WHERE organization_id IN ('%s', '%s')",
		canaryAUUID, canaryBUUID)).Scan(&distinctTenants); err != nil {
		t.Fatalf("count distinct tenants: %v", err)
	}
	if distinctTenants != 2 {
		t.Fatalf("distinct organization_id values across the 2 canary rows = %d, want 2 (canary A and B must differ)", distinctTenants)
	}
}

// TestA1DiscriminatesWithUUIDTenantColumn proves the fix is not merely
// syntactically valid but semantically load-bearing: A1 must actually
// withhold rows on a uuid tenant column, not vacuously pass because the
// session variable CheckA1 sets can never match anything of the right type.
// With exactly two canary rows seeded, restricting to one canary tenant must
// see exactly one row — not zero, and not two.
func TestA1DiscriminatesWithUUIDTenantColumn(t *testing.T) {
	ctx := context.Background()
	f := uuidFixture(t, "uuida1")

	if _, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil || len(skipped) != 0 {
		t.Fatalf("seed: skipped=%v err=%v", skipped, err)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	unres, res, err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
	if err != nil {
		t.Fatalf("A1 should pass with two canaries and a real uuid-column policy: %v", err)
	}
	if unres != 4 {
		t.Fatalf("unrestricted count = %d, want 4 (2 inherited + 2 canary)", unres)
	}
	if res != 1 {
		t.Fatalf("restricted count = %d, want exactly 1 (a genuine match, not a vacuous 0)", res)
	}
}

// TestA4PassesWithUUIDTenantColumn: the positive control must also work end
// to end against a uuid tenant column, using TenantCanaryText the same way
// the CLI gate now does for its canary-based A4 call.
func TestA4PassesWithUUIDTenantColumn(t *testing.T) {
	ctx := context.Background()
	f := uuidFixture(t, "uuida4")

	if _, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil || len(skipped) != 0 {
		t.Fatalf("seed: skipped=%v err=%v", skipped, err)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	canaryTenant, err := TenantCanaryText(ctx, f.probeDB, f.rel, CanaryA)
	if err != nil {
		t.Fatalf("TenantCanaryText: %v", err)
	}

	ref, got, err := CheckA4(ctx, f.probeDB, f.restricted, f.rel, canaryTenant)
	if err != nil {
		t.Fatalf("A4 should pass against a correct uuid-column policy: %v", err)
	}
	if len(ref) != 1 || len(got) != 1 {
		t.Fatalf("reference=%v got=%v, want exactly one row each", ref, got)
	}
}

// TestSeedCanariesEnumColumn: a NOT NULL enum column that is NOT the tenant
// column — the second gap, blocking 8 of coder's 22 scoped tables alongside
// the uuid issue. sampleValue cannot recognise an enum purely from its type
// name (every enum type has a different name), so this exercises the
// catalog-lookup fallback.
func TestSeedCanariesEnumColumn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "enumcol", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TYPE tgd_status AS ENUM ('open', 'closed')`)
	mustExec(t, ctx, f.probeDB, `CREATE TABLE with_enum (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		status tgd_status NOT NULL)`)
	// At least 2 real rows must exist for SeedCanaries's sampling mechanism
	// (TGD-BL-29) to have anything to copy.
	mustExec(t, ctx, f.probeDB, `INSERT INTO with_enum (tenant_id, status) VALUES
		('real-tenant-1', 'open'), ('real-tenant-2', 'closed')`)
	rel := schema.Relation{Schema: "public", Name: "with_enum", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the enum column seeded, not skipped", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %v, want the one table seeded", seeded)
	}

	var count int
	if err := f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM with_enum").Scan(&count); err != nil {
		t.Fatalf("count with_enum: %v", err)
	}
	if count != 4 {
		t.Fatalf("with_enum row count = %d, want 4 (2 inherited + 2 canary)", count)
	}
}

// TestSeedCanariesArrayColumn: a NOT NULL array column that is NOT the
// tenant column — the array half of the second gap.
func TestSeedCanariesArrayColumn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "arraycol", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE with_array (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		tags text[] NOT NULL)`)
	// At least 2 real rows must exist for SeedCanaries's sampling mechanism
	// (TGD-BL-29) to have anything to copy.
	mustExec(t, ctx, f.probeDB, `INSERT INTO with_array (tenant_id, tags) VALUES
		('real-tenant-1', ARRAY['a','b']), ('real-tenant-2', ARRAY['c'])`)
	rel := schema.Relation{Schema: "public", Name: "with_array", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the array column seeded, not skipped", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %v, want the one table seeded", seeded)
	}
}

// TestSeedCanariesUnsupportedTenantColumnTypeIsNamedNotRaw closes the
// TGD-BL-19-style requirement: a tenant column of a type the dispatch does
// not support must be reported with a clean, named reason BEFORE any INSERT
// is attempted — never as a raw driver "invalid input syntax" string that
// reads like a policy defect rather than an operator/config gap.
func TestSeedCanariesUnsupportedTenantColumnTypeIsNamedNotRaw(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "unsupportedtenant", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE geo (
		id bigserial PRIMARY KEY,
		loc point NOT NULL)`)
	// At least 2 real rows must exist for SeedCanaries's sampling mechanism
	// (TGD-BL-29) to have anything to copy — the unsupported-type check on
	// the tenant column must fire regardless of whether sampling would
	// otherwise have succeeded.
	mustExec(t, ctx, f.probeDB, `INSERT INTO geo (loc) VALUES (point(0,0)), (point(1,1))`)
	rel := schema.Relation{Schema: "public", Name: "geo", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "loc"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(seeded) != 0 {
		t.Fatalf("seeded = %v, want none", seeded)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly one skipped table", skipped)
	}
	reason := skipped[0].Reason
	if strings.Contains(reason, "insert:") || strings.Contains(reason, "22P02") ||
		strings.Contains(reason, "invalid input syntax") {
		t.Errorf("skip reason %q looks like a raw driver failure, not a named type-dispatch error "+
			"— it must never reach the database at all for a type the dispatch already knows it cannot handle", reason)
	}
	if !strings.Contains(reason, "unsupported") {
		t.Errorf("skip reason %q does not name the problem as an unsupported type", reason)
	}
}

// --- TGD-BL-28: per-row canary values, and A4 without an "id" column ---

// TestSeedCanariesUUIDPKWithoutDefaultDoesNotCollide reproduces coder's real
// audit_logs shape exactly: a uuid NOT NULL primary key with no database
// default (the application generates it in Go), plus other NOT NULL columns
// with no default. Before the fix, SeedCanaries computed one sample value
// per COLUMN and reused it for both canary rows — the second INSERT then hit
// "duplicate key value violates unique constraint", the precise error the
// real coder run reproduced.
func TestSeedCanariesUUIDPKWithoutDefaultDoesNotCollide(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "uuidpk_nodefault", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE audit_like (
		id uuid NOT NULL PRIMARY KEY,
		tenant_id text NOT NULL,
		status_code integer NOT NULL,
		note text NOT NULL)`)
	// At least 2 real rows must exist for SeedCanaries's sampling mechanism
	// (TGD-BL-29) to have anything to copy — with DIFFERENT status_code/note
	// values, so borrowing from two distinct real rows is what makes the two
	// canary rows differ, not a coincidence of the fixture.
	mustExec(t, ctx, f.probeDB, `INSERT INTO audit_like (id, tenant_id, status_code, note) VALUES
		('aaaaaaaa-0000-4000-8000-000000000001', 'real-tenant-1', 200, 'first real row'),
		('bbbbbbbb-0000-4000-8000-000000000002', 'real-tenant-2', 404, 'second real row')`)
	rel := schema.Relation{Schema: "public", Name: "audit_like", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the table seeded, not skipped (this is the exact "+
			"duplicate-key shape TGD-BL-28 found against real coder)", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %v, want the one table seeded", seeded)
	}

	var count int
	if err := f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM audit_like").Scan(&count); err != nil {
		t.Fatalf("count audit_like: %v", err)
	}
	if count != 4 {
		t.Fatalf("audit_like row count = %d, want 4 (2 real + 2 canary)", count)
	}

	// Scoped to just the two CANARY rows — the table also holds the 2 real
	// rows they were sampled from, which must not be counted here.
	var distinctIDs, distinctStatus, distinctNotes int
	must := func(q string, dst *int) {
		t.Helper()
		if err := f.probeDB.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	canaryRows := fmt.Sprintf(" FROM audit_like WHERE tenant_id IN ('%s', '%s')", CanaryA, CanaryB)
	must("SELECT count(DISTINCT id)"+canaryRows, &distinctIDs)
	must("SELECT count(DISTINCT status_code)"+canaryRows, &distinctStatus)
	must("SELECT count(DISTINCT note)"+canaryRows, &distinctNotes)
	if distinctIDs != 2 {
		t.Errorf("distinct id values across the 2 canary rows = %d, want 2 — the PK column itself must differ between canary rows", distinctIDs)
	}
	if distinctStatus != 2 {
		t.Errorf("distinct status_code values across the 2 canary rows = %d, want 2 — borrowed from two different real rows, so they must differ", distinctStatus)
	}
	if distinctNotes != 2 {
		t.Errorf("distinct note values across the 2 canary rows = %d, want 2", distinctNotes)
	}
}

// TestSeedCanariesUUIDColumnValuesDistinctAcrossRows is the pure, DB-free
// pin on sampleValue's uuid case that TestSeedCanariesUUIDPKWithoutDefault
// exercises end to end: SeedCanaries must call it with two different row
// indices for the same column, or the integration test above is the only
// thing standing between a regression and a real duplicate-key failure.
func TestSeedCanariesUUIDColumnValuesDistinctAcrossRows(t *testing.T) {
	a, okA := sampleValue("uuid", 0)
	b, okB := sampleValue("uuid", 1)
	if !okA || !okB {
		t.Fatalf("sampleValue(\"uuid\", ...) ok = (%v, %v), want both true", okA, okB)
	}
	if a == b {
		t.Fatalf("sampleValue(\"uuid\", 0) and sampleValue(\"uuid\", 1) produced the SAME literal %q", a)
	}
}

// compositePKFixture builds a source/probe pair for a table with a composite
// primary key and no "id" column at all — organization_members's real
// shape, and 2 further of coder's 22 real scoped tables.
func compositePKFixture(t *testing.T, name string) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t, name, nil)
	mustExec(t, ctx, f.probeDB, `CREATE TABLE memberships (
		tenant_id text NOT NULL,
		user_id integer NOT NULL,
		role text NOT NULL,
		PRIMARY KEY (tenant_id, user_id))`)
	// Real members, pre-existing — matching coder's default-org shape (rows
	// present before any canary seeding), and at least 2 for SeedCanaries's
	// sampling mechanism (TGD-BL-29) to have anything to copy.
	mustExec(t, ctx, f.probeDB, `INSERT INTO memberships (tenant_id, user_id, role) VALUES
		('real-tenant', 1, 'admin'), ('real-tenant', 2, 'member')`)
	// newFixture's CreateRestrictedRole already ran and granted privileges on
	// every table that existed at that time — this table is created after,
	// so it needs its own grant.
	mustExec(t, ctx, f.probeDB, fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON memberships TO %q", f.roleName))
	return f
}

var membershipsRel = schema.Relation{Schema: "public", Name: "memberships", Kind: "BASE TABLE",
	Class: schema.Scoped, TenantColumn: "tenant_id"}

// TestA4CompositePrimaryKeyNoIdColumn is TGD-BL-28's second red/green case:
// before the fix, CheckA4's hardcoded "SELECT id FROM ..." fails with
// "column \"id\" does not exist" against a table with no id column at all —
// exactly what the real organization_members probe reproduced. After the
// fix, A4 derives its row identity from the composite primary key and passes.
func TestA4CompositePrimaryKeyNoIdColumn(t *testing.T) {
	ctx := context.Background()
	f := compositePKFixture(t, "a4composite")

	if _, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{membershipsRel}); err != nil || len(skipped) != 0 {
		t.Fatalf("seed: skipped=%v err=%v", skipped, err)
	}
	if err := EnableRLS(ctx, f.probeDB, membershipsRel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	unres, res, a1err := CheckA1(ctx, f.probeDB, f.restricted, membershipsRel)
	if a1err != nil {
		t.Fatalf("A1 should pass: %v", a1err)
	}
	if unres != 4 || res != 1 {
		t.Fatalf("A1 unrestricted=%d restricted=%d, want 4 (2 real + 2 canary) and 1", unres, res)
	}

	ref, got, a4err := CheckA4(ctx, f.probeDB, f.restricted, membershipsRel, CanaryA)
	if a4err != nil {
		t.Fatalf("A4 should pass against a table with no id column, using the composite PK: %v", a4err)
	}
	if len(ref) != 1 || len(got) != 1 {
		t.Fatalf("reference=%v got=%v, want exactly one row each", ref, got)
	}
}

// TestA4CompositePrimaryKeyDetectsWrongColumnPolicy: the row-key rework must
// not have weakened A4's actual discriminating power — a wrong-column policy
// against a composite-PK, no-id table must still be caught.
func TestA4CompositePrimaryKeyDetectsWrongColumnPolicy(t *testing.T) {
	ctx := context.Background()
	f := compositePKFixture(t, "a4compositewrong")

	if _, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{membershipsRel}); err != nil || len(skipped) != 0 {
		t.Fatalf("seed: skipped=%v err=%v", skipped, err)
	}
	for _, q := range []string{
		"ALTER TABLE public.memberships ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE public.memberships FORCE ROW LEVEL SECURITY",
		fmt.Sprintf("CREATE POLICY tgd_policy ON public.memberships USING (user_id::text = current_setting('%s', true))", TenantSetting),
	} {
		if _, err := f.probeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	_, _, a4err := CheckA4(ctx, f.probeDB, f.restricted, membershipsRel, CanaryA)
	if a4err == nil {
		t.Fatal("A4 PASSED against a wrong-column policy on a composite-PK table; " +
			"the row-key rework must not have made A4 blind")
	}
	if !strings.Contains(a4err.Error(), "positive control") {
		t.Errorf("A4 error = %v, want it to read as the positive control failing, not a raw driver error", a4err)
	}
}

// TestCheckA4NoRowKeyIsNamedError: a relation with zero columns (legal in
// PostgreSQL — CREATE TABLE t()) has no primary key AND no columns to fall
// back to at all. This must be a distinct, named error, never a silent pass
// and never dressed up as ErrA4 (TGD-BL-19's lesson, applied here per
// TGD-BL-28's explicit requirement).
func TestCheckA4NoRowKeyIsNamedError(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "norowkey", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE empty_cols()`)
	rel := schema.Relation{Schema: "public", Name: "empty_cols", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	_, _, err := CheckA4(ctx, f.probeDB, f.restricted, rel, CanaryA)
	if err == nil {
		t.Fatal("CheckA4 PASSED against a relation with zero columns; there is nothing to key rows by")
	}
	if !errors.Is(err, ErrA4NoRowKey) {
		t.Errorf("CheckA4 error = %v, want it to wrap ErrA4NoRowKey", err)
	}
	if errors.Is(err, ErrA4) {
		t.Errorf("CheckA4 error = %v, must NOT wrap ErrA4 — a missing row key is a usage/schema "+
			"gap, not the policy failing, and must never read like one (TGD-BL-19's lesson)", err)
	}
}
