# Changelog

## [1.0.0]

A single entry, not a per-commit history. `§12`'s Definition of Done has
required a `CHANGELOG.md` entry per story across this project's entire life,
but this file has never existed until now — there was nothing to append to.
The project had no version control until its own first commit
(`ac01136`, "Initial import"), the same way that commit's own message states
it: everything before that point is collapsed into one import with no
finer-grained history to preserve. This entry is organized by the milestones
`docs/SRS.md` §11 actually tracked, not reconstructed as if per-commit
records had existed from the start.

### Added

- **Tier 0 — `tenantguard triage`**: a syntactic pass over a Go repository,
  no database required. Flags candidate SQL against likely-tenant-scoped
  tables with no mention of the tenant column, ranked as suspicion only —
  never a verdict, never exits non-zero.
- **Tier 1 — `tenantguard infer` / `verify` / `capture` / `audit`**: the
  differential isolation oracle. `infer` proposes a tenancy policy from a
  target's schema; `verify` synthesises row-level security on a disposable
  probe and runs four self-checks (`A1`–`A4`) before trusting the policy;
  `capture` records real traffic through a transparent PostgreSQL
  wire-protocol proxy; `audit` re-executes captured queries differentially
  against the probe and reports `SAFE`/`LEAK`/`UNATTRIBUTABLE` per query,
  never writing to the target's own database.
- **Tier 2 — guardrail (`internal/guardrail`)**: a fail-closed runtime
  mechanism demonstrating detection of the one class Tier 1 structurally
  cannot — a syntactically correct predicate bound to the wrong tenant
  value (`L5`). Proven as a mechanism; not shipped as a turnkey integration.
- **`TGD-NFR-03` unattributable-rate ceiling**, enforced in CI and by
  `audit` itself, baselined against real `coder` traffic and ratcheted
  down four times over the project's life as coverage broadened, a
  measurement defect was found and fixed, and (this release) a real
  capability gap (`TGD-BL-35`) was closed. Current value: `18.0/762.0`
  (≈2.36%).
- **CI**: three required jobs (`build, vet, gofmt`; `test (-race), coverage
  floor, M1-M8 mutation harness`; `lint this workflow file`), each proven
  able to actually fail — not merely written — by deliberately reverting a
  fix and confirming the pipeline goes red, then confirming green again.
- **`docs/SRS.md`**, **`docs/LIMITATIONS.md`**, **`README.md`**: the
  specification, the confirmed-and-caveated limitations list, and a
  top-level orientation document.

### Fixed

Selected defects found by running real traffic through the tool against
real third-party targets (`coder/coder`, `zitadel/zitadel`), not by
inspection alone — the full account is in `docs/SRS.md` §7:

- A differ bug that silently discarded a real, already-captured tenant
  value on any `INSERT` binding it through a cast positional parameter
  (`$N::type`, `sqlc`'s default codegen shape), inflating every rate
  measured before the fix with false negatives.
- Two connected defects surfaced by `zitadel`'s non-`public`-schema
  layout: a restricted role's grants were hardcoded to the `public`
  schema only, and a real oracle error was discarded before it reached
  the operator-facing message.
- `TGD-BL-35`: a captured write's replay colliding with the probe's own
  pre-existing history (the probe is a `TEMPLATE` copy of the live,
  already-written-to target database) — resolved by identifying the
  exact colliding row via PostgreSQL's own structured error fields and
  retrying the write once inside the same rolled-back transaction, never
  a durable write. Fixed the largest single source of `UNATTRIBUTABLE`
  verdicts this tool had produced on real traffic to date.

### Known limitations (see `docs/LIMITATIONS.md`)

Not exhaustive here by design — the linked document is the source of
truth and is expected to change independently of this changelog. As of
this release: `L5` (wrong tenant value, correct predicate shape) and `L7`
(a view without `security_invoker`) are confirmed Tier 1 blind spots;
undecoded binary parameters and DDL/cross-statement-dependency replay
both correctly resolve to `UNATTRIBUTABLE` rather than being guessed at.

### Validation

Run against two real, third-party PostgreSQL applications — `coder/coder`
(primary target, 37 real `LEAK`-shaped verdicts, 18/762 ≈ 2.36%
unattributable on the most recent full run) and `zitadel/zitadel` (second,
structurally different target — composite tenancy, non-`public` schemas —
3 `LEAK`-shaped verdicts, 204/1,187 ≈ 17.19% unattributable on a
bootstrap-heavy capture that is explicitly not read as a general
attribution-quality signal). Neither target's `LEAK` findings have been
disclosed as security findings — see `README.md`'s Validation section and
`docs/SRS.md` §7.21 for why.
