package main

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/lib/pq"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// TestAggregateProof_AllPass, TestAggregateProof_OneTableFailsA1, and
// TestAggregateProof_OneTableFailsA4 are direct, database-free tests of the
// AND-across-tables rule a sweep depends on: a policy naming several scoped
// tables is proven only if every one of them individually passes A1 and A4.
// This is unreachable through the CLI's own end-to-end tests, because
// SeedCanaries and EnableRLS derive their target column from the SAME
// policy-declared TenantColumn for every table (documented in
// TestVerifyCLI_WrongColumnTypeExitsUsageError) — there is no way to make
// one table's A1/A4 genuinely diverge from another's through that surface
// without a database-level defect that this project's design does not
// otherwise construct. Testing the aggregation directly is the only way to
// mutation-test it at all.
func TestAggregateProof_AllPass(t *testing.T) {
	ps := aggregateProof([]tableProof{
		{Table: "public.a", Seeded: true, A1Passed: true, A4Passed: true},
		{Table: "public.b", Seeded: true, A1Passed: true, A4Passed: true},
	})
	if !ps.A1Checked || !ps.A1Passed || !ps.A4Checked || !ps.A4Passed {
		t.Fatalf("got %+v, want all checked and passed", ps)
	}
}

func TestAggregateProof_OneTableFailsA1(t *testing.T) {
	ps := aggregateProof([]tableProof{
		{Table: "public.a", Seeded: true, A1Passed: true, A4Passed: true},
		{Table: "public.b", Seeded: true, A1Passed: false, A4Passed: true},
	})
	if !ps.A1Checked || ps.A1Passed {
		t.Fatalf("got %+v, want A1Passed=false — one bad table must fail the whole sweep", ps)
	}
}

func TestAggregateProof_OneTableFailsA4(t *testing.T) {
	ps := aggregateProof([]tableProof{
		{Table: "public.a", Seeded: true, A1Passed: true, A4Passed: true},
		{Table: "public.b", Seeded: true, A1Passed: true, A4Passed: false},
	})
	if !ps.A4Checked || ps.A4Passed {
		t.Fatalf("got %+v, want A4Passed=false — one bad table must fail the whole sweep", ps)
	}
}

func TestAggregateProof_EmptyReportsUnchecked(t *testing.T) {
	ps := aggregateProof(nil)
	if ps.A1Checked || ps.A4Checked {
		t.Fatalf("got %+v, want both unchecked for an empty sweep", ps)
	}
}

// TestAggregateProof_UnseededTableContributesNothing is TGD-BL-32/§9.6's
// central aggregation rule: a relation that never seeded must not count as
// either a pass (it was never attempted) or a fail (its inability to seed is
// not an A1/A4 defect) — it is simply absent from the aggregate. A sweep
// with one seeded, passing table and one unseeded table must still report
// proven.
func TestAggregateProof_UnseededTableContributesNothing(t *testing.T) {
	ps := aggregateProof([]tableProof{
		{Table: "public.a", Seeded: true, A1Passed: true, A4Passed: true},
		{Table: "public.b", Seeded: false, SkipReason: "no rows to sample"},
	})
	if !ps.A1Checked || !ps.A1Passed || !ps.A4Checked || !ps.A4Passed {
		t.Fatalf("got %+v, want all checked and passed — the unseeded table must not count against the aggregate", ps)
	}
}

// TestAggregateProof_AllUnseededReportsUnchecked: if every table is
// unseeded, A1/A4 never ran anywhere — this must read identically to the
// empty-slice case, not as a vacuous pass.
func TestAggregateProof_AllUnseededReportsUnchecked(t *testing.T) {
	ps := aggregateProof([]tableProof{
		{Table: "public.a", Seeded: false, SkipReason: "no rows to sample"},
		{Table: "public.b", Seeded: false, SkipReason: "no rows to sample"},
	})
	if ps.A1Checked || ps.A4Checked {
		t.Fatalf("got %+v, want both unchecked — nothing seeded, so A1/A4 never ran", ps)
	}
}

// TestRunOracleGate_A2A3RunDespiteOneTableFailingToSeed proves §9.6's
// decoupling directly: EnableRLS/A2/A3 must run for EVERY scoped relation
// even when another one fails to seed — before this change, the whole gate
// aborted right after SeedCanaries, so a single unseedable table (a real,
// common shape — see TGD-BL-29) held full structural coverage of every OTHER
// table hostage, even though A2/A3 have nothing to do with seeded rows at
// all.
//
// TGD-BL-32/§9.6's tiered-proof-depth model (deliberately deferred by
// TGD-BL-30) extends the same decoupling to A1/A4: a table that cannot be
// seeded no longer aborts the whole sweep at all. It is attempted on every
// OTHER scoped relation that DID seed, and the unseedable one is reported as
// structural-only (A2/A3 verified, A1/A4 never attempted for lack of rows) —
// not as a run failure. This test replaces the pre-TGD-BL-32 version, which
// asserted the opposite (that "bad" failing to seed aborted the whole run
// with exit 2 and left A1/A4 entirely unchecked) — that was the exact
// behaviour TGD-BL-30 named as deliberately out of scope and left for this
// change to close.
func TestRunOracleGate_A1A4RunOnSeedableTablesDespiteOneTableFailingToSeed(t *testing.T) {
	admin := adminDSN(t)
	ctx := context.Background()

	db, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer db.Close()
	name := "tgd_gate_a1a4_tiered"
	if _, err := db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)); err != nil {
		t.Fatalf("drop stale target: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("create target: %v", err)
	}
	t.Cleanup(func() { db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)) })

	dsn, err := replaceDatabaseInDSN(admin, name)
	if err != nil {
		t.Fatalf("build target dsn: %v", err)
	}
	target, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	// good: seedable normally, at least 2 rows to sample.
	if _, err := target.ExecContext(ctx, `CREATE TABLE good (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL, amount integer NOT NULL)`); err != nil {
		t.Fatalf("create good: %v", err)
	}
	if _, err := target.ExecContext(ctx, `INSERT INTO good (tenant_id, amount) VALUES ('a', 1), ('b', 2)`); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	// bad: nothing to sample and no fixture — guaranteed to fail seeding.
	if _, err := target.ExecContext(ctx, `CREATE TABLE bad (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL, amount integer NOT NULL)`); err != nil {
		t.Fatalf("create bad: %v", err)
	}
	target.Close()

	scoped := []schema.Relation{
		{Schema: "public", Name: "good", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "bad", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
	}

	result, gateErr := runOracleGate(ctx, dsn, scoped, nil)
	if gateErr != nil {
		t.Fatalf("runOracleGate failed despite one seedable table (%q); "+
			"want tiered success — err: %v", "good", gateErr)
	}
	if err := result.proof.PolicyProven(); err != nil {
		t.Errorf("PolicyProven() = %v, want nil — the seedable table's own A1/A4 proved it", err)
	}
	// A2/A3 were computed for BOTH tables (including "good") despite "bad"
	// failing to seed.
	if !result.proof.A2Checked || !result.proof.A2Passed {
		t.Errorf("proof.A2 = (checked=%v passed=%v), want both true — A2 must run regardless of "+
			"the OTHER table's seeding failure", result.proof.A2Checked, result.proof.A2Passed)
	}
	if !result.proof.A3Checked || !result.proof.A3Passed {
		t.Errorf("proof.A3 = (checked=%v passed=%v), want both true", result.proof.A3Checked, result.proof.A3Passed)
	}
	// A1/A4 ran and passed — on "good" alone. "bad" contributed nothing to
	// the aggregate, in either direction: its inability to seed is not an A1/A4
	// failure and must not fail them, but it also must not count as a pass.
	if !result.proof.A1Checked || !result.proof.A1Passed {
		t.Errorf("proof.A1 = (checked=%v passed=%v), want both true — 'good' seeded and its A1 passed",
			result.proof.A1Checked, result.proof.A1Passed)
	}
	if !result.proof.A4Checked || !result.proof.A4Passed {
		t.Errorf("proof.A4 = (checked=%v passed=%v), want both true — 'good' seeded and its A4 passed",
			result.proof.A4Checked, result.proof.A4Passed)
	}

	var goodSeeded, badSeeded bool
	var badReason string
	for _, tp := range result.tableProof {
		switch tp.Table {
		case "public.good":
			goodSeeded = tp.Seeded
			if !tp.A1Passed || !tp.A4Passed {
				t.Errorf("public.good tableProof = %+v, want A1Passed and A4Passed both true", tp)
			}
		case "public.bad":
			badSeeded = tp.Seeded
			badReason = tp.SkipReason
		default:
			t.Errorf("unexpected table in tableProof: %q", tp.Table)
		}
	}
	if !goodSeeded {
		t.Errorf("public.good not marked Seeded in tableProof")
	}
	if badSeeded {
		t.Errorf("public.bad incorrectly marked Seeded in tableProof")
	}
	if badReason == "" {
		t.Errorf("public.bad has no SkipReason recorded — structural-only tables must name why")
	}
}

// TestRunOracleGate_NoTableSeedsStillAborts is the tiered model's own safety
// rail: if NOT ONE scoped table could be seeded, A1/A4 never ran anywhere,
// and the run must still abort exactly as before — tiering must never turn
// "the oracle was never demonstrated to see anything" into a passing run.
func TestRunOracleGate_NoTableSeedsStillAborts(t *testing.T) {
	admin := adminDSN(t)
	ctx := context.Background()

	db, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer db.Close()
	name := "tgd_gate_none_seed"
	if _, err := db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)); err != nil {
		t.Fatalf("drop stale target: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("create target: %v", err)
	}
	t.Cleanup(func() { db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)) })

	dsn, err := replaceDatabaseInDSN(admin, name)
	if err != nil {
		t.Fatalf("build target dsn: %v", err)
	}
	target, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if _, err := target.ExecContext(ctx, `CREATE TABLE empty1 (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL)`); err != nil {
		t.Fatalf("create empty1: %v", err)
	}
	if _, err := target.ExecContext(ctx, `CREATE TABLE empty2 (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL)`); err != nil {
		t.Fatalf("create empty2: %v", err)
	}
	target.Close()

	scoped := []schema.Relation{
		{Schema: "public", Name: "empty1", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "empty2", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
	}

	result, gateErr := runOracleGate(ctx, dsn, scoped, nil)
	if gateErr == nil {
		t.Fatal("runOracleGate PASSED despite no table seeding at all; want it to abort")
	}
	if result.proof.A1Checked || result.proof.A4Checked {
		t.Errorf("proof.A1/A4 = (%v, %v), want both unchecked — neither ran anywhere",
			result.proof.A1Checked, result.proof.A4Checked)
	}
}
