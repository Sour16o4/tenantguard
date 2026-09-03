package differ

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/oracle"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// Verdict is the differ's classification of one captured query.
type Verdict string

const (
	Safe           Verdict = "SAFE"
	Leak           Verdict = "LEAK"
	Unattributable Verdict = "UNATTRIBUTABLE"
)

// Result is Diff's output for one captured query.
type Result struct {
	Verdict Verdict
	// Reason explains an Unattributable verdict, or a Leak's cause.
	Reason string
	// Tenant is the value the restricted re-execution was bound to. Empty for
	// AttrNoPredicate ("no tenant claimed") and for Unattributable.
	Tenant string
	// WithheldRows is the number of unrestricted rows/scalars absent from the
	// restricted result. Meaningful only for Leak.
	WithheldRows int
	// Vacuous is TGD-BL-39's decision: meaningful only when Verdict == Safe,
	// true when the comparison that produced it held because BOTH the
	// unrestricted and restricted re-executions matched (read path) or
	// affected (write path) zero rows — ∅=∅, not a real demonstration that
	// RLS correctly withheld/admitted anything. A vacuous SAFE is not a
	// misclassification (the literal claim "row set under RLS is identical
	// to unrestricted" still holds), but it is evidentially weaker than one
	// where both sides returned the same NON-empty result, and must not be
	// silently counted as equally strong proof by a reader or by the
	// unattributable-rate ceiling's denominator (cmd/tenantguard's
	// unattributableRateBreakdown excludes it from "attributed").
	//
	// TGD-BL-40: both paths are now instrumented. diffWrite re-executes via
	// ExecContext and uses sql.Result.RowsAffected() — populated from
	// PostgreSQL's command-complete tag regardless of whether the statement
	// has a RETURNING clause, so a write's Vacuous is exact for both shapes,
	// not just an approximation for the RETURNING case.
	Vacuous bool
}

// Diff re-executes a captured query once unrestricted and once under the
// RLS-enforcing restricted role, and compares the results.
//
// Every re-execution — unrestricted and restricted, SELECT and INSERT alike —
// runs inside a transaction that is always rolled back, never committed. This
// is what satisfies TGD-US-06 AC-6 ("no write occurs ... including for
// captured INSERT, UPDATE and DELETE statements") uniformly: a rolled-back
// transaction produces zero durable writes regardless of what the captured
// statement was. probeDB and restrictedDB must both point at the probe
// database (or an equivalent read-only-safe copy) — never at the application's
// own database; this function performs no check of that itself, and the
// caller is responsible for passing the right connections (as the CLI's
// runOracleGate already does for A1–A4).
func Diff(ctx context.Context, probeDB, restrictedDB *sql.DB, relations []schema.Relation, ev capture.Event) Result {
	if !ev.Resolved {
		return Result{Verdict: Unattributable,
			Reason: "capture could not resolve this Bind to its SQL text"}
	}

	attr := ExtractTenant(ev.SQL, relations, ev.Params)
	if attr.Kind == AttrUnattributable {
		return Result{Verdict: Unattributable, Reason: attr.Reason}
	}

	tenant := attr.Value // empty string for AttrNoPredicate — deliberately

	args, err := paramArgs(ev.Params)
	if err != nil {
		// ExtractTenant already rejects any unresolved parameter, so this is
		// unreachable in practice; kept as a defensive, honestly-labelled
		// fallback rather than a panic.
		return Result{Verdict: Unattributable, Reason: err.Error()}
	}

	if isWriteStatement(ev.SQL) {
		return diffWrite(ctx, probeDB, restrictedDB, tenant, ev.SQL, args)
	}

	unrestrictedRows, err := runRolledBack(ctx, probeDB, ev.SQL, args)
	if err != nil {
		return Result{Verdict: Unattributable,
			Reason: fmt.Sprintf("unrestricted re-execution failed: %v", err)}
	}

	restrictedRows, err := runRolledBackAsTenant(ctx, restrictedDB, tenant, ev.SQL, args)
	if err != nil {
		return Result{Verdict: Unattributable,
			Reason: fmt.Sprintf("restricted re-execution failed: %v", err)}
	}

	withheld := multisetShortfall(unrestrictedRows, restrictedRows)
	if withheld > 0 || len(unrestrictedRows) != len(restrictedRows) {
		// Any divergence at all is a leak (TGD-US-06 AC-2: rows absent from
		// the RLS result; AC-3: a differing scalar). withheld may be 0 while
		// lengths still differ if restricted somehow returns MORE distinct
		// values than unrestricted contains — recorded as the length delta so
		// this is never silently treated as SAFE.
		if withheld == 0 {
			withheld = abs(len(unrestrictedRows) - len(restrictedRows))
		}
		return Result{Verdict: Leak, Tenant: tenant, WithheldRows: withheld,
			Reason: fmt.Sprintf("unrestricted returned %d row(s), restricted (tenant=%q) returned %d",
				len(unrestrictedRows), tenant, len(restrictedRows))}
	}

	return Result{Verdict: Safe, Tenant: tenant, Vacuous: len(unrestrictedRows) == 0}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// paramArgs converts resolved capture.Params into driver arguments. Every
// parameter must already be Known — ExtractTenant enforces this before Diff
// ever reaches here, so failure here indicates a caller bypassed that check.
func paramArgs(params []capture.Param) ([]any, error) {
	args := make([]any, len(params))
	for i, p := range params {
		if p.IsNull {
			args[i] = nil
			continue
		}
		if !p.Known {
			return nil, fmt.Errorf("parameter %d was not resolved to a value", i+1)
		}
		args[i] = p.Text
	}
	return args, nil
}

// runRolledBack executes sqlText with args inside a transaction on db,
// collects a comparable row-set representation, and always rolls back.
func runRolledBack(ctx context.Context, db *sql.DB, sqlText string, args []any) ([]string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out, err := collectRows(rows)
	if err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// runRolledBackAsTenant is runRolledBack, but on a dedicated connection with
// the session tenant set first, for the restricted role. tenant == "" leaves
// the session tenant unset, which makes RLS deny every row — the mechanism
// AttrNoPredicate relies on.
func runRolledBackAsTenant(ctx context.Context, db *sql.DB, tenant, sqlText string, args []any) ([]string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("SELECT set_config('%s', $1, false)", oracle.TenantSetting), tenant); err != nil {
		return nil, fmt.Errorf("set tenant: %w", err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out, err := collectRows(rows)
	if err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// execRolledBack executes sqlText with args as a statement (ExecContext, not
// QueryContext) inside a transaction on db, and always rolls back. Returns
// the number of rows the statement reports having affected.
//
// TGD-BL-40: this is what makes write-path Vacuous detection possible.
// QueryContext (runRolledBack's mechanism) reports no row data at all for a
// write with no RETURNING clause, regardless of how many rows it actually
// touched — there is nothing in a bare "DELETE 3" command-complete tag for
// Query's row-oriented API to surface. ExecContext's sql.Result.RowsAffected
// is populated from that same command-complete tag either way, RETURNING or
// not — confirmed against lib/pq's Exec, which drains any RETURNING rows
// itself and still returns the real affected count via
// readExecuteResponse/simpleExec, so this needs no RETURNING-vs-not branch:
// one code path serves both.
func execRolledBack(ctx context.Context, db *sql.DB, sqlText string, args []any) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return 0, fmt.Errorf("exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// execRolledBackAsTenant is execRolledBack, but on a dedicated connection
// with the session tenant set first, for the restricted role — the write-path
// counterpart of runRolledBackAsTenant.
func execRolledBackAsTenant(ctx context.Context, db *sql.DB, tenant, sqlText string, args []any) (int64, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("SELECT set_config('%s', $1, false)", oracle.TenantSetting), tenant); err != nil {
		return 0, fmt.Errorf("set tenant: %w", err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return 0, fmt.Errorf("exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// collectRows reads every row generically and renders each as a single
// comparable string, then sorts the result — row order is not semantically
// meaningful for a leak comparison, and the two executions are not guaranteed
// to agree on it.
func collectRows(rows *sql.Rows) ([]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "\x00NULL"
			}
		}
		out = append(out, strings.Join(parts, "\x1f"))
	}
	sort.Strings(out)
	return out, nil
}

// multisetShortfall returns how many entries of `have` are not matched by an
// entry in `want`, respecting duplicate counts (a multiset difference, not a
// set difference — two identical leaked rows must count as two).
func multisetShortfall(have, want []string) int {
	remaining := map[string]int{}
	for _, w := range want {
		remaining[w]++
	}
	shortfall := 0
	for _, h := range have {
		if remaining[h] > 0 {
			remaining[h]--
			continue
		}
		shortfall++
	}
	return shortfall
}

// isWriteStatement reports whether executing sqlText performs a write
// (INSERT/UPDATE/DELETE), which needs a different comparison than a pure
// read — see diffWrite.
//
// Leading "--" line comments are skipped before anything else. Found
// necessary against real coder traffic (TGD-BL-31): every coder query is
// sqlc-generated with a "-- name: X :one" header comment, so a naive
// TrimSpace-only check never matched, routing every real write through the
// SELECT-style comparison instead of diffWrite.
//
// A leading-keyword check on what remains is not enough (TGD-BL-41): a CTE
// can wrap any DML, so `WITH cte AS (...) DELETE ...`'s real verb is DELETE,
// not WITH — and the write can sit entirely inside a CTE body while the
// outer statement is a plain SELECT reading the CTE's result (`WITH deleted
// AS (DELETE ... RETURNING id) SELECT count(*) FROM deleted` still performs
// the delete when executed). Found against two real coder queries,
// `DeleteOldWorkspaceBuildOrchestrations` and `ExpirePrebuildsAPIKeys`, both
// `WITH <cte> AS (...) DELETE/UPDATE ...` — both were silently compared as
// reads before this fix.
//
// statementContainsWrite walks past any leading WITH clause(s), recursing
// into each CTE body (a CTE body can itself begin with its own WITH), and
// checks the outer statement's own leading verb once the CTE list is
// consumed. A write anywhere in that walk — nested CTE body or outer
// statement — makes the whole statement a write for diffWrite's purposes:
// executing it performs the write regardless of where in the statement text
// the write verb appears.
func isWriteStatement(sqlText string) bool {
	t := strings.TrimSpace(sqlText)
	for strings.HasPrefix(t, "--") {
		nl := strings.IndexByte(t, '\n')
		if nl < 0 {
			t = ""
			break
		}
		t = strings.TrimSpace(t[nl+1:])
	}
	return statementContainsWrite(t)
}

// statementContainsWrite is isWriteStatement's recursive core, operating on
// text already past any leading comment. See isWriteStatement's doc comment
// for what it detects and why.
func statementContainsWrite(t string) bool {
	t = strings.TrimSpace(t)
	rest, nestedWrite := skipWithClause(t)
	if nestedWrite {
		return true
	}
	u := strings.ToUpper(strings.TrimSpace(rest))
	return strings.HasPrefix(u, "INSERT") || strings.HasPrefix(u, "UPDATE") || strings.HasPrefix(u, "DELETE")
}

// skipWithClause, if t begins with WITH (optionally WITH RECURSIVE),
// consumes the comma-separated CTE definition list and returns the
// remaining text (the primary statement that follows) along with whether
// any CTE body itself contains a write (checked recursively via
// statementContainsWrite, since a CTE body can itself start with WITH).
//
// If t does not begin with WITH, returns t unchanged and false.
//
// Parsing stops and returns what has been consumed so far, honestly, the
// moment the expected shape (name, optional column list, AS, optional
// [NOT] MATERIALIZED, balanced parens) is not found — the same "don't guess"
// discipline balancedParenSpan's caller already follows elsewhere in this
// package. A CTE list this cannot parse is not silently treated as
// containing no write; the primary-statement check below still runs against
// whatever text remains, which for a malformed CTE list is the safer of the
// two ways to fail (a keyword still visible reads as a write; a keyword
// swallowed as unparsed CTE text does not — this project already treats a
// missed write as the worse failure than an extra diffWrite comparison).
func skipWithClause(t string) (rest string, nestedWrite bool) {
	if !hasCaseInsensitivePrefix(t, "WITH") {
		return t, false
	}
	i := len("WITH")
	i = skipSpaceFrom(t, i)
	if hasCaseInsensitivePrefix(t[i:], "RECURSIVE") {
		i += len("RECURSIVE")
		i = skipSpaceFrom(t, i)
	}

	for {
		i = skipSpaceFrom(t, i)
		if i >= len(t) {
			return t[i:], nestedWrite
		}

		nameEnd := skipIdentifier(t, i)
		if nameEnd == i {
			return t[i:], nestedWrite
		}
		i = skipSpaceFrom(t, nameEnd)

		if i < len(t) && t[i] == '(' {
			// Optional column list, e.g. `WITH ids (id) AS (...)`.
			_, after, ok := balancedParenSpan(t, i)
			if !ok {
				return t[i:], nestedWrite
			}
			i = skipSpaceFrom(t, after)
		}

		if !hasCaseInsensitivePrefix(t[i:], "AS") {
			return t[i:], nestedWrite
		}
		i = skipSpaceFrom(t, i+len("AS"))

		if hasCaseInsensitivePrefix(t[i:], "NOT") {
			i = skipSpaceFrom(t, i+len("NOT"))
			if hasCaseInsensitivePrefix(t[i:], "MATERIALIZED") {
				i = skipSpaceFrom(t, i+len("MATERIALIZED"))
			}
		} else if hasCaseInsensitivePrefix(t[i:], "MATERIALIZED") {
			i = skipSpaceFrom(t, i+len("MATERIALIZED"))
		}

		if i >= len(t) || t[i] != '(' {
			return t[i:], nestedWrite
		}
		body, after, ok := balancedParenSpan(t, i)
		if !ok {
			return t[i:], nestedWrite
		}
		if statementContainsWrite(body) {
			nestedWrite = true
		}
		i = skipSpaceFrom(t, after)

		if i < len(t) && t[i] == ',' {
			i++
			continue
		}
		return t[i:], nestedWrite
	}
}

// hasCaseInsensitivePrefix reports whether s starts with prefix, ignoring
// case, and — for a keyword prefix — that the match ends at a word boundary
// (so "ASC" is not mistaken for "AS"). prefix must be all-uppercase ASCII
// letters.
func hasCaseInsensitivePrefix(s, prefix string) bool {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return false
	}
	if len(s) == len(prefix) {
		return true
	}
	return !isIdentByte(s[len(prefix)])
}

// skipIdentifier returns the index just past the identifier starting at i —
// a double-quoted name (consuming through its closing quote) or a run of
// identifier bytes. Returns i unchanged if s[i] starts neither.
func skipIdentifier(s string, i int) int {
	if i >= len(s) {
		return i
	}
	if s[i] == '"' {
		j := i + 1
		for j < len(s) && s[j] != '"' {
			j++
		}
		if j < len(s) {
			j++ // consume the closing quote
		}
		return j
	}
	j := i
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	return j
}

// isIdentByte reports whether b can appear in an unquoted SQL identifier.
func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// skipSpaceFrom returns the index of the first non-whitespace byte in s at
// or after i.
func skipSpaceFrom(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// diffWrite compares an INSERT/UPDATE/DELETE by success or rejection under
// RLS, not by row content.
//
// This is a deliberate departure from row-set comparison, found necessary by
// running this differ against a real INSERT with an auto-generated primary
// key (S9's shape): PostgreSQL sequences are not transactional, so a rolled-
// back re-execution of the identical statement still advances the sequence,
// and the RETURNING row's generated id differs from the first execution's
// even when the write is genuinely tenant-consistent. Comparing full rows
// would make every write to a table with any non-deterministic output column
// (a serial id, a DEFAULT now(), a random UUID) register as a false LEAK
// regardless of correctness. Neither TGD-US-06 AC-2 (written for SELECT's row
// sets) nor AC-6 (states the no-write constraint, not a comparison mechanism)
// specifies what a write's comparison should be — this is documented as an
// interpretation filling that gap, not something the design states outright.
//
// The comparison instead uses PostgreSQL's own WITH CHECK enforcement as the
// oracle, consistent with this whole project's approach of delegating
// correctness to PostgreSQL rather than reimplementing its logic: if the
// unrestricted write succeeds but the identical write under RLS bound to the
// query's own claimed tenant is REJECTED specifically for a row-security
// violation, the application is writing data inconsistent with its own
// claimed tenant scope — a write-side leak. If both succeed, or both fail for
// an unrelated reason (e.g. a check constraint), the write is consistent.
//
// TGD-BL-40: a Safe write is flagged Vacuous under the same semantics as the
// read path (differ.Result.Vacuous's doc comment) — attribution succeeded
// and the comparison was genuinely made, but it demonstrated nothing because
// neither side actually touched a row. execRolledBack's RowsAffected count
// makes this observable regardless of whether sqlText has a RETURNING
// clause, closing the gap runRolledBack's row-set-only view of a write left
// open (TGD-BL-37 found 44 real DeleteOldWorkspaceAgentStats SAFEs this
// exact way, by hand, before this instrumentation existed to catch it
// automatically).
func diffWrite(ctx context.Context, probeDB, restrictedDB *sql.DB, tenant, sqlText string, args []any) Result {
	unrestrictedAffected, unrestrictedErr := execRolledBack(ctx, probeDB, sqlText, args)
	if unrestrictedErr != nil {
		return Result{Verdict: Unattributable,
			Reason: fmt.Sprintf("unrestricted re-execution failed: %v", unrestrictedErr)}
	}

	restrictedAffected, restrictedErr := execRolledBackAsTenant(ctx, restrictedDB, tenant, sqlText, args)
	switch {
	case restrictedErr == nil:
		return Result{Verdict: Safe, Tenant: tenant,
			Vacuous: unrestrictedAffected == 0 && restrictedAffected == 0}
	case isRowSecurityViolation(restrictedErr):
		return Result{Verdict: Leak, Tenant: tenant, WithheldRows: 1,
			Reason: fmt.Sprintf("write succeeded unrestricted but was rejected by row-level "+
				"security for tenant %q: %v", tenant, restrictedErr)}
	default:
		return Result{Verdict: Unattributable,
			Reason: fmt.Sprintf("restricted re-execution failed for a reason unrelated to "+
				"row-level security: %v", restrictedErr)}
	}
}

// isRowSecurityViolation checks for PostgreSQL's documented row-level-security
// rejection message. SQLSTATE 42501 alone is not sufficient to identify this
// case — insufficient_privilege is shared with ordinary permission errors —
// so the check is on the specific, stable message PostgreSQL uses for this
// condition.
func isRowSecurityViolation(err error) bool {
	return strings.Contains(err.Error(), "row-level security")
}
