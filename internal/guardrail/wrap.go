package guardrail

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/differ"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// ErrNoTenantContext is returned for every query on a wrapped DB when ctx
// carries no tenant (TGD-US-12 AC-3) — checked before the query's own text
// is even looked at, since a query that turns out to be harmless does not
// excuse the caller from propagating context correctly; the rule is
// "context absent" -> block, unconditionally, not "block unless we happen
// not to need it this time."
var ErrNoTenantContext = errors.New("guardrail: no tenant in context — failing closed")

// ErrUnscopedQuery is returned when a query touches a declared scoped table
// with no comparison against its tenant column at all (TGD-US-12 AC-1).
var ErrUnscopedQuery = errors.New("guardrail: query touches a scoped table with no tenant predicate")

// ErrWrongTenant is returned when a query's own claimed tenant value
// disagrees with the request context's — L5 detection, TGD-US-12 AC-2, the
// one thing no other tier can do.
var ErrWrongTenant = errors.New("guardrail: query's tenant does not match the request context")

// ErrUnattributable is returned when a scoped table is referenced but the
// query's intended tenant could not be determined at all (a subquery-
// computed value, conflicting joins, an unresolved parameter) — failed
// closed for the same reason ErrNoTenantContext is: TGD-US-12 AC-3's
// "absence of context is never treated as permission" extends to "inability
// to determine intent is never treated as permission."
var ErrUnattributable = errors.New("guardrail: could not determine the query's intended tenant")

// DB wraps a *sql.DB, enforcing TGD-FR-13/TGD-US-12 on every query issued
// through it. It deliberately does NOT embed *sql.DB: embedding would
// promote every one of *sql.DB's own methods unchanged, letting a caller
// reach Query/Exec/Prepare directly and bypass the guardrail entirely by
// accident — the single most important thing for this type to get right.
// Only the methods below exist; anything not listed is not wrapped and not
// reachable through DB at all (see the package doc and this type's own
// "Not wrapped" note for what that excludes).
//
// Not wrapped, named rather than silently unsupported: PrepareContext (and
// Prepare), and the non-Context Query/Exec/QueryRow. A prepared statement's
// argument values are not known until Exec/Query is called on the *already-
// prepared* statement, by which point this type has lost the chance to
// check them before the PREPARE itself reaches the database — supporting
// it correctly needs a wrapped Stmt type this slice did not build. The
// non-Context methods are Go's own thin wrappers around
// context.Background(); omitting them here rather than reimplementing that
// forwarding is deliberate, not an oversight — a caller wanting Tier 2
// enforcement must go through Context, and by extension must always supply
// a real one, tightening rather than loosening the fail-closed rule.
type DB struct {
	inner *sql.DB
	rels  []schema.Relation
}

// Wrap returns a *DB enforcing TGD-US-12 against every query, using the
// FULL policy — every relation infer classified, Scoped, Unscoped and
// Unclassifiable alike, not merely the Scoped subset (TGD-BL-43) — the
// same policy file infer/verify/audit already use (schema.ReadPolicy), so
// Tier 1 and Tier 2 agree by construction about which tables are scoped
// rather than maintaining two separate, driftable declarations of it.
//
// Passing only policy.Scoped() here was TGD-BL-43's own defect: CheckTenant
// (internal/differ/tier2.go) needs the Unscoped and Unclassifiable
// relations too, to tell "this name resolves to a real, declared-harmless
// table" apart from "this name resolves to nothing the policy has ever
// classified at all" (a function, or a view/table added since the policy
// was last generated) — the second case must fail closed, and doing that
// requires knowing the full policy, not just its Scoped slice.
func Wrap(db *sql.DB, policy *schema.Policy) *DB {
	return &DB{inner: db, rels: policy.Relations}
}

// check is every wrapped method's shared gate: ctx must carry a tenant
// (AC-3), and differ.CheckTenant's verdict on (query, args, that tenant)
// must be Safe. A non-Safe verdict returns an error that never touches
// db.inner at all — a blocked query makes zero contact with the database,
// which is what "a runtime block, not a report" means concretely.
func (g *DB) check(ctx context.Context, query string, args []any) error {
	tenant, ok := TenantFromContext(ctx)
	if !ok {
		return ErrNoTenantContext
	}
	params, err := paramsFromArgs(args)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnattributable, err)
	}
	r := differ.CheckTenant(query, g.rels, params, tenant)
	switch r.Verdict {
	case differ.Safe:
		return nil
	case differ.Leak:
		if r.Tenant != "" {
			return fmt.Errorf("%w: %s", ErrWrongTenant, r.Reason)
		}
		return fmt.Errorf("%w: %s", ErrUnscopedQuery, r.Reason)
	default:
		return fmt.Errorf("%w: %s", ErrUnattributable, r.Reason)
	}
}

// QueryContext enforces TGD-US-12 before running query; on a Safe verdict
// it delegates to the wrapped *sql.DB unchanged.
func (g *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := g.check(ctx, query, args); err != nil {
		return nil, err
	}
	return g.inner.QueryContext(ctx, query, args...)
}

// ExecContext enforces TGD-US-12 before running query; on a Safe verdict it
// delegates to the wrapped *sql.DB unchanged.
func (g *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := g.check(ctx, query, args); err != nil {
		return nil, err
	}
	return g.inner.ExecContext(ctx, query, args...)
}

// QueryRowContext enforces TGD-US-12 before running query. Unlike
// QueryContext/ExecContext, it cannot simply return an error: *sql.Row has
// no exported constructor outside database/sql, so there is no way to hand
// back a *sql.Row carrying a preset error without executing something. Row
// (this package's own small analogue, below) is returned instead, with the
// blocked verdict's error deferred to its Scan the same way *sql.Row defers
// a real query error to Scan — the caller-visible shape is identical to
// *sql.Row's own contract, just not literally that concrete type.
func (g *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if err := g.check(ctx, query, args); err != nil {
		return &Row{err: err}
	}
	return &Row{row: g.inner.QueryRowContext(ctx, query, args...)}
}

// Row mirrors *sql.Row's Scan contract for QueryRowContext's blocked case —
// see QueryRowContext's own doc comment for why this type exists instead of
// a real *sql.Row.
type Row struct {
	row *sql.Row
	err error
}

// Scan behaves exactly as *sql.Row.Scan: if the guardrail blocked the
// query, that error is returned immediately, matching how *sql.Row defers
// its own underlying query error to Scan rather than to QueryRowContext's
// return value.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

// paramsFromArgs converts native Go query arguments to capture.Param,
// reusing differ.ExtractTenant's own input type. Every value is
// trivially Known here — unlike wire-protocol capture (internal/capture),
// which must decode bytes off the wire and can genuinely fail to, this
// package has the real, already-typed Go value in process, so there is no
// unresolved-parameter case except the driver.Valuer error path below.
func paramsFromArgs(args []any) ([]capture.Param, error) {
	params := make([]capture.Param, len(args))
	for i, a := range args {
		v, err := normalizeArg(a)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		if v == nil {
			params[i] = capture.Param{IsNull: true, Known: true}
			continue
		}
		params[i] = capture.Param{Known: true, Text: fmt.Sprint(v)}
	}
	return params, nil
}

// normalizeArg resolves a driver.Valuer (sql.NullString and similar
// wrapper types all implement it) to its underlying value before
// stringifying — without this, a NULL sql.NullString would stringify as
// "{ false}" instead of comparing as NULL, and a valid one as "{acme
// true}" instead of "acme".
func normalizeArg(a any) (any, error) {
	if v, ok := a.(driver.Valuer); ok {
		return v.Value()
	}
	return a, nil
}
