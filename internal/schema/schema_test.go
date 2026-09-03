package schema

import (
	"reflect"
	"strings"
	"testing"
)

// TestClassifySingleCandidate: one candidate column means scoped.
func TestClassifySingleCandidate(t *testing.T) {
	cls, col, _, _ := Classify("BASE TABLE", []string{"id", "tenant_id", "amount"})
	if cls != Scoped {
		t.Fatalf("class = %q, want %q", cls, Scoped)
	}
	if col != "tenant_id" {
		t.Errorf("tenant column = %q, want tenant_id", col)
	}
}

// TestClassifyNoCandidate: no candidate means unscoped, and the reason is recorded.
func TestClassifyNoCandidate(t *testing.T) {
	cls, col, _, reason := Classify("BASE TABLE", []string{"id", "version", "applied_at"})
	if cls != Unscoped {
		t.Fatalf("class = %q, want %q", cls, Unscoped)
	}
	if col != "" {
		t.Errorf("unscoped relation must have no tenant column, got %q", col)
	}
	if reason == "" {
		t.Errorf("an unscoped verdict must record why")
	}
}

// TestAmbiguousCandidatesAreUnclassifiable is the central honesty requirement:
// a table with two candidate columns must NOT be guessed at, and must not fall
// through to unscoped — that would silently exempt it from every later check.
//
// This test must fail if ambiguity is resolved by picking a candidate.
func TestAmbiguousCandidatesAreUnclassifiable(t *testing.T) {
	cls, col, cands, reason := Classify("BASE TABLE",
		[]string{"id", "org_id", "workspace_id", "payload"})

	if cls == Scoped {
		t.Errorf("two candidates resolved to Scoped on %q; ambiguity was guessed away", col)
	}
	if cls == Unscoped {
		t.Errorf("two candidates resolved to Unscoped; an ambiguous table would be " +
			"silently exempted from tenant checks")
	}
	if cls != Unclassifiable {
		t.Fatalf("class = %q, want %q", cls, Unclassifiable)
	}
	if len(cands) != 2 {
		t.Errorf("candidates = %v, want both recorded for the operator", cands)
	}
	if !strings.Contains(reason, "ambiguous") {
		t.Errorf("reason %q should name the ambiguity", reason)
	}
}

// TestViewsAreUnclassifiable: RLS cannot attach to a view, so a view with a
// tenant-shaped column must not be reported as scoped.
func TestViewsAreUnclassifiable(t *testing.T) {
	for _, kind := range []string{"VIEW", "MATERIALIZED VIEW", "FOREIGN TABLE"} {
		cls, _, _, reason := Classify(kind, []string{"id", "tenant_id"})
		if cls != Unclassifiable {
			t.Errorf("%s with a tenant_id classified %q, want %q", kind, cls, Unclassifiable)
		}
		if reason == "" {
			t.Errorf("%s must record why it cannot be classified", kind)
		}
	}
}

// TestPartitionedTableIsScoped: a policy on the parent is inherited by partitions.
func TestPartitionedTableIsScoped(t *testing.T) {
	cls, col, _, _ := Classify("PARTITIONED TABLE", []string{"id", "tenant_id"})
	if cls != Scoped || col != "tenant_id" {
		t.Errorf("partitioned table = (%q, %q), want scoped on tenant_id", cls, col)
	}
}

func TestAllCandidateColumnNames(t *testing.T) {
	for _, c := range []string{"tenant_id", "org_id", "organization_id",
		"workspace_id", "account_id", "owner"} {
		if cls, col, _, _ := Classify("BASE TABLE", []string{"id", c}); cls != Scoped || col != c {
			t.Errorf("column %q not recognised as a tenant candidate", c)
		}
	}
	if cls, _, _, _ := Classify("BASE TABLE", []string{"id", "ownership"}); cls != Unscoped {
		t.Errorf("%q should not match the %q candidate", "ownership", "owner")
	}
}

func TestPolicyCounts(t *testing.T) {
	p := &Policy{Relations: []Relation{
		{Class: Scoped}, {Class: Scoped}, {Class: Unscoped}, {Class: Unclassifiable},
	}}
	s, u, x := p.Counts()
	if s != 2 || u != 1 || x != 1 {
		t.Errorf("counts = (%d,%d,%d), want (2,1,1)", s, u, x)
	}
	if len(p.Scoped()) != 2 || len(p.Unclassifiable()) != 1 {
		t.Errorf("Scoped/Unclassifiable selectors returned the wrong sets")
	}
}

func TestStringArrayScan(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`{}`, nil},
		{`{id,tenant_id}`, []string{"id", "tenant_id"}},
		{`{"odd name",id}`, []string{"odd name", "id"}},
		{`{"has,comma",x}`, []string{"has,comma", "x"}},
	}
	for _, c := range cases {
		var a pqStringArray
		if err := a.Scan(c.in); err != nil {
			t.Errorf("Scan(%q) errored: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual([]string(a), c.want) {
			t.Errorf("Scan(%q) = %v, want %v", c.in, []string(a), c.want)
		}
	}
	var a pqStringArray
	if err := a.Scan("not-an-array"); err == nil {
		t.Errorf("malformed array literal accepted")
	}
}

// TestWritePolicy_ReadPolicy_RoundTrips is the format tenantguard infer /
// tenantguard verify|audit --policy FILE depend on: whatever WritePolicy
// writes, ReadPolicy must reconstruct exactly, including a relation with
// multiple candidates (Unclassifiable) and one with none (Unscoped).
func TestWritePolicy_ReadPolicy_RoundTrips(t *testing.T) {
	want := &Policy{Relations: []Relation{
		{Schema: "public", Name: "invoices", Kind: "BASE TABLE", Class: Scoped, TenantColumn: "tenant_id", Candidates: []string{"tenant_id"}},
		{Schema: "public", Name: "audit_log", Kind: "BASE TABLE", Class: Unscoped, Reason: "no tenant-column candidate found; believed global"},
		{Schema: "public", Name: "orgs", Kind: "BASE TABLE", Class: Unclassifiable, Candidates: []string{"org_id", "tenant_id"}, Reason: "ambiguous: 2 tenant-column candidates (org_id, tenant_id)"},
	}}

	var buf strings.Builder
	if err := WritePolicy(&buf, want); err != nil {
		t.Fatalf("WritePolicy: %v", err)
	}
	got, err := ReadPolicy(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round trip mismatch:\nwant %+v\ngot  %+v", want, got)
	}
}

// TestReadPolicy_MalformedJSONErrors: a corrupted or hand-edited-wrong policy
// file must fail loudly, never decode into a zero-value Policy that would
// silently sweep zero tables.
func TestReadPolicy_MalformedJSONErrors(t *testing.T) {
	_, err := ReadPolicy(strings.NewReader("{not json"))
	if err == nil {
		t.Fatal("ReadPolicy accepted malformed JSON")
	}
}
