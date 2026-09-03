package differ

import (
	"strings"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// Population is TGD-BL-33/TGD-BL-06's baselining prerequisite: a
// machine-readable classification of WHY a query came back UNATTRIBUTABLE,
// computed by the tool from data it already has (the SQL text and the
// row-level/structural-only relation split runOracleGate already computes)
// — never from parsing Reason's free text, and never from ad hoc SQL
// grepping outside the tool, which is how TGD-BL-33's one-off session
// analysis had to derive the same four buckets by hand.
//
// TGD-NFR-03's unattributable-rate ceiling exists to catch "the tool failed
// to look" — a question that is only well-posed over the population where
// attribution is even possible. Mixing in traffic the tool was never asked
// to attribute (NoDeclaredTable, NonQuery) or could not attempt for lack of
// seeded rows (StructuralOnly) dilutes that signal with things that have
// nothing to do with whether the attribution logic itself is trustworthy —
// TGD-BL-33 found this dilutes a real 27.8% attribution-failure rate down to
// a reported 90.6%. RowLevelUnattributed is the population an attribution
// ceiling should actually be measured against.
type Population string

const (
	// PopulationNoDeclaredTable: the query references no table the policy
	// declared at all (Scoped or not) — genuinely out of scope, not a
	// coverage or attribution defect. TGD-BL-33 found this is the largest
	// bucket by far in real coder traffic (most captured SQL is not about a
	// tenant-scoped table).
	PopulationNoDeclaredTable Population = "no_declared_table"
	// PopulationStructuralOnly: the query references at least one declared
	// scoped table, and every one it references is structural-only (A2/A3
	// proved, but SeedCanaries could not seed it — TGD-BL-32/§9.6). A
	// coverage gap, not an attribution gap: the differ was never given
	// permission to attribute against this table because the oracle was
	// never shown it can withhold rows there.
	PopulationStructuralOnly Population = "structural_only"
	// PopulationRowLevelUnattributed: the query references at least one
	// row-level-proven table (A1/A4 both passed for it), so attribution was
	// possible in principle, and still failed — a subquery-computed tenant,
	// conflicting values across a join, an undecodable parameter, a parser
	// gap (TGD-BL-34's view-alias and ON CONFLICT fixes both narrowed this
	// bucket), or the differ's own re-execution erroring out after
	// attribution succeeded (TGD-BL-33's finding F, filed as its own
	// backlog item rather than folded in here). This is the population
	// TGD-NFR-03's ceiling should be measured against.
	PopulationRowLevelUnattributed Population = "row_level_unattributed"
	// PopulationNonQuery: cursor protocol (DECLARE/FETCH/CLOSE/LOCK) or
	// session bookkeeping (BEGIN/COMMIT/SET/RESET/PREPARE/EXECUTE) — never a
	// data query at all. TGD-BL-33 found this is over half of ALL captured
	// traffic in the coder capture, almost entirely pg_dump's own per-table
	// cursor-based dump mechanism, triggered by dbtestutil's on-failure
	// diagnostic dump (C-7/TGD-BL-13) — testing-harness noise, not
	// application behaviour. Checked BEFORE table-reference matching: a
	// pg_dump DECLARE CURSOR naming a real scoped table by name is still
	// non_query, not row_level_unattributed.
	PopulationNonQuery Population = "non_query"
)

// nonQueryPrefixes are checked as whole leading keywords (word-bounded, not
// a bare substring match) against sqlText with any leading "--" line
// comment stripped first (TGD-BL-31's isWriteStatement follows the same
// comment-skipping rule; kept consistent here rather than diverging).
var nonQueryPrefixes = []string{
	"DECLARE", "FETCH", "CLOSE", "LOCK",
	"BEGIN", "COMMIT", "SET", "RESET", "PREPARE", "EXECUTE",
}

func isNonQuery(sqlText string) bool {
	s := strings.TrimSpace(sqlText)
	for strings.HasPrefix(s, "--") {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			s = ""
			break
		}
		s = strings.TrimSpace(s[nl+1:])
	}
	upper := strings.ToUpper(s)
	for _, p := range nonQueryPrefixes {
		if hasKeywordPrefix(upper, p) {
			return true
		}
	}
	return false
}

// hasKeywordPrefix reports whether s starts with kw as a whole keyword — kw
// followed by whitespace, '(', or the end of s — never a bare substring
// match that would, for instance, treat an identifier starting with "SET"
// as the SET statement.
func hasKeywordPrefix(s, kw string) bool {
	if !strings.HasPrefix(s, kw) {
		return false
	}
	if len(s) == len(kw) {
		return true
	}
	switch s[len(kw)] {
	case ' ', '\t', '\n', '\r', '(':
		return true
	default:
		return false
	}
}

// ClassifyUnattributable buckets a query into one of the four Population
// values above. relationsTouch is a small local helper shared by the two
// membership checks below — row-level and structural-only relations are
// disjoint sets by construction (runOracleGate never puts a relation in
// both), so a query can only match one or the other, or both only in the
// sense of touching two DIFFERENT declared tables, handled by the
// row-level-takes-priority rule.
func ClassifyUnattributable(sqlText string, rowLevel, structuralOnly []schema.Relation) Population {
	if isNonQuery(sqlText) {
		return PopulationNonQuery
	}
	referenced := referencedTables(sqlText)
	if relationsTouch(referenced, rowLevel) {
		return PopulationRowLevelUnattributed
	}
	if relationsTouch(referenced, structuralOnly) {
		return PopulationStructuralOnly
	}
	return PopulationNoDeclaredTable
}

func relationsTouch(referenced map[string]bool, relations []schema.Relation) bool {
	for _, r := range relations {
		if referenced[strings.ToLower(r.Name)] {
			return true
		}
	}
	return false
}
