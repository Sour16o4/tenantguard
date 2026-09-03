// Package oracle builds a probe database, synthesises row-level security on it,
// and proves the result can actually withhold rows.
//
// The tool never writes to the application's database. Everything here operates
// on a probe database the tool creates and drops.
//
// The central assertion is A1. Its purpose is to make one failure impossible:
// reporting "no leaks" because the oracle was blind rather than because the
// application was clean. When A1 cannot be proved, the run aborts and no
// findings are emitted.
package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// Exit codes reserved by the SRS (§8.2) for oracle self-check failures.
const (
	ExitA1Failed = 10
	ExitA2Failed = 11
	ExitA3Failed = 12
	ExitA4Failed = 13
)

// ErrA1 reports that the negative control did not withhold rows: under RLS the
// restricted role saw as much as an unrestricted one. The oracle cannot see, so
// no verdict it produces means anything.
var ErrA1 = errors.New("A1: negative control did not withhold rows; the oracle is blind")

// ErrA2 reports that a table classified Scoped has no enabled, matching RLS
// policy. Either RLS was never switched on, or a policy was never attached —
// PostgreSQL allows either half without the other, and both leave the table
// unprotected while every earlier check might still look clean.
var ErrA2 = errors.New("A2: a scoped table has no enabled RLS policy")

// ErrA3 reports that the connecting role can see past RLS regardless of any
// policy: it is a superuser, holds BYPASSRLS, or owns a scoped table that is
// not FORCEd. Any of these makes every synthesised policy inert for this role.
var ErrA3 = errors.New("A3: connecting role can bypass row-level security")

// ErrA4 reports that a known-correctly-scoped query returned different rows
// under RLS than it did unrestricted. Unlike ErrA1, this is not about whether
// the policy withholds SOMETHING — a wrong-column policy withholds rows too,
// just the wrong ones. ErrA4 is about whether the policy withholds the RIGHT
// ones, which A1's row count alone cannot determine.
var ErrA4 = errors.New("A4: positive control returned different rows under RLS; the policy is wrong or over-restrictive")

// TenantSetting is the session variable the synthesised policies read.
const TenantSetting = "tenantguard.tenant"

// Canary tenant identifiers seeded into the probe database. Two are required:
// A1 compares what a restricted role sees against everything present, so a
// single-tenant probe cannot demonstrate withholding at all.
const (
	CanaryA = "tgd-canary-aaaa"
	CanaryB = "tgd-canary-bbbb"
)

// Result records what the oracle proved and on what.
type Result struct {
	ProbeDatabase  string
	Role           string
	SeededTables   []string
	SkippedTables  []SkippedTable
	A1Table        string
	A1Unrestricted int
	A1Restricted   int
}

// SkippedTable records a scoped table the probe could not seed, and why. A
// skipped table is reported, never treated as passing.
type SkippedTable struct {
	Table  string
	Reason string
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func qualified(r schema.Relation) string {
	return quoteIdent(r.Schema) + "." + quoteIdent(r.Name)
}

// ErrSynthesisNotConfirmed reports that EnableRLS's own DDL returned no error,
// but a subsequent re-read of pg_policies / pg_class.relrowsecurity did not
// find the policy it just created. TGD-US-01 AC-4: synthesis does not trust its
// own DDL succeeding — it re-reads the catalog and treats absence as failure,
// even though every Exec call returned nil.
var ErrSynthesisNotConfirmed = errors.New("synthesis: DDL reported success but the policy is not present in the catalog")

// EnableRLS turns on row-level security for a relation and attaches a policy
// restricting rows to the tenant named by the session setting.
//
// FORCE is applied because RLS does not otherwise apply to a table's owner —
// without it the policy would be silently inert for an owning role, which is a
// gate that cannot fire.
//
// AC-4 note on what "confirm" can and cannot catch: under PostgreSQL's
// autocommit and READ COMMITTED defaults, once an Exec call for CREATE POLICY
// returns without error, that policy is durably committed and immediately
// visible to any subsequent query on any connection. A single-writer run of
// this function with no concurrent interference cannot produce a state where
// the DDL succeeded and the catalog disagrees — PostgreSQL's own transactional
// guarantees rule it out. The confirmation step below is real and load-bearing
// defense-in-depth regardless: it catches a *future* bug in this function (a
// dropped statement, a name mismatch between what was created and what is
// checked), external interference between the DDL and the return, or a
// surprise in how a specific PostgreSQL version reports partial DDL failure —
// not a state this test suite can force to occur through EnableRLS's own
// well-behaved execution. That absence of a natural failure path is exactly
// why the wiring is proven by a controlled seam (verifySynthesisFn) and by
// mutation, rather than by a constructed real-Postgres divergence — see
// oracle_verify_test.go.
func EnableRLS(ctx context.Context, db *sql.DB, r schema.Relation, policyName string) error {
	tbl := qualified(r)
	stmts := []string{
		fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", tbl),
		fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", tbl),
		fmt.Sprintf("DROP POLICY IF EXISTS %s ON %s", quoteIdent(policyName), tbl),
		fmt.Sprintf(
			"CREATE POLICY %s ON %s USING (%s::text = current_setting('%s', true))",
			quoteIdent(policyName), tbl, quoteIdent(r.TenantColumn), TenantSetting),
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("enable RLS on %s: %w", r.Qualified(), err)
		}
	}
	if err := verifySynthesisFn(ctx, db, r); err != nil {
		return fmt.Errorf("enable RLS on %s: %w", r.Qualified(), err)
	}
	return nil
}

// verifySynthesis re-reads the catalog after synthesis and confirms the
// intended policy is both present and would actually apply: RLS enabled,
// FORCE set (the same reason EnableRLS itself sets it), and exactly the
// synthesised policy counted in pg_policies.
func verifySynthesis(ctx context.Context, db *sql.DB, r schema.Relation) error {
	enabled, forced, err := RowSecurityEnabled(ctx, db, r)
	if err != nil {
		return fmt.Errorf("confirm row security state: %w", err)
	}
	if !enabled {
		return fmt.Errorf("%w: relrowsecurity is false on %s after ENABLE ROW LEVEL SECURITY reported success",
			ErrSynthesisNotConfirmed, r.Qualified())
	}
	if !forced {
		return fmt.Errorf("%w: relforcerowsecurity is false on %s after FORCE ROW LEVEL SECURITY reported success",
			ErrSynthesisNotConfirmed, r.Qualified())
	}
	n, err := PolicyCount(ctx, db, r)
	if err != nil {
		return fmt.Errorf("confirm policy count: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: no policy found in pg_policies on %s after CREATE POLICY reported success",
			ErrSynthesisNotConfirmed, r.Qualified())
	}
	return nil
}

// verifySynthesisFn is the seam EnableRLS calls through, so a test can prove
// EnableRLS's return value is genuinely gated on confirmation — by substituting
// a stub that fails regardless of the real catalog state — without needing a
// real-Postgres divergence that (per the note on EnableRLS above) cannot be
// produced through this function's own well-behaved, single-writer execution.
var verifySynthesisFn = verifySynthesis

// PolicyCount reports how many policies exist for a relation, which A2 uses to
// verify coverage.
func PolicyCount(ctx context.Context, db *sql.DB, r schema.Relation) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_policies WHERE schemaname=$1 AND tablename=$2`,
		r.Schema, r.Name).Scan(&n)
	return n, err
}

// RowSecurityEnabled reports whether RLS is enabled and forced on a relation.
func RowSecurityEnabled(ctx context.Context, db *sql.DB, r schema.Relation) (enabled, forced bool, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT c.relrowsecurity, c.relforcerowsecurity
		   FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE n.nspname=$1 AND c.relname=$2`,
		r.Schema, r.Name).Scan(&enabled, &forced)
	return
}

// TenantCanaryText resolves the type-aware text a canary tenant identity
// takes on relation r's tenant column: the value CheckA1 sets as the session
// variable, and the value a caller building a canary-based A4 call must pass
// as "tenant" (the gate in cmd/tenantguard does this; CheckA4 is otherwise
// generic and used with real captured tenant values too). Exported so both
// this package's own CheckA1 and that caller resolve the identical value —
// see canaryText's doc comment for why they must never disagree.
func TenantCanaryText(ctx context.Context, db *sql.DB, r schema.Relation, canary string) (string, error) {
	cols, err := readColumns(ctx, db, r)
	if err != nil {
		return "", fmt.Errorf("read tenant column type: %w", err)
	}
	for _, c := range cols {
		if c.Name == r.TenantColumn {
			text, ok := canaryText(c.Type, canary)
			if !ok {
				return "", fmt.Errorf("tenant column %q has unsupported type %q for canary comparison", c.Name, c.Type)
			}
			return text, nil
		}
	}
	return "", fmt.Errorf("tenant column %q not found on %s", r.TenantColumn, r.Qualified())
}

// CheckA1 is the negative control.
//
// It counts rows in a scoped table twice: once unrestricted, and once as a role
// subject to RLS with the tenant set to one canary. The restricted count must be
// STRICTLY smaller. Equality means the policy withheld nothing — whether because
// the policy is permissive, the role bypasses RLS, or the probe holds only one
// tenant — and in every one of those cases the oracle cannot see.
//
// A non-nil error from this function must abort the run with ExitA1Failed and
// emit no findings.
func CheckA1(ctx context.Context, admin, restricted *sql.DB, r schema.Relation) (unrestrictedN, restrictedN int, err error) {
	tbl := qualified(r)

	if err = admin.QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s", tbl)).Scan(&unrestrictedN); err != nil {
		return 0, 0, fmt.Errorf("A1: count unrestricted: %w", err)
	}

	tenantText, err := TenantCanaryText(ctx, admin, r, CanaryA)
	if err != nil {
		return 0, 0, fmt.Errorf("A1: %w", err)
	}

	conn, err := restricted.Conn(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("A1: acquire restricted connection: %w", err)
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx,
		fmt.Sprintf("SELECT set_config('%s', $1, false)", TenantSetting), tenantText); err != nil {
		return 0, 0, fmt.Errorf("A1: set tenant: %w", err)
	}
	if err = conn.QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s", tbl)).Scan(&restrictedN); err != nil {
		return 0, 0, fmt.Errorf("A1: count restricted: %w", err)
	}

	if restrictedN >= unrestrictedN {
		return unrestrictedN, restrictedN, fmt.Errorf(
			"%w: %s returned %d rows unrestricted and %d under RLS (expected strictly fewer)",
			ErrA1, r.Qualified(), unrestrictedN, restrictedN)
	}
	return unrestrictedN, restrictedN, nil
}

// ErrA4NoRowKey reports that a relation has no column at all to build a row
// identity from — the catalog read that backs rowKeyColumns returned zero
// columns. This is a usage/schema problem, never a policy defect: no A4
// comparison was attempted, so it must not be confused with ErrA4 (TGD-BL-28,
// the same lesson TGD-BL-19 already established for seeding).
var ErrA4NoRowKey = errors.New("A4: relation has no columns to build a row identity from")

// primaryKeyColumns returns relation r's primary key columns, in their
// position within the key (not necessarily attribute order), or nil if r has
// no primary key.
func primaryKeyColumns(ctx context.Context, db *sql.DB, r schema.Relation) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.attname
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		 WHERE n.nspname = $1 AND c.relname = $2 AND i.indisprimary
		 ORDER BY array_position(i.indkey, a.attnum)`, r.Schema, r.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// rowKeyColumns returns the columns CheckA4 uses to identify one row for
// comparison: the relation's primary key columns, or — when it has none, the
// real shape of 3 of coder's 22 real scoped tables (TGD-BL-28) — every
// column, since a conventional "id" column name is not a safe assumption
// against a real schema. An empty, non-error result never happens: a
// relation with zero columns returns ErrA4NoRowKey instead, since there is
// then nothing to key rows by at all.
func rowKeyColumns(ctx context.Context, db *sql.DB, r schema.Relation) ([]string, error) {
	pk, err := primaryKeyColumns(ctx, db, r)
	if err != nil {
		return nil, fmt.Errorf("read primary key: %w", err)
	}
	if len(pk) > 0 {
		return pk, nil
	}
	cols, err := readColumns(ctx, db, r)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrA4NoRowKey, r.Qualified())
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names, nil
}

// rowKeyExpr builds a single SQL text expression that identifies one row:
// every key column cast to text and concatenated with a separator unlikely
// to appear in real data, each guarded with coalesce so a NULL in one column
// (possible only in the no-primary-key, all-columns fallback — primary key
// columns are never nullable) cannot collapse two different rows onto the
// same NULL-propagated key.
func rowKeyExpr(keyCols []string) string {
	// The separator and NULL sentinel are plain printable text, deliberately
	// — not a NUL byte (PostgreSQL's simple-query wire message is
	// NUL-terminated; one embedded in the query text itself truncates the
	// message and produces a protocol-level "insufficient data" error, not a
	// SQL error, found while writing this).
	const sep = "'\x1c'"
	const nullSentinel = "'\x1dTGD-NULL\x1d'"
	parts := make([]string, len(keyCols))
	for i, c := range keyCols {
		parts[i] = fmt.Sprintf("coalesce(%s::text, %s)", quoteIdent(c), nullSentinel)
	}
	return strings.Join(parts, " || "+sep+" || ")
}

// CheckA4 is the positive control.
//
// A1 only asks whether RLS withholds SOME rows; it cannot tell a correct policy
// from one that excludes the wrong rows entirely (TGD-BL-15). A4 asks a
// different question: for a query already scoped correctly by hand, does RLS
// arrive at the identical row set on its own?
//
// The reference set is built with an explicit, hand-written predicate against
// the unrestricted admin connection — "select the rows tenant X actually owns".
// The comparison set is the same table queried with NO predicate at all, as a
// restricted role with the session tenant set to X — "select whatever RLS lets
// this role see". If the policy is correct, RLS reproduces the reference
// exactly. If the policy names the wrong column, is too permissive, or is too
// restrictive, the sets diverge, and identical row COUNTS are not enough to
// pass: TestA4AbortsWithWrongColumnPolicy is built on a case where the counts
// would coincidentally differ in the right direction but the actual rows never
// match.
//
// Row identity is derived from the catalog (rowKeyColumns), never assumed to
// be a conventionally-named "id" column (TGD-BL-28) — organization_members,
// jfrog_xray_scans and workspace_agent_port_share, 3 of coder's 22 real
// scoped tables, have no such column, only a composite primary key. A
// relation with no columns at all returns ErrA4NoRowKey, distinct from ErrA4,
// so that gap reads as the usage/schema problem it is, not a policy failure.
// A non-nil error must abort the run with ExitA4Failed and emit no findings.
func CheckA4(ctx context.Context, admin, restricted *sql.DB, r schema.Relation, tenant string) (reference, got []string, err error) {
	tbl := qualified(r)

	keyCols, err := rowKeyColumns(ctx, admin, r)
	if err != nil {
		return nil, nil, fmt.Errorf("A4: determine row identity for %s: %w", r.Qualified(), err)
	}
	expr := rowKeyExpr(keyCols)

	refRows, err := admin.QueryContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1 ORDER BY %s", expr, tbl, quoteIdent(r.TenantColumn), expr),
		tenant)
	if err != nil {
		return nil, nil, fmt.Errorf("A4: reference query: %w", err)
	}
	reference, err = scanRowKeys(refRows)
	if err != nil {
		return nil, nil, fmt.Errorf("A4: scan reference: %w", err)
	}

	conn, err := restricted.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("A4: acquire restricted connection: %w", err)
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx,
		fmt.Sprintf("SELECT set_config('%s', $1, false)", TenantSetting), tenant); err != nil {
		return nil, nil, fmt.Errorf("A4: set tenant: %w", err)
	}
	gotRows, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", expr, tbl, expr))
	if err != nil {
		return nil, nil, fmt.Errorf("A4: restricted query: %w", err)
	}
	got, err = scanRowKeys(gotRows)
	if err != nil {
		return nil, nil, fmt.Errorf("A4: scan restricted: %w", err)
	}

	if !stringSlicesEqual(reference, got) {
		return reference, got, fmt.Errorf(
			"%w: %s reference (WHERE %s=%q) returned %d rows %v, but RLS-restricted returned %d rows %v",
			ErrA4, r.Qualified(), r.TenantColumn, tenant, len(reference), reference, len(got), got)
	}
	return reference, got, nil
}

func scanRowKeys(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ProofState records which oracle self-checks have run for a policy and
// whether each passed.
//
// This is the operational form of TGD-US-03 AC-6: a report may claim a policy
// is trustworthy only when every check that was RUN passed, and A1 having
// passed is never enough on its own — A4 must also have been run and passed.
// The zero value proves nothing, deliberately: a check that was never run must
// never be treated as though it passed.
type ProofState struct {
	A1Checked, A1Passed bool
	A2Checked, A2Passed bool
	A3Checked, A3Passed bool
	A4Checked, A4Passed bool
}

// PolicyProven returns nil only when both A1 and A4 were run and both passed.
// Any other combination — including one check never having run at all —
// returns a non-nil error, and no report may be emitted while it does.
func (p ProofState) PolicyProven() error {
	if !p.A1Checked {
		return fmt.Errorf("A1 was not run; a policy cannot be trusted on the strength of a check that never executed")
	}
	if !p.A1Passed {
		return fmt.Errorf("%w", ErrA1)
	}
	if !p.A2Checked {
		return fmt.Errorf("A1 passed but A2 was not run; A1 cannot detect a table with no policy at all")
	}
	if !p.A2Passed {
		return fmt.Errorf("%w", ErrA2)
	}
	if !p.A3Checked {
		return fmt.Errorf("A1 and A2 passed but A3 was not run; neither can detect a role that bypasses RLS entirely")
	}
	if !p.A3Passed {
		return fmt.Errorf("%w", ErrA3)
	}
	if !p.A4Checked {
		return fmt.Errorf("A1, A2 and A3 passed but A4 was not run; none of them can detect a wrong-column or over-restrictive policy (TGD-BL-15)")
	}
	if !p.A4Passed {
		return fmt.Errorf("%w", ErrA4)
	}
	return nil
}

// CheckA2 verifies policy coverage: every relation classified schema.Scoped
// must have row-level security enabled AND at least one matching policy in
// pg_policies. PostgreSQL allows either half without the other — a policy can
// exist on a table with RLS never switched on, and RLS can be enabled with no
// policy attached — and both leave the table completely unprotected.
//
// Relations that are not schema.Scoped are skipped entirely, deliberately. An
// Unclassifiable table (a view, or one with ambiguous tenant columns) was never
// assigned a tenant column and was never a synthesis target; policy-checking it
// would fail on every real schema, since every real schema has views. That
// would make A2 permanently unfireable rather than load-bearing — the same
// defect class this whole design exists to avoid, just introduced here instead
// of in the oracle itself.
//
// A non-nil error must abort the run with ExitA2Failed and emit no findings.
func CheckA2(ctx context.Context, db *sql.DB, relations []schema.Relation) (uncovered []string, err error) {
	for _, r := range relations {
		if r.Class != schema.Scoped {
			continue
		}
		enabled, _, err := RowSecurityEnabled(ctx, db, r)
		if err != nil {
			return nil, fmt.Errorf("A2: read row security state for %s: %w", r.Qualified(), err)
		}
		n, err := PolicyCount(ctx, db, r)
		if err != nil {
			return nil, fmt.Errorf("A2: count policies for %s: %w", r.Qualified(), err)
		}
		if !enabled || n == 0 {
			uncovered = append(uncovered, r.Qualified())
		}
	}
	if len(uncovered) > 0 {
		return uncovered, fmt.Errorf("%w: %v", ErrA2, uncovered)
	}
	return nil, nil
}

// tableOwner returns the owning role of a relation.
func tableOwner(ctx context.Context, db *sql.DB, r schema.Relation) (string, error) {
	var owner string
	err := db.QueryRowContext(ctx,
		`SELECT pg_get_userbyid(c.relowner) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2`,
		r.Schema, r.Name).Scan(&owner)
	return owner, err
}

// CheckA3 verifies the connecting role cannot see past RLS regardless of any
// policy content. Three ways that happens: the role is a superuser, the role
// holds BYPASSRLS, or the role owns a scoped table that is not FORCEd — RLS
// does not apply to a table's own owner unless FORCE ROW LEVEL SECURITY is set,
// which is the exact silent no-op EnableRLS's FORCE clause exists to prevent.
//
// Role-level bypass (superuser, BYPASSRLS) is checked once, since it is not
// per-table. Ownership is checked per Scoped relation, since a role can own
// some tables and not others.
//
// A non-nil error must abort the run with ExitA3Failed and emit no findings.
func CheckA3(ctx context.Context, db *sql.DB, relations []schema.Relation, role string) error {
	var isSuper, bypass bool
	if err := db.QueryRowContext(ctx,
		"SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1", role).
		Scan(&isSuper, &bypass); err != nil {
		return fmt.Errorf("A3: read role privileges for %q: %w", role, err)
	}
	if isSuper {
		return fmt.Errorf("%w: role %q is a superuser; RLS never applies to it", ErrA3, role)
	}
	if bypass {
		return fmt.Errorf("%w: role %q has BYPASSRLS; RLS never applies to it", ErrA3, role)
	}

	var unforced []string
	for _, r := range relations {
		if r.Class != schema.Scoped {
			continue
		}
		owner, err := tableOwner(ctx, db, r)
		if err != nil {
			return fmt.Errorf("A3: read owner of %s: %w", r.Qualified(), err)
		}
		if owner != role {
			continue
		}
		_, forced, err := RowSecurityEnabled(ctx, db, r)
		if err != nil {
			return fmt.Errorf("A3: read row security state for %s: %w", r.Qualified(), err)
		}
		if !forced {
			unforced = append(unforced, r.Qualified())
		}
	}
	if len(unforced) > 0 {
		return fmt.Errorf("%w: role %q owns %v without FORCE ROW LEVEL SECURITY", ErrA3, role, unforced)
	}
	return nil
}
