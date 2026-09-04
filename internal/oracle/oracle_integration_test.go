package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// These tests need a real PostgreSQL, because RLS is the thing under test and
// there is no honest way to fake it. Set TGD_TEST_DSN to an admin connection:
//
//	TGD_TEST_DSN='postgres://user@127.0.0.1:5432/postgres?sslmode=disable' go test ./...
//
// Without it they skip, and the skip is loud: an oracle that was never exercised
// must not look like one that passed.
func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TGD_TEST_DSN")
	if dsn == "" {
		t.Skip("TGD_TEST_DSN not set; A1 was NOT exercised in this run")
	}
	return dsn
}

func openAdmin(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", adminDSN(t))
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping admin: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// dsnFor rewrites the admin DSN to target another database, optionally as
// another role.
func dsnFor(t *testing.T, dbName, role, pass string) string {
	t.Helper()
	base := adminDSN(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("unexpected DSN shape %q: %v", base, err)
	}
	if role != "" {
		u.User = url.UserPassword(role, pass)
	}
	u.Path = "/" + dbName
	return u.String()
}

// fixture builds a source database with a scoped table, then a probe from it.
type fixture struct {
	admin      *sql.DB
	source     string
	probe      string
	probeDB    *sql.DB
	rel        schema.Relation
	roleName   string
	rolePass   string
	restricted *sql.DB
	// InheritedRows is how many application rows newFixture seeded into
	// invoices before any canary seeding — always at least 2 (TGD-BL-29's
	// sampling mechanism needs at least two existing rows to copy), padded
	// with generic filler tenants beyond whatever the caller's tenants
	// argument supplied.
	InheritedRows int
}

func newFixture(t *testing.T, name string, tenants []string) *fixture {
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
			Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id",
		},
	}

	exec := func(db *sql.DB, q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	exec(admin, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.probe))
	exec(admin, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.source))
	exec(admin, fmt.Sprintf("CREATE DATABASE %q", f.source))
	t.Cleanup(func() {
		admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.probe))
		admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", f.source))
	})

	src, err := sql.Open("postgres", dsnFor(t, f.source, "", ""))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	exec(src, `CREATE TABLE invoices (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		amount integer NOT NULL)`)
	// Application rows, present before any canary seeding — at least 2,
	// padded with generic filler tenants beyond whatever the caller asked
	// for, since SeedCanaries's sampling mechanism (TGD-BL-29) needs
	// existing rows to copy and has no fixture-fallback configured here.
	seedTenants := append([]string{}, tenants...)
	for len(seedTenants) < 2 {
		seedTenants = append(seedTenants, fmt.Sprintf("tgd-fixture-filler-%d", len(seedTenants)))
	}
	for _, tn := range seedTenants {
		exec(src, fmt.Sprintf("INSERT INTO invoices (tenant_id, amount) VALUES ('%s', 1)", tn))
	}
	f.InheritedRows = len(seedTenants)
	src.Close()

	if err := CreateProbeDatabase(ctx, admin, f.source, f.probe); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	f.probeDB, err = sql.Open("postgres", dsnFor(t, f.probe, "", ""))
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	t.Cleanup(func() { f.probeDB.Close() })

	if err := CreateRestrictedRole(ctx, f.probeDB, f.roleName, f.rolePass, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		f.probeDB.ExecContext(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %q", f.roleName))
	})

	f.restricted, err = sql.Open("postgres", dsnFor(t, f.probe, f.roleName, f.rolePass))
	if err != nil {
		t.Fatalf("open restricted: %v", err)
	}
	t.Cleanup(func() { f.restricted.Close() })
	return f
}

// --- the happy path ---

func TestA1PassesWithTwoTenantsAndAProperPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "ok", nil)

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(seeded) != 1 || len(skipped) != 0 {
		t.Fatalf("seeded=%v skipped=%v, want the one table seeded", seeded, skipped)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	unres, res, err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
	if err != nil {
		t.Fatalf("A1 should pass with two canaries and a real policy: %v", err)
	}
	if res >= unres {
		t.Errorf("A1 returned (%d unrestricted, %d restricted) without erroring", unres, res)
	}
}

// --- the three ways A1 must abort ---

// TestA1AbortsWithOnlyOneTenant. Withholding cannot be demonstrated when every
// row belongs to the tenant doing the looking. This is coder's real situation.
func TestA1AbortsWithOnlyOneTenant(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "onetenant", nil)

	// newFixture's own filler rows (TGD-BL-29: at least 2 must exist for
	// SeedCanaries's sampling to have anything to copy) must be cleared
	// first, or the probe would not actually be single-tenant.
	if _, err := f.probeDB.ExecContext(ctx, "DELETE FROM invoices"); err != nil {
		t.Fatalf("clear filler rows: %v", err)
	}
	// Seed only ONE canary, by hand.
	if _, err := f.probeDB.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO invoices (tenant_id, amount) VALUES ('%s', 1)", CanaryA)); err != nil {
		t.Fatalf("seed one tenant: %v", err)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	_, _, err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
	if err == nil {
		t.Fatal("A1 PASSED with a single tenant; it cannot demonstrate withholding " +
			"and the oracle would be trusted while blind")
	}
	if !errors.Is(err, ErrA1) {
		t.Errorf("A1 failed with %v, want it to wrap ErrA1 so the caller can exit %d", err, ExitA1Failed)
	}
}

// TestA1AbortsWithPermissivePolicy: USING(true) withholds nothing.
func TestA1AbortsWithPermissivePolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "permissive", nil)

	if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, q := range []string{
		"ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY",
		"CREATE POLICY tgd_policy ON public.invoices USING (true)",
	} {
		if _, err := f.probeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	_, _, err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
	if err == nil {
		t.Fatal("A1 PASSED against a USING(true) policy; a permissive policy " +
			"withholds nothing and every later verdict would be meaningless")
	}
	if !errors.Is(err, ErrA1) {
		t.Errorf("A1 failed with %v, want ErrA1", err)
	}
}

// TestA1AbortsWhenRoleBypassesRLS: BYPASSRLS makes the policy inert.
func TestA1AbortsWhenRoleBypassesRLS(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "bypass", nil)

	if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}
	if _, err := f.probeDB.ExecContext(ctx,
		fmt.Sprintf("ALTER ROLE %q BYPASSRLS", f.roleName)); err != nil {
		t.Fatalf("grant BYPASSRLS: %v", err)
	}

	_, _, err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
	if err == nil {
		t.Fatal("A1 PASSED with a BYPASSRLS role; the policy is inert and the " +
			"oracle sees everything while reporting itself constrained")
	}
	if !errors.Is(err, ErrA1) {
		t.Errorf("A1 failed with %v, want ErrA1", err)
	}
}

// --- supporting properties ---

// TestProbeIsSeparateFromSource: the application's database must be untouched.
func TestProbeIsSeparateFromSource(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "isolation", []string{"real-tenant"})

	src, err := sql.Open("postgres", dsnFor(t, f.source, "", ""))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer src.Close()

	var before int
	if err := src.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&before); err != nil {
		t.Fatalf("count source before: %v", err)
	}

	if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	var after int
	if err := src.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&after); err != nil {
		t.Fatalf("count source after: %v", err)
	}
	if after != before {
		t.Errorf("source row count changed %d -> %d; the tool wrote to the application database", before, after)
	}

	var pols int
	if err := src.QueryRowContext(ctx,
		"SELECT count(*) FROM pg_policies WHERE tablename='invoices'").Scan(&pols); err != nil {
		t.Fatalf("count source policies: %v", err)
	}
	if pols != 0 {
		t.Errorf("source database gained %d policies; synthesis must target the probe only", pols)
	}
}

// TestSkippedTablesAreReported: a table the seeder cannot handle is reported,
// never silently omitted.
func TestSkippedTablesAreReported(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "skipped", nil)

	if _, err := f.probeDB.ExecContext(ctx, `CREATE TABLE weird (
		id serial PRIMARY KEY,
		tenant_id text NOT NULL,
		shape point NOT NULL)`); err != nil {
		t.Fatalf("create weird table: %v", err)
	}
	weird := schema.Relation{Schema: "public", Name: "weird", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel, weird})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(seeded) != 1 {
		t.Errorf("seeded = %v, want only the supported table", seeded)
	}
	if len(skipped) != 1 || skipped[0].Table != "public.weird" {
		t.Fatalf("skipped = %+v, want public.weird reported", skipped)
	}
	if skipped[0].Reason == "" {
		t.Errorf("a skipped table must record why")
	}
}

// TestRLSIsForced: without FORCE, RLS does not apply to the table owner.
func TestRLSIsForced(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "forced", nil)
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}
	enabled, forced, err := RowSecurityEnabled(ctx, f.probeDB, f.rel)
	if err != nil {
		t.Fatalf("read rowsecurity flags: %v", err)
	}
	if !enabled {
		t.Errorf("row security not enabled")
	}
	if !forced {
		t.Errorf("row security not FORCED; the policy would be inert for the table owner")
	}
	n, err := PolicyCount(ctx, f.probeDB, f.rel)
	if err != nil || n != 1 {
		t.Errorf("policy count = %d (err %v), want 1", n, err)
	}
}

// TestDropProbeDatabaseRemovesIt: nothing exercised this path before, so a
// no-op implementation would have gone undetected.
func TestDropProbeDatabaseRemovesIt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dropcheck", nil)

	var exists bool
	check := func() bool {
		t.Helper()
		if err := f.admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", f.probe).Scan(&exists); err != nil {
			t.Fatalf("check pg_database: %v", err)
		}
		return exists
	}
	if !check() {
		t.Fatalf("probe database %q does not exist before Drop; fixture setup is broken", f.probe)
	}

	f.probeDB.Close() // no active connections may remain, or DROP DATABASE fails
	if err := DropProbeDatabase(ctx, f.admin, f.probe); err != nil {
		t.Fatalf("DropProbeDatabase: %v", err)
	}
	if check() {
		t.Errorf("probe database %q still exists after DropProbeDatabase", f.probe)
	}
}

// TestSeedCanariesOnlyTouchesItsArgument: SeedCanaries must write only to the
// *sql.DB it is handed. This guards against a version that silently reopens or
// falls back to the application's connection.
func TestSeedCanariesOnlyTouchesItsArgument(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "argonly", []string{"real-tenant"})

	src, err := sql.Open("postgres", dsnFor(t, f.source, "", ""))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer src.Close()
	var before int
	if err := src.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&before); err != nil {
		t.Fatalf("count source: %v", err)
	}

	seeded, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel})
	if err != nil || len(seeded) != 1 {
		t.Fatalf("seed probe: seeded=%v err=%v", seeded, err)
	}

	var probeCount, sourceAfter int
	if err := f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&probeCount); err != nil {
		t.Fatalf("count probe: %v", err)
	}
	if err := src.QueryRowContext(ctx, "SELECT count(*) FROM invoices").Scan(&sourceAfter); err != nil {
		t.Fatalf("count source after: %v", err)
	}
	if want := f.InheritedRows + 2; probeCount != want {
		// The probe is created via TEMPLATE, so it legitimately inherits the
		// source's pre-existing rows (f.InheritedRows, at least 2 —
		// TGD-BL-29's sampling mechanism needs existing rows to copy) plus
		// the 2 seeded canaries.
		t.Errorf("probe row count = %d, want %d (%d inherited + 2 canaries)", probeCount, want, f.InheritedRows)
	}
	if sourceAfter != before {
		t.Errorf("SOURCE row count changed %d -> %d; SeedCanaries wrote outside its argument", before, sourceAfter)
	}
}

// TestCreateRestrictedRoleDefaultsDenyBypass isolates O2: an earlier mutation
// (BYPASSRLS in the CREATE ROLE statement) survived because
// TestA1AbortsWhenRoleBypassesRLS also grants BYPASSRLS explicitly afterward,
// masking the default. This checks the role's own default in isolation.
func TestCreateRestrictedRoleDefaultsDenyBypass(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "roledefault", nil)

	var bypass bool
	if err := f.probeDB.QueryRowContext(ctx,
		"SELECT rolbypassrls FROM pg_roles WHERE rolname=$1", f.roleName).Scan(&bypass); err != nil {
		t.Fatalf("read rolbypassrls: %v", err)
	}
	if bypass {
		t.Errorf("CreateRestrictedRole's own role has BYPASSRLS by default")
	}
}

// TestCreateRestrictedRoleGrantsNonPublicSchema is TGD-BL-46's regression
// test. CreateRestrictedRole used to hardcode its GRANTs to schema public —
// coder and casdoor both happened to keep every scoped table there, so this
// was never exercised until zitadel's real schema (adminapi/auth/eventstore/
// projections/system, zero relations in public) hit it for real: verify
// could not prove a single one of zitadel's 121 scoped tables, all failing
// identically with "permission denied for schema adminapi" on the restricted
// connection (SRS §7.15). This test reproduces that shape directly: a scoped
// table in a schema OTHER than public, exactly the property every fixture
// elsewhere in this file lacks (newFixture's own table is always
// public.invoices).
func TestCreateRestrictedRoleGrantsNonPublicSchema(t *testing.T) {
	ctx := context.Background()
	admin := openAdmin(t)

	const source, probe = "tgd_src_nonpub", "tgd_probe_nonpub"
	exec := func(db *sql.DB, q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(admin, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", probe))
	exec(admin, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", source))
	exec(admin, fmt.Sprintf("CREATE DATABASE %q", source))
	t.Cleanup(func() {
		admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", probe))
		admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", source))
	})

	src, err := sql.Open("postgres", dsnFor(t, source, "", ""))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	// adminapi.styling2's own shape (SRS §7.15): the scoped table lives
	// outside public, the same as every one of zitadel's real 121 relations.
	exec(src, `CREATE SCHEMA adminapi`)
	exec(src, `CREATE TABLE adminapi.styling2 (
		aggregate_id text NOT NULL,
		instance_id text NOT NULL,
		primary_color text,
		PRIMARY KEY (instance_id, aggregate_id))`)
	exec(src, `INSERT INTO adminapi.styling2 (aggregate_id, instance_id, primary_color) VALUES
		('a1', 'tenant-one', '#111'), ('a2', 'tenant-two', '#222')`)
	src.Close()

	if err := CreateProbeDatabase(ctx, admin, source, probe); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	probeDB, err := sql.Open("postgres", dsnFor(t, probe, "", ""))
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	t.Cleanup(func() { probeDB.Close() })

	rel := schema.Relation{Schema: "adminapi", Name: "styling2", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "instance_id"}

	const roleName, rolePass = "tgd_role_nonpub", "tgdpass"
	if err := CreateRestrictedRole(ctx, probeDB, roleName, rolePass, []schema.Relation{rel}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		probeDB.ExecContext(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %q", roleName))
	})

	restricted, err := sql.Open("postgres", dsnFor(t, probe, roleName, rolePass))
	if err != nil {
		t.Fatalf("open restricted: %v", err)
	}
	t.Cleanup(func() { restricted.Close() })

	var count int
	err = restricted.QueryRowContext(ctx, "SELECT count(*) FROM adminapi.styling2").Scan(&count)
	if err != nil {
		t.Fatalf("restricted role could not read adminapi.styling2 — CreateRestrictedRole's "+
			"grants did not reach this table's own schema: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (both rows, no RLS enabled yet on this table)", count)
	}
}

// --- A4: the positive control ---
//
// A1 alone is insufficient (TGD-BL-15): a policy on the wrong column still
// satisfies A1's "strictly fewer rows" inequality, because it excludes rows —
// just not the correct ones. A4 catches this by checking WHICH rows survive,
// not merely how many: a known-correctly-scoped reference query, run
// unrestricted, must return exactly the same rows a bare query returns when RLS
// does the filtering instead.

// TestA4PassesWithCorrectPolicy is the case a correct policy must satisfy. A
// build where A4 can never pass is as useless as one where it can never fail.
func TestA4PassesWithCorrectPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a4ok", nil)

	if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	ref, got, err := CheckA4(ctx, f.probeDB, f.restricted, f.rel, CanaryA)
	if err != nil {
		t.Fatalf("A4 should pass against a correct policy: %v", err)
	}
	if len(ref) == 0 {
		t.Fatalf("reference set is empty; the test proves nothing")
	}
	if len(got) != len(ref) {
		t.Fatalf("restricted set length = %d, want %d (matching the reference)", len(got), len(ref))
	}
}

// TestA4AbortsWithWrongColumnPolicy is the exact case that survived A1 during
// TGD-BL-10's mutation sweep: a policy comparing id::text to the tenant setting.
func TestA4AbortsWithWrongColumnPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a4wrongcol", nil)

	if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, q := range []string{
		"ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY",
		fmt.Sprintf("CREATE POLICY tgd_policy ON public.invoices USING (id::text = current_setting('%s', true))", TenantSetting),
	} {
		if _, err := f.probeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	_, _, err := CheckA4(ctx, f.probeDB, f.restricted, f.rel, CanaryA)
	if err == nil {
		t.Fatal("A4 PASSED against a policy on the wrong column; it excludes rows " +
			"but not the correct ones, and A1 alone cannot see this")
	}
	if !errors.Is(err, ErrA4) {
		t.Errorf("A4 failed with %v, want it to wrap ErrA4 so the caller can exit %d", err, ExitA4Failed)
	}
	if errors.Is(err, ErrA1) {
		t.Errorf("A4's error wraps ErrA1; an operator could not tell blind-oracle from over-restrictive apart")
	}
}

// TestA4AbortsWithOverRestrictivePolicy: USING(false) hides real data from its
// own tenant, which A1 also cannot see (0 rows is still "strictly fewer").
func TestA4AbortsWithOverRestrictivePolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a4restrictive", nil)

	if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, q := range []string{
		"ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY",
		"CREATE POLICY tgd_policy ON public.invoices USING (false)",
	} {
		if _, err := f.probeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	_, _, err := CheckA4(ctx, f.probeDB, f.restricted, f.rel, CanaryA)
	if err == nil {
		t.Fatal("A4 PASSED against USING(false); the policy hides every row, " +
			"including a valid tenant's own data, and A1 cannot see this either")
	}
	if !errors.Is(err, ErrA4) {
		t.Errorf("A4 failed with %v, want ErrA4", err)
	}
}

// TestA1AndA4AreNotRedundant runs both checks against the same probe database
// under two different defects, and requires each to be caught by exactly the
// check the design says should catch it.
func TestA1AndA4AreNotRedundant(t *testing.T) {
	ctx := context.Background()

	t.Run("A1-only: single tenant with a CORRECT policy", func(t *testing.T) {
		f := newFixture(t, "redundancy_a1only", nil)
		// newFixture's own filler rows (TGD-BL-29) must be cleared first, or
		// the probe would not actually be single-tenant.
		if _, err := f.probeDB.ExecContext(ctx, "DELETE FROM invoices"); err != nil {
			t.Fatalf("clear filler rows: %v", err)
		}
		if _, err := f.probeDB.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO invoices (tenant_id, amount) VALUES ('%s', 1)", CanaryA)); err != nil {
			t.Fatalf("seed one tenant: %v", err)
		}
		if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
			t.Fatalf("enable RLS: %v", err)
		}

		_, _, a1err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
		if a1err == nil {
			t.Error("A1 should fail: a single-tenant probe cannot demonstrate withholding")
		}

		_, _, a4err := CheckA4(ctx, f.probeDB, f.restricted, f.rel, CanaryA)
		if a4err != nil {
			t.Errorf("A4 should PASS: the policy is correct and returns identical rows either way; got %v", a4err)
		}
	})

	t.Run("A4-only: two tenants with a WRONG-COLUMN policy", func(t *testing.T) {
		f := newFixture(t, "redundancy_a4only", nil)
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		for _, q := range []string{
			"ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY",
			fmt.Sprintf("CREATE POLICY tgd_policy ON public.invoices USING (id::text = current_setting('%s', true))", TenantSetting),
		} {
			if _, err := f.probeDB.ExecContext(ctx, q); err != nil {
				t.Fatalf("exec %q: %v", q, err)
			}
		}

		_, _, a1err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
		if a1err != nil {
			t.Errorf("A1 should PASS (deceptively): 0 restricted rows is still "+
				"\"strictly fewer\" than 2 unrestricted; got %v", a1err)
		}

		_, _, a4err := CheckA4(ctx, f.probeDB, f.restricted, f.rel, CanaryA)
		if a4err == nil {
			t.Error("A4 should FAIL: the wrong-column policy returns 0 rows where " +
				"the reference query returns 1, and A1 alone would miss this")
		}
	})
}

// --- A2: policy coverage ---
//
// A2 checks that every table classified Scoped has RLS enabled and at least one
// matching policy in pg_policies. It must never touch a table the schema
// inference could not classify — an Unclassifiable table is reported, not
// policy-checked, and silently checking it would make A2 unfireable on any
// real schema that has views or ambiguous columns (which every real schema does).

// TestA2PassesWithProperPolicy: the happy path a working A2 must satisfy.
func TestA2PassesWithProperPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a2ok", nil)
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	uncovered, err := CheckA2(ctx, f.probeDB, []schema.Relation{f.rel})
	if err != nil {
		t.Fatalf("A2 should pass with RLS enabled and a policy present: %v", err)
	}
	if len(uncovered) != 0 {
		t.Errorf("uncovered = %v, want none", uncovered)
	}
}

// TestA2AbortsWhenPolicyDropped: RLS stays enabled, but the policy is gone.
func TestA2AbortsWhenPolicyDropped(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a2dropped", nil)
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}
	if _, err := f.probeDB.ExecContext(ctx, "DROP POLICY tgd_policy ON public.invoices"); err != nil {
		t.Fatalf("drop policy: %v", err)
	}

	uncovered, err := CheckA2(ctx, f.probeDB, []schema.Relation{f.rel})
	if err == nil {
		t.Fatal("A2 PASSED with the policy dropped; RLS is enabled but enforces nothing")
	}
	if !errors.Is(err, ErrA2) {
		t.Errorf("A2 failed with %v, want it to wrap ErrA2 so the caller can exit %d", err, ExitA2Failed)
	}
	if len(uncovered) != 1 || uncovered[0] != f.rel.Qualified() {
		t.Errorf("uncovered = %v, want [%s]", uncovered, f.rel.Qualified())
	}
}

// TestA2AbortsWhenRLSNeverEnabled: a policy exists, but RLS was never switched
// on for the table — PostgreSQL allows creating a policy on a table with RLS
// disabled, and the policy is then completely inert.
func TestA2AbortsWhenRLSNeverEnabled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a2noenable", nil)
	if _, err := f.probeDB.ExecContext(ctx,
		fmt.Sprintf("CREATE POLICY tgd_policy ON public.invoices USING (%s::text = current_setting('%s', true))",
			f.rel.TenantColumn, TenantSetting)); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	// Deliberately no ENABLE ROW LEVEL SECURITY.

	_, err := CheckA2(ctx, f.probeDB, []schema.Relation{f.rel})
	if err == nil {
		t.Fatal("A2 PASSED with a policy present but RLS never enabled; the policy is inert")
	}
	if !errors.Is(err, ErrA2) {
		t.Errorf("A2 failed with %v, want ErrA2", err)
	}
}

// TestA2NeverPolicyChecksUnclassifiableTables is the boundary assertion the
// design requires: an Unclassifiable table is reported by inference, not
// policy-checked by A2. Silently checking it would make A2 unfireable on any
// real schema, because every real schema has views or ambiguous tables.
//
// This test must fail if CheckA2 stops filtering by Class.
func TestA2NeverPolicyChecksUnclassifiableTables(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a2unclass", nil)
	if _, err := f.probeDB.ExecContext(ctx, `CREATE TABLE ambiguous_tbl (
		id serial PRIMARY KEY, org_id text, workspace_id text)`); err != nil {
		t.Fatalf("create ambiguous table: %v", err)
	}
	ambiguous := schema.Relation{
		Schema: "public", Name: "ambiguous_tbl", Kind: "BASE TABLE",
		Class: schema.Unclassifiable, Candidates: []string{"org_id", "workspace_id"},
	}
	// No RLS, no policy on ambiguous_tbl at all — if A2 checked it, it would fail.

	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS on the real scoped table: %v", err)
	}

	uncovered, err := CheckA2(ctx, f.probeDB, []schema.Relation{f.rel, ambiguous})
	if err != nil {
		t.Fatalf("A2 should pass: the only Scoped table is correctly covered, "+
			"and the Unclassifiable table must not be checked at all; got %v", err)
	}
	for _, u := range uncovered {
		if u == ambiguous.Qualified() {
			t.Errorf("A2 reported the Unclassifiable table %q as uncovered; "+
				"it must never be policy-checked", u)
		}
	}
}

// --- A3: role privilege ---

func tableOwnerFor(t *testing.T, ctx context.Context, db *sql.DB, r schema.Relation) string {
	t.Helper()
	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT pg_get_userbyid(c.relowner) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname=$1 AND c.relname=$2`, r.Schema, r.Name).Scan(&owner); err != nil {
		t.Fatalf("read table owner: %v", err)
	}
	return owner
}

// TestA3PassesWithNonOwningNonBypassRole: the happy path.
func TestA3PassesWithNonOwningNonBypassRole(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a3ok", nil)
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	if err := CheckA3(ctx, f.probeDB, []schema.Relation{f.rel}, f.roleName); err != nil {
		t.Fatalf("A3 should pass for a non-owning, non-superuser, non-bypass role: %v", err)
	}
}

// TestA3AbortsWhenOwnerWithoutForce: the exact FORCE ROW LEVEL SECURITY no-op
// the design's §6 exists to catch — RLS never applies to a table's owner
// unless FORCE is set.
//
// The table must be owned by a NON-superuser, non-bypass role, or CheckA3's
// earlier superuser check fires first and the test passes for the wrong
// reason without ever exercising the FORCE logic at all. The fixture's tables
// are created as the admin connection, which is the superuser in these tests,
// so ownership is transferred explicitly to f.roleName first.
func TestA3AbortsWhenOwnerWithoutForce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a3owner", nil)
	if _, err := f.probeDB.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE public.invoices OWNER TO %q", f.roleName)); err != nil {
		t.Fatalf("transfer ownership: %v", err)
	}
	if _, err := f.probeDB.ExecContext(ctx,
		"ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}
	// Deliberately no FORCE.
	if _, err := f.probeDB.ExecContext(ctx,
		fmt.Sprintf("CREATE POLICY tgd_policy ON public.invoices USING (%s::text = current_setting('%s', true))",
			f.rel.TenantColumn, TenantSetting)); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	owner := tableOwnerFor(t, ctx, f.probeDB, f.rel)
	if owner != f.roleName {
		t.Fatalf("ownership transfer did not take effect: owner = %q, want %q", owner, f.roleName)
	}

	err := CheckA3(ctx, f.probeDB, []schema.Relation{f.rel}, owner)
	if err == nil {
		t.Fatalf("A3 PASSED for the table owner %q without FORCE; RLS does not "+
			"apply to an owner unless forced, so the policy is silently inert", owner)
	}
	if !errors.Is(err, ErrA3) {
		t.Errorf("A3 failed with %v, want it to wrap ErrA3 so the caller can exit %d", err, ExitA3Failed)
	}
}

// TestA3PassesWhenOwnerWithForce: FORCE fixes exactly the case above, with the
// same non-superuser owner so the superuser check cannot mask the result.
func TestA3PassesWhenOwnerWithForce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a3ownerforce", nil)
	if _, err := f.probeDB.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE public.invoices OWNER TO %q", f.roleName)); err != nil {
		t.Fatalf("transfer ownership: %v", err)
	}
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil { // EnableRLS applies FORCE
		t.Fatalf("enable RLS: %v", err)
	}
	owner := tableOwnerFor(t, ctx, f.probeDB, f.rel)
	if owner != f.roleName {
		t.Fatalf("ownership transfer did not take effect: owner = %q, want %q", owner, f.roleName)
	}

	if err := CheckA3(ctx, f.probeDB, []schema.Relation{f.rel}, owner); err != nil {
		t.Fatalf("A3 should pass for the owner once FORCE is set: %v", err)
	}
}

// TestA3AbortsWithBypassRLSRole.
func TestA3AbortsWithBypassRLSRole(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a3bypass", nil)
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}
	if _, err := f.probeDB.ExecContext(ctx,
		fmt.Sprintf("ALTER ROLE %q BYPASSRLS", f.roleName)); err != nil {
		t.Fatalf("grant BYPASSRLS: %v", err)
	}

	err := CheckA3(ctx, f.probeDB, []schema.Relation{f.rel}, f.roleName)
	if err == nil {
		t.Fatal("A3 PASSED for a role with BYPASSRLS; RLS never applies to it regardless of policy")
	}
	if !errors.Is(err, ErrA3) {
		t.Errorf("A3 failed with %v, want ErrA3", err)
	}
}

// TestA3AbortsWithSuperuserRole.
func TestA3AbortsWithSuperuserRole(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "a3super", nil)
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	var superRole string
	if err := f.probeDB.QueryRowContext(ctx,
		"SELECT rolname FROM pg_roles WHERE rolsuper LIMIT 1").Scan(&superRole); err != nil {
		t.Fatalf("find a superuser role: %v", err)
	}

	err := CheckA3(ctx, f.probeDB, []schema.Relation{f.rel}, superRole)
	if err == nil {
		t.Fatalf("A3 PASSED for the superuser role %q; RLS never applies to superusers", superRole)
	}
	if !errors.Is(err, ErrA3) {
		t.Errorf("A3 failed with %v, want ErrA3", err)
	}
}

// --- ProofState, extended to all four checks ---

// TestPolicyProvenRequiresAllFourChecks extends the six-case table to require
// A2 and A3 as well. The new case named in the task is included explicitly:
// A1, A3, A4 passed but A2 never run must still refuse.
func TestPolicyProvenRequiresAllFourChecks(t *testing.T) {
	pass := func(a1c, a1p, a2c, a2p, a3c, a3p, a4c, a4p bool) ProofState {
		return ProofState{
			A1Checked: a1c, A1Passed: a1p,
			A2Checked: a2c, A2Passed: a2p,
			A3Checked: a3c, A3Passed: a3p,
			A4Checked: a4c, A4Passed: a4p,
		}
	}
	cases := []struct {
		name  string
		state ProofState
		want  bool
	}{
		{"nothing run", ProofState{}, false},
		{"A1 passed, A4 never run", pass(true, true, false, false, false, false, false, false), false},
		{"A1 passed, A2 A3 A4 all never run", pass(true, true, false, false, false, false, false, false), false},
		{"A1, A3, A4 passed, A2 never run", pass(true, true, false, false, true, true, true, true), false},
		{"A1, A2, A4 passed, A3 never run", pass(true, true, true, true, false, false, true, true), false},
		{"all four run, A2 failed", pass(true, true, true, false, true, true, true, true), false},
		{"all four run, A3 failed", pass(true, true, true, true, true, false, true, true), false},
		{"all four run and passed", pass(true, true, true, true, true, true, true, true), true},
	}
	for _, c := range cases {
		err := c.state.PolicyProven()
		got := err == nil
		if got != c.want {
			t.Errorf("%s: PolicyProven() = %v, want nil-ness %v", c.name, err, c.want)
		}
	}
}
