package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/oracle"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// End-to-end tests: they build the real binary and exec it, exactly as an
// operator would run it. Nothing here calls a Go function from this package
// directly — the point is to prove the wiring between flags, the oracle gate,
// exit codes, and stdout/stderr, not to re-test the oracle package's own logic
// (proven separately in internal/oracle).

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TGD_TEST_DSN")
	if dsn == "" {
		t.Skip("TGD_TEST_DSN not set; the CLI end-to-end path was NOT exercised in this run")
	}
	return dsn
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tenantguard")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build tenantguard binary: %v\n%s", err, out)
	}
	return bin
}

// setupTargetDatabase creates a fresh database with an invoices(tenant_id)
// table, structurally identical to the fixture used throughout internal/oracle,
// and returns a DSN pointing at it. This is the --dsn a real operator would
// supply — the CLI's own gate creates and drops the probe from here.
func setupTargetDatabase(t *testing.T, admin string, name string) string {
	t.Helper()
	db, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)); err != nil {
		t.Fatalf("drop stale target: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("create target: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name))
	})

	dsn, err := replaceDatabaseInDSN(admin, name)
	if err != nil {
		t.Fatalf("build target dsn: %v", err)
	}
	target, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	defer target.Close()
	// status is a second text column, deliberately present so the wrong-column
	// tests below can point a policy's tenant column at a plausible-but-wrong
	// TEXT column. Pointing it at id (bigint) instead would make SeedCanaries
	// fail to insert the string canary into a bigint column at all, silently
	// mark the table skipped, and leave the probe with zero rows — a real,
	// but different and more degenerate, failure than the one these tests
	// intend to exercise (found while writing these tests; see the CLI
	// report).
	if _, err := target.Exec(`CREATE TABLE invoices (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL, status text NOT NULL DEFAULT 'open',
		amount integer NOT NULL)`); err != nil {
		t.Fatalf("create invoices table: %v", err)
	}
	// At least 2 real rows must exist for SeedCanaries's sampling mechanism
	// (TGD-BL-29/§9.6) to have anything to copy — the probe inherits these
	// via CreateProbeDatabase's TEMPLATE copy, same as any real target.
	if _, err := target.Exec(`INSERT INTO invoices (tenant_id, amount) VALUES
		('tgd-fixture-filler-0', 1), ('tgd-fixture-filler-1', 1)`); err != nil {
		t.Fatalf("seed baseline invoices rows: %v", err)
	}
	return dsn
}

// writePolicyFile writes rels as a policy file (the format `tenantguard
// infer` produces) and returns its path — the input verify/audit now
// require, standing in for a human having reviewed and accepted it.
func writePolicyFile(t *testing.T, rels ...schema.Relation) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create policy file: %v", err)
	}
	defer f.Close()
	if err := schema.WritePolicy(f, &schema.Policy{Relations: rels}); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	return path
}

// invoicesScoped is the single-table policy most tests need: public.invoices
// scoped by tenant_id, matching setupTargetDatabase's fixture.
func invoicesScoped(t *testing.T) string {
	t.Helper()
	return writePolicyFile(t, schema.Relation{
		Schema: "public", Name: "invoices", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id",
	})
}

type cliResult struct {
	stdout, stderr string
	exitCode       int
}

func runCLIBinary(t *testing.T, bin string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", bin, err)
		}
		code = ee.ExitCode()
	}
	return cliResult{stdout: so.String(), stderr: se.String(), exitCode: code}
}

// TestVerifyCLI_CleanRunExitsZeroWithReport is the clean-run half of the
// proof required alongside the abort half below: a build where verify can
// never exit zero would be as useless as one that can never fail.
func TestVerifyCLI_CleanRunExitsZeroWithReport(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_clean")
	policy := invoicesScoped(t)

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %q", err, r.stdout)
	}
	if !rep.Proven {
		t.Errorf("report.Proven = false on a clean run")
	}
	if !rep.Checks.A1 || !rep.Checks.A2 || !rep.Checks.A3 || !rep.Checks.A4 {
		t.Errorf("not all four checks reported passed: %+v", rep.Checks)
	}
	if len(rep.Tables) != 1 || rep.Tables[0] != "public.invoices" {
		t.Errorf("report.Tables = %v, want [public.invoices]", rep.Tables)
	}
	if rep.Tier != 1 {
		t.Errorf("report.Tier = %d, want 1", rep.Tier)
	}
	if rep.ProofSource != "probe" {
		t.Errorf("report.ProofSource = %q, want %q", rep.ProofSource, "probe")
	}
}

// TestVerifyCLI_WrongColumnTypeExitsUsageError drives an abort through the
// real CLI path, not by calling Go functions.
//
// This does NOT reproduce M3's shape, and that gap is itself the finding:
// SeedCanaries and EnableRLS both derive their target column from the SAME
// policy-declared tenant column, so whichever column is named becomes, by
// construction, a valid discriminator (the two canaries get written into it).
// A "wrong but plausible text column" behaves identically to a correct one
// from every self-check's point of view — verified directly: pointing the
// policy's tenant column at a second real text column produced a clean,
// PROVEN exit 0, not an abort, because SeedCanaries happily wrote the two
// distinct canaries into that column too. M3's defect (seed into the correct
// column, synthesise against a different, wrong one) requires a mismatch
// this CLI's policy-file surface cannot express; reaching it needs the
// oracle functions called directly with deliberately inconsistent relations,
// exactly as internal/oracle's own tests do.
//
// What this CLI surface CAN reach is a different, equally real defect: naming
// a non-text column (id, the bigint primary key). SeedCanaries cannot insert
// the string canary literal into it, silently marks the table skipped (its
// own contract — a skipped table is reported, never treated as passing — see
// probe.go), and the probe ends up with zero rows in the target table at all.
// A1 then correctly reports it cannot demonstrate withholding on an empty
// table (0 unrestricted, 0 restricted — not strictly fewer) and aborts with
// its own mapped code. This is a genuine, CLI-reachable operator mistake —
// naming the wrong column entirely — distinct from any of M1–M8, which all
// assume a semantically valid tenant column and test POLICY defects, not
// input misconfiguration.
func TestVerifyCLI_WrongColumnTypeExitsUsageError(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_wrongcol")
	policy := writePolicyFile(t, schema.Relation{
		Schema: "public", Name: "invoices", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "id",
	})

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error — seeding was skipped, no oracle "+
			"check ever ran). stderr:\n%s", r.exitCode, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout is not empty on an aborted run: %q — a finding must never be "+
			"printed from a policy that was not proven, in any output format", r.stdout)
	}
	if !strings.Contains(r.stderr, "seed") || strings.Contains(r.stderr, "oracle is blind") {
		t.Errorf("stderr does not read as a seeding/input problem, or still carries the old "+
			"oracle-shaped message: %q", r.stderr)
	}
}

// TestVerifyCLI_UsageErrorExitsTwo covers the flag-validation path.
func TestVerifyCLI_UsageErrorExitsTwo(t *testing.T) {
	bin := buildBinary(t)
	r := runCLIBinary(t, bin, "verify", "--dsn", "postgres://x/y")
	if r.exitCode != 2 {
		t.Errorf("exit code = %d, want 2 (usage error) for missing required flags", r.exitCode)
	}
	if r.stdout != "" {
		t.Errorf("stdout not empty on a usage error: %q", r.stdout)
	}
}

// TestAuditCLI_CleanRunNotesUnimplementedDiffer: audit shares the identical
// gate as verify, and must say plainly when no --events file was given
// rather than silently pretending to have audited anything.
func TestAuditCLI_CleanRunNotesUnimplementedDiffer(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_audit_clean")
	policy := invoicesScoped(t)

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v", err)
	}
	if !rep.Proven {
		t.Errorf("report.Proven = false on a clean audit run")
	}
	if r.stderr == "" {
		t.Errorf("audit's stderr must note that no --events file was given, " +
			"and it is empty")
	}
}

// writeEventsFile encodes evs as JSONL (Recorder's exact wire format) to a
// temp file and returns its path, for --events tests.
func writeEventsFile(t *testing.T, evs []capture.Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create events file: %v", err)
	}
	defer f.Close()
	rec := capture.NewRecorder(f)
	for _, e := range evs {
		rec.Emit(e)
	}
	return path
}

// TestAuditCLI_WithEventsProducesPerQueryVerdicts is the end-to-end proof
// that TGD-US-06's differ is actually wired into the CLI, not just present in
// internal/differ: a real captured-events file, fed through the real binary,
// against a real proven policy, must come back with SAFE, LEAK and
// UNATTRIBUTABLE verdicts that match what the query shapes deserve — declared
// here BEFORE the binary runs, per the standing rule that a corpus written
// after seeing output proves nothing.
//
// The gate's own SeedCanaries call is what puts exactly two rows into the
// probe (tenant_id = oracle.CanaryA and oracle.CanaryB) — the events below
// are written against those two known values, not arbitrary data this test
// invented.
func TestAuditCLI_WithEventsProducesPerQueryVerdicts(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_audit_events")
	policy := invoicesScoped(t)

	events := writeEventsFile(t, []capture.Event{
		{
			// Correctly scoped: unrestricted and restricted-as-CanaryA both
			// see exactly the one CanaryA row. SAFE.
			Kind: capture.KindBind, Resolved: true,
			SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
			Params: []capture.Param{{Known: true, Text: oracle.CanaryA}},
		},
		{
			// No predicate at all: unrestricted sees both canary rows,
			// restricted (no tenant claimed) sees none under RLS. LEAK.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM invoices",
		},
		{
			// No declared scoped table referenced by name. UNATTRIBUTABLE.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT now()",
		},
	})

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--events", events)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}

	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if !rep.Proven {
		t.Fatalf("report.Proven = false; gate must have passed for this test to mean anything")
	}
	if len(rep.Queries) != 3 {
		t.Fatalf("got %d query verdicts, want 3: %+v", len(rep.Queries), rep.Queries)
	}

	want := []string{"SAFE", "LEAK", "UNATTRIBUTABLE"}
	for i, w := range want {
		if rep.Queries[i].Verdict != w {
			t.Errorf("query %d (%s): verdict = %s, want %s (reason: %s)",
				i, rep.Queries[i].SQL, rep.Queries[i].Verdict, w, rep.Queries[i].Reason)
		}
	}
	if rep.Queries[1].WithheldRows != 4 {
		t.Errorf("LEAK query: WithheldRows = %d, want 4 (2 filler + 2 canary rows, all withheld "+
			"since the restricted role claims no tenant at all)",
			rep.Queries[1].WithheldRows)
	}
	if len(rep.Queries[0].Params) != 1 || rep.Queries[0].Params[0] != oracle.CanaryA {
		t.Errorf("SAFE query: Params = %v, want [%s] (TGD-US-10 AC-1: full bound parameter list)",
			rep.Queries[0].Params, oracle.CanaryA)
	}
	if rep.Counts == nil {
		t.Fatalf("report.Counts is nil; want it set whenever queries were audited")
	}
	if rep.Counts.Safe != 1 || rep.Counts.Leak != 1 || rep.Counts.Unattributable != 1 {
		t.Errorf("report.Counts = %+v, want {Safe:1 Leak:1 Unattributable:1}", *rep.Counts)
	}
	if rep.Counts.Safe+rep.Counts.Leak+rep.Counts.Unattributable != len(rep.Queries) {
		t.Errorf("counts do not sum to the query total: counts=%+v total=%d", *rep.Counts, len(rep.Queries))
	}
}

// TestAuditCLI_StructuralOnlyTableNeverProducesSafeOrLeak is
// TGD-BL-32/§9.6's central safety property: a query attributed to a
// structural-only table (A2/A3 proved, but it never seeded so A1/A4 never
// ran) must never come back SAFE or LEAK, no matter how correctly-shaped its
// predicate looks — reporting SAFE for a table the oracle was never shown
// could withhold anything would reproduce exactly the "reported clean
// because the oracle was blind" failure A1 exists to rule out (design doc
// §9.6). The policy below proves 'invoices' at the row level and leaves
// 'projects' structural-only (its tenant column, "id", cannot hold the
// canary literal — same shape as TestVerifyCLI_OneUnseedableTableDegradesToStructuralOnly);
// the captured query below is a real, correctly-parameterized SELECT against
// 'projects' that WOULD be SAFE if the table had been proven.
func TestAuditCLI_StructuralOnlyTableNeverProducesSafeOrLeak(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_structural_safety")

	db, err := sql.Open("postgres", target)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE projects (
		id bigserial PRIMARY KEY, org_id text NOT NULL, name text NOT NULL)`); err != nil {
		t.Fatalf("create projects table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (org_id, name) VALUES ('acme', 'widgets')`); err != nil {
		t.Fatalf("seed projects row: %v", err)
	}
	db.Close()

	policy := writePolicyFile(t,
		schema.Relation{Schema: "public", Name: "invoices", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		// projects' declared tenant column is "id" (bigint) — cannot hold the
		// canary string literal, so SeedCanaries can never seed it.
		schema.Relation{Schema: "public", Name: "projects", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "id"},
	)

	events := writeEventsFile(t, []capture.Event{
		{
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM projects WHERE org_id = 'acme'",
		},
	})

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--events", events)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 — 'invoices' alone proves the policy. stderr:\n%s", r.exitCode, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if !rep.Proven {
		t.Fatalf("report.Proven = false; gate must have passed for this test to mean anything")
	}
	if len(rep.TablesStructuralOnly) != 1 || rep.TablesStructuralOnly[0].Table != "public.projects" {
		t.Fatalf("report.TablesStructuralOnly = %+v, want exactly one entry for public.projects", rep.TablesStructuralOnly)
	}
	if len(rep.Queries) != 1 {
		t.Fatalf("got %d query verdicts, want 1: %+v", len(rep.Queries), rep.Queries)
	}
	if rep.Queries[0].Verdict != "UNATTRIBUTABLE" {
		t.Errorf("verdict = %s, want UNATTRIBUTABLE — public.projects was never proven at the row "+
			"level and must never be attributed SAFE or LEAK, however correctly-shaped its predicate "+
			"looks (reason: %s)", rep.Queries[0].Verdict, rep.Queries[0].Reason)
	}
	if rep.Queries[0].Population != "structural_only" {
		t.Errorf("population = %q, want %q (TGD-BL-33/TGD-BL-06)", rep.Queries[0].Population, "structural_only")
	}
	if rep.Counts.Population.StructuralOnly != 1 {
		t.Errorf("counts.population.structural_only = %d, want 1", rep.Counts.Population.StructuralOnly)
	}
}

// TestAuditCLI_UnattributableRateByDenominator is TGD-BL-33/TGD-BL-06's
// baselining prerequisite exercised end-to-end: one query lands in each of
// the four Population buckets (plus 50 SAFE and one LEAK, all on the
// row-level table, so the "touching a declared table" denominators have a
// non-trivial attributed count too — and the row_level_touching_real_app_sql
// rate, 1/52 ≈ 0.0192, stays under the baselined ceiling (18/762 ≈ 0.0236,
// TGD-BL-35 fix, SRS §7.18/§7.19 — 50 SAFE events, not 4, specifically
// because the ceiling ratcheted down here and 1/6 ≈ 0.1667 no longer
// clears it), so this test exercises a clean exit-0 report rather than the
// ceiling itself, which
// TestAuditCLI_UnattributableCeilingFailsAboveIt/PassesAtOrBelowIt cover
// directly), and the report's own UnattributableRateByDenominator must
// reproduce the same four numbers a reader would otherwise have to
// re-derive by hand — this is the entire point: the next run is comparable
// to this one without re-running an ad hoc script.
func TestAuditCLI_UnattributableRateByDenominator(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_rate_breakdown")

	db, err := sql.Open("postgres", target)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE projects (
		id bigserial PRIMARY KEY, org_id text NOT NULL, name text NOT NULL)`); err != nil {
		t.Fatalf("create projects table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (org_id, name) VALUES ('acme', 'widgets')`); err != nil {
		t.Fatalf("seed projects row: %v", err)
	}
	db.Close()

	policy := writePolicyFile(t,
		schema.Relation{Schema: "public", Name: "invoices", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		// projects' declared tenant column is "id" (bigint) — cannot hold the
		// canary literal, so it stays structural-only.
		schema.Relation{Schema: "public", Name: "projects", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "id"},
	)

	const numSafe = 50          // enough that 1 unattributed row-level query stays under the 18/762 ceiling (TGD-BL-35 fix, SRS §7.18/§7.19).
	safeEvent := capture.Event{ // SAFE: correctly scoped on the row-level table.
		Kind: capture.KindBind, Resolved: true,
		SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
		Params: []capture.Param{{Known: true, Text: oracle.CanaryA}},
	}
	eventList := make([]capture.Event, 0, numSafe+5)
	for i := 0; i < numSafe; i++ {
		eventList = append(eventList, safeEvent)
	}
	eventList = append(eventList,
		capture.Event{ // LEAK: no predicate at all, on the row-level table.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM invoices",
		},
		capture.Event{ // no_declared_table: touches nothing declared.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT now()",
		},
		capture.Event{ // structural_only: touches only the unseedable table.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM projects WHERE org_id = 'acme'",
		},
		capture.Event{ // non_query: cursor-protocol/session bookkeeping.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "BEGIN READ WRITE",
		},
		capture.Event{ // row_level_unattributed: touches the row-level table, but the
			// tenant is subquery-computed — a genuine attribution failure.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM invoices WHERE tenant_id = (SELECT 'x')",
		},
	)
	events := writeEventsFile(t, eventList)

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--events", events)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if len(rep.Queries) != numSafe+5 {
		t.Fatalf("got %d query verdicts, want %d: %+v", len(rep.Queries), numSafe+5, rep.Queries)
	}

	wantVerdicts := make([]string, 0, numSafe+5)
	wantPopulations := make([]string, 0, numSafe+5)
	for i := 0; i < numSafe; i++ {
		wantVerdicts = append(wantVerdicts, "SAFE")
		wantPopulations = append(wantPopulations, "")
	}
	wantVerdicts = append(wantVerdicts, "LEAK", "UNATTRIBUTABLE", "UNATTRIBUTABLE", "UNATTRIBUTABLE", "UNATTRIBUTABLE")
	wantPopulations = append(wantPopulations, "", "no_declared_table", "structural_only", "non_query", "row_level_unattributed")
	for i := range rep.Queries {
		if rep.Queries[i].Verdict != wantVerdicts[i] {
			t.Errorf("query %d: verdict = %s, want %s (sql=%q reason=%q)",
				i, rep.Queries[i].Verdict, wantVerdicts[i], rep.Queries[i].SQL, rep.Queries[i].Reason)
		}
		if rep.Queries[i].Population != wantPopulations[i] {
			t.Errorf("query %d: population = %q, want %q", i, rep.Queries[i].Population, wantPopulations[i])
		}
	}

	if rep.Counts == nil {
		t.Fatalf("report.Counts is nil")
	}
	wantPC := populationCounts{NoDeclaredTable: 1, StructuralOnly: 1, RowLevelUnattributed: 1, NonQuery: 1}
	if rep.Counts.Population != wantPC {
		t.Fatalf("counts.population = %+v, want %+v", rep.Counts.Population, wantPC)
	}
	sum := rep.Counts.Population.NoDeclaredTable + rep.Counts.Population.StructuralOnly +
		rep.Counts.Population.RowLevelUnattributed + rep.Counts.Population.NonQuery
	if sum != rep.Counts.Unattributable {
		t.Errorf("population counts sum to %d, want %d (Counts.Unattributable)", sum, rep.Counts.Unattributable)
	}

	byLabel := map[string]unattributableRateEntry{}
	for _, e := range rep.UnattributableRateByDenominator {
		byLabel[e.Label] = e
	}
	wantEntries := map[string]unattributableRateEntry{
		"all_captured_queries":                     {Denominator: numSafe + 5, Unattributable: 4},
		"real_app_sql_any_table":                   {Denominator: numSafe + 4, Unattributable: 3},
		"real_app_sql_touching_any_declared_table": {Denominator: numSafe + 3, Unattributable: 2},
		"row_level_touching_real_app_sql":          {Denominator: numSafe + 2, Unattributable: 1},
	}
	if len(byLabel) != len(wantEntries) {
		t.Fatalf("got %d denominator entries, want %d: %+v", len(byLabel), len(wantEntries), rep.UnattributableRateByDenominator)
	}
	for label, want := range wantEntries {
		got, ok := byLabel[label]
		if !ok {
			t.Errorf("missing denominator entry %q", label)
			continue
		}
		if got.Denominator != want.Denominator || got.Unattributable != want.Unattributable {
			t.Errorf("%s: denominator=%d unattributable=%d, want denominator=%d unattributable=%d",
				label, got.Denominator, got.Unattributable, want.Denominator, want.Unattributable)
			continue
		}
		wantRate := float64(want.Unattributable) / float64(want.Denominator)
		if got.Rate != wantRate {
			t.Errorf("%s: rate = %v, want %v", label, got.Rate, wantRate)
		}
	}
}

// TestAuditCLI_UnattributableCeilingFailsAboveIt is TGD-NFR-03/TGD-FR-08/
// TGD-BL-06's own proof the gate can fire, driven end-to-end through the
// built binary: two row_level_unattributed queries against one SAFE one
// (row_level_touching_real_app_sql rate 2/3 ≈ 0.667) is well above the
// baselined ceiling (18/762 ≈ 0.0236, TGD-BL-35 fix, SRS §7.18/§7.19).
// The full report must still reach stdout
// (TGD-US-07 AC-1: a ceiling breach is a completed, proven run, not an
// oracle self-check abort) — only the exit code and a distinct stderr
// message change.
func TestAuditCLI_UnattributableCeilingFailsAboveIt(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_ceiling_fail")
	policy := invoicesScoped(t)

	events := writeEventsFile(t, []capture.Event{
		{Kind: capture.KindBind, Resolved: true,
			SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
			Params: []capture.Param{{Known: true, Text: oracle.CanaryA}}},
		{Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM invoices WHERE tenant_id = (SELECT 'x')"},
		{Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM invoices WHERE tenant_id = (SELECT 'y')"},
	})

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--events", events)
	if r.exitCode != ExitUnattributableCeilingExceeded {
		t.Fatalf("exit code = %d, want %d (ExitUnattributableCeilingExceeded). stderr:\n%s",
			r.exitCode, ExitUnattributableCeilingExceeded, r.stderr)
	}
	if !strings.Contains(r.stderr, unattributableCeilingDenominator) {
		t.Errorf("stderr does not name the denominator %q: %q", unattributableCeilingDenominator, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON on a ceiling breach — the completed run's real report "+
			"must still be reported (TGD-US-07 AC-1): %v\nstdout: %s", err, r.stdout)
	}
	if len(rep.Queries) != 3 {
		t.Fatalf("report.Queries has %d entries, want 3 — the full report, not a suppressed one", len(rep.Queries))
	}
}

// TestAuditCLI_UnattributableCeilingPassesAtOrBelowIt is the companion proof
// that the gate does not fire on a healthy rate: one row_level_unattributed
// query against 50 SAFE ones (rate 1/51 ≈ 0.0196) sits under the baselined
// ceiling (18/762 ≈ 0.0236, TGD-BL-35 fix, SRS §7.18/§7.19). 50, not 5, is
// deliberate: the previous baseline (122/378 ≈ 0.32275) tolerated 1/6 ≈
// 0.1667, but this lower, tool-improvement-driven ceiling does not.
func TestAuditCLI_UnattributableCeilingPassesAtOrBelowIt(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_ceiling_pass")
	policy := invoicesScoped(t)

	const numSafe = 50
	safeEvent := capture.Event{Kind: capture.KindBind, Resolved: true,
		SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
		Params: []capture.Param{{Known: true, Text: oracle.CanaryA}}}
	eventList := make([]capture.Event, 0, numSafe+1)
	for i := 0; i < numSafe; i++ {
		eventList = append(eventList, safeEvent)
	}
	eventList = append(eventList, capture.Event{Kind: capture.KindQuery, Resolved: true,
		SQL: "SELECT * FROM invoices WHERE tenant_id = (SELECT 'x')"})
	events := writeEventsFile(t, eventList)

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--events", events)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 — 1/%d is under the baselined ceiling. stderr:\n%s", r.exitCode, numSafe+1, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if len(rep.Queries) != numSafe+1 {
		t.Fatalf("report.Queries has %d entries, want %d", len(rep.Queries), numSafe+1)
	}
}

// TestAuditCLI_VacuousSafeExcludedFromAttributedDenominator is TGD-BL-39
// exercised end-to-end: a vacuous SAFE (both legs matched zero rows) must be
// reported with vacuous=true and must NOT count toward the "attributed"
// population the ceiling-relevant denominators rest on. One real SAFE, one
// vacuous SAFE, and one row_level_unattributed query: if the vacuous SAFE
// were (wrongly) still counted as attributed, row_level_touching_real_app_sql
// would be denominator=3, rate=1/3≈0.333; excluding it correctly gives
// denominator=2, rate=1/2=0.5 — a materially different, HIGHER number, which
// is the whole point: counting a vacuous pass as real evidence understates
// the true unattributable rate.
func TestAuditCLI_VacuousSafeExcludedFromAttributedDenominator(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_vacuous_denom")
	policy := invoicesScoped(t)

	events := writeEventsFile(t, []capture.Event{
		{ // real SAFE: matches the real canary row.
			Kind: capture.KindBind, Resolved: true,
			SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
			Params: []capture.Param{{Known: true, Text: oracle.CanaryA}},
		},
		{ // vacuous SAFE: matches nothing on either leg.
			Kind: capture.KindBind, Resolved: true,
			SQL:    "SELECT * FROM invoices WHERE tenant_id = $1",
			Params: []capture.Param{{Known: true, Text: "nonexistent-tenant-xyz"}},
		},
		{ // row_level_unattributed: genuine attribution failure.
			Kind: capture.KindQuery, Resolved: true,
			SQL: "SELECT * FROM invoices WHERE tenant_id = (SELECT 'x')",
		},
	})

	// The corrected rate (1/2 = 0.5) exceeds the baselined ceiling (18/762 ≈ 0.0236, TGD-BL-35 fix)
	// — a real, useful confirmation this scenario's numbers are what they
	// claim: the report is still fully written (TGD-US-07 AC-1), only the
	// exit code changes.
	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--events", events)
	if r.exitCode != ExitUnattributableCeilingExceeded {
		t.Fatalf("exit code = %d, want %d (0.5 exceeds the baselined ceiling). stderr:\n%s",
			r.exitCode, ExitUnattributableCeilingExceeded, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if len(rep.Queries) != 3 {
		t.Fatalf("got %d query verdicts, want 3: %+v", len(rep.Queries), rep.Queries)
	}
	if rep.Queries[0].Verdict != "SAFE" || rep.Queries[0].Vacuous {
		t.Errorf("query 0 = %+v, want SAFE, vacuous=false (real match)", rep.Queries[0])
	}
	if rep.Queries[1].Verdict != "SAFE" || !rep.Queries[1].Vacuous {
		t.Errorf("query 1 = %+v, want SAFE, vacuous=true (matched nothing)", rep.Queries[1])
	}

	if rep.Counts.Safe != 2 {
		t.Errorf("counts.safe = %d, want 2", rep.Counts.Safe)
	}
	if rep.Counts.VacuousSafe != 1 {
		t.Errorf("counts.vacuous_safe = %d, want 1", rep.Counts.VacuousSafe)
	}

	var got *unattributableRateEntry
	for _, e := range rep.UnattributableRateByDenominator {
		if e.Label == "row_level_touching_real_app_sql" {
			e := e
			got = &e
		}
	}
	if got == nil {
		t.Fatalf("no row_level_touching_real_app_sql entry: %+v", rep.UnattributableRateByDenominator)
	}
	if got.Denominator != 2 || got.Unattributable != 1 {
		t.Fatalf("row_level_touching_real_app_sql = %+v, want denominator=2 unattributable=1 "+
			"(the vacuous SAFE must not inflate the denominator)", *got)
	}
	if got.Rate != 0.5 {
		t.Errorf("rate = %v, want 0.5", got.Rate)
	}
}

// TestVerifyCLI_SharedDatabaseHeaderDefaultsFalse is TGD-FR-18's header,
// checked on the command that never audits anything: the field must appear
// explicitly as false, not be silently absent, so a reader of the JSON can
// tell "known not shared" from "the tool never asked."
func TestVerifyCLI_SharedDatabaseHeaderDefaultsFalse(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_shared_default")
	policy := invoicesScoped(t)

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stdout, `"shared_database":false`) {
		t.Fatalf("stdout must state shared_database:false explicitly, got: %s", r.stdout)
	}
}

// TestAuditCLI_SharedDatabaseFlagIsRecordedVerbatim: TGD-FR-18's header is an
// operator declaration, not automatic detection — the CLI has no way to
// observe from a single --dsn invocation whether the target's OWN test
// harness reuses one database across tests. --shared-database is that
// declaration, and the report must echo it back unchanged.
func TestAuditCLI_SharedDatabaseFlagIsRecordedVerbatim(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_shared_true")
	policy := invoicesScoped(t)

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--shared-database")
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stdout, `"shared_database":true`) {
		t.Fatalf("stdout must state shared_database:true when the flag was given, got: %s", r.stdout)
	}
}

// Renamed from TestAuditCLI_UnattributableRateIsReportedNeverEnforced:
// TGD-BL-06 has since baselined and enforced TGD-NFR-03's ceiling
// (checkUnattributableCeiling, tested directly in ceiling_test.go and
// TestAuditCLI_UnattributableCeilingFailsAboveIt/PassesAtOrBelowIt) — the
// old name and its "the ceiling must not fail a run" comment are no longer
// true in general and would read as a stale claim if left as-is. This
// scenario still exits 0, but for a narrower, still-real reason: both
// events touch no declared table at all ("SELECT now()", and an unresolved
// Bind whose empty SQL text matches nothing), so
// row_level_touching_real_app_sql — the denominator the ceiling is
// baselined against — has zero queries in it. checkUnattributableCeiling
// finds no matching entry and passes: a metric a run never measured must
// never block it (TestCheckUnattributableCeiling_NoMatchingEntryPasses
// proves this directly; this test proves the same property reached through
// a real captured scenario).
func TestAuditCLI_UnattributableRateReportedEvenWhenCeilingHasNothingToCheck(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_unattr_rate")
	policy := invoicesScoped(t)

	events := writeEventsFile(t, []capture.Event{
		{Kind: capture.KindQuery, Resolved: true, SQL: "SELECT now()"},           // Unattributable
		{Kind: capture.KindBind, Resolved: false, Note: "unresolved on purpose"}, // Unattributable
	})

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy, "--events", events)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 — neither query touches a row-level-proven table, "+
			"so the baselined ceiling has nothing to check here. stderr:\n%s", r.exitCode, r.stderr)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &raw); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	rate, ok := raw["unattributable_rate"].(float64)
	if !ok {
		t.Fatalf("report has no numeric unattributable_rate field: %v", raw)
	}
	if rate != 1.0 {
		t.Errorf("unattributable_rate = %v, want 1.0 (both queries were unattributable)", rate)
	}
	counts, ok := raw["counts"].(map[string]any)
	if !ok {
		t.Fatalf("report has no counts object: %v", raw)
	}
	if counts["unattributable"].(float64) != 2 {
		t.Errorf("counts.unattributable = %v, want 2", counts["unattributable"])
	}
}

// TestAuditCLI_AbortBehavesIdenticallyToVerify is the direct AC-5 assertion:
// no findings report in any output format when the policy is not proven —
// checked here against a real invocation of the binary, not a Go function
// call, and specifically on the `audit` path, which is the one AC-5 names.
func TestAuditCLI_AbortBehavesIdenticallyToVerify(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_audit_abort")
	policy := writePolicyFile(t, schema.Relation{
		Schema: "public", Name: "invoices", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "id",
	})

	r := runCLIBinary(t, bin, "audit", "--dsn", target, "--policy", policy)
	if r.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error — seeding skipped)", r.exitCode)
	}
	if r.stdout != "" {
		t.Errorf("AC-5 violated: audit printed to stdout on an aborted run: %q", r.stdout)
	}
}

// TestVerifyCLI_ProbeDatabaseIsAlwaysDropped confirms the CLI's own cleanup
// promise: the probe it creates does not survive the process, on either the
// success or the failure path.
func TestVerifyCLI_ProbeDatabaseIsAlwaysDropped(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_dropcheck")
	goodPolicy := invoicesScoped(t)
	badPolicy := writePolicyFile(t, schema.Relation{
		Schema: "public", Name: "invoices", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "id",
	})

	countProbes := func() int {
		db, err := sql.Open("postgres", admin)
		if err != nil {
			t.Fatalf("open admin: %v", err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(
			"SELECT count(*) FROM pg_database WHERE datname LIKE 'tgd_probe_%'").Scan(&n); err != nil {
			t.Fatalf("count probes: %v", err)
		}
		return n
	}

	before := countProbes()
	// Success path.
	runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", goodPolicy)
	// Failure path.
	runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", badPolicy)
	after := countProbes()

	if after != before {
		t.Errorf("probe database count changed %d -> %d; a probe leaked on one of the two runs", before, after)
	}
}

// TestVerifyCLI_NonexistentTableExitsNonzero: TGD-US-01 AC-2's protective
// intent (fail loudly naming the bad table), driven through a policy file
// naming a table that does not exist. EnableRLS's own DDL against a
// nonexistent relation errors, and that error must reach the operator as a
// non-zero exit with nothing on stdout, not a silent or ambiguous outcome.
func TestVerifyCLI_NonexistentTableExitsNonzero(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_notable")
	policy := writePolicyFile(t, schema.Relation{
		Schema: "public", Name: "does_not_exist", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "tenant_id",
	})

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode == 0 {
		t.Fatalf("exit code = 0 for a nonexistent table; want non-zero. stdout: %q", r.stdout)
	}
	if r.stdout != "" {
		t.Errorf("stdout not empty for a failed run: %q", r.stdout)
	}
	if r.stderr == "" {
		t.Errorf("stderr empty; the operator gets no indication which table was wrong")
	}
}

// TestVerifyCLI_NonexistentTenantColumnExitsNonzero: the AC-3 analogue —
// naming a column that does not exist on the table at all (distinct from
// TestVerifyCLI_WrongColumnTypeExitsUsageError's id, which exists but is the
// wrong type). CREATE POLICY against a nonexistent column errors at the
// database level, and that must surface as a non-zero exit with no stdout.
func TestVerifyCLI_NonexistentTenantColumnExitsNonzero(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_nocolumn")
	policy := writePolicyFile(t, schema.Relation{
		Schema: "public", Name: "invoices", Kind: "BASE TABLE",
		Class: schema.Scoped, TenantColumn: "does_not_exist",
	})

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode == 0 {
		t.Fatalf("exit code = 0 for a nonexistent tenant column; want non-zero. stdout: %q", r.stdout)
	}
	if r.stdout != "" {
		t.Errorf("stdout not empty for a failed run: %q", r.stdout)
	}
}

// TestVerifyCLI_MissingPolicyFileExitsUsageError: --policy naming a file
// that does not exist must fail as a usage error before any probe database
// is created — TGD-US-09 AC-2's refusal to infer on the fly starts here.
func TestVerifyCLI_MissingPolicyFileExitsUsageError(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_nopolicy")

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy",
		filepath.Join(t.TempDir(), "does-not-exist.json"))
	if r.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error). stderr: %q", r.exitCode, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout not empty: %q", r.stdout)
	}
}

// TestVerifyCLI_PolicyWithNoScopedRelationsExitsUsageError: a policy file
// that exists and parses but names nothing Scoped (e.g. every relation came
// back Unclassifiable) has nothing for the gate to sweep — a usage error,
// not a silent zero-table "pass".
func TestVerifyCLI_PolicyWithNoScopedRelationsExitsUsageError(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_noscoped")
	policy := writePolicyFile(t, schema.Relation{
		Schema: "public", Name: "invoices", Kind: "BASE TABLE",
		Class: schema.Unscoped,
	})

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error — no scoped relations). stderr: %q", r.exitCode, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout not empty: %q", r.stdout)
	}
}

// TestVerifyCLI_SweepsEveryScopedTableInOnePolicy is TGD-US-09's whole point
// at the CLI: a policy naming MULTIPLE scoped tables is proven in a single
// run, and the report lists all of them — not one hand-picked pair.
func TestVerifyCLI_SweepsEveryScopedTableInOnePolicy(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_multitable")

	db, err := sql.Open("postgres", target)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE projects (
		id bigserial PRIMARY KEY, org_id text NOT NULL, name text NOT NULL)`); err != nil {
		t.Fatalf("create projects table: %v", err)
	}
	// At least 2 real rows must exist for SeedCanaries's sampling mechanism
	// (TGD-BL-29/§9.6) to have anything to copy.
	if _, err := db.Exec(`INSERT INTO projects (org_id, name) VALUES
		('tgd-fixture-filler-0', 'a'), ('tgd-fixture-filler-1', 'b')`); err != nil {
		t.Fatalf("seed baseline projects rows: %v", err)
	}
	db.Close() // must not hold a connection open — CreateProbeDatabase's

	policy := writePolicyFile(t,
		schema.Relation{Schema: "public", Name: "invoices", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		schema.Relation{Schema: "public", Name: "projects", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "org_id"},
	)

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if len(rep.Tables) != 2 {
		t.Fatalf("report.Tables = %v, want 2 entries", rep.Tables)
	}
	want := map[string]bool{"public.invoices": true, "public.projects": true}
	for _, tbl := range rep.Tables {
		if !want[tbl] {
			t.Errorf("unexpected table in report: %q", tbl)
		}
		delete(want, tbl)
	}
	if len(want) != 0 {
		t.Errorf("missing tables from report: %v", want)
	}
}

// TestVerifyCLI_OneUnseedableTableDegradesToStructuralOnly is
// TGD-BL-32/§9.6's tiered proof-depth model, exercised end-to-end through
// the built binary. It replaces the pre-TGD-BL-32 version of this test
// (TestVerifyCLI_OneBadTableAbortsTheWholeSweep), which asserted the
// opposite: that one table's seeding gap (here, a tenant column that cannot
// hold the canary literal) aborted the WHOLE sweep with empty stdout. That
// was the exact "one table's seeding gap holds every other table's
// row-level proof hostage" problem TGD-BL-30 already solved for A2/A3 and
// explicitly deferred for A1/A4 — this closes it: the good table is proven
// at the row level; the bad one is reported structural-only, by name, with
// its reason, never silently dropped and never conflated with a passing
// table.
func TestVerifyCLI_OneUnseedableTableDegradesToStructuralOnly(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_tiered")

	db, err := sql.Open("postgres", target)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE projects (
		id bigserial PRIMARY KEY, org_id text NOT NULL, name text NOT NULL)`); err != nil {
		t.Fatalf("create projects table: %v", err)
	}
	db.Close()

	policy := writePolicyFile(t,
		schema.Relation{Schema: "public", Name: "invoices", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		// projects' tenant column names "id" (bigint) — cannot hold the
		// canary string literal, so it can never seed. See setupTargetDatabase's
		// comment on why this specific misconfiguration is used.
		schema.Relation{Schema: "public", Name: "projects", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "id"},
	)

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 — 'invoices' seeded and proved at the row level; "+
			"'projects' failing to seed must degrade it, not abort the run. stderr:\n%s", r.exitCode, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if !rep.Proven {
		t.Errorf("report.Proven = false, want true")
	}
	if len(rep.TablesRowLevel) != 1 || rep.TablesRowLevel[0].Table != "public.invoices" {
		t.Errorf("report.TablesRowLevel = %+v, want exactly [public.invoices]", rep.TablesRowLevel)
	}
	if rep.TablesRowLevel[0].SeedSource != "sampled" {
		t.Errorf("report.TablesRowLevel[0].SeedSource = %q, want %q (setupTargetDatabase seeds 2 real rows)",
			rep.TablesRowLevel[0].SeedSource, "sampled")
	}
	if len(rep.TablesStructuralOnly) != 1 || rep.TablesStructuralOnly[0].Table != "public.projects" {
		t.Fatalf("report.TablesStructuralOnly = %+v, want exactly one entry for public.projects", rep.TablesStructuralOnly)
	}
	if rep.TablesStructuralOnly[0].Reason == "" {
		t.Errorf("report.TablesStructuralOnly[0].Reason is empty — must name why projects could not seed")
	}
}

// TestVerifyCLI_FixtureSeededTableReportsFixtureSource is TGD-BL-36's report
// half, exercised end-to-end: a table with fewer than two rows to sample,
// given a fixture in the policy file, still becomes row-level-proven (A1/A4
// pass) — but the report must mark it SeedSource="fixture", never
// "sampled", so a reader can tell a hand-authored-data proof from a
// real-target-data one.
func TestVerifyCLI_FixtureSeededTableReportsFixtureSource(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_fixture_source")

	db, err := sql.Open("postgres", target)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	// Empty on purpose — nothing to sample, forcing the fixture path.
	if _, err := db.Exec(`CREATE TABLE projects (
		id bigserial PRIMARY KEY, org_id text NOT NULL, name text NOT NULL)`); err != nil {
		t.Fatalf("create projects table: %v", err)
	}
	db.Close()

	policy := writePolicyFile(t,
		schema.Relation{Schema: "public", Name: "invoices", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "tenant_id"},
		schema.Relation{
			Schema: "public", Name: "projects", Kind: "BASE TABLE", Class: schema.Scoped, TenantColumn: "org_id",
			Fixture: []map[string]string{
				{"name": "'a'"},
				{"name": "'b'"},
			},
		},
	)

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy)
	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 — the fixture should let projects prove at the row level too. stderr:\n%s",
			r.exitCode, r.stderr)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, r.stdout)
	}
	if len(rep.TablesStructuralOnly) != 0 {
		t.Fatalf("report.TablesStructuralOnly = %+v, want none — the fixture should have seeded projects", rep.TablesStructuralOnly)
	}
	if len(rep.TablesRowLevel) != 2 {
		t.Fatalf("report.TablesRowLevel = %+v, want 2 entries", rep.TablesRowLevel)
	}
	bySource := map[string]string{}
	for _, rl := range rep.TablesRowLevel {
		bySource[rl.Table] = rl.SeedSource
	}
	if bySource["public.invoices"] != "sampled" {
		t.Errorf("public.invoices SeedSource = %q, want %q (setupTargetDatabase seeds real rows)",
			bySource["public.invoices"], "sampled")
	}
	if bySource["public.projects"] != "fixture" {
		t.Errorf("public.projects SeedSource = %q, want %q (nothing to sample, fixture-seeded)",
			bySource["public.projects"], "fixture")
	}
}

// TestVerifyCLI_TextOutputIsSelectable is TGD-US-10 AC-2: --output json is
// the default and existing tests already exercise it implicitly, so this
// test exercises the flag itself and the alternate --output text path, which
// otherwise has no test reaching it at all.
func TestVerifyCLI_TextOutputIsSelectable(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_textoutput")
	policy := invoicesScoped(t)

	rJSON := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy, "--output", "json")
	if rJSON.exitCode != 0 {
		t.Fatalf("--output json: exit code = %d, want 0. stderr:\n%s", rJSON.exitCode, rJSON.stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(rJSON.stdout), "{") {
		t.Errorf("--output json: stdout does not look like JSON: %q", rJSON.stdout)
	}

	target2 := setupTargetDatabase(t, admin, "tgd_cli_textoutput2")
	policy2 := invoicesScoped(t)
	rText := runCLIBinary(t, bin, "verify", "--dsn", target2, "--policy", policy2, "--output", "text")
	if rText.exitCode != 0 {
		t.Fatalf("--output text: exit code = %d, want 0. stderr:\n%s", rText.exitCode, rText.stderr)
	}
	if strings.HasPrefix(strings.TrimSpace(rText.stdout), "{") {
		t.Errorf("--output text: stdout looks like JSON, want human-readable lines: %q", rText.stdout)
	}
	if !strings.Contains(rText.stdout, "proven: true") {
		t.Errorf("--output text: stdout does not contain a human-readable proven line: %q", rText.stdout)
	}
}

// TestVerifyCLI_BadOutputFlagExitsUsageError: an unrecognised --output value
// must fail as a usage error, not silently fall back to a default format.
func TestVerifyCLI_BadOutputFlagExitsUsageError(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_badoutput")
	policy := invoicesScoped(t)

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policy, "--output", "xml")
	if r.exitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error). stderr: %q", r.exitCode, r.stderr)
	}
}

// TestInferCLI_WritesPolicyFileVerifyCanConsume is the end-to-end proof that
// TGD-US-09 has a real product surface: infer against a real schema, then
// feed the file it writes straight into verify with no hand-editing.
func TestInferCLI_WritesPolicyFileVerifyCanConsume(t *testing.T) {
	admin := adminDSN(t)
	bin := buildBinary(t)
	target := setupTargetDatabase(t, admin, "tgd_cli_infer")

	policyPath := filepath.Join(t.TempDir(), "inferred.json")
	rInfer := runCLIBinary(t, bin, "infer", "--dsn", target, "--out", policyPath)
	if rInfer.exitCode != 0 {
		t.Fatalf("infer: exit code = %d, want 0. stderr:\n%s", rInfer.exitCode, rInfer.stderr)
	}
	data, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("infer did not write %s: %v", policyPath, err)
	}
	var policy schema.Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatalf("policy file is not valid JSON: %v", err)
	}
	var found *schema.Relation
	for i := range policy.Relations {
		if policy.Relations[i].Name == "invoices" {
			found = &policy.Relations[i]
		}
	}
	if found == nil {
		t.Fatalf("inferred policy does not list invoices at all: %+v", policy.Relations)
	}
	if found.Class != schema.Scoped || found.TenantColumn != "tenant_id" {
		t.Fatalf("invoices classified %+v, want Scoped/tenant_id", found)
	}

	r := runCLIBinary(t, bin, "verify", "--dsn", target, "--policy", policyPath)
	if r.exitCode != 0 {
		t.Fatalf("verify with inferred policy: exit code = %d, want 0. stderr:\n%s", r.exitCode, r.stderr)
	}
}

// TestInferCLI_UsageErrorExitsTwo covers infer's own flag validation.
func TestInferCLI_UsageErrorExitsTwo(t *testing.T) {
	bin := buildBinary(t)
	r := runCLIBinary(t, bin, "infer", "--dsn", "postgres://x/y")
	if r.exitCode != 2 {
		t.Errorf("exit code = %d, want 2 (usage error) for missing --out", r.exitCode)
	}
}
