package oracle

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// --- §9.6/TGD-BL-29: row sampling closes the CHECK-constraint class ---

// TestSeedCanariesCrossColumnCheckConstraint reproduces coder's exact
// chat_model_configs shape: a CHECK spanning two columns' values
// (deleted = true OR ai_provider_id IS NOT NULL), where ai_provider_id is
// nullable with no default and deleted is NOT NULL with a database default
// of false. Neither column's OWN type or nullability is the problem — no
// synthesis strategy that reasons about one column at a time can ever
// satisfy a constraint that relates two columns to each other. Sampling
// closes it by construction: the two rows are borrowed from real,
// already-valid data, so whatever relationship the CHECK enforces already
// held.
func TestSeedCanariesCrossColumnCheckConstraint(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "crosscheck", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE chat_model_configs_like (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		deleted boolean NOT NULL DEFAULT false,
		ai_provider_id uuid,
		CHECK (deleted = true OR ai_provider_id IS NOT NULL))`)
	// Two real, already-valid rows: deleted=false with a real ai_provider_id,
	// exactly the shape a live application would have. A row that violates
	// its own table's CHECK could never have existed to sample from.
	mustExec(t, ctx, f.probeDB, `INSERT INTO chat_model_configs_like (tenant_id, deleted, ai_provider_id) VALUES
		('real-tenant-1', false, '11111111-1111-4111-8111-111111111111'),
		('real-tenant-2', false, '22222222-2222-4222-8222-222222222222')`)
	rel := schema.Relation{Schema: "public", Name: "chat_model_configs_like", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the table seeded, not skipped (this is the exact "+
			"cross-column CHECK shape TGD-BL-29 found against real coder)", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %v, want the one table seeded", seeded)
	}

	var count int
	if err := f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM chat_model_configs_like").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 4 {
		t.Fatalf("row count = %d, want 4 (2 real + 2 canary)", count)
	}

	// The two canary rows must have inherited a non-NULL ai_provider_id from
	// the samples — leaving it NULL (the pre-sampling behaviour for a
	// nullable column) is exactly what violated the CHECK against real coder.
	var nullCount int
	if err := f.probeDB.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT count(*) FROM chat_model_configs_like WHERE tenant_id IN ('%s', '%s') AND ai_provider_id IS NULL",
		CanaryA, CanaryB)).Scan(&nullCount); err != nil {
		t.Fatalf("count null ai_provider_id: %v", err)
	}
	if nullCount != 0 {
		t.Errorf("canary rows with NULL ai_provider_id = %d, want 0 — sampling must borrow the real value, not leave it NULL", nullCount)
	}
}

// TestSeedCanariesPreservesForeignKeyReferences answers §9.6's FK-ordering
// question directly: a sampled row's FK column is borrowed verbatim, and the
// row it references is already present (the probe is a full data-and-schema
// copy of the target, never a partial one) — no parent-copying and no
// constraint deferral is needed for the reference itself to resolve.
func TestSeedCanariesPreservesForeignKeyReferences(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "fkpreserve", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE orgs2 (id serial PRIMARY KEY, name text NOT NULL)`)
	mustExec(t, ctx, f.probeDB, `INSERT INTO orgs2 (name) VALUES ('org-a'), ('org-b')`)
	mustExec(t, ctx, f.probeDB, `CREATE TABLE docs (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		org_ref_id integer NOT NULL REFERENCES orgs2(id))`)
	var orgA, orgB int
	if err := f.probeDB.QueryRowContext(ctx, "SELECT id FROM orgs2 WHERE name='org-a'").Scan(&orgA); err != nil {
		t.Fatalf("read org-a id: %v", err)
	}
	if err := f.probeDB.QueryRowContext(ctx, "SELECT id FROM orgs2 WHERE name='org-b'").Scan(&orgB); err != nil {
		t.Fatalf("read org-b id: %v", err)
	}
	mustExec(t, ctx, f.probeDB, fmt.Sprintf(
		"INSERT INTO docs (tenant_id, org_ref_id) VALUES ('real-tenant-1', %d), ('real-tenant-2', %d)", orgA, orgB))
	rel := schema.Relation{Schema: "public", Name: "docs", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the table seeded", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %v, want the one table seeded", seeded)
	}

	// Both canary rows' org_ref_id must be one of the two REAL, already
	// existing parent ids — a dangling or fabricated reference would mean
	// the FK column was synthesised rather than borrowed.
	rows, err := f.probeDB.QueryContext(ctx, fmt.Sprintf(
		"SELECT org_ref_id FROM docs WHERE tenant_id IN ('%s', '%s') ORDER BY org_ref_id", CanaryA, CanaryB))
	if err != nil {
		t.Fatalf("query canary org_ref_id: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, v)
	}
	want := []int{orgA, orgB}
	if orgB < orgA {
		want = []int{orgB, orgA}
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("canary org_ref_id values = %v, want %v (the two real parent ids, borrowed verbatim)", got, want)
	}
}

// --- fixture fallback for tables with nothing to sample ---

// TestSeedCanariesFixtureFallbackForEmptyTable: a freshly migrated table with
// zero rows (coder's own post-migration state, §9.1) has nothing to sample —
// an operator-supplied fixture in the policy file is the named fallback.
func TestSeedCanariesFixtureFallbackForEmptyTable(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "fixturefallback", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE empty_orders (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		total integer NOT NULL)`)
	rel := schema.Relation{
		Schema: "public", Name: "empty_orders", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id",
		Fixture: []map[string]string{
			{"total": "100"},
			{"total": "200"},
		},
	}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want the table seeded via fixture", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %v, want the one table seeded", seeded)
	}

	var totalA, totalB int
	if err := f.probeDB.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT total FROM empty_orders WHERE tenant_id = '%s'", CanaryA)).Scan(&totalA); err != nil {
		t.Fatalf("read canary A total: %v", err)
	}
	if err := f.probeDB.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT total FROM empty_orders WHERE tenant_id = '%s'", CanaryB)).Scan(&totalB); err != nil {
		t.Fatalf("read canary B total: %v", err)
	}
	if totalA != 100 || totalB != 200 {
		t.Errorf("total values = (%d, %d), want (100, 200) from the fixture", totalA, totalB)
	}
}

// TestSeedCanariesNoSampleNoFixtureIsNamedError: an empty table with no
// fixture supplied must be reported with a clean, named reason — never a
// silent skip and never the pre-TGD-BL-29 unconditional synthetic generator
// quietly reappearing to paper over the gap.
func TestSeedCanariesNoSampleNoFixtureIsNamedError(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "noneavailable", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE empty_orders2 (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		total integer NOT NULL)`)
	rel := schema.Relation{Schema: "public", Name: "empty_orders2", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(seeded) != 0 {
		t.Fatalf("seeded = %v, want none — there is nothing to seed from", seeded)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly one skipped table", skipped)
	}
	reason := skipped[0].Reason
	if !strings.Contains(reason, "no rows to sample") || !strings.Contains(reason, "no fixture") {
		t.Errorf("skip reason %q does not name both the empty table and the missing fixture", reason)
	}
	if strings.Contains(reason, "insert:") {
		t.Errorf("skip reason %q looks like a raw driver failure, not a named pre-flight reason", reason)
	}

	// Confirm the table really is empty of anything but the pre-existing 0
	// rows — nothing was silently invented for it.
	var count int
	if err := f.probeDB.QueryRowContext(ctx, "SELECT count(*) FROM empty_orders2").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("empty_orders2 row count = %d, want 0 — nothing should have been inserted", count)
	}
}

// TestSeedCanariesFixtureMissingColumnIsNamedError: a fixture that omits a
// required NOT NULL, no-default column must be reported by name, not
// silently insert NULL and not crash with a raw constraint-violation error.
func TestSeedCanariesFixtureMissingColumnIsNamedError(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "fixturemissing", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE empty_orders3 (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		total integer NOT NULL)`)
	rel := schema.Relation{
		Schema: "public", Name: "empty_orders3", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id",
		Fixture: []map[string]string{
			{}, // missing "total"
			{"total": "200"},
		},
	}

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
	if !strings.Contains(skipped[0].Reason, `"total"`) {
		t.Errorf("skip reason %q does not name the missing column", skipped[0].Reason)
	}
}

// --- TGD-BL-36: seed provenance (sampled vs. fixture) is a distinct
// strength of evidence, not two paths to an identical result ---
//
// A row-level A1/A4 pass on a table seeded by sampling rests on the
// target's own real historical data (only the tenant column rewritten) — it
// says something about the target's actual schema and data. A pass on a
// table seeded from a fixture rests on data an operator hand-wrote in the
// policy file — it proves the oracle mechanism works against a
// constraint-satisfying shape, not that it has been exercised against
// anything coder itself produced. SeedCanaries must report which happened,
// per table, so a caller (and the CLI report, TGD-BL-36) can say so rather
// than reporting a plain "row-level proven" that lumps the two together.

func TestSeedCanaries_ReportsSampledSource(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "provenance_sampled", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE sampled_orders (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL, total integer NOT NULL)`)
	mustExec(t, ctx, f.probeDB, `INSERT INTO sampled_orders (tenant_id, total) VALUES
		('real-tenant-1', 10), ('real-tenant-2', 20)`)
	rel := schema.Relation{Schema: "public", Name: "sampled_orders", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id"}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %+v, want exactly one", seeded)
	}
	if seeded[0].Table != "public.sampled_orders" {
		t.Errorf("seeded[0].Table = %q, want public.sampled_orders", seeded[0].Table)
	}
	if seeded[0].Source != SeedSourceSampled {
		t.Errorf("seeded[0].Source = %q, want %q", seeded[0].Source, SeedSourceSampled)
	}
}

func TestSeedCanaries_ReportsFixtureSource(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "provenance_fixture", nil)

	mustExec(t, ctx, f.probeDB, `CREATE TABLE fixture_orders (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL, total integer NOT NULL)`)
	rel := schema.Relation{
		Schema: "public", Name: "fixture_orders", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id",
		Fixture: []map[string]string{
			{"total": "100"},
			{"total": "200"},
		},
	}

	seeded, skipped, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{rel})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", skipped)
	}
	if len(seeded) != 1 {
		t.Fatalf("seeded = %+v, want exactly one", seeded)
	}
	if seeded[0].Table != "public.fixture_orders" {
		t.Errorf("seeded[0].Table = %q, want public.fixture_orders", seeded[0].Table)
	}
	if seeded[0].Source != SeedSourceFixture {
		t.Errorf("seeded[0].Source = %q, want %q", seeded[0].Source, SeedSourceFixture)
	}
}
