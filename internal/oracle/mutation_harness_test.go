package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// This file is TGD-US-11: the unified mutation harness for M1–M8 from the
// design's §8. The assertion is not "some check failed" — it is "the specific
// gate mapped to this mutant actually fired." A mutant caught by a different
// gate than the one mapped to it is a finding: either the mapping is wrong, or
// the mapped gate cannot fire. A mutant caught by nothing is a blind spot.
//
// A mutant firing an EXTRA gate beyond the one mapped to it is not a failure —
// several of these mutants remove a defence with more than one independent
// check behind it, and catching the same defect twice is a property to keep,
// not a defect to hide. The failure condition is specifically: the mapped gate
// stayed silent while some other gate fired instead.

type checkOutcome struct {
	fired bool
	err   error
}

type mutantResult struct {
	a1, a2, a3, a4 checkOutcome
}

func (r mutantResult) fired(gate string) bool {
	switch gate {
	case "A1":
		return r.a1.fired
	case "A2":
		return r.a2.fired
	case "A3":
		return r.a3.fired
	case "A4":
		return r.a4.fired
	}
	panic("unknown gate " + gate)
}

func (r mutantResult) anyFired() []string {
	var out []string
	for _, g := range []string{"A1", "A2", "A3", "A4"} {
		if r.fired(g) {
			out = append(out, g)
		}
	}
	return out
}

// runAllChecks runs all four oracle self-checks against the same probe
// database and reports which fired. a3Role lets each mutant name the role A3
// should evaluate — most mutants leave the connecting role alone, but M4 and
// M5 specifically corrupt that role's privileges.
func runAllChecks(ctx context.Context, f *fixture, tenant, a3Role string) mutantResult {
	var r mutantResult

	_, _, err := CheckA1(ctx, f.probeDB, f.restricted, f.rel)
	r.a1 = checkOutcome{err != nil, err}

	_, err = CheckA2(ctx, f.probeDB, []schema.Relation{f.rel})
	r.a2 = checkOutcome{err != nil, err}

	err = CheckA3(ctx, f.probeDB, []schema.Relation{f.rel}, a3Role)
	r.a3 = checkOutcome{err != nil, err}

	_, _, err = CheckA4(ctx, f.probeDB, f.restricted, f.rel, tenant)
	r.a4 = checkOutcome{err != nil, err}

	return r
}

func exec(t *testing.T, ctx context.Context, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestMutationHarnessM1ThroughM8 is the unified harness. Each mutant gets a
// fresh probe database via a fresh fixture, so mutants cannot interfere with
// each other.
func TestMutationHarnessM1ThroughM8(t *testing.T) {
	ctx := context.Background()

	assertMapped := func(t *testing.T, name, mapped string, r mutantResult) {
		t.Helper()
		if r.fired(mapped) {
			extra := []string{}
			for _, g := range r.anyFired() {
				if g != mapped {
					extra = append(extra, g)
				}
			}
			if len(extra) > 0 {
				t.Logf("%s: mapped gate %s fired (also fired: %v — additional coverage, not a failure)",
					name, mapped, extra)
			} else {
				t.Logf("%s: mapped gate %s fired, no others", name, mapped)
			}
			return
		}
		got := r.anyFired()
		if len(got) == 0 {
			t.Fatalf("%s: BLIND SPOT — mutant caught by NOTHING. Expected %s to fire. "+
				"This is the exact failure class the oracle exists to prevent: a mutant "+
				"survived undetected.", name, mapped)
		}
		t.Fatalf("%s: WRONG GATE — expected %s to fire, but only %v fired instead. "+
			"The mapping is wrong, or %s cannot fire for this mutant.", name, mapped, got, mapped)
	}

	t.Run("M1: drop one scoped table's RLS policy -> A2", func(t *testing.T) {
		f := newFixture(t, "mh_m1", nil)
		if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
			t.Fatalf("enable RLS: %v", err)
		}
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		exec(t, ctx, f.probeDB, "DROP POLICY tgd_policy ON public.invoices")

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M1", "A2", r)
	})

	t.Run("M2: weaken predicate to USING(true) -> A1", func(t *testing.T) {
		f := newFixture(t, "mh_m2", nil)
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, "CREATE POLICY tgd_policy ON public.invoices USING (true)")

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M2", "A1", r)
	})

	t.Run("M3: policy on the wrong column -> A4 (TGD-BL-15, not the design's original 'Fixture corpus')", func(t *testing.T) {
		// The design's §8 table names "Fixture corpus (§7)" as M3's catcher —
		// a Tier 1 differ component that does not exist yet. TGD-BL-15 already
		// established, by mutation against oracle.go directly, that A4 catches
		// this defect: the wrong-column predicate excludes rows (satisfying
		// A1's "strictly fewer" check) but excludes the WRONG rows, which only
		// A4's content comparison can see. A4 is asserted here as the mapped
		// gate on that prior, already-recorded finding — not decided fresh.
		f := newFixture(t, "mh_m3", nil)
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, fmt.Sprintf(
			"CREATE POLICY tgd_policy ON public.invoices USING (id::text = current_setting('%s', true))",
			TenantSetting))

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M3", "A4", r)
		if r.a1.fired {
			t.Logf("M3: A1 ALSO fired (%v) — worth noting, since TGD-BL-15 established A1 "+
				"passes deceptively for this exact mutant with two tenants; a differing "+
				"tenant count in this run could change that count-based outcome", r.a1.err)
		}
	})

	t.Run("M4: RLS enabled but not forced, connect as owner -> A3", func(t *testing.T) {
		f := newFixture(t, "mh_m4", nil)
		exec(t, ctx, f.probeDB, fmt.Sprintf("ALTER TABLE public.invoices OWNER TO %q", f.roleName))
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY")
		// Deliberately no FORCE.
		exec(t, ctx, f.probeDB, fmt.Sprintf(
			"CREATE POLICY tgd_policy ON public.invoices USING (%s::text = current_setting('%s', true))",
			f.rel.TenantColumn, TenantSetting))

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M4", "A3", r)
		if r.a1.fired {
			t.Logf("M4: A1 ALSO fired (%v) — mechanically expected: without FORCE, RLS "+
				"does not apply to the owner's own session at all, so the owner's "+
				"connection sees every row, which is indistinguishable from a blind "+
				"oracle from A1's point of view. Both gates independently catch this.",
				r.a1.err)
		}
	})

	t.Run("M5: grant BYPASSRLS to the connecting role -> A3", func(t *testing.T) {
		f := newFixture(t, "mh_m5", nil)
		if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
			t.Fatalf("enable RLS: %v", err)
		}
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		exec(t, ctx, f.probeDB, fmt.Sprintf("ALTER ROLE %q BYPASSRLS", f.roleName))

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M5", "A3", r)
		if r.a1.fired {
			t.Logf("M5: A1 ALSO fired (%v) — mechanically expected: BYPASSRLS makes every "+
				"row visible to the restricted connection regardless of policy, which is "+
				"the same observable shape as a blind oracle.", r.a1.err)
		}
	})

	t.Run("M6: omit a scoped table from the declared policy entirely -> A2", func(t *testing.T) {
		f := newFixture(t, "mh_m6", nil)
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// RLS synthesis is simply never run for this table at all.

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M6", "A2", r)
	})

	t.Run("M7: policy reads the wrong session variable -> A4 (corrected mapping — A1 is a row-count check and cannot see this)", func(t *testing.T) {
		// The design's original §8 mapped this to A1. Running it found that A1
		// never fires: a policy reading a session variable nothing ever sets
		// evaluates to NULL, which excludes every row regardless of tenant.
		// That satisfies A1's "strictly fewer rows" check exactly as well as a
		// correct policy does — A1 has no way to tell "filtering everything
		// out" from "filtering correctly," because it only counts rows, never
		// inspects which ones. Only A4's content comparison sees that the
		// excluded rows are the WRONG ones (all of them, including the
		// requesting tenant's own data). This is the same shape as M3, and the
		// mapping is corrected to A4 for the same reason.
		f := newFixture(t, "mh_m7", nil)
		if _, _, err := SeedCanaries(ctx, f.probeDB, []schema.Relation{f.rel}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, fmt.Sprintf(
			"CREATE POLICY tgd_policy ON public.invoices USING (%s::text = current_setting('tenantguard.wrong_variable_nobody_sets', true))",
			f.rel.TenantColumn))

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M7", "A4", r)
	})

	t.Run("M8: a policy that withholds nothing, on a single-tenant probe -> A1", func(t *testing.T) {
		// M2 and M7 both showed A4 firing ALONGSIDE A1 (or instead of it), which
		// leaves open whether A1 has any independent value at all in this
		// harness, or whether A4's content comparison always subsumes it. This
		// mutant is built specifically to answer that.
		//
		// With only ONE tenant present, USING(true) is behaviourally identical
		// to a correct policy: there is no second tenant's row to wrongly admit,
		// so A4's reference query (WHERE tenant=CanaryA, matching the only row
		// present) and the unrestricted, unfiltered query return the exact same
		// set. A4 cannot see the defect, because the data in front of it does
		// not expose it. A1 can, because A1 does not ask "is the CONTENT
		// correct" — it asks "did ANY reduction happen at all", and none did.
		//
		// This is the general property of the two checks, stated as a fact
		// about them: A1's independent coverage is a policy that filters
		// nothing, in a state where A4's specific reference happens to match
		// anyway. It exists as a structural safeguard for exactly the situation
		// a second tenant would immediately expose.
		f := newFixture(t, "mh_m8", nil)
		// Deliberately ONE canary, seeded by hand — not SeedCanaries, which
		// always seeds two. A real run never constructs a probe this way
		// (§9.4's whole point is that the probe always gets two canaries); this
		// isolates what A1 alone can see when that guarantee is absent.
		// newFixture's own filler rows (TGD-BL-29: at least 2 must exist for
		// SeedCanaries's sampling to have anything to copy) must be cleared
		// first, or the probe would not actually be single-tenant.
		exec(t, ctx, f.probeDB, "DELETE FROM invoices")
		exec(t, ctx, f.probeDB, fmt.Sprintf(
			"INSERT INTO invoices (tenant_id, amount) VALUES ('%s', 1)", CanaryA))
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY")
		exec(t, ctx, f.probeDB, "CREATE POLICY tgd_policy ON public.invoices USING (true)")

		r := runAllChecks(ctx, f, CanaryA, f.roleName)
		assertMapped(t, "M8", "A1", r)

		if r.a4.fired {
			t.Errorf("M8: A4 ALSO fired (%v) — that contradicts the premise this mutant "+
				"is built to demonstrate. With a single tenant, A4's reference and the "+
				"permissive policy's output should be identical.", r.a4.err)
		} else {
			t.Logf("M8: A4 correctly PASSED — content matches unrestricted with only one " +
				"tenant present, exactly as A1's independent coverage requires")
		}
		if r.a2.fired {
			t.Errorf("M8: A2 ALSO fired (%v) — a policy exists and RLS is enabled, "+
				"A2 checks coverage only, not predicate content, so this is unexpected", r.a2.err)
		}
		if r.a3.fired {
			t.Errorf("M8: A3 ALSO fired (%v) — the role is not the owner, not superuser, "+
				"and does not bypass RLS, so this is unexpected", r.a3.err)
		}
	})
}
