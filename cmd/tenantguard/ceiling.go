package main

import (
	"errors"
	"fmt"
)

// unattributableCeilingDenominator names which of unattributableRateBreakdown's
// four candidate denominators (TGD-BL-33) TGD-NFR-03's ceiling is measured
// against: real application SQL touching a table the oracle has actually
// proven it can see (A1/A4 both passed). This is the one TGD-BL-33
// recommended, and TGD-BL-06 baselines against, because it is the only
// candidate that isolates "attribution was possible in principle and still
// failed" from testing-harness noise (PopulationNonQuery), traffic the
// policy never declared tenant-scoped (PopulationNoDeclaredTable), and
// coverage gaps in tables the tool was never shown it can see
// (PopulationStructuralOnly) — none of which say anything about whether the
// extraction/attribution logic itself is trustworthy.
const unattributableCeilingDenominator = "row_level_touching_real_app_sql"

// unattributableCeilingRate is TGD-NFR-03's baseline. History, in order:
// TGD-BL-06 (130/611, 2026-09-01) → TGD-BL-38 lowered it (131/664,
// 2026-09-02, broadened row-level coverage) → TGD-BL-42 RAISES it to
// 122.0/378.0 (≈0.32275, 61/189 simplified), 2026-09-02, a fresh session on
// the same date. This is the third baseline and the first RAISE this
// project has ever recorded — read the rest of this comment before treating
// a raise as suspicious; it is the deliberate, documented exception
// TGD-US-07 AC-2 itself names, not a routine action.
//
// **Why this baseline is higher, and why that is not a regression.** Every
// prior measurement (130/611, 131/664, and the un-set 131/519 lower bound
// TGD-BL-39 computed) was produced by a differ carrying TGD-BL-42:
// extractFromInsert's positional-VALUES matcher was anchored `^\$(\d+)$`,
// so any INSERT binding its tenant column through a type-cast parameter
// ($N::uuid — sqlc's default codegen shape, and the shape most of coder's
// own INSERTs use) silently lost its real, already-captured tenant value
// and fell back to AttrNoPredicate, the same "no tenant claimed" bucket a
// genuinely unscoped INSERT produces. Downstream this fed diffWrite an
// empty tenant, most often producing UNATTRIBUTABLE and inflating every
// prior measurement's numerator with false negatives that were not
// evidence of anything — cases where attribution was in fact possible and
// the code simply threw the value away. TGD-BL-42 fixed extractFromInsert
// (and, for the same defect class, tenantCompareRegex's WHERE-path CAST(...)
// handling) to tolerate a cast, proven red-before-green with 14 new unit
// tests plus a mutation (reverting the fix reproduces 7 of them failing,
// confirming the tests are load-bearing) — see internal/differ/extract.go
// and internal/differ/extract_test.go. Every prior recorded rate, and every
// prior LEAK count this project has quoted (98 at TGD-BL-32/-34, 106 at
// TGD-BL-37, 96 at this session's own first, pre-fix re-run) is understated
// or overstated by an unknown amount and is superseded — see SRS §7.2/§7.3.
//
// **This session's measurement, on the fixed binary.** Fresh coder/coder
// clone, freshly migrated coder_target, coderd/database/dbpurge run through
// tenantguard's capture proxy a second time this session (the same C-7
// shared-database test interference reproduced, expected), audited with a
// freshly inferred, fixture-completed 22-of-22-row-level-provable policy
// (TGD-BL-37-style fixtures, including one for workspace_build_orchestrations
// this specific fresh clone needed that the prior clone's captured traffic
// had already populated organically). Counts: safe=498, leak=37,
// unattributable=3804 (of 4339 total). row_level_touching_real_app_sql:
// 122/378 UNATTRIBUTABLE = 0.32275. The 37 LEAKs were inspected, not merely
// counted: every one traces to an already-documented genuine
// no-predicate shape (GetChatFileByID/GetProvisionerJobByID/
// GetProvisionerDaemons/GetChatFileMetadataByChatID: filtered by a bare id,
// never the tenant column; GetWorkspaceAgentStats/InsertWorkspaceAgentStats:
// an INSERT...SELECT/CTE-aggregate shape extractFromInsert's literal-VALUES
// path never covers, same as TGD-BL-37 already found) — none are a new
// instance of TGD-BL-42 or any other defect shape found this session.
//
// **Why the rate is higher than TGD-BL-38's, beyond "the bug was
// inflating things before":** this is also a different, freely-varying
// capture (a fresh coder clone and a fresh, shared, test-polluted
// coder_target database — not the same saved capture TGD-BL-06/-38 used,
// which no longer exists in any environment this project has run in — see
// SRS §7.2) of a different size (378-query denominator vs. 664), so the
// two numbers are not a like-for-like before/after on identical traffic.
// Both the fix and a smaller, differently-composed real-traffic sample
// contribute to where 122/378 lands; this is named rather than the whole
// movement being attributed to the fix alone.
//
// Ratchet rule (TGD-US-07 AC-2): this value may only ever be LOWERED by a
// later measurement, UNLESS the raise is accompanied by a recorded
// decision naming why a higher rate is acceptable — this comment, and
// TGD-BL-42, is that decision: the previous ceiling was measured by a
// differ now known to have been wrong, on a population contaminated by
// that same defect, so the earlier, lower numbers were never a real,
// trustworthy floor to hold future runs to. Setting the ceiling to a
// correctly-measured, higher number is the ratchet rule being honoured,
// not violated — the rule protects against silently raising the ceiling
// to paper over a regression; it does not require the tool to keep
// enforcing a number now known to have been computed by broken code.
//
// This is a coder-specific number from one capture of one target's test
// suite, not a general claim about the tool's attribution quality — see
// TGD-NFR-03's SRS row for what would justify revising it (a second
// target's own measurement, not a bad run against coder).
const unattributableCeilingRate = 122.0 / 378.0

// ErrUnattributableCeilingExceeded reports that the measured rate against
// unattributableCeilingDenominator exceeded unattributableCeilingRate.
var ErrUnattributableCeilingExceeded = errors.New("unattributable rate exceeds the baselined ceiling (TGD-NFR-03/TGD-BL-06)")

// ExitUnattributableCeilingExceeded is §8.2's exit code 3, reserved for
// TGD-NFR-03/TGD-FR-08 since the SRS was written and unused until this
// build, since the ceiling was unbaselined (TGD-BL-06) for the whole life
// of the project before now.
const ExitUnattributableCeilingExceeded = 3

// checkUnattributableCeiling enforces TGD-NFR-03/TGD-FR-08: a non-nil error
// means the run must exit ExitUnattributableCeilingExceeded. "At or below
// the ceiling passes" (TGD-US-07 AC-1) — only a rate STRICTLY greater than
// unattributableCeilingRate fails.
//
// breakdown not containing an entry for unattributableCeilingDenominator at
// all — verify, audit with no --events, or --events whose traffic touches
// no row-level-proven table — passes: there is nothing in that population
// to have exceeded a ceiling on, and a metric the run never measured must
// never block it.
func checkUnattributableCeiling(breakdown []unattributableRateEntry) error {
	for _, e := range breakdown {
		if e.Label != unattributableCeilingDenominator {
			continue
		}
		if e.Rate > unattributableCeilingRate {
			return fmt.Errorf("%w: %s rate %.4f (%d/%d) exceeds the baselined ceiling %.4f",
				ErrUnattributableCeilingExceeded, e.Label, e.Rate, e.Unattributable, e.Denominator,
				unattributableCeilingRate)
		}
		return nil
	}
	return nil
}
