package main

import (
	"strings"
	"testing"
)

// These are TGD-NFR-03/TGD-BL-06's own proof that the gate can fire at all —
// TGD-NFR-03 was unbaselined for this project's entire life before this
// session, so nothing had ever exercised the enforcement path. Pure,
// database-free: checkUnattributableCeiling takes only the already-computed
// []unattributableRateEntry a report carries.

func TestCheckUnattributableCeiling_AboveFails(t *testing.T) {
	// 0.90 is deliberately far above any plausible baseline (currently
	// 18/762 ≈ 0.0236, TGD-BL-35 fix, SRS §7.18/§7.19) rather than a fixed
	// value close to a specific historical baseline, so this test does not
	// need updating every time the baseline itself is legitimately
	// re-measured.
	err := checkUnattributableCeiling([]unattributableRateEntry{
		{Label: unattributableCeilingDenominator, Denominator: 100, Unattributable: 90, Rate: 0.90},
	})
	if err == nil {
		t.Fatal("got nil, want an error — 0.90 exceeds the baselined ceiling")
	}
}

func TestCheckUnattributableCeiling_AtOrBelowPasses(t *testing.T) {
	// Exactly at the ceiling.
	err := checkUnattributableCeiling([]unattributableRateEntry{
		{Label: unattributableCeilingDenominator, Denominator: 611, Unattributable: 130, Rate: unattributableCeilingRate},
	})
	if err != nil {
		t.Errorf("got %v, want nil — exactly at the ceiling must pass ('at or below it passes')", err)
	}

	// Below the ceiling.
	err = checkUnattributableCeiling([]unattributableRateEntry{
		{Label: unattributableCeilingDenominator, Denominator: 1000, Unattributable: 10, Rate: 0.01},
	})
	if err != nil {
		t.Errorf("got %v, want nil — 0.01 is below the baselined ceiling", err)
	}
}

// TestCheckUnattributableCeiling_MessageNamesRateAndDenominator: the
// message a real run's stderr would carry on a breach must name both, not
// just say "ceiling exceeded" — an operator reading it needs to know which
// of the four denominators fired and what the actual number was.
func TestCheckUnattributableCeiling_MessageNamesRateAndDenominator(t *testing.T) {
	err := checkUnattributableCeiling([]unattributableRateEntry{
		{Label: unattributableCeilingDenominator, Denominator: 10, Unattributable: 5, Rate: 0.5},
	})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{unattributableCeilingDenominator, "0.5", "5", "10"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not name %q", msg, want)
		}
	}
}

// TestCheckUnattributableCeiling_OtherDenominatorsIgnored: the ceiling is
// baselined against exactly ONE of TGD-BL-33's four candidate denominators
// (TGD-BL-06) — an entry for any of the other three, however high its own
// rate, must never trip the gate; only unattributableCeilingDenominator's
// own entry is checked.
func TestCheckUnattributableCeiling_OtherDenominatorsIgnored(t *testing.T) {
	err := checkUnattributableCeiling([]unattributableRateEntry{
		{Label: "all_captured_queries", Denominator: 100, Unattributable: 99, Rate: 0.99},
		{Label: unattributableCeilingDenominator, Denominator: 1000, Unattributable: 5, Rate: 0.005},
	})
	if err != nil {
		t.Errorf("got %v, want nil — the baselined denominator's own rate (0.005) is well under the ceiling", err)
	}
}

// TestCheckUnattributableCeiling_NoMatchingEntryPasses: a run with no
// queries in the baselined population at all (verify, or audit with no
// --events, or --events touching no row-level table) has nothing to gate
// on and must not be blocked by a metric it never measured.
func TestCheckUnattributableCeiling_NoMatchingEntryPasses(t *testing.T) {
	if err := checkUnattributableCeiling(nil); err != nil {
		t.Errorf("got %v, want nil for an empty breakdown", err)
	}
	err := checkUnattributableCeiling([]unattributableRateEntry{
		{Label: "all_captured_queries", Denominator: 100, Unattributable: 99, Rate: 0.99},
	})
	if err != nil {
		t.Errorf("got %v, want nil — no entry for the baselined denominator", err)
	}
}
