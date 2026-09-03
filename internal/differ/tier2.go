package differ

import (
	"fmt"
	"strings"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// CheckTenant is Tier 2's own decision (TGD-FR-13/TGD-US-12): does sqlText's
// own claimed tenant (via ExtractTenant, the SAME attribution logic Tier 1
// uses) match intendedTenant — the one fact Tier 1 never has, supplied here
// out-of-band from request/session context instead of read out of the
// query's own predicate. This is the whole of what makes L5 ("correct
// predicate, wrong tenant value") detectable at Tier 2 and invisible at
// Tier 1 (TGD-US-06's own AC-4/AC-5): Tier 1 has no independent ground
// truth to compare the extracted value against, so a self-consistent wrong
// value passes; Tier 2 does, so it does not.
//
// relations must be the FULL policy — every relation infer classified,
// Scoped, Unscoped and Unclassifiable alike — not merely the Scoped subset.
// TGD-BL-43: an earlier version of this function accepted only Scoped
// relations and asked "does the query name a Scoped table at all?" — a
// table/view/function it could not find an answer to (because it was never
// passed in, not because it was genuinely irrelevant) defaulted to Safe,
// which let U2's shape (a function wrapping real access to a Scoped table)
// through completely uninspected. See resolveReferences's own doc comment
// for the fix: every relation-shaped name the query mentions must resolve
// to something the FULL policy actually classified, or the query is
// fail-closed — "unknown is denied," not "unknown is assumed harmless."
//
// Verdict is reused from Diff's own three-way model (Safe/Leak/
// Unattributable) so both tiers speak one vocabulary and the fixture corpus
// (corpus_test.go) can assert against both with a single type — but
// CheckTenant never re-executes anything: no probe database, no RLS, no
// second query against anything. See internal/guardrail's package doc for
// the full cost/coverage argument behind choosing this mechanism over
// re-executing under RLS at runtime, including the corpus's own measured
// consequence: this mechanism closes L5 exactly (its reason for existing)
// but does NOT close every fixture Tier 1's row-set re-execution closes via
// a mechanism this function has no equivalent of — reported, not hidden,
// wherever the actual corpus run diverges from design §7.4's predicted set.
func CheckTenant(sqlText string, relations []schema.Relation, params []capture.Param, intendedTenant string) Result {
	res := resolveReferences(sqlText, relations)
	if res.blocked {
		// Unattributable, not Leak: "couldn't determine what this reads"
		// is exactly what an unresolvable or Unclassifiable relation is —
		// distinct from Leak's "determined, and it's wrong." The wrapper
		// (internal/guardrail) blocks on either verdict identically; this
		// distinction is for the corpus/report vocabulary, not for
		// enforcement behavior, and it is also what lets U2's own fixture
		// land exactly where design §7.4 originally predicted it would
		// (UNATTRIBUTABLE), rather than merely "blocked, but mislabelled."
		return Result{Verdict: Unattributable, Reason: res.reason}
	}
	if !res.anyScoped {
		// Every relation-shaped name the query mentions (there may be none
		// at all, e.g. SELECT now()) resolved to something the policy
		// declared Unscoped, or nothing was named at all — either way,
		// there is nothing here for a guardrail to enforce.
		return Result{Verdict: Safe}
	}

	attr := ExtractTenant(sqlText, relations, params)
	switch attr.Kind {
	case AttrUnattributable:
		// A scoped table IS referenced, but the value could not be pinned
		// down at all (subquery, conflicting join values, an unresolved
		// parameter). Fail closed: TGD-US-12 AC-3's "absence of context is
		// never treated as permission" extends naturally to "inability to
		// determine intent is never treated as permission" for an
		// enforcement mechanism — an audit tool can afford to report
		// Unattributable and move on; a guardrail cannot afford to guess.
		return Result{Verdict: Unattributable, Reason: attr.Reason}
	case AttrNoPredicate:
		// TGD-US-12 AC-1, directly: a scoped table with no comparison
		// against its tenant column at all.
		return Result{Verdict: Leak,
			Reason: "query touches a declared scoped table with no comparison against its tenant column"}
	case AttrResolved:
		if attr.Value != intendedTenant {
			// TGD-US-12 AC-2 — L5, exactly: the query's own claimed value
			// disagrees with the one independent source of truth Tier 1
			// never had.
			return Result{Verdict: Leak, Tenant: attr.Value,
				Reason: fmt.Sprintf("query's tenant predicate (%q) does not match the request context's tenant (%q)",
					attr.Value, intendedTenant)}
		}
		return Result{Verdict: Safe, Tenant: attr.Value}
	default:
		// Unreachable: Attribution has exactly three values, all handled
		// above. Named rather than silently falling through to a nil
		// Result a caller might mistake for Safe.
		return Result{Verdict: Unattributable, Reason: "unrecognised attribution kind"}
	}
}

// referenceResolution is resolveReferences' own result: whether the query
// must be blocked outright (an unresolvable or Unclassifiable relation),
// and if not, whether any Scoped relation was among what it named.
type referenceResolution struct {
	blocked   bool
	reason    string
	anyScoped bool
}

// resolveReferences is TGD-BL-43's fix: every FROM/JOIN/INTO/UPDATE target
// sqlText names (via referencedRelationNames — real relation names only,
// never an alias, unlike referencedTables) is looked up against the FULL
// policy. Three outcomes per name, and the query blocks the instant ANY one
// of them is the first two:
//
//   - Not found in relations at all: a function call (U2's own shape —
//     "SELECT * FROM scoped_summary()", where scoped_summary is never a
//     relation infer's schema query can see at all, since it queries
//     pg_class, not pg_proc) or a table/view the policy was built against a
//     different schema state than the one now running. Blocked: this
//     mechanism has no way to know what such a name actually reads, and
//     "unknown is denied," not "unknown is probably fine," is the whole of
//     what fail-closed means (the design doc's own §9/TGD-US-12 framing).
//   - Found, Class Unclassifiable: infer already declined to classify this
//     relation — EVERY view (Classify's own unconditional rule: "row-level
//     security cannot be attached to a view; its tenancy follows the
//     underlying tables"), every materialized view, every foreign table,
//     and any base table with an ambiguous multi-candidate tenant column.
//     schema.go's own package doc states the discipline this reuses:
//     "Unclassifiable... is never equivalent to Unscoped." Blocked for the
//     identical reason.
//   - Found, Class Scoped or Unscoped: resolved. Scoped relations set
//     anyScoped; Unscoped ones contribute nothing further to check.
//
// A query naming NO relation-shaped target at all (SELECT now(), SELECT 1)
// resolves with anyScoped=false and blocked=false — there is nothing to
// resolve, which is a different, safe case from resolving to something
// unrecognised.
func resolveReferences(sqlText string, relations []schema.Relation) referenceResolution {
	byName := make(map[string]schema.Relation, len(relations))
	for _, r := range relations {
		byName[strings.ToLower(r.Name)] = r
	}
	ctes := cteNames(sqlText)

	var res referenceResolution
	for name := range referencedRelationNames(sqlText) {
		if ctes[name] {
			// A CTE's own name (WITH scoped AS (...) SELECT * FROM scoped)
			// is never a real relation and could never appear in any
			// policy — resolving it against one and failing closed would
			// block ordinary, harmless CTE usage, not the class of bypass
			// this function exists to catch. See cteNames' own doc comment.
			continue
		}
		r, found := byName[name]
		if !found {
			return referenceResolution{blocked: true, reason: fmt.Sprintf(
				"query references %q, which does not resolve to any relation the policy classified — "+
					"an unresolvable relation fails closed rather than being assumed harmless", name)}
		}
		switch r.Class {
		case schema.Unclassifiable:
			return referenceResolution{blocked: true, reason: fmt.Sprintf(
				"query references %q, which the policy could not classify (%s) — "+
					"fails closed rather than being assumed unscoped", name, r.Reason)}
		case schema.Scoped:
			res.anyScoped = true
		}
	}
	return res
}

// cteNames returns every name declared by a leading WITH clause in sqlText,
// including inside a CTE's own body should IT begin with a further nested
// WITH — found necessary, not hypothetical: an early version of
// resolveReferences (TGD-BL-43) had no notion of this at all, and
// misclassified "WITH scoped AS (SELECT * FROM invoices WHERE tenant_id =
// $1) SELECT * FROM scoped" (S4's own corpus fixture) and the equivalent
// aggregate shape (L4) as referencing an unresolvable relation — "scoped"/
// "agg", the CTE's own name — since a CTE alias can never appear in any
// policy (it is not a real relation PostgreSQL's catalog would ever list;
// infer's own relationQuery selects from pg_class, which has no notion of
// a query-local CTE at all) and so could never resolve, blocking ordinary,
// harmless CTE usage wholesale rather than catching the class of bypass
// TGD-BL-43 exists to close. Reuses skipWithClause's own balanced-paren
// parsing helpers (skipIdentifier, balancedParenSpan, hasCaseInsensitivePrefix,
// skipSpaceFrom, all differ.go) rather than a second, divergent parser for
// the same WITH-list grammar.
func cteNames(sqlText string) map[string]bool {
	names := map[string]bool{}
	t := strings.TrimSpace(sqlText)
	for strings.HasPrefix(t, "--") {
		nl := strings.IndexByte(t, '\n')
		if nl < 0 {
			return names
		}
		t = strings.TrimSpace(t[nl+1:])
	}
	collectCTENames(t, names)
	return names
}

// collectCTENames does the actual walk, recursing into each CTE's own body
// in case it begins with a further nested WITH — the same recursive shape
// statementContainsWrite already uses for the identical grammar, applied
// here to collect names instead of detecting a write.
func collectCTENames(t string, names map[string]bool) {
	if !hasCaseInsensitivePrefix(t, "WITH") {
		return
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
			return
		}

		nameStart := i
		nameEnd := skipIdentifier(t, i)
		if nameEnd == nameStart {
			return
		}
		names[strings.ToLower(strings.Trim(t[nameStart:nameEnd], `"`))] = true
		i = skipSpaceFrom(t, nameEnd)

		if i < len(t) && t[i] == '(' {
			_, after, ok := balancedParenSpan(t, i)
			if !ok {
				return
			}
			i = skipSpaceFrom(t, after)
		}

		if !hasCaseInsensitivePrefix(t[i:], "AS") {
			return
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
			return
		}
		body, after, ok := balancedParenSpan(t, i)
		if !ok {
			return
		}
		collectCTENames(strings.TrimSpace(body), names)
		i = skipSpaceFrom(t, after)

		if i < len(t) && t[i] == ',' {
			i++
			continue
		}
		return
	}
}
