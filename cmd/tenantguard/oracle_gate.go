package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"github.com/Sour16o4/tenantguard/internal/oracle"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// gateResult is what the oracle gate produces: whether the policy is proven,
// which checks ran and passed, and the first error PolicyProven reported (nil
// when proven). Nothing about a findings report belongs here — this is the
// gate that decides whether one may ever be produced.
type gateResult struct {
	relations []schema.Relation
	proof     oracle.ProofState
	// tableProof records the outcome per scoped relation, in the same order
	// as relations: whether it seeded, and — for the ones that did — its
	// A1/A4 result. The aggregate ProofState above folds in only the seeded
	// subset (TGD-BL-32/§9.6); a report can still show every table's own
	// outcome, seeded or not.
	tableProof []tableProof
	err        error // PolicyProven()'s result; nil means proven
}

// tableProof is one scoped relation's outcome within a sweep.
//
// Seeded distinguishes two entirely different reasons a relation can lack
// A1/A4 coverage (TGD-BL-32/§9.6's tiered proof-depth model): Seeded=false
// means the relation was never attempted at all, because SeedCanaries could
// not put canary rows into it — SkipReason names why, and A1Passed/A4Passed
// are meaningless. Seeded=true means A1/A4 genuinely ran against real
// canary rows on this table, and A1Passed/A4Passed report their result.
//
// SeedSource (meaningful only when Seeded is true) is TGD-BL-36's further
// distinction within "seeded": oracle.SeedSourceSampled means the canary
// rows are the target's own real historical data with only the tenant
// column rewritten — a pass says something about the target's actual
// schema and data. oracle.SeedSourceFixture means the rows are hand-authored
// in the policy file by whoever wrote it — a pass proves the oracle
// mechanism works against a constraint-satisfying shape, not that it has
// been exercised against anything the target itself produced. These are not
// the same strength of evidence and must not be reported identically.
type tableProof struct {
	Table      string
	Seeded     bool
	SkipReason string
	SeedSource string
	A1Passed   bool
	A4Passed   bool
	// A1Err/A4Err (TGD-BL-46) are the real error CheckA1/CheckA4 returned
	// when A1Passed/A4Passed is false — nil when the check passed. Kept
	// distinct from ProofState's own generic ErrA1/ErrA4 so the operator-
	// facing message can name the actual cause (a permission/grant gap, a
	// connection error) rather than always reading as "the RLS policy is
	// wrong" — the same misleading-message shape TGD-BL-19 already fixed
	// for a skipped canary insert.
	A1Err error
	A4Err error
}

// runOracleGate builds a probe database from adminDSN's current database,
// attempts to seed two canaries into every schema.Scoped relation in rels,
// synthesises RLS on each, and runs all four oracle self-checks. A1 and A4
// are inherently per-relation (each needs its own withholding proof), so the
// gate loops them across every scoped relation that seeded; A2 and A3
// already operate on the whole set at once, seeded or not. The probe
// database and its restricted role are always dropped before this function
// returns, on every exit path.
//
// TGD-BL-32/§9.6's tiered proof-depth model: the aggregate ProofState folds
// in only the relations that seeded (tableProof.Seeded) — one of those
// failing A1 or A4 still aborts the whole run, since that is a genuine
// oracle defect, but a relation that simply had no rows to seed no longer
// does. It is instead reported structural-only: A2/A3 (catalog-only checks)
// still ran and passed for it, but A1/A4 (row-level) never had anything to
// operate on. tableProof records every relation's own outcome — seeded or
// not, passed or not — so a caller can distinguish "this table's policy is
// wrong" from "this table was never proven at the row level at all."
//
// This never writes to the target database named in adminDSN (§3.3): every
// write happens against the probe this function creates for itself.
//
// onProven, when non-nil, runs after all four checks pass and before the
// probe database and restricted role are torn down — the only window in
// which the differ can re-execute captured queries against them. It is never
// called when the policy is not proven: ProofState gates entry to this
// callback, not just the final return, so a caller cannot reach the differ
// through this function without a proven policy. Its first relation
// argument, rowLevel, is what the differ must attribute against — a
// structural-only relation is deliberately excluded: it was never shown its
// RLS actually withholds anything, so it must be invisible to ExtractTenant
// entirely, and a query touching it falls through to the differ's existing
// "no declared scoped table referenced by name" path and comes back
// UNATTRIBUTABLE, never SAFE or LEAK (TGD-BL-32/§9.6). Its second argument,
// structuralOnly, must NOT be handed to the differ for attribution — it
// exists only so a caller can classify which population an UNATTRIBUTABLE
// verdict falls into (TGD-BL-33/differ.ClassifyUnattributable), which needs
// to tell "touches nothing declared" apart from "touches a table we simply
// couldn't seed."
func runOracleGate(ctx context.Context, adminDSN string, rels []schema.Relation,
	onProven func(probeDB, restrictedDB *sql.DB, rowLevel, structuralOnly []schema.Relation) error) (gateResult, error) {
	var scoped []schema.Relation
	for _, r := range rels {
		if r.Class == schema.Scoped {
			scoped = append(scoped, r)
		}
	}
	if len(scoped) == 0 {
		return gateResult{}, fmt.Errorf("%w: policy file lists no relation classified scoped", errNoScopedRelations)
	}

	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		return gateResult{}, fmt.Errorf("open admin connection: %w", err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		return gateResult{}, fmt.Errorf("connect: %w", err)
	}

	var template string
	if err := admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&template); err != nil {
		return gateResult{}, fmt.Errorf("read current database: %w", err)
	}

	probeName := fmt.Sprintf("tgd_probe_%d", time.Now().UnixNano())
	if err := oracle.CreateProbeDatabase(ctx, admin, template, probeName); err != nil {
		return gateResult{}, fmt.Errorf("create probe database: %w", err)
	}
	defer func() {
		// Best-effort: the process is exiting either way, and a leaked probe
		// database is a cleanup annoyance, not a correctness failure — the
		// trust boundary in §3.3 is about the TARGET database, which this
		// function never touches.
		_ = oracle.DropProbeDatabase(context.Background(), admin, probeName)
	}()

	probeDSN, err := replaceDatabaseInDSN(adminDSN, probeName)
	if err != nil {
		return gateResult{}, fmt.Errorf("build probe DSN: %w", err)
	}
	probeDB, err := sql.Open("postgres", probeDSN)
	if err != nil {
		return gateResult{}, fmt.Errorf("open probe database: %w", err)
	}
	defer probeDB.Close()

	seeded, skipped, err := oracle.SeedCanaries(ctx, probeDB, scoped)
	if err != nil {
		return gateResult{}, fmt.Errorf("seed canaries: %w", err)
	}
	if len(seeded)+len(skipped) != len(scoped) {
		// Defensive: SeedCanaries's own contract is that every relation it was
		// given ends up in exactly one of seeded or skipped. Reaching here
		// would mean that contract broke, not that the operator misconfigured
		// anything — reported distinctly so the two are never confused.
		return gateResult{}, fmt.Errorf("seed canaries: %d of %d scoped relations were neither seeded nor skipped",
			len(scoped)-len(seeded)-len(skipped), len(scoped))
	}
	seedSource := make(map[string]oracle.SeedSource, len(seeded))
	for _, s := range seeded {
		seedSource[s.Table] = s.Source
	}

	roleName := fmt.Sprintf("tgd_cli_role_%d", time.Now().UnixNano())
	rolePass, err := randomHex(16)
	if err != nil {
		return gateResult{}, fmt.Errorf("generate role password: %w", err)
	}
	if err := oracle.CreateRestrictedRole(ctx, probeDB, roleName, rolePass, scoped); err != nil {
		return gateResult{}, fmt.Errorf("create restricted role: %w", err)
	}
	defer probeDB.ExecContext(context.Background(), fmt.Sprintf("DROP ROLE IF EXISTS %q", roleName))

	// EnableRLS, A2 and A3 do not need a single seeded row — they inspect
	// catalog state (pg_policies/relrowsecurity, role privileges/ownership),
	// not row visibility. Running them for every scoped relation regardless
	// of any OTHER relation's seeding outcome (§9.6's decoupling) means one
	// table failing to seed no longer holds full structural coverage of the
	// other 21 hostage — it previously did, because this whole block used to
	// sit behind the seeding-skip abort below.
	for i, r := range scoped {
		policyName := fmt.Sprintf("tgd_cli_policy_%d", i)
		if err := oracle.EnableRLS(ctx, probeDB, r, policyName); err != nil {
			// EnableRLS's own confirmation (TGD-US-01 AC-4) already re-read the
			// catalog before returning this error, so nothing further to check.
			// Unrelated to seeding — a real DDL/catalog failure — so this still
			// aborts the whole gate immediately, unchanged from before.
			return gateResult{}, fmt.Errorf("synthesise policy on %s: %w", r.Qualified(), err)
		}
	}

	var ps oracle.ProofState
	_, a2err := oracle.CheckA2(ctx, probeDB, scoped)
	ps.A2Checked, ps.A2Passed = true, a2err == nil

	a3err := oracle.CheckA3(ctx, probeDB, scoped, roleName)
	ps.A3Checked, ps.A3Passed = true, a3err == nil

	restrictedDSN, err := replaceCredentialsInDSN(probeDSN, roleName, rolePass)
	if err != nil {
		return gateResult{}, fmt.Errorf("build restricted DSN: %w", err)
	}
	restricted, err := sql.Open("postgres", restrictedDSN)
	if err != nil {
		return gateResult{}, fmt.Errorf("open restricted connection: %w", err)
	}
	defer restricted.Close()

	// TGD-BL-32/§9.6's tiered proof-depth model. A skipped table means A1/A4
	// have nothing to operate on for THAT table — its probe rows never
	// existed, and A1 would abort on "0 unrestricted, 0 restricted"
	// regardless of whether the policy is correct. Previously this aborted
	// the WHOLE sweep here, before A1/A4 were attempted on any table at all
	// — one table's seeding gap held every other table's row-level proof
	// hostage, the exact "structural coverage" problem TGD-BL-30 already
	// solved for A2/A3 but explicitly left open for A1/A4. Now: A1/A4 are
	// attempted on every relation that DID seed; a relation that could not
	// is recorded as structural-only (A2/A3 verified above, A1/A4 never
	// attempted) rather than failing the run. onProven — and therefore the
	// differ — is later given only the row-level-proven subset, so a query
	// touching a structural-only table can never be attributed at all (it
	// naturally becomes UNATTRIBUTABLE, never SAFE/LEAK — internal/differ's
	// existing "no declared scoped table referenced" path, not a new check).
	skippedReason := make(map[string]string, len(skipped))
	for _, s := range skipped {
		skippedReason[s.Table] = s.Reason
	}

	tableProofs := make([]tableProof, 0, len(scoped))
	rowLevel := make([]schema.Relation, 0, len(scoped))
	structuralOnly := make([]schema.Relation, 0, len(skipped))
	for _, r := range scoped {
		if reason, isSkipped := skippedReason[r.Qualified()]; isSkipped {
			tableProofs = append(tableProofs, tableProof{Table: r.Qualified(), Seeded: false, SkipReason: reason})
			structuralOnly = append(structuralOnly, r)
			continue
		}

		_, _, a1err := oracle.CheckA1(ctx, probeDB, restricted, r)

		// A4 here checks against the canary tenant, not real captured
		// traffic, so it needs the same type-aware canary text CheckA1 uses
		// internally — CanaryA's raw text is only ever valid as-is for a
		// text-typed tenant column (see oracle.TenantCanaryText).
		a4err := error(nil)
		if canaryTenant, cerr := oracle.TenantCanaryText(ctx, probeDB, r, oracle.CanaryA); cerr != nil {
			a4err = cerr
		} else {
			_, _, a4err = oracle.CheckA4(ctx, probeDB, restricted, r, canaryTenant)
		}
		// TGD-BL-28. A missing row key means A4 never attempted a
		// comparison at all — the same distinction TGD-BL-19 already draws
		// for seeding: this is a usage/schema gap, not the policy returning
		// the wrong rows, and folding it into A4Passed=false would let it
		// surface through PolicyProven as ErrA4, reading like a policy
		// defect. Abort immediately, distinctly, before any table's result
		// is aggregated.
		//
		// Defensive, currently unreachable through this gate's own call
		// graph: SeedCanaries succeeding already requires the tenant column
		// to exist, which alone makes rowKeyColumns' all-columns fallback
		// non-empty — a relation only reaches oracle.ErrA4NoRowKey by having
		// zero columns, which SeedCanaries would itself have skipped before
		// A1/A4 ever ran. Kept because CheckA4's own contract documents this
		// error, and a future change to either function's preconditions
		// should not silently start folding it into ErrA4 here. Proven
		// directly against CheckA4 (internal/oracle's
		// TestCheckA4NoRowKeyIsNamedError, which calls it without seeding
		// first) — not proven through this gate, which cannot reach it.
		if errors.Is(a4err, oracle.ErrA4NoRowKey) {
			return gateResult{relations: scoped, proof: ps}, fmt.Errorf("%w: %s: %v", errNoRowKey, r.Qualified(), a4err)
		}

		tableProofs = append(tableProofs, tableProof{
			Table: r.Qualified(), Seeded: true, SeedSource: string(seedSource[r.Qualified()]),
			A1Passed: a1err == nil, A4Passed: a4err == nil,
			A1Err: a1err, A4Err: a4err,
		})
		rowLevel = append(rowLevel, r)
	}
	// A2/A3 were already computed above, unconditionally, before this loop —
	// merge in only A1/A4 here rather than recomputing or overwriting them.
	a1a4 := aggregateProof(tableProofs)
	ps.A1Checked, ps.A1Passed = a1a4.A1Checked, a1a4.A1Passed
	ps.A4Checked, ps.A4Passed = a1a4.A4Checked, a1a4.A4Passed

	result := gateResult{relations: scoped, proof: ps, tableProof: tableProofs}
	if err := ps.PolicyProven(); err != nil {
		// len(rowLevel) == 0 means not one scoped relation seeded at all —
		// PolicyProven()'s generic "A1 was not run" is technically accurate
		// but loses the actionable detail of WHICH table(s) failed to seed
		// and why. Name it explicitly in that specific case; a run where at
		// least one table seeded but still failed A1/A4 (a genuine oracle
		// defect) keeps PolicyProven()'s own message, since aggregateProof
		// no longer reaches this branch at all for a seeding-only gap once
		// any table has seeded (TGD-BL-32/§9.6).
		if len(rowLevel) == 0 && len(skipped) > 0 {
			return result, fmt.Errorf("%w: %s: %s", errSeedingSkipped, skipped[0].Table, skipped[0].Reason)
		}
		return result, nameUnderlyingTableError(err, tableProofs)
	}
	if onProven != nil {
		// rowLevel, not scoped: a structural-only table (never row-level
		// proven) must never be handed to the differ for attribution, which
		// is what keeps a query touching it from ever being attributed
		// SAFE/LEAK. structuralOnly is passed alongside it only so the
		// caller can classify an UNATTRIBUTABLE verdict's population
		// (TGD-BL-33/differ.ClassifyUnattributable) — it is never handed to
		// ExtractTenant itself.
		if err := onProven(probeDB, restricted, rowLevel, structuralOnly); err != nil {
			return result, err
		}
	}
	return result, nil
}

// aggregateProof combines every SEEDED table's A1/A4 result into one
// ProofState (TGD-BL-32/§9.6): a table that never seeded contributes nothing
// to A1Checked/A4Checked in either direction — it is neither a pass (it was
// never attempted) nor a fail (its inability to seed is not an A1/A4
// defect) — see the tableProof.Seeded field. Among the tables that DID seed,
// A1Checked/A4Checked go true once any of them was attempted, but
// A1Passed/A4Passed are true only if EVERY seeded table's check passed — one
// bad seeded table must still fail the whole run, not be averaged away or
// silently dropped. An empty slice, or a slice where nothing seeded, reports
// both unchecked, matching ProofState's own zero-value contract (a check
// that never ran proves nothing) and PolicyProven()'s existing refusal to
// treat that as proof.
func aggregateProof(tableProofs []tableProof) oracle.ProofState {
	var ps oracle.ProofState
	for _, tp := range tableProofs {
		if !tp.Seeded {
			continue
		}
		if !ps.A1Checked {
			ps.A1Checked, ps.A1Passed = true, true
		}
		if !ps.A4Checked {
			ps.A4Checked, ps.A4Passed = true, true
		}
		if !tp.A1Passed {
			ps.A1Passed = false
		}
		if !tp.A4Passed {
			ps.A4Passed = false
		}
	}
	return ps
}

// nameUnderlyingTableError (TGD-BL-46) makes PolicyProven()'s generic
// ErrA1/ErrA4 name the specific table and real underlying cause when one is
// known, rather than always reading as "the RLS policy is wrong." err is
// PolicyProven()'s own return value (bare ErrA1/ErrA2/ErrA3/ErrA4, or nil);
// tableProofs is the same per-table detail aggregateProof folded from.
//
// Without this, a permission/grant gap (a table's schema never granted to
// the restricted role — TGD-BL-46's own root cause, found running zitadel's
// real, non-public-schema tables) surfaced as the same bare "A1: negative
// control did not withhold rows; the oracle is blind" a genuine RLS defect
// produces — the same misleading-message shape TGD-BL-19 already fixed for
// a skipped canary insert reading like an A1 failure. An operator hitting
// this would investigate the wrong thing entirely: the policy, not the
// grant.
//
// A pure function, not inlined, for the identical reason aggregateProof is
// (its own doc comment): there is no way to make one table's A1/A4 outcome
// diverge from another's through the CLI's own end-to-end surface without a
// database-level defect this project's design does not otherwise construct
// — direct, database-free testing is the only way to mutation-test this at
// all.
func nameUnderlyingTableError(err error, tableProofs []tableProof) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, oracle.ErrA1) && !errors.Is(err, oracle.ErrA4) {
		return err
	}
	for _, tp := range tableProofs {
		if errors.Is(err, oracle.ErrA1) && tp.A1Err != nil {
			return fmt.Errorf("%w: %s: %v", err, tp.Table, tp.A1Err)
		}
		if errors.Is(err, oracle.ErrA4) && tp.A4Err != nil {
			return fmt.Errorf("%w: %s: %v", err, tp.Table, tp.A4Err)
		}
	}
	return err
}

// errNoScopedRelations reports that a policy file was read successfully but
// contains not one relation classified schema.Scoped — nothing for the gate
// to seed, synthesise, or check. A usage error, not a policy defect: no
// self-check ever ran.
var errNoScopedRelations = errors.New("no scoped relations in policy")

// errSeedingSkipped reports that SeedCanaries could not write its canary
// tenants into any scoped relation at all. Narrowed by TGD-BL-32/§9.6's
// tiered proof-depth model: ONE relation failing to seed no longer produces
// this — it now degrades just that relation to structural-only
// (tableProof.Seeded=false) while the rest of the sweep proceeds normally.
// This is returned only when NOT ONE scoped relation seeded, so there is
// nothing anywhere for A1/A4 to have proven — a usage/configuration problem,
// not a policy defect.
var errSeedingSkipped = errors.New("could not seed canary tenants")

// errNoRowKey reports that A4 could not determine a row identity to compare
// with at all (oracle.ErrA4NoRowKey) — a relation with zero columns. A
// usage/schema problem, not a policy defect: no A4 comparison was ever
// attempted for that table, so it must never be confused with an A4/exit-13
// failure (TGD-BL-28, following TGD-BL-19's precedent for seeding).
var errNoRowKey = errors.New("could not determine a row identity for A4")

// exitCodeFor maps a ProofState.PolicyProven() result to the exit codes
// reserved in the SRS (§8.2) — 10/11/12/13 for A1/A2/A3/A4 respectively — so
// an operator can tell a blind oracle from a bypassing role from a wrong
// policy without reading the message. A nil error is not passed here; callers
// check that separately.
//
// The default case (2, usage/configuration error) is reached only if
// PolicyProven reports a check was never run — which should not happen from
// this file's own gate, since runOracleGate always runs all four. Reaching it
// would mean this file's wiring itself has a bug, not that the target's
// policy is wrong.
func exitCodeFor(err error) int {
	switch {
	case errors.Is(err, errSeedingSkipped), errors.Is(err, errNoScopedRelations), errors.Is(err, errNoRowKey):
		return 2
	case errors.Is(err, oracle.ErrA1):
		return oracle.ExitA1Failed
	case errors.Is(err, oracle.ErrA2):
		return oracle.ExitA2Failed
	case errors.Is(err, oracle.ErrA3):
		return oracle.ExitA3Failed
	case errors.Is(err, oracle.ErrA4):
		return oracle.ExitA4Failed
	default:
		return 2
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// replaceDatabaseInDSN and replaceCredentialsInDSN do minimal, deliberately
// narrow surgery on a postgres:// DSN — this CLI only needs to swap the path
// (database name) or the userinfo (role/password), never anything else.
func replaceDatabaseInDSN(dsn, dbName string) (string, error) {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return "", fmt.Errorf("not a postgres:// DSN: %q", dsn)
	}
	authhost, pathAndQuery, hasPath := strings.Cut(rest, "/")
	query := ""
	if hasPath {
		if _, q, hasQuery := strings.Cut(pathAndQuery, "?"); hasQuery {
			query = "?" + q
		}
	}
	return fmt.Sprintf("%s://%s/%s%s", scheme, authhost, dbName, query), nil
}

func replaceCredentialsInDSN(dsn, user, pass string) (string, error) {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return "", fmt.Errorf("not a postgres:// DSN: %q", dsn)
	}
	_, hostAndPath, hasAt := strings.Cut(rest, "@")
	if !hasAt {
		hostAndPath = rest
	}
	return fmt.Sprintf("%s://%s:%s@%s", scheme, user, pass, hostAndPath), nil
}
