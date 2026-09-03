package differ

import (
	"strings"
	"testing"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/schema"
)

func tier2TestRelations() []schema.Relation {
	return []schema.Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE",
			Class: schema.Scoped, TenantColumn: "tenant_id"},
		{Schema: "public", Name: "countries", Kind: "BASE TABLE",
			Class: schema.Unscoped, Reason: "no tenant-column candidate found; believed global"},
		{Schema: "public", Name: "invoices_view", Kind: "VIEW",
			Class: schema.Unclassifiable, Reason: "row-level security cannot be attached to a view"},
	}
}

// TestCheckTenant_UnresolvableRelationBlocks is TGD-BL-43's core fix, as a
// pure unit test: a name the policy never classified at all (a function,
// or a table infer has never seen) must fail closed, not pass as though
// nothing needed checking.
func TestCheckTenant_UnresolvableRelationBlocks(t *testing.T) {
	r := CheckTenant("SELECT * FROM scoped_summary()", tier2TestRelations(), nil, "acme")
	if r.Verdict != Unattributable {
		t.Fatalf("got %+v, want Unattributable — an unresolvable relation must fail closed", r)
	}
	if !strings.Contains(r.Reason, "scoped_summary") {
		t.Errorf("reason %q does not name the unresolvable relation", r.Reason)
	}
}

// TestCheckTenant_UnclassifiableRelationBlocks: a relation infer DID see
// but could not classify (every view, unconditionally, per Classify) must
// also fail closed — "Unclassifiable is never equivalent to Unscoped"
// (schema.go's own package doc), extended here to Tier 2 enforcement.
func TestCheckTenant_UnclassifiableRelationBlocks(t *testing.T) {
	r := CheckTenant("SELECT * FROM invoices_view WHERE tenant_id = $1",
		tier2TestRelations(), []capture.Param{{Known: true, Text: "acme"}}, "acme")
	if r.Verdict != Unattributable {
		t.Fatalf("got %+v, want Unattributable — an Unclassifiable relation must fail closed "+
			"even with a syntactically correct predicate", r)
	}
}

// TestCheckTenant_UnscopedRelationAllowed: a real, declared-Unscoped table
// (infer found no tenant-column candidate) is not something to enforce —
// resolving to Unscoped is not the same as failing to resolve at all.
func TestCheckTenant_UnscopedRelationAllowed(t *testing.T) {
	r := CheckTenant("SELECT * FROM countries", tier2TestRelations(), nil, "acme")
	if r.Verdict != Safe {
		t.Fatalf("got %+v, want Safe — countries is declared Unscoped, not unresolvable", r)
	}
}

// TestCheckTenant_NoRelationReferencedAllowed: a query naming no
// relation-shaped target at all (SELECT now()) has nothing to resolve —
// distinct from resolving to something unrecognised.
func TestCheckTenant_NoRelationReferencedAllowed(t *testing.T) {
	r := CheckTenant("SELECT now()", tier2TestRelations(), nil, "acme")
	if r.Verdict != Safe {
		t.Fatalf("got %+v, want Safe — nothing is referenced at all", r)
	}
}

// TestCheckTenant_CTENameNotTreatedAsUnresolvable: a CTE's own name is
// never a real relation and must not be required to resolve against the
// policy — this is what keeps TGD-BL-43's fix from blocking ordinary,
// harmless CTE-shaped queries (S4/L4's own corpus fixtures).
func TestCheckTenant_CTENameNotTreatedAsUnresolvable(t *testing.T) {
	r := CheckTenant(
		"WITH scoped AS (SELECT * FROM invoices WHERE tenant_id = $1) SELECT * FROM scoped",
		tier2TestRelations(), []capture.Param{{Known: true, Text: "acme"}}, "acme")
	if r.Verdict != Safe {
		t.Fatalf("got %+v, want Safe — the outer SELECT FROM scoped refers to the CTE, "+
			"not an unresolvable relation", r)
	}
}

// TestCheckTenant_NestedCTENameNotTreatedAsUnresolvable: a CTE body that
// itself begins with WITH (a nested CTE) must have ITS names excluded too
// — cteNames recurses into each CTE body for exactly this reason.
func TestCheckTenant_NestedCTENameNotTreatedAsUnresolvable(t *testing.T) {
	sql := "WITH outer_cte AS (" +
		"WITH inner_cte AS (SELECT * FROM invoices WHERE tenant_id = $1) " +
		"SELECT * FROM inner_cte" +
		") SELECT * FROM outer_cte"
	r := CheckTenant(sql, tier2TestRelations(), []capture.Param{{Known: true, Text: "acme"}}, "acme")
	if r.Verdict != Safe {
		t.Fatalf("got %+v, want Safe — both outer_cte and inner_cte are CTE names, not relations", r)
	}
}

func TestCteNames_TopLevelAndNested(t *testing.T) {
	sql := "WITH a AS (WITH b AS (SELECT 1) SELECT * FROM b), c AS (SELECT 2) SELECT * FROM a, c"
	names := cteNames(sql)
	for _, want := range []string{"a", "b", "c"} {
		if !names[want] {
			t.Errorf("cteNames(%q) = %v, missing %q", sql, names, want)
		}
	}
}

func TestCteNames_NoWithClauseIsEmpty(t *testing.T) {
	names := cteNames("SELECT * FROM invoices WHERE tenant_id = $1")
	if len(names) != 0 {
		t.Errorf("got %v, want empty — no WITH clause at all", names)
	}
}
