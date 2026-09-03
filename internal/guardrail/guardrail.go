// Package guardrail implements Tier 2 (TGD-FR-13/TGD-US-12): a Go library
// the target application imports, wrapping its own *sql.DB and returning an
// error on an unscoped or wrong-tenant query instead of letting it run. Not
// a report — a runtime block, on the calling application's own hot path.
//
// # Enforcement mechanism, argued before building — the choice this package
// rests on, and what it costs
//
// Three mechanisms were real candidates:
//
//  1. Reuse the RLS oracle at runtime: re-execute every query a second time
//     under a synthesised, RLS-enforcing role bound to the context's tenant
//     (Tier 1's own Diff, called from the hot path instead of offline).
//  2. A predicate check before execution: extract the query's own claimed
//     tenant value (ExtractTenant, already built, already TGD-BL-42-hardened)
//     and compare it against the context's tenant — no re-execution, no
//     second query, nothing touches the database for a blocked query.
//  3. Something else: e.g. a middleware that only sets the RLS session
//     variable per request and leans on RLS itself, already deployed in
//     production, to filter rows.
//
// **(3) was rejected first, on the ACs' own wording.** TGD-US-12 AC-1 requires
// a blocked query to "return an error and no rows." Real RLS enforcement does
// not error — a wrongly-scoped SELECT under RLS just silently returns fewer
// or zero rows, which is real defense-in-depth but is indistinguishable from
// "no matching record" to the caller, and satisfies none of AC-1/AC-2/AC-3's
// literal requirement of an *error*. It also requires RLS already deployed
// and correct in the production database, which is Tier 1's whole proof
// burden (A1–A4) repeated at Tier 2, not a "days, not point-and-shoot"
// simplification (design §9's own framing for Tier 2's cost) but the exact
// same cost paid twice.
//
// **(2) was chosen over (1) on cost, on the hot-path constraint, and on what
// TGD-US-12's ACs actually describe.** (1) doubles every wrapped query's
// database round trips — every SELECT becomes two, every write becomes two —
// which is disqualifying for a library meant to sit transparently in front
// of an application's entire query volume, not an offline batch audit run
// once against a captured file. It also requires the SAME production RLS
// deployment (3) does, for every wrapped connection, all the time. (2) costs
// a single in-process regex pass per query — no I/O, no second connection,
// no RLS anywhere near production — and AC-1/AC-2's wording ("returns an
// error... instead of" the query running at all) reads far more naturally as
// a pre-execution check than as "run it, then diff two executions after the
// fact," which is what (1) actually is. (2) also directly reuses
// ExtractTenant, the SAME code Tier 1 already proved and just spent a
// session fixing (TGD-BL-42) — sharing the attribution logic, not
// duplicating it, closing the design doc's own open question ("whether
// Tier 2's driver wrapper shares the differ with Tier 1 or reimplements it")
// with a concrete, load-bearing answer: shares ExtractTenant, does NOT share
// Diff/the RLS re-execution.
//
// **What (2) costs, stated plainly rather than left to be discovered by a
// failing test.** ExtractTenant is a text heuristic, not a SQL parser or a
// row-set comparison — it finds A comparison naming the tenant column
// ANYWHERE in the statement text, with no understanding of the statement's
// boolean structure. A query whose predicate correctly NAMES the intended
// tenant but is defeated by an OR, or whose join scopes one side and leaves
// another scoped table's own predicate to be found (by this same alias-
// blind text scan) attached to a different table's comparison, will resolve
// to the SAME (correct-looking) value this mechanism checks against context
// — and pass. Tier 1 catches those shapes not through attribution but
// through Diff's actual RLS row-set re-execution, which (2) deliberately
// does not have. This is not hypothetical: running design §7's own 24-
// fixture corpus through CheckTenant (internal/differ/tier2.go) is what
// found it — see corpus_test.go's TestCorpus_Tier2ExactSetEquality and its
// own doc comment for exactly which fixtures diverge from §7.4's predicted
// Tier 2 set, and why, reported as a design finding rather than reconciled
// away by building a second mechanism to catch what (2) alone cannot.
//
// **Why (2) is still the right choice despite that gap.** L5 — a correct
// predicate naming the wrong tenant — is the entire, stated reason Tier 2
// exists (TGD-US-12's own story; design §9.4/§7.4). (2) closes L5 exactly,
// at zero marginal query cost, without any production RLS dependency. The
// fixtures it does not close (documented, not hidden) are ones a fail-closed
// Tier 2 wrapper never claims to catch — TGD-FR-13's contract is "an
// unscoped or wrong-tenant query is blocked," and every fixture (2) misses
// still HAS a syntactically well-formed, correctly-valued predicate; the
// defect is in the query's boolean structure around it, a different and
// narrower problem than what this package is built to solve, best closed
// (if ever) by mechanism (1) as a distinct, heavier-weight, opt-in addition
// — not by abandoning (2)'s cost profile for the whole corpus's sake.
package guardrail

import "context"

// tenantContextKey is unexported so no other package can set or read this
// context value except through WithTenant/TenantFromContext — the same
// discipline a real security-relevant context value needs regardless of how
// small this package otherwise is.
type tenantContextKey struct{}

// WithTenant returns a copy of ctx carrying tenant as the request's intended
// tenant — the one fact Tier 1 never has and Tier 2 exists to supply
// (TGD-US-12). The caller is responsible for setting this from a source it
// actually trusts (an authenticated session, not a request parameter) —
// this package has no way to verify that and does not claim to.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantContextKey{}, tenant)
}

// TenantFromContext returns the tenant WithTenant attached to ctx, and
// whether one was present and non-empty. An empty string is deliberately
// treated the same as absent (ok=false) — TGD-US-12 AC-3's fail-closed rule
// must not be defeatable by propagating a present-but-empty tenant value.
func TenantFromContext(ctx context.Context) (string, bool) {
	v, _ := ctx.Value(tenantContextKey{}).(string)
	return v, v != ""
}
