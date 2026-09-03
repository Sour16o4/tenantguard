package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// TGD-US-01 AC-4: synthesis re-reads the catalog and does not trust its own
// DDL succeeding. This file proves two separate things:
//
//  1. verifySynthesis's own detection logic — built directly against real,
//     deterministically constructed bad catalog states (RLS not enabled, RLS
//     not forced, no policy), each bypassing EnableRLS so the state is
//     constructed rather than hoped-for.
//  2. That EnableRLS's return value is genuinely gated on that detection —
//     proven through a controlled seam (verifySynthesisFn), because, as
//     documented on EnableRLS itself, a real "DDL succeeded but the catalog
//     disagrees" state cannot be produced through this function's own
//     single-writer, well-behaved execution: PostgreSQL's autocommit and READ
//     COMMITTED defaults make a committed CREATE POLICY immediately visible to
//     any later query. The seam substitutes a stub for the confirmation
//     function without touching Postgres at all, and a source mutation
//     removes the call to prove the wiring can fail.

// TestVerifySynthesisDetectsRLSNotEnabled: a policy exists (PostgreSQL allows
// CREATE POLICY before RLS is switched on), but RLS itself was never enabled.
func TestVerifySynthesisDetectsRLSNotEnabled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "verify_notenabled", nil)
	exec(t, ctx, f.probeDB, fmt.Sprintf(
		"CREATE POLICY tgd_policy ON public.invoices USING (%s::text = current_setting('%s', true))",
		f.rel.TenantColumn, TenantSetting))
	// Deliberately no ENABLE ROW LEVEL SECURITY at all.

	err := verifySynthesis(ctx, f.probeDB, f.rel)
	if err == nil {
		t.Fatal("verifySynthesis PASSED with RLS never enabled; the policy that exists is completely inert")
	}
	if !errors.Is(err, ErrSynthesisNotConfirmed) {
		t.Errorf("got %v, want it to wrap ErrSynthesisNotConfirmed", err)
	}
}

// TestVerifySynthesisDetectsRLSNotForced: RLS is enabled and a policy exists,
// but FORCE was never set — the exact silent no-op EnableRLS's FORCE clause
// exists to prevent, constructed here directly rather than through EnableRLS.
func TestVerifySynthesisDetectsRLSNotForced(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "verify_notforced", nil)
	exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY")
	// Deliberately no FORCE.
	exec(t, ctx, f.probeDB, fmt.Sprintf(
		"CREATE POLICY tgd_policy ON public.invoices USING (%s::text = current_setting('%s', true))",
		f.rel.TenantColumn, TenantSetting))

	err := verifySynthesis(ctx, f.probeDB, f.rel)
	if err == nil {
		t.Fatal("verifySynthesis PASSED without FORCE; RLS does not apply to the table owner without it")
	}
	if !errors.Is(err, ErrSynthesisNotConfirmed) {
		t.Errorf("got %v, want it to wrap ErrSynthesisNotConfirmed", err)
	}
}

// TestVerifySynthesisDetectsMissingPolicy: RLS is enabled and forced, but no
// policy was ever created — constructed directly, bypassing EnableRLS, since
// (per the note on EnableRLS) EnableRLS's own CREATE POLICY statement cannot
// itself return success while leaving no policy behind.
func TestVerifySynthesisDetectsMissingPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "verify_missing", nil)
	exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices ENABLE ROW LEVEL SECURITY")
	exec(t, ctx, f.probeDB, "ALTER TABLE public.invoices FORCE ROW LEVEL SECURITY")
	// Deliberately no CREATE POLICY.

	err := verifySynthesis(ctx, f.probeDB, f.rel)
	if err == nil {
		t.Fatal("verifySynthesis PASSED with no policy present; RLS enabled with zero policies denies everyone by default")
	}
	if !errors.Is(err, ErrSynthesisNotConfirmed) {
		t.Errorf("got %v, want it to wrap ErrSynthesisNotConfirmed", err)
	}
}

// TestVerifySynthesisPassesOnCorrectState: the happy path a working
// confirmation must satisfy, built through the real EnableRLS path.
func TestVerifySynthesisPassesOnCorrectState(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "verify_ok", nil)
	if err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy"); err != nil {
		t.Fatalf("EnableRLS (which now calls verifySynthesis internally) should succeed: %v", err)
	}
	if err := verifySynthesis(ctx, f.probeDB, f.rel); err != nil {
		t.Fatalf("verifySynthesis should pass against EnableRLS's own successful output: %v", err)
	}
}

// TestEnableRLSIsGatedOnConfirmation proves EnableRLS's return value is
// genuinely gated on verifySynthesisFn, not merely on its own DDL statements
// returning no error.
//
// A real Postgres state where the DDL succeeds and confirmation would
// legitimately fail cannot be constructed through EnableRLS's own execution —
// see the note on EnableRLS. So this substitutes a stub for verifySynthesisFn
// that fails unconditionally, regardless of what is actually in the catalog.
// EnableRLS's own SQL still runs and still succeeds against real Postgres;
// only the confirmation step is faked. If EnableRLS's returned error reflects
// the stub, the wiring is proven.
func TestEnableRLSIsGatedOnConfirmation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "gate_stub", nil)

	sentinel := errors.New("stub: confirmation deliberately failed")
	orig := verifySynthesisFn
	defer func() { verifySynthesisFn = orig }()

	verifySynthesisFn = func(_ context.Context, _ *sql.DB, _ schema.Relation) error {
		return sentinel
	}

	err := EnableRLS(ctx, f.probeDB, f.rel, "tgd_policy")
	if err == nil {
		t.Fatal("EnableRLS returned nil despite the confirmation stub failing; " +
			"its return value is not actually gated on confirmation")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("EnableRLS error = %v, want it to wrap the stub's sentinel error", err)
	}

	// The real DDL must still have run against Postgres, independent of the
	// stub — confirm the policy genuinely exists despite the stubbed failure.
	verifySynthesisFn = orig
	if verr := verifySynthesis(ctx, f.probeDB, f.rel); verr != nil {
		t.Errorf("EnableRLS's actual DDL did not take effect even though only "+
			"confirmation was stubbed: %v", verr)
	}
}
