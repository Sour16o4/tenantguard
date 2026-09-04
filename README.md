# tenantguard

A multi-tenant isolation verifier for Go applications backed by PostgreSQL.

**The thesis:** PostgreSQL decides what each query should have been allowed
to return, not a SQL parser. Give the database a synthesised row-level-security
policy for your declared tenant columns, run each captured query a second time
under that policy, and diff the row sets. A row the application received that
RLS would have withheld is a proven leak — not a heuristic suggesting one. A
parser can be defeated by joins, CTEs, views, subqueries, and tautologies, and
it structurally cannot catch a query scoped to the *wrong* tenant, which is
syntactically perfect. PostgreSQL evaluates all of that itself.

## The three tiers

| Tier | Requires | Proves |
|---|---|---|
| **0 — triage** | Nothing. Any Go repository, no database. | Ranked *suspicions* from column-naming heuristics — never a verdict. Cannot detect a leak and never claims to; never exits non-zero. Useful for deciding where to point Tier 1. |
| **1 — differential** | The target already runs PostgreSQL in its test suite (or a live connection); a declared tenancy policy (`infer`'s output, human-reviewed); real traffic seeded by **at least two tenants** in the same scoped table — a single-tenant seed makes the restricted and unrestricted row sets identical by construction and proves nothing. | Real `SAFE`/`LEAK`/`UNATTRIBUTABLE` verdicts, per captured query, against real PostgreSQL. This is the tier that ships in v1.0.0. |
| **2 — guardrail** | Application code changes: tenant-context propagation, a driver wrapper. Not a point-and-shoot story — real integration work. | A fail-closed runtime guardrail that catches the one class Tier 1 structurally cannot: a query whose predicate is syntactically correct but bound to the *wrong* tenant value (no independent ground truth exists at Tier 1 to catch this against). |

Nothing above Tier 1 ships as a turnkey CLI feature today — Tier 2 exists as a
demonstrated mechanism (`internal/guardrail`), not an integration you can run
against your own app out of the box.

## Install

```
go install github.com/Sour16o4/tenantguard/cmd/tenantguard@latest
```

Or build from a checkout:

```
git clone https://github.com/Sour16o4/tenantguard
cd tenantguard
go build ./cmd/tenantguard
```

Requires PostgreSQL. The proxy speaks the PostgreSQL wire protocol only and
declines TLS, so the target's connection string must not require it.

## Commands

```
tenantguard triage  [--output json|text] PATH
tenantguard infer   --dsn URL --out FILE
tenantguard verify  --dsn URL --policy FILE [--shared-database] [--output json|text]
tenantguard capture --listen ADDR --upstream ADDR [--out FILE]
tenantguard audit   --dsn URL --policy FILE [--events FILE] [--shared-database] [--output json|text]
```

- **`triage`** — Tier 0. Scans a Go repository for candidate SQL and flags
  statements against likely-tenant-scoped tables with no mention of the
  tenant column anywhere in their text. No database. Never exits non-zero.
- **`infer`** — reads `--dsn`'s schema and writes a *proposed* tenancy policy
  to `--out`, classifying each relation `Scoped`, `Unscoped`, or
  `Unclassifiable`. This is a proposal for a human to review before it is
  ever pointed at `verify`/`audit` — the tool does not accept its own
  inference automatically.
- **`verify`** — builds a disposable probe database (a `TEMPLATE` copy of
  `--dsn`'s own database, never written back to), synthesises RLS from
  `--policy`, and runs four self-checks per scoped table. Exits non-zero,
  with no report at all, if the oracle itself can't be trusted on this
  policy — a policy that fails self-check is never used to judge real
  traffic.
- **`capture`** — point the target's own PostgreSQL connection-string
  environment variable at `--listen` (the variable name is target-specific —
  `CODER_PG_CONNECTION_URL` for `coder`, never assume `DATABASE_URL`) and run
  its test suite. Records queries as newline-delimited JSON. Capture only —
  it does not judge anything and never writes to the target's own database.
- **`audit`** — runs the same self-checks as `verify`; if they pass and
  `--events` is given, re-executes every captured query differentially and
  reports a `SAFE`/`LEAK`/`UNATTRIBUTABLE` verdict for each, entirely against
  the disposable probe — never against `--dsn`'s live data.

## Scope

- **PostgreSQL only.** No MySQL, no SQLite, no CockroachDB. The oracle is
  Postgres's own row-level security; there is no equivalent assumed for any
  other engine.
- **Shared-schema tenancy with a tenant column only.** Schema-per-tenant and
  database-per-tenant are a different detection problem entirely.
- **Not for access-control applications.** If a tenant-shaped column exists
  (`owner_id`, `team_id`) but visibility is actually computed in application
  code — roles, membership, admin override — where legitimate cross-boundary
  reads are the normal case (a public repo, an admin console), a synthesised
  RLS policy would be actively *wrong*, not merely unverified: it would deny
  every legitimate read and this tool would report each denial as a false
  `LEAK`. This is why `go-gitea/gitea` and `mattermost/mattermost` were
  evaluated and rejected as validation targets.
- **Go applications only** for the Tier 2 guardrail. Tiers 0 and 1 are
  wire-level and language-agnostic in principle but validated against Go
  only.
- **No remediation.** The tool reports; it does not rewrite queries.

## Known limitations

Read **[`docs/LIMITATIONS.md`](docs/LIMITATIONS.md)** before trusting a clean
result. Two are confirmed blind spots that return `SAFE` when a leak may
actually be present — not bugs, structural consequences of what each tier
can observe:

- **L5** — a query with a syntactically correct predicate bound to the
  *wrong* tenant value. Tier 1 has no ground truth for "which tenant should
  this have been" beyond the value already in the query; confirmed on both
  `coder` and `zitadel` (neither sets tenant context out-of-band anywhere in
  its own source). Tier 2 is the fix, and requires application changes.
- **L7** — a view created without `security_invoker`. PostgreSQL evaluates
  its RLS using the view owner's privileges, and in essentially any real
  target that owner is the same privileged role this tool's own unrestricted
  baseline connects as — so a leak through such a view is invisible on both
  legs of the diff at once. No fix is designed; see the entry for why no
  view-owner configuration makes this both leaky and catchable.

Two further gaps, found while characterizing real-target traffic and correctly
resolving toward `UNATTRIBUTABLE` rather than guessing:

- **Undecoded binary parameters** — a bound value in a PostgreSQL type outside
  the capture decoder's fixed set (observed on `zitadel`: `numeric`/similar
  types) cannot be recovered as a value, so a predicate built on it is
  reported `UNATTRIBUTABLE`, never guessed at.
- **DDL and cross-statement-dependency replay** — a captured `CREATE`/`ALTER`
  statement has no row-tenant semantics to compare, and a write replayed in
  isolation can depend on a sibling statement from its original transaction
  that Tier 1 never re-executes alongside it. Both resolve to
  `UNATTRIBUTABLE`, never `SAFE`.

## Validation, stated at the strength it actually holds

v1.0.0 has been run against two real, third-party PostgreSQL applications —
not fixtures. Neither run is a general claim about attribution quality on
arbitrary targets; each number below carries the denominator it was measured
against, because a rate without one is not a claim, it's a number.

**`coder/coder`** — the primary validation target, exercised repeatedly
across this project's history via its own real `dbpurge` test suite run
through the capture proxy. Established: the differential mechanism, the
oracle, and Tier 1 correctly separate `SAFE` from `LEAK` on a real,
unfamiliar 132-relation production schema — 37 real `LEAK`-shaped verdicts
produced on real traffic (none independently confirmed as disclosable
security findings; see `LIMITATIONS.md` and below). Most recent full run:
18 of 762 row-level-touching real application queries came back
`UNATTRIBUTABLE` (≈2.36%), the number the shipped ceiling (`TGD-NFR-03`) is
currently calibrated against.

**`zitadel/zitadel`** — a second, structurally different target (composite
`instance_id`/`org_id` tenancy, non-`public` schemas, real `pgx`/`pgxpool`
statement-naming), used to find and fix defects a single target's schema
never exercised. What it established: the tool correctly caught a tenancy
shape `coder` never presented, found and fixed two real defects (missing
non-`public`-schema grants; a discarded restricted-role error), and produced
3 `LEAK`-shaped verdicts (all the identical bootstrap query that resolves
which tenant a request belongs to from its hostname — plausibly a category
error for that one query's own architectural role, not a confirmed finding).
**What it explicitly does NOT establish:** a trustworthy `LEAK`/`SAFE`
distribution for `zitadel` in general — the only capture available is
`start-from-init`'s one-time bootstrap/migration sequence, not steady-state
application traffic, and its own measured unattributable rate (204 of 1,187
row-level-touching real application queries, ≈17.19%) reflects that
capture's composition, not a general property of this tool's attribution
quality against `zitadel`.

**No `LEAK` verdict from either target has been disclosed as a security
finding.** Both sets require a code-path review this tool does not and
cannot perform (it observes SQL, not the application logic that produced it)
to rule out a compensating control before any report to a maintainer would
be responsible rather than an overclaim.

If `LIMITATIONS.md` and this README ever disagree, `LIMITATIONS.md` is
right and this file needs fixing.
