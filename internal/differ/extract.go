// Package differ re-executes a captured query under RLS and compares the row
// sets to prove or refute an isolation leak.
//
// It is not a SQL parser, and does not try to be one — PostgreSQL remains the
// authority on what a query means and does. This package's only job before
// re-execution is finding A PLAUSIBLE tenant value to bind the restricted
// session to; the diff itself, enforced by RLS on re-execution, is what
// actually proves a leak, regardless of how sloppily the captured query's own
// WHERE clause was written. That division of labour is why most fixture
// shapes (joins, ORs, subquery-in-IN, missing predicates) need no special
// handling in extraction at all: RLS applies transparently on top of whatever
// the query already does.
package differ

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

// Attribution classifies what ExtractTenant could determine.
type Attribution int

const (
	// AttrResolved: a usable tenant value was found. Value holds it.
	AttrResolved Attribution = iota
	// AttrNoPredicate: a declared scoped table is referenced, but no
	// comparison naming its tenant column yielded a usable value — including
	// a self-referential tautology (tenant_id = tenant_id) and non-parameter
	// right-hand sides this heuristic does not attempt to resolve. Diff must
	// treat this as "no tenant claimed," not as Unattributable: re-executing
	// with no session tenant set makes RLS deny every row, and any row the
	// unrestricted side still returns is then a proven leak.
	AttrNoPredicate
	// AttrUnattributable: the tool genuinely cannot determine a tenant, and
	// must never guess. Reason explains why.
	AttrUnattributable
)

// TenantAttribution is ExtractTenant's result.
type TenantAttribution struct {
	Kind   Attribution
	Value  string // meaningful only when Kind == AttrResolved
	Reason string // meaningful only when Kind == AttrUnattributable
}

// paramRef matches $N. tenantCompare matches "<column>" optionally prefixed by
// a table alias and optionally cast, compared with '=' to whatever follows —
// captured separately so the caller can classify the right-hand side.
var paramRef = regexp.MustCompile(`^\$(\d+)$`)

// pgIdent is a single PostgreSQL type-name word: an identifier, optionally
// dotted (schema-qualified, e.g. public.chat_mode). Used to build up
// multi-word type names (timestamp with time zone, double precision) as a
// sequence of these.
const pgIdent = `[A-Za-z_][A-Za-z0-9_.]*`

// castSuffix matches zero or more `::type` casts trailing a value — TGD-BL-42:
// sqlc's default codegen casts every positional VALUES/comparison parameter
// ($N::uuid), and this must not be mistaken for "not a parameter reference"
// the way an anchored, cast-naive match would. Handles: whitespace around
// `::` ($1 :: uuid, as well as $1::uuid), multi-word type names (timestamp
// with time zone, double precision), array types (bigint[], text[]), and
// chained/nested casts ($1::uuid::text) via the `*` repetition — each
// individually optional, together covering every cast shape seen in a real
// coder capture (TGD-BL-42's own evidence).
const castSuffix = `(?:\s*::\s*` + pgIdent + `(?:\s+` + pgIdent + `)*(?:\s*\[\s*\])?)*`

// paramRefCastChain matches $N followed by castSuffix, anchored full-string:
// the "$N" or "$N::type[::type...]" shape.
var paramRefCastChain = regexp.MustCompile(`^\$(\d+)` + castSuffix + `$`)

// paramRefCastFunc matches the SQL-standard CAST($N AS type) form, anchored
// full-string, case-insensitive. TGD-BL-42's fix covers this alongside the
// far more common `::` shape since both silently discarded a real,
// resolvable tenant value the same way before this fix.
var paramRefCastFunc = regexp.MustCompile(`(?i)^CAST\s*\(\s*\$(\d+)\s+AS\s+` +
	pgIdent + `(?:\s+` + pgIdent + `)*(?:\s*\[\s*\])?\s*\)$`)

// resolveParamRef reports which positional parameter (1-based, as written in
// the SQL — caller subtracts 1) a value expression refers to, tolerating a
// trailing type cast in either PostgreSQL shorthand (::type, including
// chained/array/multi-word forms) or SQL-standard CAST(...) syntax
// (TGD-BL-42). ok is false for anything else — a bare identifier, a
// subquery, a function call, or an expression this heuristic does not
// attempt to resolve — the same conservative "don't guess" fallback the
// callers already apply on a false ok.
func resolveParamRef(s string) (paramNumber int, ok bool) {
	if m := paramRefCastChain.FindStringSubmatch(s); m != nil {
		return atoiSafe(m[1]), true
	}
	if m := paramRefCastFunc.FindStringSubmatch(s); m != nil {
		return atoiSafe(m[1]), true
	}
	return 0, false
}

func tenantCompareRegex(column string) *regexp.Regexp {
	// (?:\w+\.)?                optional table alias, e.g. i.
	// column                    the tenant column name, verbatim
	// (?:::\w+)?                optional type cast, e.g. ::text
	// \s*=\s*                   the comparison
	// (CAST\(...\)|\$\d+|\w+|\([^)]*) the right-hand side: a CAST(...)
	//                           expression (captured whole, so
	//                           resolveParamRef can see the $N inside it —
	//                           TGD-BL-42), a parameter (any trailing ::cast
	//                           is left unconsumed by \$\d+ and handled by
	//                           resolveParamRef, not by this regex), a bare
	//                           identifier (self-reference/other column), or
	//                           the start of a parenthesised subquery
	pattern := `(?i)(?:\w+\.)?` + regexp.QuoteMeta(column) +
		`(?:::\w+)?\s*=\s*(CAST\s*\([^)]*\)|\$\d+|\w+|\([^)]*)`
	return regexp.MustCompile(pattern)
}

// tableRef matches FROM/JOIN/INTO/UPDATE <name>, optionally quoted and
// schema-qualified, plus an optional trailing alias (`AS <alias>` or a bare
// `<alias>`), used only to decide whether a declared scoped table is
// referenced at all — not to understand the query's structure. INTO and
// UPDATE are needed for INSERT/UPDATE statements, which name their target
// table that way rather than with FROM.
//
// The alias group exists for TGD-BL-34: coder's GetWorkspaceBuildByID /
// GetTemplateVersionByID query a VIEW aliased to the real scoped table's own
// name — `FROM workspace_build_with_user AS workspace_builds` — precisely so
// downstream code can keep referring to it as if it were the base table.
// Matching only the source name (the view) missed this entirely; matching
// the alias too catches it. A handful of SQL keywords that can legally
// follow a table name in this position — WHERE, ON, USING, SET (an UPDATE's
// next clause) — are excluded so they are never mistaken for an alias.
var tableRef = regexp.MustCompile(
	`(?i)\b(?:from|join|into|update)\s+"?(?:\w+\.)?"?(\w+)"?` +
		`(?:\s+(?:as\s+)?"?(\w+)"?)?`)

var aliasStopWords = map[string]bool{
	"where": true, "on": true, "using": true, "set": true,
	"as": true, "join": true, "inner": true, "left": true, "right": true,
	"full": true, "outer": true, "cross": true, "natural": true,
	"order": true, "group": true, "limit": true, "offset": true,
	"returning": true, "values": true, "for": true,
}

// referencedTables returns the lowercase table names mentioned after FROM or
// JOIN anywhere in sqlText — the source name, and, when present, its alias
// (TGD-BL-34): either can name a declared scoped table.
func referencedTables(sqlText string) map[string]bool {
	out := map[string]bool{}
	for _, m := range tableRef.FindAllStringSubmatch(sqlText, -1) {
		out[strings.ToLower(m[1])] = true
		if alias := strings.ToLower(m[2]); alias != "" && !aliasStopWords[alias] {
			out[alias] = true
		}
	}
	return out
}

// referencedRelationNames returns only the real names tableRef matched after
// FROM/JOIN/INTO/UPDATE — never an alias. Distinct from referencedTables,
// whose flat name-or-alias map is right for "is X mentioned by name or
// alias" (ExtractTenant's use) but wrong for CheckTenant's own resolution
// (TGD-BL-43): iterating referencedTables' keys against a policy's relation
// names would misreport every aliased table as "unresolvable," since an
// alias is never itself a relation name.
func referencedRelationNames(sqlText string) map[string]bool {
	out := map[string]bool{}
	for _, m := range tableRef.FindAllStringSubmatch(sqlText, -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

// ReferencesScopedTable reports whether sqlText names, by table or alias, at
// least one relation relations declares Scoped.
//
// TGD-BL-43: CheckTenant (tier2.go) no longer uses this as its own gate — a
// query naming NO scoped table by this check could still be routing through
// a function or view the policy never classified at all (U2's shape),
// which this function has no way to see, since it only ever looks AT the
// Scoped subset, never at what a name failed to resolve against. CheckTenant
// now uses resolveReferences (tier2.go), which requires the FULL policy and
// fails closed on an unresolvable or Unclassifiable name instead of
// defaulting to "nothing to enforce." Kept exported and tested here as a
// correct, narrower utility in its own right — "does this text mention a
// declared Scoped table at all" is still a real, useful question — just not
// the one a fail-closed guardrail can safely ask on its own.
func ReferencesScopedTable(sqlText string, relations []schema.Relation) bool {
	referenced := referencedTables(sqlText)
	for _, r := range relations {
		if r.Class == schema.Scoped && referenced[strings.ToLower(r.Name)] {
			return true
		}
	}
	return false
}

// ExtractTenant finds a plausible tenant value for a captured query, given the
// tenancy policy (which tables are scoped, and by which column) and the
// query's resolved parameters.
//
// If ANY parameter used anywhere in the query is not Known (its value could
// not be decoded — most commonly an undecoded binary parameter), the result is
// AttrUnattributable regardless of position: a query cannot be safely
// re-executed at all without every one of its real parameter values, since
// substituting a guess for an unrelated parameter could silently change what
// the query matches, producing an untrustworthy re-execution of a query that
// never actually ran that way.
func ExtractTenant(sqlText string, relations []schema.Relation, params []capture.Param) TenantAttribution {
	var unrecoverable []string
	for i, p := range params {
		if !p.IsNull && !p.Known {
			unrecoverable = append(unrecoverable, fmt.Sprintf("$%d", i+1))
		}
	}
	if len(unrecoverable) > 0 {
		return TenantAttribution{Kind: AttrUnattributable,
			Reason: fmt.Sprintf("parameter position(s) %s could not be decoded; "+
				"the query cannot be safely re-executed without every real value",
				strings.Join(unrecoverable, ", "))}
	}

	if attr, ok := extractFromInsert(sqlText, relations, params); ok {
		return attr
	}

	referenced := referencedTables(sqlText)
	var scopedReferenced []schema.Relation
	for _, r := range relations {
		if r.Class != schema.Scoped {
			continue
		}
		if referenced[strings.ToLower(r.Name)] {
			scopedReferenced = append(scopedReferenced, r)
		}
	}
	if len(scopedReferenced) == 0 {
		return TenantAttribution{Kind: AttrUnattributable,
			Reason: "no declared scoped table is referenced by name in this query " +
				"(a function call or view can hide table access from a text scan)"}
	}

	var resolvedValue string
	haveResolved := false
	anyComparisonFound := false

	for _, r := range scopedReferenced {
		re := tenantCompareRegex(r.TenantColumn)
		matches := re.FindAllStringSubmatch(sqlText, -1)
		for _, m := range matches {
			rhs := m[1]
			anyComparisonFound = true

			if strings.HasPrefix(rhs, "(") {
				return TenantAttribution{Kind: AttrUnattributable,
					Reason: "tenant value is computed by a subquery, not present as a literal"}
			}

			paramNumber, ok := resolveParamRef(rhs)
			if !ok {
				// A bare identifier: a self-referential tautology
				// (tenant_id = tenant_id) or some other column. Neither
				// yields a usable value; treated as no comparison found for
				// this occurrence, not as Unattributable.
				continue
			}

			idx := paramNumber - 1
			if idx < 0 || idx >= len(params) {
				// A parameter index the capture layer never recorded — cannot
				// happen for a genuinely resolved Bind, but handled rather
				// than panicking on malformed input.
				return TenantAttribution{Kind: AttrUnattributable,
					Reason: "tenant comparison references a parameter position that was never captured"}
			}
			value := params[idx].Text
			if !haveResolved {
				resolvedValue = value
				haveResolved = true
				continue
			}
			if value != resolvedValue {
				return TenantAttribution{Kind: AttrUnattributable,
					Reason: "conflicting tenant values across the scoped tables this query joins"}
			}
		}
	}

	if haveResolved {
		return TenantAttribution{Kind: AttrResolved, Value: resolvedValue}
	}
	_ = anyComparisonFound // a tautology or other unresolved comparison; still AttrNoPredicate
	return TenantAttribution{Kind: AttrNoPredicate}
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// insertPrefix matches INSERT INTO <table> and the opening paren of its
// column list — but deliberately not the column list's own content or the
// VALUES list at all. Both of those are found by balancedParenSpan below
// instead of a second regex capture group, because a naive `([^)]*)` cannot
// handle a VALUES entry that is itself a function call with its own parens
// (gen_random_uuid()) — found as a real defect (TGD-BL-34) truncating the
// VALUES list at that inner paren and producing a false "different lengths"
// verdict for coder's UpsertProvisionerDaemon, which resolves cleanly once
// parsed correctly.
var insertPrefix = regexp.MustCompile(`(?is)insert\s+into\s+"?(?:\w+\.)?"?(\w+)"?\s*\(`)

// valuesPrefix matches the VALUES keyword and its list's opening paren,
// searched for only after the column list's own closing paren — so a
// "values" appearing inside a column name or default expression before that
// point is never mistaken for the real clause.
var valuesPrefix = regexp.MustCompile(`(?is)\bvalues\s*\(`)

// balancedParenSpan returns the text between the '(' at s[openIdx] and its
// matching ')' (tracking nesting depth, so an inner function call's own
// parens do not end the span early), and the index of the first byte after
// that ')'. ok is false if s[openIdx] is not '(' or no matching ')' exists.
//
// Known boundary, same as splitTopLevel's: a ')' inside a quoted string
// literal is counted as closing a paren it does not actually close. Not
// exercised by any fixture or real capture in this corpus so far — named
// here rather than silently assumed away.
func balancedParenSpan(s string, openIdx int) (content string, afterClose int, ok bool) {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '(' {
		return "", 0, false
	}
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[openIdx+1 : i], i + 1, true
			}
		}
	}
	return "", 0, false
}

// extractFromInsert returns (attribution, true) when sqlText is recognisably
// an INSERT into a declared scoped table naming that table's tenant column in
// its column list; (zero, false) for anything else, so the caller falls
// through to the WHERE-clause-oriented path unchanged.
func extractFromInsert(sqlText string, relations []schema.Relation, params []capture.Param) (TenantAttribution, bool) {
	pm := insertPrefix.FindStringSubmatchIndex(sqlText)
	if pm == nil {
		return TenantAttribution{}, false
	}
	table := strings.ToLower(sqlText[pm[2]:pm[3]])

	var rel *schema.Relation
	for i := range relations {
		if relations[i].Class == schema.Scoped && strings.ToLower(relations[i].Name) == table {
			rel = &relations[i]
			break
		}
	}
	if rel == nil {
		// Not a declared scoped table — not this function's concern; let the
		// normal path decide (it will find no scoped table referenced either,
		// since referencedTables also matches INTO, and correctly report
		// Unattributable rather than AttrNoPredicate for an unrecognised
		// table, exactly as a SELECT against an unknown table would).
		return TenantAttribution{}, false
	}

	// pm[1] is the index right after the column list's opening '(' — back up
	// one to hand balancedParenSpan the '(' itself.
	colsText, afterCols, ok := balancedParenSpan(sqlText, pm[1]-1)
	if !ok {
		return TenantAttribution{}, false
	}
	vm := valuesPrefix.FindStringIndex(sqlText[afterCols:])
	if vm == nil {
		return TenantAttribution{}, false
	}
	valsOpenIdx := afterCols + vm[1] - 1
	valsText, _, ok := balancedParenSpan(sqlText, valsOpenIdx)
	if !ok {
		return TenantAttribution{}, false
	}

	cols := splitTopLevel(colsText)
	vals := splitTopLevel(valsText)
	if len(cols) != len(vals) {
		return TenantAttribution{Kind: AttrUnattributable,
			Reason: "INSERT column list and VALUES list have different lengths"}, true
	}

	for i, c := range cols {
		if !strings.EqualFold(strings.TrimSpace(c), rel.TenantColumn) {
			continue
		}
		rhs := strings.TrimSpace(vals[i])
		if strings.HasPrefix(rhs, "(") {
			return TenantAttribution{Kind: AttrUnattributable,
				Reason: "tenant value is computed by a subquery, not present as a literal"}, true
		}
		paramNumber, ok := resolveParamRef(rhs)
		if !ok {
			// A literal or expression this heuristic does not resolve —
			// conservative fallback to "no usable value found," same as a
			// WHERE-clause comparison it cannot interpret.
			return TenantAttribution{Kind: AttrNoPredicate}, true
		}
		idx := paramNumber - 1
		if idx < 0 || idx >= len(params) {
			return TenantAttribution{Kind: AttrUnattributable,
				Reason: "tenant comparison references a parameter position that was never captured"}, true
		}
		return TenantAttribution{Kind: AttrResolved, Value: params[idx].Text}, true
	}
	return TenantAttribution{Kind: AttrNoPredicate}, true
}

// splitTopLevel splits a comma-separated list on commas that are not nested
// inside parentheses — sufficient for a column or VALUES list, which is as
// far as this heuristic goes; it does not handle a comma inside a quoted
// string literal, a boundary not exercised by any fixture in this corpus.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}
