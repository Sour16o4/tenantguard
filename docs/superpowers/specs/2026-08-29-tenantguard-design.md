# Design — `tenantguard` (`TGD`): multi-tenant isolation verifier

**Status:** **specified.** Gate 0 resolved — the proxy holds (§11) — and the
specification has been written to `docs/SRS.md`, which is now the authoritative
document. This design is retained as the record of how the approach was chosen and
what Gate 0 established; where the two differ, the SRS governs.

Nothing in §9's budgets is baselined; every number there is a proposal awaiting
measurement. The SRS carries them forward as `[U]` and the 90-minute Tier 1 figure
is **withdrawn** there rather than inherited.

Findings carry an evidence tag throughout: **[executed]** means observed by
running code, **[read-only]** means established by reading source. The two are
not interchangeable and the distinction is preserved wherever a claim rests on
one rather than the other.

**Date:** 2026-08-29
**Conventions:** `cmd/` + `internal/` layout, `docs/SRS.md` authoritative
(written 2026-08-29), ID-tagged backlog (`TGD-BL-nn`).

---

## 1. This project in one line

One missing tenant predicate in one query leaks one customer's data to another;
`tenantguard` finds those queries in a Go codebase by letting PostgreSQL — not a
SQL parser — decide what the query should have been allowed to return.

## 2. Problem statement

Shared-schema multi-tenancy scopes rows by a tenant column. Isolation therefore
depends on every single query carrying a correct tenant predicate, forever,
across every code path including the ones written at 6pm on a Friday. There is
no compiler check for this. The failure is silent, the blast radius is the
customer list, and the bug is usually found by the customer.

Existing approaches are syntactic: parse the SQL, assert a predicate is present.
That is defeated by joins, CTEs, views, subqueries, set-returning functions, and
tautologies — and it structurally cannot detect a query scoped to the *wrong*
tenant, which is syntactically perfect.

## 3. Approach: PostgreSQL as the oracle

Synthesise row-level-security policies from a declared tenancy policy. For each
query the application issues, execute it a second time under an RLS-enforcing
role bound to that query's tenant, and diff the row sets. **Any row the
application received that RLS would have withheld is a proven leak.**

The database resolves joins, views, CTEs and functions itself, so none of that
becomes parser work. A divergence *is* a violation rather than a heuristic
suggesting one.

Correctness is not asserted by the tool's own checker here — it is delegated
to an independent authority.
The cost is that the synthesised RLS becomes the definition of ground truth, and
therefore the thing most worth attacking. §6 and §8 exist for that reason.

**Syntactic analysis is retained but demoted** to a triage pass (§9, Tier 0). It
is fast and needs no database, so it is useful for ranking what to investigate.
It never decides pass or fail.

## 4. Non-goals for v1

Stated so the claim stays narrow and defensible:

- **PostgreSQL only.** No MySQL, no SQLite, no CockroachDB.
- **Shared-schema tenancy with a tenant column only.** Schema-per-tenant and
  database-per-tenant are a different detection problem; claiming them would be
  a lie of scope.
- **Go applications only** for the Tier 2 guardrail. Tiers 0 and 1 are
  language-agnostic at the wire level but will only be validated against Go.
- **No remediation.** The tool reports; it does not rewrite queries.

## 5. Verdict model — three outcomes, never two

Every captured query resolves to exactly one of:

| Verdict | Meaning |
|---|---|
| `SAFE` | Row set under RLS is identical to unrestricted. |
| `LEAK` | Unrestricted returned rows RLS withheld. Proven violation. |
| `UNATTRIBUTABLE` | The tool could not determine which tenant the query intended, so no comparison was possible. |

**`UNATTRIBUTABLE` never collapses into `SAFE`.** It is reported as its own
class, counted separately, and printed in the summary line.

**It is also not a free pass.** A run that marks everything `UNATTRIBUTABLE` is
never wrong and never useful, so the class carries two obligations that a merely
descriptive third state would not:

1. Fixtures with an expected verdict of `UNATTRIBUTABLE` (§7), inside the same
   exact-set assertion as the other two classes. The tool must produce this
   verdict *precisely* when it should.
2. A ceiling on the unattributable rate for real targets (§9), which fails the
   run when exceeded.

**`SAFE` carries one qualifier, `Vacuous` (§9.7, `TGD-BL-39`), that is NOT a
fourth outcome.** A `SAFE` where both legs of the comparison matched or
affected zero rows is still `SAFE` — the model above remains exactly three
outcomes — but is flagged `Vacuous: true`, since it proved nothing about
whether RLS would have withheld anything had the query matched something.
`TGD-NFR-03`'s ceiling excludes a vacuous `SAFE` from its "attributed"
denominator for the same reason `UNATTRIBUTABLE` is never a free pass:
counting a comparison that demonstrated nothing as though it demonstrated
correctness would let real traffic heavy in empty-result queries improve
the reported rate without the tool proving anything more.

## 6. Oracle self-check (pre-flight)

Four assertions run before any query is judged. Any failure **aborts with a
distinct exit code and emits no report** — the tool must never be able to print
"0 leaks" because it was blind.

| ID | Assertion | Guards against |
|---|---|---|
| `A1` | **Negative control.** A deliberately unscoped probe against a scoped table returns *strictly fewer* rows as the RLS role than unrestricted. | A blind oracle reporting a clean repo |
| `A2` | Every table declared tenant-scoped has an enabled, matching policy in `pg_policies`, and `pg_class.relrowsecurity` is true for it. | Missing or partial coverage |
| `A3` | The connecting role is not a superuser, does not hold `rolbypassrls`, and is not the table owner — or, if it is the owner, `pg_class.relforcerowsecurity` is true for every scoped table. | The silent `FORCE ROW LEVEL SECURITY` no-op |
| `A4` | **Positive control.** A known-correctly-scoped query returns an identical row set under both roles. | Over-restrictive RLS making everything look like a leak |

`A1` and `A4` are deliberately symmetric. `A1` catches an oracle that cannot
see; `A4` catches one that sees nothing. Both directions produce a false report,
in opposite ways, and both abort.

`A3` is the subtle one. RLS does not apply to a table's owner unless
`FORCE ROW LEVEL SECURITY` is set. A tool connecting as the owner would find
RLS silently inert while every status indicator stayed green — the exact shape
of a gate that cannot fire.

## 7. Fixture corpus — 24 queries, exact-set equality across all three classes

The acceptance criterion is **exact set equality on all three verdict classes**,
not a precision or recall threshold: the tool must classify every fixture
correctly, with no substitutions between classes.

### 7.1 `SAFE` (10)

| # | Query shape |
|---|---|
| S1 | `SELECT * FROM invoices WHERE tenant_id = $1` — baseline |
| S2 | `SELECT name FROM invoices WHERE tenant_id = $1 AND status = 'paid'` |
| S3 | Join with **both** sides scoped |
| S4 | CTE whose inner select is scoped |
| S5 | `SELECT count(*) FROM invoices WHERE tenant_id = $1` |
| S6 | View that carries its own tenant predicate |
| S7 | `WHERE id = $1 AND tenant_id = $2` |
| S8 | Scoped select with `ORDER BY` + `LIMIT` |
| S9 | `INSERT INTO invoices (tenant_id, …) VALUES ($1, …) RETURNING *` |
| S10 | A second, different scoped table (`audit_log`) |

### 7.2 `LEAK` (10)

| # | Query shape | Why it earns its place |
|---|---|---|
| L1 | `SELECT * FROM invoices WHERE id = $1` | The classic: PK lookup, no tenant predicate |
| L2 | `SELECT count(*) FROM invoices` | Leaks a number without returning a row |
| L3 | Join scoping only the joined side | Base table unconstrained |
| L4 | CTE aggregate with no scoping | |
| L5 | Correct predicate, **wrong tenant value** | Invisible to every syntactic method |
| L6 | `WHERE tenant_id = tenant_id` | Tautology; defeats Tier 0 |
| L7 | View lacking its own scoping | |
| L8 | `WHERE tenant_id IN (SELECT tenant_id FROM tenants)` | Subquery expands to all tenants |
| L9 | `SELECT max(amount) FROM invoices` | Second aggregate-leak shape |
| L10 | `WHERE tenant_id = $1 OR status = 'public'` | `OR` defeats the scoping; defeats Tier 0 |

L6 and L10 are the empirical argument for demoting syntactic analysis: both pass
a naive predicate-presence check. L5 is the argument for the differential
specifically — no parser can catch it. L2 and L9 are the argument against
result-set inspection, which sees no rows and would conclude nothing happened.

### 7.3 `UNATTRIBUTABLE` (4)

| # | Shape | Why the tenant cannot be determined |
|---|---|---|
| U1 | `WHERE tenant_id = (SELECT tenant_id FROM users WHERE id = $1)` | Tenant is computed by a subquery, not present as a value |
| U2 | `SELECT * FROM scoped_summary()` | Set-returning function; table access hidden in the body |
| U3 | Join across two scoped tables with **conflicting** tenant values (`i.tenant_id = $1 AND a.tenant_id = $2`, `$1 ≠ $2`) | Two competing tenant intents; could be a cross-tenant leak or a legitimate system query — the tool cannot tell |
| U4 | Statement captured with unresolved `Bind` parameters | **Capture-layer fixture**, not a SQL-layer one: exercises the proxy losing extended-protocol state (§11). The tool must degrade to `UNATTRIBUTABLE`, never to `SAFE`. |

U4 is the operationally important one. It is the failure mode the proxy will
actually have, and the fixture exists to prove that failure surfaces rather than
silently passing.

### 7.4 Which tier the corpus assertion applies to

The exact-set assertion in §7 is defined at the **harness level**, where the
intended tenant for every fixture is supplied out-of-band. This distinction is
load-bearing and was missing from the first draft of this design:

**At Tier 1 the tool has no independent source of intended tenant.** The proxy
sees only the wire traffic, so the only available answer to "which tenant should
this query have been scoped to?" is the value in the query's own predicate. That
is sufficient for L1–L4 and L6–L10, where the predicate is absent, tautological,
or structurally defeated. It is **not** sufficient for **L5**, where the
predicate is present, well-formed, and simply names the wrong tenant — judged
against itself, it is self-consistent and passes.

Therefore:

| Fixture | Tier 1 (proxy) | Tier 2 (context propagation) |
|---|---|---|
| L1–L4, L6–L10 | `LEAK` | `LEAK` |
| **L5** | **`SAFE` — known limitation** | `LEAK` |
| U1–U3 | `UNATTRIBUTABLE` | `UNATTRIBUTABLE` |
| U4 | `UNATTRIBUTABLE` | n/a (no proxy) |
| S1–S10 | `SAFE` | `SAFE` |

L5 must be documented in `LIMITATIONS.md` as a Tier 1 blind spot, and the Tier 1
report must not claim completeness it does not have. Two consequences follow:

1. The corpus runs at **both** tiers, with **two different expected sets**. A
   single expected set would silently pass whichever tier it was not written for.
2. Wrong-tenant scoping is a genuine motivation for Tier 2 existing at all,
   rather than Tier 2 being merely a convenience. That argument belongs in the
   SRS.

An alternative worth evaluating: recover intended tenant at Tier 1 from
`SET`/`set_config` traffic on the same connection, if the target application sets
one. If it does, L5 becomes Tier 1-detectable for that class of application, and
the limitation narrows rather than disappears.

**`TGD-BL-09` status after Gate 0 — partially answered, item stays open.**

- **`coder`: confirmed no.** *(read-only — source inspection, not executed.)* No
  `set_config`, `SET LOCAL` or `current_setting` tenant context appears anywhere
  in `coderd/database/`; the only matches are commented-out `search_path` lines
  inside `pg_dump` fixture files. **L5 therefore remains a Tier 1 blind spot for
  `coder`**, and its `LIMITATIONS.md` entry stands unqualified for that target.
- **`zitadel`: still open.** Only `go.mod` was read, which establishes the driver
  and nothing about session handling. The item is **not** closed on that basis.

`TGD-BL-09` remains open until `zitadel`'s session handling is inspected.

## 8. Mutation testing — each mutant mapped to its expected gate

The assertion is **not** "some check failed." It is **"the specific gate expected
to catch this mutant actually fired."** A mutant caught by the wrong gate is
itself a finding, because it means the gate claimed to be load-bearing is not.

| Mutant | Expected catcher |
|---|---|
| M1 — drop one scoped table's RLS policy | `A2` |
| M2 — weaken a policy predicate to `USING (true)` | `A1` |
| M3 — point a policy at the wrong column | `A4` |
| M4 — RLS enabled but not forced, connect as table owner | `A3` |
| M5 — grant `BYPASSRLS` to the connecting role | `A3` |
| M6 — omit a scoped table from the declared policy entirely | `A2` |
| M7 — policy reads the wrong session variable | `A4` |
| M8 — a policy that withholds nothing, run on a single-tenant probe | `A1` |

**M3 and M7 were corrected from an earlier draft**, and the correction is a
stated property of `A1` and `A4`, not a bug found in either check.

`A1` is a **row-count** check: it asks only whether *some* reduction happened
between unrestricted and restricted. It cannot see *which* rows were excluded,
which means **any mutant whose effect is total exclusion — for any reason —
satisfies `A1` exactly as well as a correct policy does**, and lands on `A4`
instead. This was first found for `M3` (`TGD-BL-15`: a wrong-column predicate
excludes rows, just the wrong ones) and confirmed to generalise for `M7` (a
session variable nothing sets evaluates to `NULL`, which also excludes every
row regardless of tenant). Both are the identical shape — "the policy filters
everything out" — and both are structurally invisible to a check that only
counts rows. The original mapping assumed a 1:1 mutant→gate correspondence that
does not hold for this shape of defect; `A4`'s content comparison is the only
one of the four that can see it.

That raises the converse question directly: if every "excludes everything"
mutant lands on `A4`, does `A1` have any independent coverage at all, or is it
decorative? **`M8` answers this.** With only one tenant present, `USING (true)`
produces output identical to a correct policy — there is no second tenant's row
to wrongly admit, so `A4`'s reference query and the unfiltered query return the
same set, and `A4` passes. `A1` still fires, because `A1` never inspects
content: it asks whether reduction happened at all, and none did. Verified
**[executed]**: `A4` passes, `A2`/`A3` also pass (a policy exists and the role
is unprivileged — neither of those checks inspects predicate content), and only
`A1` fires.

**The general property, stated so the next person does not have to re-derive
it:** `A1`'s independent coverage is specifically a policy that filters
nothing, in a state where `A4`'s available reference data cannot yet reveal
that — the same class of blind spot §9.1 discusses for the probe's own seed
data. `A4`'s coverage is everything else: any policy whose output diverges from
the correct answer, in either direction — too permissive (M2, M4, M5, M6, which
`A4` catches *in addition to* their mapped gate here) or too restrictive (M3,
M7). The two checks are not two independent detectors of the same thing; they
are asymmetric, and each has exactly one shape of defect only it can see.

Outcomes:

- Mutant caught by its mapped gate → pass. An **additional** gate also firing is
  not a failure — several mutants here are legitimately caught twice (`M2`,
  `M4`, `M5`, `M6` all also trip `A1` or `A4` alongside their mapped gate), and
  that overlap is defence-in-depth worth keeping, not noise to suppress.
- Mutant caught by a **different** gate, with the mapped gate silent → **finding**;
  the mapping is wrong, or the mapped gate cannot fire. Gets a `TGD-BL-nn`.
- Mutant caught by **nothing** → blind spot. Gets a `TGD-BL-nn`.

This exists because a gate that cannot fire is indistinguishable from a gate
that passes, and only a deliberate mutant tells them apart.

**Run [executed]** as `TestMutationHarnessM1ThroughM8` in
`internal/oracle/mutation_harness_test.go`, against real PostgreSQL. All eight
mutants caught their mapped gate. The harness was itself proven capable of
failing: disabling `A1`'s inequality check turns `M8` into a genuine blind spot
(nothing catches it) and `M2` into a wrong-gate finding (`A4` fires instead) —
confirming the harness has bite rather than passing vacuously.

## 9. Tier model, preconditions, and budgets

| | Preconditions | Output | Budget |
|---|---|---|---|
| **Tier 0** | None. Any Go repo. | Ranked suspicions, explicitly labelled unverified. **Never exits non-zero. Cannot prove a leak and must not imply it does.** | **< 5 min**, clone → output *(proposed)* |
| **Tier 1** | (a) Repo already runs PostgreSQL in its test suite; (b) **multi-tenant seed data** — see §9.1; (c) a declared tenancy policy, generated by the tool and human-reviewed | Proven leaks, with `SAFE`/`LEAK`/`UNATTRIBUTABLE` counts | **UNBASELINED** — see §9.1 |
| **Tier 2** | App changes: tenant context propagation, driver wrapper | Fail-closed guardrail | Days. Documented as such; not a point-and-shoot story. |

Tier 1 generates its policy file by reading `information_schema` and inferring
scoped tables from column names (`tenant_id`, `org_id`, `workspace_id`,
`account_id`, `owner`), so the human reviews roughly ten lines rather than
authoring them.

Capture at Tier 1 is via a PostgreSQL wire-protocol proxy: the target's own
connection-URL environment variable is pointed at the proxy and no application
code changes. **The variable is target-specific and must not be assumed** — it is
`CODER_PG_CONNECTION_URL` for `coder`, not `DATABASE_URL` (§10.1). Gate 0 has
confirmed this property holds (§11.5), subject to the TLS constraint in §9.3.

**Capture is bidirectional, not unidirectional.** Earlier drafts of this section
described capture as reading the frontend (client-to-server) stream only. That is
insufficient, for the reason set out in §9.5: parameter *type* information travels
in the backend direction, and without it binary-format parameters cannot be decoded
to values. The proxy reads both directions; it still modifies neither.

### 9.1 Multi-tenant seed data is a Tier 1 precondition, not an assumption

`A1` requires **at least two tenants with rows in the same scoped table**. Most
Go test suites seed one organisation per test and tear it down. Under a
single-tenant seed, the unrestricted and RLS-restricted row sets are identical,
`A1` fails, and the tool aborts **on a completely healthy repository**.

That behaviour is correct — it is fail-safe, and aborting beats reporting a
clean run against a blind oracle. But it means the real Tier 1 cost is **seeding,
not policy review**, and the earlier 90-minute estimate was built on the wrong
bottleneck.

Consequently:

- Multi-tenant seed data is an explicit, documented Tier 1 precondition.
- `TGD-BL-03` is now **closed and confirmed**: `coder`'s suite does *not* produce
  two tenants in one table, so seeding must be arranged rather than assumed.
- **`A1` no longer depends on the target satisfying this.** §9.4 moves the
  negative control onto a tool-owned probe database, which removes the target's
  seed data from `A1`'s critical path. This subsection stands as the analysis that
  led there; §9.4 is the resolution.
- The Tier 1 time-to-first-finding budget is **re-baselined after `TGD-BL-10`**
  (probe-database seeding), not after `TGD-BL-03`. It remains `UNBASELINED`.

### 9.2 Unattributable ceiling — **baselined and enforced (`TGD-BL-06`, closed)**

*[executed]* A run fails when the unattributable rate — measured against
`row_level_touching_real_app_sql` (`TGD-BL-33`'s recommended denominator, not
"captured queries": that original framing is superseded, having been shown to
dilute the signal roughly 4-to-1 with cursor-protocol/session-bookkeeping
traffic and out-of-scope application SQL) — exceeds **21.28%** (`130/611`,
exact fraction), measured against one real `coder/coder` capture. See SRS
§7.1 for the full provenance, the ratchet rule, and — stated there rather than
left implicit — that this is a `coder`-specific number, not a general claim
about the tool.

The ceiling **ratchets in one direction only: down**. It may never be raised
to make a failing run pass; raising it requires a documented decision
recorded in the backlog, not a config edit — enforced in practice by there
being no `--ceiling` flag at all, only a source constant
(`cmd/tenantguard/ceiling.go`).

### 9.3 TLS is a hard Tier 1 constraint

*[executed]* A target connecting with `sslmode=require` **fails outright**. The
proxy answers `SSLRequest` with `N` to keep the stream readable, and a client that
requires TLS aborts the connection rather than falling back.

**Tier 1 therefore requires a test DSN that does not demand TLS.** This belongs in
`LIMITATIONS.md` as a stated constraint, not as an implementation note.

`coder` happens not to be affected — `ConnectionParams.DSN()` hardcodes
`sslmode=disable` *[read-only]* — but **that is a property of `coder`, not a
property of the tool.** The limitation must not be documented as solved because
the first target avoids it. Any target whose test harness requires TLS is out of
scope for Tier 1 until the proxy can terminate TLS, which is not planned for v1.

### 9.4 What the tool does when a target seeds only one tenant

`A1` aborting on a single-tenant target is correct but useless, and after
`TGD-BL-03` this is known to be `coder`'s actual state. Three options were
considered:

| Option | Rejected because |
|---|---|
| **(a)** Tool seeds a second tenant into the target's own database | Writes to the database under observation. Foreign keys, `NOT NULL` columns and unique constraints make generic synthetic seeding hard on an unknown schema, and an extra tenant appearing in list endpoints can break the very suite being observed. |
| **(b)** Harness supplies the second tenant | Requires writing code in the target repo, which forfeits Tier 1's no-code-changes property — the thing Gate 0 was run to protect. |
| **(c)** Documented precondition, operator satisfies it | Honest but inert. It makes the tool work on no real repository out of the box. |

**Decision: none of the three as stated. `A1` moves to a tool-owned probe
database.**

The reasoning is that `A1` was mis-sited. **`A1` is a self-check on the
synthesised RLS, not on the application.** It asks "does RLS withhold rows?",
which is a property of the policies and the schema — not of the application's
seed data. Requiring the *application's* database to contain two tenants imports
a constraint `A1` never actually needed.

So the tool creates its own probe database from the target's schema, seeds two
canary tenants there, and proves row-withholding against that. For `coder` this
uses the mechanism `coder` already uses — `CREATE DATABASE … WITH TEMPLATE
tpl_<migrations-hash>`. Generally it is the target's migrations, or
`pg_dump --schema-only`.

This keeps option (a)'s zero-operator-cost property while removing its only
serious objection: **the tool never writes to the application's database.**

**Residual gap, stated rather than hidden.** `A1` then proves RLS withholds rows
*on the probe database*. If the live test database's schema has diverged from the
probe's, the oracle could still be blind where it matters. Mitigations:

- `A2` (policy coverage) and `A3` (role privileges) continue to run against the
  **live** target database, so structural coverage is verified where it counts.
- When the live database *does* happen to contain two or more tenants with rows in
  one scoped table, `A1` also runs there and upgrades confidence from
  probe-verified to target-verified. The report states which of the two it got.

Requires `CREATE DATABASE` privilege, which `coder`'s harness already holds since
it creates a database per test. Implementation is `TGD-BL-10`, **on Tier 1's
critical path**.

### 9.5 Backend capture is a correctness requirement, not an enhancement

Discovered by running the implementation against live drivers, not by reading:
**100 of 961 captured parameters (10.4%) arrived in binary format.** A binary
parameter is a type-specific byte encoding; it cannot be rendered as a value
without the parameter's type OID, and that OID is never present in the frontend
stream.

The consequence follows directly from the verdict model. The differ must
re-execute a captured query bound to a tenant, which needs parameter **values**.
A parameter whose value is unknown makes its query **`UNATTRIBUTABLE`** — by §5's
rule it can never be counted `SAFE`.

**Why this did not surface against the primary target.** `coder` drives
`database/sql` through `lib/pq`, which uses the text format. Every parameter was
text, so nothing was undecodable and the gap stayed invisible. *(The exact
breakdown for `TGD-BL-11` is unrecoverable — the spike did not record format
codes.)*

**Why it would break the second target.** `zitadel` uses `pgx`, which prefers the
binary format for common types. A Tier 1 run against it would return a large
fraction `UNATTRIBUTABLE` — plausibly breaching the unattributable ceiling in §9.2
and failing the run, not because `zitadel` leaks but because the tool cannot read
what it captured. **A tool that fails its own ceiling on a healthy repository is
the same class of uselessness as `A1` aborting on one** (§9.1).

That makes backend capture a **correctness requirement**. Design: track the
client's `Describe` messages to know which statement each backend reply concerns,
read `ParameterDescription` (`t`) for parameter type OIDs and `RowDescription`
(`T`) for result column OIDs, and decode binary parameters by OID. Parameters
whose OID is absent or whose type has no decoder remain undecodable — and are
reported as such rather than guessed.

Recorded as `TGD-BL-14` and `TGD-FR-19`. It changes the capture layer from
unidirectional to bidirectional, which §9's proxy description above now states.

### 9.6 Design review: is generic row synthesis the right seeding mechanism? (`TGD-BL-29`)

Prompted by three consecutive real `coder` findings (`TGD-BL-27`, `-28`, `-29`),
each a different, previously-unseen class of constraint `SeedCanaries` had no
representation for (a column's *type*, a column's *uniqueness*, a CHECK
spanning *multiple columns' values*) — on a target where 10 of 22 real scoped
tables carry at least one CHECK constraint, none audited. Requested explicitly
as analysis only; nothing below is implemented.

**§9.4's decision — write to a tool-owned probe, never the target — is
unaffected and stands.** Nothing found this session argues for touching the
target's database or the harness instead. What §9.4 left unexamined is
*how the probe gets its rows*. It already named the risk, aimed at a different
target: rejecting option (a) (seed the target), it wrote that "foreign keys,
`NOT NULL` columns and unique constraints make generic synthetic seeding hard
on an unknown schema." That sentence is equally true of the probe — created
via `CREATE DATABASE ... TEMPLATE <target>`, so schema-identical to it — and
was never re-applied once the probe became the thing being seeded instead.
Three findings, one different constraint class each, are the concrete shape
of the "hard on an unknown schema" risk §9.4 already named but did not
re-check against its own chosen mechanism. **This is a mechanism gap, not a
site gap:** the fix is in *how* the probe gets seeded, not in reopening
*where*.

One fact reframes every alternative: `CreateProbeDatabase`'s `TEMPLATE` clone
copies **data**, not only schema (`TestSeedCanariesOnlyTouchesItsArgument`
already asserts the probe's row count as inherited rows *plus* the 2 canaries).
The probe was never a blank schema needing rows invented from nothing — it
already holds whatever the target held at probe-creation time.

| Option | Buys | Costs |
|---|---|---|
| **Sample real target rows (read-only), rewrite only the tenant column** | Every constraint — type, uniqueness, CHECK, cross-column — satisfied by construction, since the row was already valid. Closes all three found classes and the unbounded tail at once. §3.3 holds (read against the target, write only to the probe). No harness changes. | Needs ≥2 existing rows per table to sample; an empty table (`coder`'s own post-migration state per `TGD-BL-03`) has nothing to copy. Rewriting the tenant column can still hit a constraint that references it (smaller residual than today's). `LIMITATIONS.md`/§3.3 need a new sentence: the probe transiently holds real row *content*, not just structure (already true today via `TEMPLATE`, currently unstated). Reads more of the target than schema introspection alone (still read-only). |
| **Defer/drop constraints during seeding** | Reuses the FK/trigger-disabling pattern (`session_replication_role = replica`) already in `SeedCanaries`. The probe never needs constraints restored — it is disposable, and A1/A4 read visibility, not integrity. | PostgreSQL CHECK constraints cannot be `DEFERRABLE` at all — closing `TGD-BL-29`'s class needs literal `DROP CONSTRAINT` (read from `pg_get_constraintdef` first), not a flag. Fixes the CHECK class only; does nothing for `TGD-BL-27` (type) or `-28` (uniqueness). Residual unbounded tail remains (domain-embedded `CHECK`, `EXCLUDE`, enforcement triggers). |
| **Seed only a representative subset of tables** | Reduces synthetic-seeding surface proportionally. | Doesn't address the actual bottleneck: all three findings broke in *seeding*, a step every table still needs attempted regardless of sample size, and each was a table-specific column shape — exactly what a representative sample learns nothing about for the tables left out. Wrong lever for this problem (see the tiered-proof answer below). |
| **Operator-supplied seed fixture** | Sidesteps constraint-reasoning entirely — the operator's data is valid by definition. Same shape as the already-accepted policy-file pattern (tool-owned artifact, human-reviewed), not a harness change. | This is §9.4's rejected option (c); its criticism stands at full scope — 22 hand-written fixtures is real, unbounded operator work, disqualifying as the *primary* mechanism. Survives only as a **fallback**, scoped to the tables sampling can't handle. |

**Recommendation: sampling as the primary mechanism; an operator-supplied
fixture as a small fallback scoped only to tables with no rows to sample —
named explicitly in the report, not silently dropped.** Constraint-dropping is
worth keeping only as a narrow supplement (a sampled row whose rewritten
tenant column itself trips a CHECK), not as the mechanism. This is a real
implementation change, not scoped or attempted this session.

**A1/A4 per-table, and what A2/A3 already carry.** A2/A3 are pure catalog
reads, independent of seeded rows, and already loop over every scoped
relation — but `runOracleGate` today aborts on the *first* seeding failure
before `EnableRLS`, A2, or A3 run for **any** table, including the ones that
seeded fine. That is a separate, real inefficiency this review surfaced,
worth fixing regardless of the seeding-mechanism question: decouple
"attempt `EnableRLS` + A2 + A3 per relation" from "this relation's seeding
succeeded," and full structural coverage stops being hostage to one table's
seeding gap.

A1 mostly proves the *synthesis mechanism* — `EnableRLS`'s DDL template is
identical across tables, so a bug there would show up anywhere it runs, and a
representative pass would likely generalise. A4 is different by design
(`TGD-BL-15`): its purpose is catching a policy file naming the *wrong
column* for *this* table, a per-table authoring mistake that a different
table's pass cannot detect by construction. Sampling A4 trades away exactly
the failure mode it exists for — not a trade to make by default.

What the three findings actually show is that seeding succeeding, not A1/A4
being cheap once it has, is the bottleneck. The right lever is not "prove
fewer tables" but **stop making one table's seeding failure disqualify the
whole sweep's proof depth.** Keep attempting A1/A4 on every scoped table;
report each one's reached level — **structurally verified** (A2/A3 only) or
**row-level verified** (A1/A4 also passed) — instead of one aggregate
proven/not-proven boolean.

**This changes §6 and `TGD-FR-02`'s model, stated plainly rather than folded
into an implementation patch.** `ProofState.PolicyProven()` is currently a
single all-or-nothing gate; a tiered model needs a genuine third state per
table, and — more consequentially — changes what the differ may say about a
query touching a structural-only table. `SAFE`/`LEAK` today presume A1/A4
proved the oracle can see for that specific table; a query against a
structural-only table has no such proof, and calling it `SAFE` would
reproduce exactly the "reported clean because the oracle was blind" failure
`A1` exists to rule out. Those verdicts need to fold into something
distinguishable — a new state, or `UNATTRIBUTABLE` carrying a reason that
names "not row-level proven for this table," never silently absorbed into
`SAFE`. This touches `ProofState`, `aggregateProof`, the report schema, the
differ's verdict logic, and `LIMITATIONS.md`/SRS — a design of its own, not a
patch, and the part of this review most in need of careful design work before
any code is written.

**Implemented — `TGD-BL-32`.** See the backlog entry below for what was
actually built, tested, and re-run against real `coder` data. The
implementation took the narrower of the two options this review left open —
`UNATTRIBUTABLE` carrying an implicit reason via the differ's own existing
"no declared scoped table referenced" path, achieved by never handing a
structural-only relation to the differ at all — rather than a new named
verdict state, which stayed simpler and needed no change to `differ.Verdict`
or `differ.Result` at all.

### 9.7 Design review: does a 0=0 comparison deserve its own verdict? (`TGD-BL-39`)

Requested explicitly as analysis only, per instruction — nothing below is
implemented. Prompted by `TGD-BL-37`'s real find: 44 of the run's new `SAFE`
verdicts (`DeleteOldWorkspaceAgentStats`) came from a comparison where BOTH
legs matched zero rows — the purge's retention cutoff never reached the
fixture rows' fresh timestamps — the second time this session a `SAFE` has
turned out to rest on an empty comparison rather than a real one (the first
was `TGD-BL-34`'s 36 view-alias queries, where the captured id no longer
existed in the probe).

**The claim `SAFE` makes, per §5: "row set under RLS is identical to
unrestricted."** When both sides are `∅`, that claim is literally true —
`∅ = ∅` — so today's `SAFE` is not *false*. The question is whether it is
*informative*: a `SAFE` earned by two non-empty, identical row sets has
demonstrated that RLS let through exactly what it should have; a `SAFE`
earned by two empty sets has demonstrated only that the query matched
nothing in this replay — it says nothing about what RLS would do if the
query HAD matched something, which is the entire question isolation-testing
exists to answer.

**Two distinct mechanical shapes, not one — found while looking at this,
not assumed going in:**

1. **Read-path (`Diff`'s `SELECT` branch).** `unrestrictedRows` and
   `restrictedRows` are both already fully materialised (`collectRows`) by
   the time `multisetShortfall` runs — detecting "both empty" here costs
   nothing beyond a length check already sitting next to the comparison
   that produces `SAFE`.
2. **Write-path (`diffWrite`).** `diffWrite` calls `runRolledBack`, which
   executes via `tx.QueryContext` and discards the row data it collects
   (`_, unrestrictedErr := runRolledBack(...)`) — it only ever asks "did
   this error," never "how many rows did this touch." For a write with no
   `RETURNING` clause (`DeleteOldWorkspaceAgentStats` is `sqlc`'s `:exec`
   type, exactly this shape), `QueryContext` reports zero result columns
   regardless of how many rows the statement actually affected — the row
   *count* diffWrite would need to detect "0 rows affected" is not
   information `QueryContext` surfaces at all. Getting it means switching to
   `ExecContext` and reading `sql.Result.RowsAffected()`, a real change to
   the write-path's execution mechanism, not a report-layer flag reading
   data already collected.

That asymmetry matters for scoping any fix: the read-path case is close to
free; the write-path case is not.

**Three options, weighed rather than picked by default:**

| Option | Buys | Costs |
|---|---|---|
| **Leave it — `SAFE` stays `SAFE`.** | Simplest. `SAFE`'s literal claim (`∅=∅`) is technically true. No change to §5's model, the fixture corpus, or the ceiling. | Silently inflates the "attributed" population `TGD-NFR-03`'s denominator rests on with verdicts that proved nothing — an operator reading "SAFE" cannot tell a real pass from a vacuous one without re-deriving it by hand, the same asymmetry this project's own conventions exist to close elsewhere (e.g. `TGD-FR-11`'s undecodable-vs-decoded distinction). |
| **A fourth verdict** (e.g. `INCONCLUSIVE`), sibling to `SAFE`/`LEAK`/`UNATTRIBUTABLE`. | Cleanly distinguishes "compared and matched" from "nothing to compare" — the same justification §5 already gives for keeping `UNATTRIBUTABLE` from collapsing into `SAFE` ("not a free pass") applied consistently to a second way a verdict can be non-informative. | Breaks §5's own title ("three outcomes, never two") and its exact-set fixture-corpus discipline (`TGD-NFR-09`) — every fixture, the CLI report schema, `unattributableRateBreakdown`'s counting, and the ceiling's own denominator all need to decide where this new class sits, which is real design work of the same weight as `TGD-US-06`'s original verdict model, not a small addition. `UNATTRIBUTABLE` is the wrong place to fold it (checked, not assumed): `UNATTRIBUTABLE`'s definition is "could not determine which tenant" — here attribution succeeded and the comparison itself was vacuous, a different failure a reader must not have to disambiguate by reading the reason string. |
| **A flag alongside `SAFE`** (e.g. `queryVerdict.Vacuous bool`), not a new top-level verdict. | Keeps §5's three-outcome model and its "never two" title literally true; keeps the exact-set fixture corpus's existing three classes unchanged. | Still needs the write-path instrumentation change above (not free); still needs a decision on whether a vacuous `SAFE` counts toward `real_app_sql_touching_any_declared_table`/`row_level_touching_real_app_sql`'s attributed population — a decision this table's own existence argues should be visible in the schema, not buried in a boolean nobody is required to check. |

**Decided and implemented — the flag, not a fourth verdict.** `SAFE`'s
literal claim (`∅=∅`) is still true; a vacuous comparison is a
strength-of-evidence distinction on an existing verdict, not a
categorically different outcome the way `UNATTRIBUTABLE` is — the same
relationship `LEAK`'s existing `WithheldRows` already has to `LEAK` itself
(a magnitude marker on a verdict, not a fourth verdict for "a LEAK with only
one row withheld"). A fourth verdict would also touch the exhaustive 3-way
partition that `TGD-NFR-09`'s exact-set fixture corpus, every
counts-sum-to-total assertion in the CLI tests, and §5's own title all
depend on, for a distinction better modelled as a qualifier. §5 gained one
clarifying sentence (below) rather than a rewrite, since the three-outcome
model itself is unchanged.

**Read path: implemented.** `differ.Result` gained `Vacuous bool`
(meaningful only when `Verdict == Safe`), set in `Diff`'s `SELECT` branch as
`len(unrestrictedRows) == 0` — free, since both legs' row data is already
materialised by the time `SAFE` is decided. Proven red before green:
`TestDiff_VacuousSafeWhenBothLegsMatchNothing` (a query against a tenant
value with zero real rows) failed to compile before the field existed,
passes now; `TestDiff_CorrectlyScopedQueryIsSafe` was extended to assert
`Vacuous == false` for a real, non-empty match, locking in the non-vacuous
case as much as the vacuous one.

**Write path: implemented (`TGD-BL-40`, this session).** `diffWrite` now
re-executes both legs via `execRolledBack`/`execRolledBackAsTenant`
(`ExecContext` + `sql.Result.RowsAffected()`) instead of `runRolledBack`'s
`QueryContext`, which reported no row data for a write without `RETURNING`
regardless of how many rows it actually touched. `RowsAffected()` is
populated from PostgreSQL's command-complete tag either way — confirmed
against `lib/pq`'s `Exec`, which drains any `RETURNING` rows itself and
still reports the real count — so one code path now serves both shapes; no
`RETURNING`-vs-not branch was needed. A `Safe` write is flagged `Vacuous`
when both legs affected zero rows, the write-path mirror of the read path's
"both legs matched zero rows" rule. Proven with three cases, same pattern as
the read path: `TestDiff_WriteVacuousWhenBothLegsAffectZeroRows` (no
`RETURNING`, the shape the old `QueryContext` path could never observe),
`TestDiff_WriteVacuousWithReturningClauseHandledCorrectly` (`RETURNING`
present, to confirm the fix didn't regress the one case that already worked
by accident), and `TestDiff_WriteNotVacuousWhenRowsAffected` (a real,
non-empty write, `RETURNING` present) — plus `TestDiff_WriteAcceptedByRLSIsSafe`'s
existing no-`RETURNING` non-vacuous case. Written against the project's own
fixture database; not executed in this sandbox (no live Postgres available —
see this backlog entry's own note on what remains unproven).

**The ceiling's denominator: fixed, not left to say why it should keep
counting them.** `cmd/tenantguard/unattributableRateBreakdown`'s
`attributed` term changed from `Safe + Leak` to `(Safe - VacuousSafe) +
Leak` — a vacuous `SAFE` no longer inflates
`real_app_sql_touching_any_declared_table`/`row_level_touching_real_app_sql`.
Proven end-to-end: `TestAuditCLI_VacuousSafeExcludedFromAttributedDenominator`
constructs one real `SAFE`, one vacuous `SAFE`, and one genuine attribution
failure, and asserts the corrected denominator (2, not 3) and the resulting
rate (0.5, not 0.333) directly — a real, numerically-checked demonstration
that excluding vacuous passes moves the rate, not just a boolean's presence.

**`TGD-BL-41` — CTE-wrapped write routing: fixed, this session, ahead of
`TGD-BL-40`, on the grounds that it is a correctness bug (a write compared
as a read, the wrong verdict mechanism), not a measurement one.**
`isWriteStatement`'s leading-keyword check, even after `TGD-BL-31`'s
comment-skipping fix, still only recognised `INSERT`/`UPDATE`/`DELETE` as
the query's OWN first keyword. A `WITH cte AS (...) DELETE ...` — real
`coder` shape, `DeleteOldWorkspaceBuildOrchestrations` and
`ExpirePrebuildsAPIKeys` — has `WITH` as its first keyword, so both were
routed through the read-path row-set comparison instead of `diffWrite`,
regardless of whether the write actually touched a row. The instruction was
explicit that a leading-keyword match is not sufficient: a CTE can wrap any
DML, and the write can sit in a nested CTE body while the outer statement is
a plain `SELECT` reading that CTE's result — a shape a keyword-position fix
alone would still miss.

Fixed by classifying the statement's actual kind rather than matching a
position: `statementContainsWrite` walks past a leading `WITH`
(`skipWithClause`, reusing `extract.go`'s existing `balancedParenSpan` rather
than inventing new paren-tracking), recursing into every CTE body — each
body is itself run back through `statementContainsWrite`, since a CTE body
can start with its own `WITH` — and then checks the primary statement's own
leading verb once the CTE list is consumed. A write found anywhere in that
walk, nested CTE body or outer statement, classifies the whole statement as
a write: executing it performs the write regardless of where in the text the
verb appears. Handles `WITH RECURSIVE`, a CTE's optional column list
(`WITH ids (id) AS (...)`), and `[NOT] MATERIALIZED`, none of which the old
check needed to consider. Consistent with this package's stated
non-goal of being a SQL parser: parsing stops and returns whatever text
remains the moment the expected shape breaks down, rather than guessing — a
missed CTE is read as "keyword still visible = still a write" wherever
possible, which this project already treats as the safer failure than a
swallowed write.

Proven red before green with both named shapes:
`TestIsWriteStatement_CTEWrappedWrite` (nine cases — single/multiple CTEs,
`RECURSIVE`, a column list, `[NOT] MATERIALIZED`, a leading comment before
the `WITH`, each verb) and `TestIsWriteStatement_WriteNestedInsideCTE`
(three cases, including a doubly-nested `WITH` whose only write is two
levels down) all failed before the fix, pass after;
`TestIsWriteStatement_PlainCTEReadIsNotAWrite` guards the negative case, so
the fix does not over-match every CTE-shaped `SELECT` into `diffWrite`.
`TestDiff_CTEWrappedWriteRoutesToDiffWrite` proves the routing end-to-end
against real Postgres (reusing `TestDiff_WriteRejectedByRLSIsLeak`'s
"policy that can never match" mechanism, wrapped in a CTE): only `diffWrite`
can turn a `WITH CHECK` rejection into `Leak` with that reason text; the
read path would either misreport a coincidental `∅=∅` `SAFE` or fail
outright as a different kind of error. Written against the project's own
fixture database; not executed in this sandbox (no live Postgres — see
below).

**What re-running real `coder` traffic revealed — see `TGD-BL-39`'s closing
entry below for the full numbers.** The corrected computation, applied to
the exact capture `TGD-BL-38` baselined the ceiling against, produces a rate
HIGHER than the recorded baseline — the previously-recorded 131/664 was
itself measured with vacuous `SAFE`s still counted as attributed evidence,
which is now understood to have been the wrong denominator, not merely a
smaller population. See `TGD-NFR-03`'s SRS row and §7.1 for the corrected
status and why the stored constant was NOT changed in this session.

**What this session could NOT do, and why: no live Postgres was available in
the sandbox this work ran in** (no root/sudo to install it, no Docker
socket access, and — separately — no `coder` checkout or saved capture file
either, so a real `coder` re-run was out of reach regardless of database
availability). `TGD-BL-41`'s and `TGD-BL-40`'s fixes are proven at the
routing-logic level with pure unit tests (`go test`, no DB, all green) and
have DB-backed integration tests written in the existing style — they
compile and skip cleanly without `TGD_TEST_DSN`, matching how every other
DB-backed test in this suite already behaves in an environment without
Postgres, but they have not actually been run against real Postgres in this
session, and the "90 of 145 vacuous `SAFE`s reclassify" and "re-run `coder`"
steps this backlog item calls for were not performed. Both are next steps
for a session with that infrastructure, not skipped by choice.

## 10. Validation targets

Verified by reading source, not assumed.

| Target | Verified | Unverified / risk |
|---|---|---|
| **`coder/coder`** | See §10.1 — precondition (a) met, injection point confirmed, TLS not an obstacle. | Precondition (b) resolved by §9.4 rather than by the target. **First target.** |
| **`casdoor/casdoor`** ⚠ **DISQUALIFIED** | `owner` is part of the composite primary key on entities and is consistently used to filter (`Find(&permissions, &Permission{Owner: owner})`). A genuine shared-schema tenant column — the tenant-shape verification was never the problem. | `TGD-BL-04` resolved: its actual test harness (`.github/workflows/build.yml`) runs exclusively against `mysql:5.7`, and its own default config (`conf/app.conf`) is `driverName = mysql`. No PostgreSQL CI path exists. Out of scope per SRS §3.2 (`C-1`, MySQL explicitly excluded) — the wrong database engine, not merely an unverified one. |
| **`zitadel/zitadel`** | PostgreSQL-only. `instance_id` and `resource_owner` tenant columns. | Versioned table naming (`users6`…`users10`) and split `eventstore`/`projections`/`system` schemas will stress policy inference — `TGD-BL-07`. Valuable *because* it is the hard case; third target, not first. |
| **`go-gitea/gitea`** — candidate, unverified | *[read-only]*: its own CI (`.github/workflows/pull-db-tests.yml`) runs a real `postgres:14` service and a dedicated `test-integration`/`test-migration` job against it — not merely a dependency, an actually-exercised path, unlike `casdoor`. Go, multi-user with `owner_id`/organization-scoped tables — plausibly shared-schema tenant-shaped, same pattern as `casdoor`'s `owner`. | Whether a real, single tenant-column shape exists the way `casdoor`'s `owner` and `coder`'s `organization_id` do — not yet checked with the same rigor `casdoor` originally got (which is exactly what disqualified it, so this is not assumed clean either). |
| **`mattermost/mattermost`** (`server/`) — candidate, unverified | *[read-only]*: Go server, PostgreSQL-primary since MySQL support was deprecated in recent major versions; `TeamId` appears across many tables, a plausible shared-schema tenant column. | Neither the current PostgreSQL-only claim nor the `TeamId` shared-schema shape has been verified against source or a real CI run — named as a candidate worth checking next, not vetted. |

### 10.1 `coder/coder` — findings from Gate 0

| Finding | Evidence |
|---|---|
| **Injection point is `CODER_PG_CONNECTION_URL`, not `DATABASE_URL`.** Setting it to the proxy address needs no code change. | **[executed]** `TGD-BL-11` — coder's `dbpurge` suite ran through the proxy with only this variable changed; a test passed end to end. |
| **A database is created per test**, via `CREATE DATABASE … WITH TEMPLATE tpl_<migrations-hash>`. All of it traverses the same host and port, so a proxy in front observes both the DDL and the per-test connections. | *[read-only]* `dbtestutil/postgres.go` |
| **Corrected by execution.** A freshly migrated coder database contains **one** organisation (`name='coder'`, `is_default=true`) and one system user — seeded by migrations `000193_default_organization` / `000198_ensure_default_org`, not by `dbtestutil`. The earlier claim of *zero* organisations was wrong: a single-line `grep` for `INSERT INTO organizations` missed a multi-line statement. **The conclusion is unchanged** — `A1` needs two tenants and finds one, so it still aborts. | **[executed]** `TGD-BL-11` |
| **There is no RLS anywhere in `coder`'s schema.** Policy synthesis is *necessary rather than optional* and has nothing to conflict with. | **[executed]** `TGD-BL-11` — on a migrated database, `pg_policies` = 0 rows and 0 tables carry `relrowsecurity`. |
| **`organization_id` is the tenant-column candidate** — present on **24 distinct tables** in the live migrated schema. (The 252-occurrences-across-118-files figure remains a source count.) | **[executed]** `TGD-BL-11` for the 24-table figure; *[read-only]* for the source count. |
| `ConnectionParams.DSN()` hardcodes `sslmode=disable`, so §9.3's TLS constraint does not bite this target. | *[read-only]* |

## 11. Gate 0 — proxy spike: **RESOLVED, proxy holds**

Every claim below is tagged **[executed]** (observed by running code) or
**[read-only]** (established by reading source). The distinction is load-bearing
and is preserved deliberately.

### 11.1 Correction to this design's original premise

The first draft of this section asserted that "`coder` and `zitadel` both use
`pgx`." **That was wrong, and wrong in the direction that mattered.**

| Target | Driver | Statement naming |
|---|---|---|
| `coder` | `github.com/lib/pq v1.10.9`, via their `coder/pq` fork *(no `jackc/pgx` in `go.mod` at all)* | **Sequential per connection** — `"1"`, `"2"` — plus heavy use of the unnamed statement `""` |
| `zitadel` | `github.com/jackc/pgx/v5 v5.9.2` | **SHA of the SQL text** — identical SQL yields an identical name everywhere |

*[read-only]* for both, from each project's `go.mod`.

**Both naming schemes must be handled.** The two targets exercise different
drivers, which is better coverage than assumed but removes any option to support
only one.

### 11.2 Per-connection statement scoping is mandatory, not optional

*[executed]* Four connections were made to prepare four **different** SQL texts
under `lib/pq`. All four received the statement name `"1"`.

A proxy keyed on a **global** statement map therefore resolves `"1"` to whichever
SQL it saw last, and — critically — resolves it to *something*. It would then
judge that wrong query for isolation and emit a confident `SAFE` or `LEAK` about
a statement the application never ran. **That is strictly worse than failing**,
and it is the same class of defect as the blind oracle §6 exists to prevent.

A proxy keyed **per connection** resolved all four correctly.

The original design had this risk backwards: it is **real for `lib/pq`** and
**absent for `pgx` v5.10**, whose SHA-based naming makes identical names imply
identical SQL. The driver that was assumed safe is the dangerous one.

### 11.3 Result — 821 Binds, 0 unresolved, 821/821 parameters recovered

*[executed]* against PostgreSQL 18.6, four workloads:

| Workload | Binds | Unresolved | Params recovered |
|---|---|---|---|
| `pgx`, `QueryExecModeCacheStatement` (the default) | 120 | 0 | 120/120 |
| `pgx`, cache eviction — 601 distinct statements vs. 512 capacity | 601 | 0 | 601/601 |
| `lib/pq`, mixed unnamed + explicitly prepared | 80 | 0 | 80/80 |
| `lib/pq`, collision probe (§11.2) | 20 | 0 | 20/20 |

Also established *[executed]*:

- **Cache eviction is safe.** Exceeding capacity forces a re-`Parse`, which the
  proxy observes. Nothing goes unresolved.
- **Simple protocol is easier, not harder.** `pgx` interpolates parameters
  client-side, so the proxy captures fully-resolved SQL verbatim.
- **Framing holds.** A deliberately small (137-byte) read buffer was used to force
  messages to split across reads and to arrive several per read; the framer
  handled both.

### 11.4 End-to-end run (`TGD-BL-11`) — **executed**

Gate 0's original gap is closed. `coder`'s `dbpurge` suite ran through the proxy on
2026-08-29, redirected by `CODER_PG_CONNECTION_URL` alone, with **no change to
coder's source**.

| Measure | Synthetic (Gate 0) | Real (`TGD-BL-11`) |
|---|---|---|
| Binds | 821 | **1,314** |
| Unresolved | 0 | **0** |
| Parameters recovered | 821/821 | **1,314/1,314** |
| Connections | 4 | **163** |
| Simple-protocol queries | 3 | **2,266** |
| Parse : Bind | 1 : 15 | **1 : 1** |
| Statement names | hash (`pgx`), sequential (`lib/pq`) | **unnamed `""`, 1,317/1,317** |

**Three ways real traffic differs from the synthetic workloads, all material:**

1. **No statement caching at all.** Parse:Bind is 1:1, not 1:15 — every query is
   re-parsed. The cache-eviction path Gate 0 exercised is never reached here.
2. **Every statement is unnamed.** `coder` uses `database/sql` with arguments, which
   `lib/pq` serves via the unnamed statement. **The sequential-name collision in
   §11.2 never occurs in the primary target** — it requires explicit `db.Prepare`,
   which this path does not use. Per-connection scoping remains *necessary* (163
   connections each hold their own `""` concurrently), but §11.2's stated rationale
   describes a case `coder` does not produce.
3. **Simple protocol dominates** — 2,266 simple queries against 1,314 Binds. Gate 0
   barely exercised it.

**`TGD-NFR-04` converts from synthetic to measured:** 100% resolution on real
traffic, on 1,314 real Binds.

**Test outcome — attributed, but weakly.** Four `dbpurge` tests failed in the batch
run. The likely cause is the harness rather than the proxy: with
`CODER_PG_CONNECTION_URL` set, `dbtestutil` skips per-test database creation, so all
tests share one database and interfere. Supporting evidence is **a single isolated
re-run** — one of the failing tests passed against a freshly migrated database
through the same proxy.

That is one data point, not a controlled comparison. The failures have **not** been
reproduced against an unproxied baseline, and no other failing test was retried. The
attribution is plausible and unconfirmed; confirming it means running the same
subset directly against PostgreSQL and diffing outcomes.

#### `TGD-BL-12` — defect found, fixed, and proven by red/green

**The defect.** The proxy ignored the `Close` (`'C'`) frontend message, so statement
state was only ever added or overwritten, never removed. A `Bind` naming a closed
statement resolved to **stale SQL**, and the differ would have judged a query the
application never ran — silently, with a confident verdict. Exactly the `R-2` class.

**Why `coder` never exposed it.** `coder` uses only the unnamed statement, which the
protocol destroys on the next unnamed `Parse`. Ordering was enforced by PostgreSQL,
not by the proxy — so the 100% figure in the table above rested on a protocol
accident rather than on the proxy's own bookkeeping.

**Proof, in three parts.** A raw wire client was written that does
`Parse(named)` → `Close` → `Bind(same name, no re-Parse)`:

| Step | Proxy | Result |
|---|---|---|
| **Red** | pre-fix | `Bind` reported `resolved=true` against **stale SQL** — defect reproduced |
| **Green** | post-fix | `Close` observed, state removed, `Bind` reported **unresolvable** |
| **Regression** | post-fix | All four synthetic workloads re-run: **821 Binds, 0 unresolved, 821/821 parameters**, with **101 real `Close` messages** in the stream — `lib/pq` sends them routinely, so the fix path is genuinely exercised |

The red case was run and observed failing *before* the change, so the fix is
demonstrated rather than asserted.

### 11.5 Verdict

**Tier 1 keeps its no-code-changes property.** The driver-wrapper fallback is not
needed. `docs/SRS.md` is written against the proxy, subject to §11.4 and to the
TLS constraint in §9.3.

## 12. Backlog

| ID | Status | Item | Blocks |
|---|---|---|---|
| `TGD-BL-01` | **CLOSED — check run, real collision found** | Name availability check for `tenantguard`. Run against GitHub, `pkg.go.dev`, and npm — see SRS §7.8: 36 existing GitHub repos of the same/similar name, an existing Go module sharing the identical `cmd/tenantguard` binary name, and a published `tenantguard-cli` npm package in the same problem space. Recorded, not resolved by a rename — that decision belongs to the project's owner. | — |
| `TGD-BL-02` | **CLOSED** | **Gate 0** — proxy spike. Resolved: the proxy holds; 821/821 Binds resolved across four workloads. See §11. | — |
| `TGD-BL-03` | **CLOSED — confirmed** | `coder`'s template database is schema-only and `dbtestutil` seeds zero organisations, so `A1` would abort on a healthy repo and Tier 1's real cost is seeding *[read-only]*. The narrower claim that the default flow yields **exactly one** organisation is **read, not executed** — the first-user path was inspected, not run. Resolution is §9.4. | — |
| `TGD-BL-04` | **CLOSED — verified: MySQL, not PostgreSQL; `casdoor` disqualified as a target** | Read `casdoor`'s actual test harness, not its dependency list (`go.mod` alone was inconclusive — `lib/pq` is present as a dependency, but that reflects `xorm`'s multi-driver support, not what the tests exercise). `.github/workflows/build.yml`'s `go-tests` and `e2e` jobs both run against a `mysql:5.7` service container exclusively — no PostgreSQL CI job exists at all. `conf/app.conf`'s own default is `driverName = mysql`. Per SRS §3.2 (`C-1`: PostgreSQL RLS is the oracle; MySQL explicitly out of scope, "no equivalent is assumed"), `casdoor` is disqualified as a second target — not because it lacks a shared-schema tenant column (it has one, `owner`, verified genuinely), but because its actual, exercised path is the wrong database engine entirely, which this tool has no mechanism to bridge (the wire proxy only speaks PostgreSQL protocol). See SRS §11's M3 row for named replacement candidates. | Target 2 — **needs a different candidate**, see SRS §11 |
| `TGD-BL-05` | **CLOSED — measured as far as this environment allows; full metric needs a new target** | Re-baseline Tier 1 time-to-first-finding. See SRS §7.9: the tool-mechanical setup phases (`infer`+`verify`) measured under 1 second against a real 132-relation schema; the full metric (human review + live-capture time) needs a genuinely unfamiliar target, not `coder` — filed as `TGD-BL-45`. | — |
| `TGD-BL-06` | **closed — baselined and enforced, gate proven able to fire** | `TGD-NFR-03` baselined at `unattributableCeilingRate = 130.0/611.0 ≈ 0.2128` against `row_level_touching_real_app_sql` (`TGD-BL-33`'s recommended denominator), measured on the real `coder` capture `TGD-BL-31`–`-34` used, post-`TGD-BL-34`'s fixes. `cmd/tenantguard/ceiling.go`: `checkUnattributableCeiling`, called after the report is written (a breach is a completed, proven run, not an oracle-self-check abort — the report still reaches stdout), returning `ExitUnattributableCeilingExceeded` (3, reserved in §8.2 since the SRS was written, unused until now) with a stderr message naming the denominator, the measured rate and raw counts, and the ceiling. **Proven able to fire, not assumed** — `TGD-NFR-03` had been unbaselined this project's entire life, so nothing had exercised it before this session: (1) the stored baseline temporarily lowered to `0.01`, confirmed to flip `TestAuditCLI_UnattributableCeilingPassesAtOrBelowIt` and two `ceiling_test.go` cases to failing, then restored — the pass/fail boundary reads from the recorded constant, not a value hardcoded into the comparison; (2) the enforcement call in `runGateCommand` temporarily removed, confirmed to flip `TestAuditCLI_UnattributableCeilingFailsAboveIt` to failing (exit 0 instead of 3), then restored — the check is load-bearing, a deletion cannot pass silently. A pre-existing test (`TestAuditCLI_UnattributableRateByDenominator`, from `TGD-BL-33`) whose synthetic scenario's rate (33%) now legitimately exceeds the new ceiling was rewritten with more `SAFE` traffic to bring it back under 21.28%, preserving its original purpose (testing the population breakdown, not ceiling enforcement, which has its own dedicated tests) rather than silently loosening an assertion to make it pass. Another (`TestAuditCLI_UnattributableRateIsReportedNeverEnforced`) was renamed to `...ReportedEvenWhenCeilingHasNothingToCheck`, since its old name and rationale ("the ceiling must not fail a run at this milestone") were no longer true in general once this item closed — it still passes, but for the narrower, still-real reason that its scenario has zero queries in the baselined population. Full suite re-verified: `go vet`, `gofmt -l` clean, `go test ./... -race -count=1` all green, mutation harness 8/8, fixture corpus exact on both tiers. SRS updated: `TGD-NFR-03`'s row and new §7.1 (exact value, denominator, capture provenance, ratchet rule, and the explicit statement that 21.28% is a `coder`-specific baseline from one capture, not a general claim — a second target's own measurement would justify revising it, never a single bad run), `TGD-FR-08` marked `[E]`, `TGD-US-07` AC-1/AC-2/AC-3 updated (AC-2 narrowed: there is no `--ceiling` flag to "refuse" a value from — the ratchet is enforced by there being no config surface to edit at all, only a source constant a code change and a backlog entry can move). | — |
| `TGD-BL-07` | open | Policy inference against `zitadel`'s versioned/multi-schema layout. Backs `TGD-US-09` AC-4, formally moved from `M2` to `M4` this session (SRS §11) — bundled with `TGD-BL-09`'s `zitadel` session-handling gap under one milestone rather than split across two. | Target 3 |
| `TGD-BL-09` | **open — half answered** | `SET`/`set_config` tenant context. `coder`: confirmed **no** *[read-only]*, so L5 stays a Tier 1 blind spot there. `zitadel`: **still open** — only `go.mod` was read, not session handling; not closed on that basis. | §7.4 scope |
| `TGD-BL-10` | **CLOSED — implemented, `A1` proven both ways** | **Probe-database seeding for `A1`** (§9.4): implemented in `internal/schema` (inference) and `internal/oracle` (probe creation, canary seeding, RLS synthesis, `A1`). **[E]**: `A1` passes with two canaries and a real policy, and — proven by watching each one fail before the fix — **aborts** on a single-tenant probe, a `USING(true)` policy, and a `BYPASSRLS` role. All three abort paths were shown silently passing under a deliberately weakened inequality (`>` instead of `>=`), then shown failing correctly once restored. 11 integration tests, all gated behind `TGD_TEST_DSN` with a loud skip when unset — an oracle test suite that was never run must not look like one that passed. 7 mutations run against `oracle.go`/`probe.go`, all caught after two targeting errors in the mutation harness itself were found and corrected. **Scope boundary found by mutation, not by design:** `A1` alone cannot detect a policy attached to the wrong column — a policy on `id` still satisfies `A1`'s inequality, because the wrong predicate still excludes rows, just not the right ones. Catching that requires `A4` (positive control), not yet built. See `TGD-BL-15`. | — |
| `TGD-BL-11` | **CLOSED — executed** | `coder`'s `dbpurge` suite run end-to-end through the proxy: 1,314 Binds, 0 unresolved, 1,314/1,314 parameters recovered, 163 connections. `TGD-NFR-04` is measured, not synthetic. See §11.4. | — |
| `TGD-BL-12` | **CLOSED — fixed, red/green proven** | Proxy ignored `Close` (`'C'`); a `Bind` on a closed statement resolved to stale SQL. Fixed by removing per-connection statement state on close. Red case observed failing pre-fix; green passes post-fix; regression 821/821 with 101 `Close` messages exercised. See §11.4. | — |
| `TGD-BL-13` | open | Tier 1 operational gaps found during `TGD-BL-11`: `dbtestutil` does **not** migrate an externally-supplied database, and with `CODER_PG_CONNECTION_URL` set it skips per-test database creation so tests share one database and interfere. Neither is stated in the SRS. | Tier 1 docs |
| `TGD-BL-15` | **CLOSED — `A4` built, proven, and shown non-redundant with `A1`** | `CheckA4` implemented in `internal/oracle`: a hand-written reference query (`WHERE tenant_column = tenant`, run unrestricted) is compared row-for-row against the same table queried with no predicate at all, as the restricted role with the session tenant set — a correct policy must reproduce the reference exactly. Proven, all against a real PostgreSQL: passes on a correct policy; aborts (wrapping a new `ErrA4`, exit code 13, distinct from `ErrA1`/exit 10) on the `id::text` wrong-column policy that survived `A1`, and separately on a `USING (false)` policy that hides a valid tenant's own rows. Both abort cases proven by watching them fail: comparison weakened to length-only, tenant-set skipped, and the wrap-`ErrA4` line replaced, each caught by a distinct assertion — 7 mutations, 7 caught. **Non-redundancy demonstrated directly, not asserted**: a single-tenant probe with a *correct* policy makes `A1` abort (cannot demonstrate withholding) while `A4` passes (rows are identical) — the A1-only case; the two-tenant `id::text` policy makes `A1` pass (0 restricted rows is "strictly fewer" than 2, deceptively) while `A4` aborts (0 rows returned where 1 is correct) — the A4-only case. Neither check subsumes the other. A `ProofState.PolicyProven()` gate was added and tested directly (six cases, including "A1 passed, A4 never run" → must still refuse) so `TGD-US-03` AC-6 (`A1` passing is necessary but not sufficient) is an executable assertion, not only a documentation claim. | — |
| `TGD-BL-14` | **CLOSED — implemented and measured** | **Backend-direction capture with OID-based binary decoding** (§9.5, `TGD-FR-19`). Without it every binary parameter is `UNATTRIBUTABLE`; `coder` is text-only so it never surfaced, but `zitadel` uses `pgx` which favours binary, so the second Tier 1 target would breach the unattributable ceiling. **Correctness requirement, not an enhancement.** Implemented in `internal/capture`: `Describe` correlation (FIFO, because backend replies carry no statement name), `ParameterDescription`/`RowDescription` parsing, OID-keyed decoders. Measured **[E]**: value recovery 861/961 → **961/961** on an identical workload set. Residual **[R]**: `zitadel` untested; types outside the decoder set stay undecodable by design. | — |
| `TGD-BL-16` | **CLOSED — A2/A3 wired, M1–M8 harness green** | `CheckA2` (policy coverage: `pg_class.relrowsecurity` + `pg_policies` count; deliberately skips any relation not `schema.Scoped`, proven by a dedicated boundary test) and `CheckA3` (role privilege: superuser, `BYPASSRLS`, owner+`FORCE`) implemented and proven against real PostgreSQL, both abort directions each. **[executed]**: 12 mutations against `CheckA2`/`CheckA3`/the extended `ProofState` (now requiring all four checks), all caught. `ProofState.PolicyProven()` extended to `A1`–`A4`, including the case named by the design's own review — "A1, A3, A4 passed, A2 never run" → must still refuse. **Unified `M1`–`M8` mutation harness built** (`TestMutationHarnessM1ThroughM8`): found `M3` and `M7` mapped to the wrong gate in the original §8 table — both are "exclude every row" defects, which `A1`'s row-count check cannot distinguish from correct filtering, so only `A4` catches them. Corrected in §8, as a stated property of the two checks rather than a bug in either. `M8` was added specifically to answer whether `A1` has any coverage `A4` doesn't already subsume — **[executed]**: on a single-tenant probe, `USING (true)` is indistinguishable from a correct policy to `A4` (no second tenant's row to wrongly admit), but `A1` still fires, since it never inspects content. All eight mutants now pass their mapped gate; the harness itself was proven capable of failing by disabling `A1`, which correctly turned `M8` into a blind spot and `M2` into a wrong-gate finding. | — |

| `TGD-BL-17` | **CLOSED — `TGD-US-01` AC-4 (synthesis confirmation), proven by seam and mutation** | `EnableRLS` now re-reads `pg_policies`/`pg_class` after its DDL via `verifySynthesis`, and does not report success on its own say-so. **A real "DDL succeeded, catalog disagrees" state could not be constructed** — PostgreSQL's autocommit + READ COMMITTED defaults make a committed `CREATE POLICY` immediately visible to any later query on any connection, ruling out a single-writer divergence with no concurrent interference. Stated as fact rather than worked around. Proven instead: **[executed]** three deterministic bad-catalog states (RLS not enabled, RLS not forced, no policy) built directly, bypassing `EnableRLS`, each caught; **[executed]** the wiring itself, via a controlled seam (`verifySynthesisFn`, substituted with a stub that fails unconditionally while `EnableRLS`'s real DDL still ran against Postgres and succeeded) — `EnableRLS`'s returned error was confirmed to carry the stub's sentinel; and the literal mutation this AC calls for — removing the call to the seam — made the wiring test fail red for the predicted reason, then restored. |
| `TGD-BL-18` | **CLOSED — CLI wiring: `verify`/`audit`, exit codes, minimal report, proven end-to-end** | `cmd/tenantguard` gained `verify` and `audit`, both running the same gate: build a probe from `--dsn`'s own database as template, seed two canaries, synthesise RLS on `--table`/`--tenant-column`, run `A1`–`A4`, and either print a minimal JSON report (`proven`, per-check pass/fail) to stdout and exit 0, or print nothing to stdout, a reason to stderr, and exit the mapped code (10/11/12/13). **[executed]**, against real PostgreSQL, invoking the **built binary** via `os/exec` rather than calling Go functions: a clean run exits 0 with a valid report; an aborted run exits the mapped code with stdout asserted byte-empty; `audit`'s clean path additionally notes on stderr that its differential stage (`TGD-US-06`) is not implemented; the probe database is confirmed dropped on both the success and failure paths. 5 mutations run against the exit-code mapping and the CLI's report/cleanup logic, all caught. `TGD-US-01` AC-4 and `TGD-US-02` AC-5 close on this evidence.

  **Two findings surfaced by testing the CLI end-to-end, neither silently patched:**

  1. **The CLI cannot reach an `A4`-only abort at all**, and this is structural, not a bug to fix in this pass. `SeedCanaries` and `EnableRLS` both derive their write/synthesis target from the same `--tenant-column` value, so whichever column is named becomes, by construction, a valid discriminator — the two canaries get written into it regardless of whether it is the application's real tenant column. Verified directly: pointing `--tenant-column` at a second genuine text column produced a clean, proven exit 0, not an abort. `M3`'s defect (seed into the correct column, synthesise against a different, wrong one) requires a mismatch between what was seeded and what was synthesised against — expressible only by calling the oracle functions directly with deliberately inconsistent relations, as `internal/oracle`'s own tests do, not through any single-flag CLI surface.
  2. **`SeedCanaries`'s `SkippedTable` result is discarded by the CLI gate.** Pointing `--tenant-column` at a non-text column (`id`, the bigint primary key) makes the canary insert fail with a type error; `SeedCanaries` correctly marks the table skipped per its own contract rather than raising a hard error, but `runOracleGate` ignores that list, leaving the probe with zero rows in the target table. `A1` then aborts on "0 unrestricted, 0 restricted — not strictly fewer," which is a *correct* abort but for a message ("the oracle is blind") that reads as a policy problem when the real fault is an operator naming the wrong column entirely. The CLI test asserting this behaviour documents it as the actual, current behaviour — surfacing the skip reason directly in the CLI's stderr message would be a real improvement, tracked as `TGD-BL-19`, not built in this pass.

  **Scope narrowed and stated plainly, not silently:** no policy-file format exists (§8.1's `--policy <f>` is not implemented; this CLI takes `--table`/`--tenant-column` directly, one table per invocation), `infer` is not wired to any command, and `audit`'s differential stage (per-query `SAFE`/`LEAK`/`UNATTRIBUTABLE` verdicts, `TGD-US-06`) does not exist — `audit` currently runs the identical self-check gate as `verify` and says so on success. | — |
| `TGD-BL-19` | **CLOSED — fixed, red/green proven, mutation-proven** | `runOracleGate` now checks `SeedCanaries`'s `skipped` result before proceeding to any oracle self-check, and fails immediately with a distinct `errSeedingSkipped` (exit 2 — usage/configuration error, not an `A1`–`A4` code, since no self-check ever ran) naming the table and the real Postgres error. **[executed]**: red confirmed first — the previous `--tenant-column id` test asserted exit 10 with an "oracle is blind" message, genuinely misleading for an unseedable-column input mistake; rewritten to assert exit 2 with a seeding-specific message, confirmed failing against the unfixed code, then green after the fix. 2 mutations (skip check removed, exit-code mapping removed), both caught. | — |
| `TGD-BL-20` | **CLOSED — CI pipeline built and executed, not only written; `TGD-US-11` AC-6** | `.github/workflows/ci.yml`: three jobs — build/vet/gofmt/script-executable-bit, test+race+coverage-floor+the `M1`–`M8` harness (with a real Postgres service), and a workflow-YAML lint job. `scripts/` holds four executable helpers (`check-coverage.sh`, `assert_tests_ran.py`, `check_workflow_yaml.py`, `run_tests_with_assertions.sh`), each `chmod +x` on disk from creation, applied prospectively since this repository has no git index yet to retroactively fix. `assert_tests_ran.py` is the load-bearing piece: it parses `go test -json` and fails the job on any skipped test anywhere, and separately confirms all eight `M1`–`M8` subtests reported pass by name, not merely "some tests ran."

  **Executed, not only written** — `actionlint` and `act` were `go install`ed fresh (both real, independent, verified tools; not hand-rolled) and used to lint, then actually run, the workflow against real Docker containers including a genuine PostgreSQL service. Both directions of the central requirement were demonstrated: `A1`'s inequality check was disabled (the same mutation that blinds `M8`) and the pipeline was re-run — it failed, naming `M2`, `M8`, and the standalone `A1` abort tests by name via `assert_tests_ran.py`'s output; reverted, it passed green again. The silent-skip failure mode this whole CI effort exists to prevent was separately reproduced: removing the Postgres `services:` block entirely made the `wait for postgres` step time out and fail the job outright, rather than letting every oracle/CLI test skip and the job pass green having tested nothing.

  **Four real defects found and fixed by running the pipeline, none of which a syntax check would have caught:** `pg_isready` absent from the runner image (replaced with a pure-bash `/dev/tcp` TCP check); `actionlint`'s default discovery requires a git repository to find `.github/workflows/`, which this checkout does not reliably have (fixed by passing the file path explicitly); `PyYAML` not preinstalled (added an explicit install step); that install then blocked by PEP 668 "externally managed environment" (fixed with `--break-system-packages`, safe on an ephemeral runner).

  **Stated, not hidden: at the time this was written, this repository had no git history and no remote, so GitHub itself had never run this workflow.** `act` is a well-regarded, widely used local Actions runner, but it is not GitHub's own infrastructure, and whether GitHub's hosted `ubuntu-latest` matches `act`'s local image in every particular is `[R]`. The repository setting that is literally GitHub's merge-blocking mechanism — marking this job a required status check in branch protection — needs push access this session does not have, and is unconfigured and unverified. The workflow is *structured* to support it (three independent jobs, a `needs:` dependency, a non-zero exit failing the job like any other); the setting itself is a task for whoever pushes this repository, not something claimed as done here. | — |

| `TGD-BL-21` | **CLOSED — differ core built, red/green proven, mutation-proven; `TGD-US-06`** | `internal/differ`: `ExtractTenant` (a text heuristic, not a parser — finds a plausible tenant value from a WHERE-clause comparison or, for `INSERT`, from the column-list/VALUES-list position) and `Diff` (re-executes unrestricted and restricted-as-tenant inside a transaction that is always rolled back, and multiset-compares row sets). 24 unit/integration tests; 12 mutations (5 against `extract.go`, 7 against `differ.go`), all caught, against real PostgreSQL. | — |

  **A finding made by running the differ, not by reading the design: PostgreSQL sequences are not transactional.** A rolled-back re-execution of an `INSERT` into a table with a `bigserial` column still advances the sequence, so the `RETURNING` row's generated id differs from the first execution's even for a fully correct, tenant-consistent write — naive row-diffing would report every such write as a false `LEAK`, unconditionally. Neither `TGD-US-06` AC-2 (written for `SELECT`'s row sets) nor AC-6 (states the no-write constraint, not a comparison mechanism) specifies what a write's comparison should be. Resolved by `diffWrite`: for `INSERT`/`UPDATE`/`DELETE`, compare **acceptance vs. rejection** under RLS's `WITH CHECK`/`USING` enforcement instead of row content — consistent with this project's whole approach of delegating correctness to PostgreSQL rather than reimplementing it. Documented in code as an interpretation filling a gap the design left open, not something the design states outright. | — |

| `TGD-BL-22` | **CLOSED — two missing grants found and fixed by running the differ's own insert fixture, not by inspection** | `oracle.CreateRestrictedRole` granted `SELECT` only, sufficient for `A1`–`A4` but not for `diffWrite`'s dry-run `INSERT`/`UPDATE`/`DELETE` attempts. Fixed in two steps, each found by reading the exact Postgres error text from a real run: (1) extended the table grant to `SELECT, INSERT, UPDATE, DELETE`; (2) a `bigserial` column's `DEFAULT nextval(...)` needs a separate `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public` — not implied by the table-level `INSERT` grant, a classic PostgreSQL gotcha. Both regression-tested against the full `internal/oracle` suite (unrelated to writes) after each change; both passed clean. | — |

| `TGD-BL-23` | **CLOSED — 24-fixture corpus built exactly to design doc §7, exact-set equality proven; `TGD-US-06` AC-4** | `internal/differ/corpus_test.go`: all 24 fixtures (`S1`–`S10`, `L1`–`L10`, `U1`–`U4`) declared with their expected Tier 1 (and, as documentation, Tier 2) verdicts before any `Diff` call ran; all 24 landed exactly where declared on the first real run. The harness's own falsifiability was checked directly: a deliberately wrong expected verdict was injected and confirmed to fail the test before being reverted, and the three assertion mechanisms (per-fixture check, set-size cross-check, tier-consistency self-check) were each independently mutation-tested. | — |

  **A second, structural blind spot found while building `L7` ("view lacking its own scoping"), not anticipated by the design.** The design doc's §7.2 table lists `L7` as an ordinary, tool-catchable `LEAK`, with no blind-spot annotation (unlike `L5`, which §7.4 explicitly flags). Verified empirically against real PostgreSQL 18 before trusting it: a view without `security_invoker=true` evaluates RLS using the **view owner's** privileges, not the querying role's. Since the view owner in any realistic target (and in this test harness) is the same privileged/migration role this tool already uses as its "unrestricted, sees-everything" baseline, RLS is bypassed identically on both legs of the diff — restricted and unrestricted see the same rows, and the verdict comes back `SAFE` regardless of whether a real leak exists. Confirmed there is no view-ownership configuration that produces a genuinely detectable leak through this construction: a non-superuser, non-`BYPASSRLS` owner just makes the view correctly scoped instead, since the policy expression reads the **calling session's** GUC regardless of whose privileges gate the `SELECT`. Unlike `L5`, Tier 2 (context propagation) does not fix this — it is a blind spot at every tier, because the flaw is in the re-execution method's choice of baseline, not in tenant attribution. Per user decision, `L7`'s expected verdict was changed to `SAFE`/`SAFE` (a second documented limitation, not a defect), and it is recorded in `LIMITATIONS.md` alongside `L5`. | — |

| `TGD-BL-24` | **CLOSED — differ wired into `audit`; two scope/authority conflicts surfaced and resolved by user decision before building, not worked around silently** | `cmd/tenantguard`: new `capture.DecodeEvents` (Recorder's exact inverse, mutation-tested, including one documented equivalent-mutant blind spot — a null parameter's `value_base64` is always empty on the wire, so its explicit no-op case is behaviourally identical to falling through to the default branch); a new `--events FILE` flag reads a prior `tenantguard capture --out FILE` JSONL run (no live pipe between `capture` and `audit` exists — same narrowing pattern as `--table`/`--tenant-column` vs. a policy file, stated plainly rather than silently); `runOracleGate` gained an `onProven` callback so the probe/restricted connections stay live for the differ pass without weakening the gate (`onProven` is only ever invoked after `ProofState.PolicyProven()` succeeds); a second, redundant `ProofState` assertion sits at the report layer itself, per the standing rule that the report is gated, not only the pipeline that produced it (an intentional, documented blind spot to mutation testing, since it cannot be reached without a bug in the gate it duplicates). | — |

  **Two conflicts surfaced and resolved by explicit user decision before implementation, not by silent interpretation:** (1) `TGD-NFR-03`'s 5% ceiling is marked `[U]` — "advisory until `TGD-BL-06`", an **M3** milestone item not yet reached — and `TGD-US-07` AC-3 states the ceiling must not fail a run until baselined. The task's own instruction said to enforce it; the SRS says it must not yet. Resolved: this build reports `unattributable_rate` (only when `--events` produced at least one verdict; a nil/omitted field distinguishes "not audited" from a genuine 0%) and never fails a run because of it — enforcement remains an open `M3` item, not silently built early. (2) `TGD-FR-18` says the tool "shall **detect**" shared-database mode; its own `[E] C-7` citation is evidence about *`coder`'s* test harness from a prior run, not evidence the tool implements detection — and it structurally cannot, since a single `--dsn` invocation carries no signal distinguishing a reused database from a dedicated one. Resolved: `--shared-database` is an **operator declaration**, echoed verbatim into the report header (`"shared_database"`, never omitted, so "declared not shared" is distinguishable from "never asked") — not automatic detection. Both narrowings are stated here and in the CLI's own `--help` text, not left implicit. | — |

| `TGD-BL-25` | open — pre-existing, not caused by `M2` work | `TestVerifyCLI_ProbeDatabaseIsAlwaysDropped` flakes under plain `go test ./...`: Go parallelizes across packages by default, so `internal/oracle`, `internal/differ` and `cmd/tenantguard` all create/drop `tgd_probe_*` databases concurrently against the same test Postgres instance, and this test's `SELECT count(*) ... WHERE datname LIKE 'tgd_probe_%'` can catch another package's in-flight probe. Confirmed: reliably green in isolation (`go test ./cmd/tenantguard/...`, 3/3 runs) and reliably green for the full suite under `go test ./... -p 1`. Fix is either scoping the count to this test's own probe name prefix (it already uses a distinct `tgd_cli_dropcheck` target, but `runOracleGate` still names probes `tgd_probe_<nanotime>` generically) or accepting `-p 1` for this suite. Not fixed here — outside `M2`'s scope. | — |

| `TGD-BL-26` | **partially closed — multi-table CLI, `infer`, and report gaps closed; one gap left open by design** | Follow-up to the gap list produced by re-reading `TGD-US-04/05/09/10` against the code as it stood after `TGD-BL-21`–`25`. Closed: (1) `tenantguard infer --dsn URL --out FILE` — `schema.Infer`/`schema.Classify` existed as a tested library with zero CLI callers; now the only way a policy file (`schema.WritePolicy`/`ReadPolicy`, JSON) comes into existence. (2) `verify`/`audit` take `--policy FILE` instead of a single hand-supplied `--table`/`--tenant-column` pair, and sweep **every** `schema.Scoped` relation in the file in one run — `runOracleGate` now loops `EnableRLS`/`CheckA1`/`CheckA4` per relation (`CheckA2`/`CheckA3`/`SeedCanaries` already took slices) and aggregates via a new pure function, `aggregateProof`, which is proven only when **every** table's A1 and A4 both passed — one bad table aborts the whole sweep, not just its own row. `aggregateProof` is unit-tested and mutation-tested directly (database-free), because the CLI's own e2e surface cannot reach a genuine per-table A1/A4 divergence — `SeedCanaries` and `EnableRLS` derive both from the same `TenantColumn` per relation, the same structural limitation `TGD-BL-18` already documented for the single-table case, now confirmed to also block testing the AND-fold at the e2e layer. (3) `TGD-US-05` AC-2: `ExtractTenant`'s unattributable reason now names every unrecoverable `$N` position instead of "at least one parameter." (4) `TGD-US-05` AC-3 / `TGD-US-10` AC-3: `verifyReport` gained `Counts` (`safe`/`leak`/`unattributable`, summing to the query total), `Tier` (always `1`), and `ProofSource` (always `"probe"`). (5) `TGD-US-10` AC-1: `queryVerdict.Params` carries the full bound parameter list, not just the tenant value. (6) `TGD-US-10` AC-2: a real `--output json\|text` flag exists, so the format is actually selectable and testable rather than being an unexercisable default. All of the above proven against real PostgreSQL (`go test ./... -p 1 -count=1`, full suite green) with new/updated CLI e2e tests plus the database-free `aggregateProof` unit tests, and multiple mutations run against each new piece of logic (positions-in-reason, counts summation, `--output` dispatch, the AND-fold, `ReadPolicy`'s error path) — every one caught. **Left open by design, not silently skipped:** `TGD-US-10` AC-1's "the test or code path a LEAK was reached from" — capture records only a connection id, no provenance to a test name or call site, and building that is a capture-layer change out of this session's scope. `TGD-US-09` AC-2's "explicit human acceptance" is narrowed to "the policy file must exist and verify/audit will not infer on the fly" — there is no per-entry reviewed/accepted flag inside the file itself; a future build might want one. **The remaining `TGD-US-10` AC-1 clause is formally moved from `M2` to `M5` (SRS §11), not left dangling under a closed milestone** — a cross-cutting capture-layer feature, not required to prove Tier 1 produces real findings. | — |

| `TGD-BL-27` | **closed — fixed, red/green proven, mutation-tested** | Both gaps this entry originally reported are now closed. (1) `probe.go` gained `canaryText`/`canaryLiteral`, a type dispatch for the tenant column's canary identity mirroring `sampleValue`'s own switch-on-normalized-type-name pattern: `uuid` gets two fixed, distinct, `::uuid`-cast literals (`canaryAUUID`/`canaryBUUID`); every previously-supported text-like type is byte-for-byte unchanged. `oracle.TenantCanaryText` is the single source of truth both `SeedCanaries`'s INSERT and `CheckA1`'s `set_config` call now use (`EnableRLS`'s policy compares `tenant_column::text = current_setting(...)` as plain text equality, so the two must agree bit-for-bit or `CheckA1` sets a session variable that can never match the row it just seeded — the gate's canary-based `CheckA4` call was updated the same way). (2) `sampleValue` gained array support (any `T[]` gets `'{}'::T[]` — an empty array is valid for any element type and satisfies `NOT NULL` without inventing element data); a new `enumSampleValue` catalog-lookup fallback covers `NOT NULL` enum columns, which (unlike every other type `sampleValue` handles) cannot be recognised from the type name alone since every enum type in a database has a different name. A tenant column of a type the dispatch does not recognise is now reported with a named `"...has unsupported type..."` reason *before* any INSERT is attempted, never as a raw driver syntax error (TGD-BL-19's lesson, now also enforced for the tenant column specifically). Proven: `internal/oracle/canary_type_test.go` — pure unit tests pin `canaryLiteral`/`canaryText`/`sampleValue`'s exact output (catching a same-value-for-A-and-B mutant, a missing `::uuid` cast, wrong array syntax); integration tests seed a real `uuid` tenant column, a real enum `NOT NULL` column, and a real array `NOT NULL` column, each confirmed red before the fix (the `uuid` case reproduces the exact `22P02` error above) and green after; `TestA1DiscriminatesWithUUIDTenantColumn` proves the fix is semantically load-bearing, not just syntactically valid — 2 canary rows seeded, `CheckA1` restricted count is exactly 1 (a genuine match), not the vacuous 0 a partial fix (uuid INSERT fixed, `CheckA1` left hardcoded) produces — confirmed by reverting just that wiring and watching the count fall back to 0. Full suite green (`168 tests, 0 skipped, 8/8 mutation-harness subtests, 63.2% coverage`) with no regressions to the pre-existing text-tenant-column path. | — |
| `TGD-BL-28` | **closed — both gaps fixed, red/green proven against reproductions of the exact real shapes, mutation-tested** | (1) `SeedCanaries`'s per-column value reuse: `columnSampleValue` is now called with a DIFFERENT row index for each canary row (`2*i` / `2*i+1`), and every case in `sampleValue` (plus the new `enumSampleValue`, which now also takes `i` and selects among a type's labels by `i mod len(labels)`) varies its output with that index — including two cases with no natural notion of "distinct" before this: `boolean` now alternates `false`/`true` by parity, and the array branch wraps a per-row-distinct single element (`ARRAY[sampleValue(elem, i)]::elem[]`) when the element type is itself known, falling back to the old always-`'{}'` behaviour only when it is not. (2) `CheckA4`'s hardcoded `id` assumption: a new `rowKeyColumns` derives row identity from the catalog — the relation's primary key columns (`pg_index`/`indisprimary`) if it has one, else every column — and `rowKeyExpr` concatenates them (each `coalesce`d against a NUL-free sentinel, cast to text) into one comparable key; `CheckA4`'s return type changed from `[]int64` to `[]string` accordingly (every caller only ever checked `len()`, confirmed by grep before changing it). A relation with zero columns (legal in PostgreSQL, `CREATE TABLE t()`) returns the new `ErrA4NoRowKey`, kept distinct from `ErrA4` all the way to the CLI gate — `runOracleGate` checks for it specifically and aborts before `aggregateProof` ever sees it, so it cannot surface through `PolicyProven` dressed up as a policy failure (TGD-BL-19's lesson, applied one layer further out; noted as defensive/unreached through the gate's own call graph, since `SeedCanaries` succeeding already guarantees a non-empty row key — proven directly against `CheckA4` itself instead, which does not share that precondition). Proven: `internal/oracle/canary_type_test.go` gained a reproduction of coder's exact `audit_logs` shape (`id uuid NOT NULL PRIMARY KEY`, no default, plus other no-default `NOT NULL` columns) — confirmed red (`duplicate key value violates unique constraint`, the literal error the real run hit) before the fix, green after, with every non-tenant column checked for actual per-row distinctness, not just a non-error exit code; and a reproduction of `organization_members`'s exact shape (composite PK, no `id` column at all) — confirmed red (`column "id" does not exist`, again the literal real error) before, green after, with `CheckA1` *and* `CheckA4` both passing non-vacuously, plus a wrong-column-policy variant proving the row-key rework did not weaken A4's actual discriminating power. `TestCheckA4NoRowKeyIsNamedError` pins `ErrA4NoRowKey` distinct from `ErrA4`. Both fixes reverted individually and confirmed to reproduce their real-world error exactly (mutation proof, matching this session's `TGD-BL-27` practice). One bug found and fixed while writing the composite-PK test itself: `rowKeyExpr`'s original NULL sentinel embedded a raw `\x00` byte in the constructed SQL text, which corrupted PostgreSQL's NUL-terminated simple-query wire message (`insufficient data left in message`, a protocol error, not a SQL one) rather than the intended NULL-collision guard — replaced with non-NUL control-character sentinels. Full suite green: `175 tests, 0 skipped, 8/8 mutation-harness subtests, 63.7% coverage`, no regressions. | — |
| `TGD-BL-29` | **closed — §9.6's sampling mechanism implemented; see `TGD-BL-30` for the fix and the fourth real `coder` re-run** | Re-ran the identical real `coder` checkout end to end a third time: migrations, `infer` (**22/95/15 again**, a third independent confirmation), `verify` against the full 22-table policy file. The sweep now gets **past both `TGD-BL-28` gaps** — `audit_logs` (the uuid-PK-no-default shape) seeds successfully, and the run advances to `public.chat_model_configs`, alphabetically well past `audit_logs` — before stopping on a **new, third defect**: `chat_model_configs_ai_provider_required_when_active CHECK (deleted = true OR ai_provider_id IS NOT NULL)`. `ai_provider_id` is nullable with no default, so `SeedCanaries` — correctly, by its own existing contract — leaves it `NULL`; `deleted` is `NOT NULL boolean DEFAULT false`, so `SeedCanaries` — also correctly, by contract — lets the database supply it, giving `false`. Both individually-correct choices combine into `false = true OR NULL IS NOT NULL` = `false`, and the second canary row's `INSERT` fails `violates check constraint ... (23514)`. This is a **different shape from both `TGD-BL-27` and `TGD-BL-28`**, not a variant of either: it is not about one column's type or one column's uniqueness, but a **CHECK constraint spanning two columns' semantic relationship**, which nothing in `SeedCanaries`'s column-by-column, type-only dispatch has ever had a way to see — the constraint references *another column's* value, not just its own. The same table's `chat_model_configs_compression_threshold_check CHECK (compression_threshold >= 0 AND compression_threshold <= 100)` is a second, independent way this table would ALSO have failed even had the first constraint been satisfied: `compression_threshold integer NOT NULL` with no default gets `sampleValue`'s generic `1000+i`, which is always `> 100` — a single-column bound this dispatch also has no way to see, since `sampleValue` only ever reasons about a column's *type*, never its *value range*. Sized against the catalog: **10 of the 22** real scoped tables carry at least one `CHECK` constraint (`chat_model_configs`: 5, `workspace_build_orchestrations`: 7, `mcp_server_configs`: 5, `chat_user_model_overrides`: 3, `chat_organization_model_overrides`, `custom_roles`, `groups`, `provisioner_jobs`, `workspace_builds`: 1 each, `workspaces`: 2) — read-only cataloguing only, not independently verified as each being the cross-column or out-of-range shape specifically. **Per the standing instruction, this is the third consecutive "assumption nothing had tested" finding against the same target, so it is filed and this session stops here rather than patching it.** `SeedCanaries`'s design generates a value for each column in isolation and has no representation of cross-column or single-column-range constraints at all; three real, independent, previously-invisible gaps in three consecutive fix cycles is a signal the seeding/probe approach itself needs a design review — a pre-flight catalog read of every `CHECK` constraint on a relation before attempting to seed it, or a fallback that reports which constraint blocked seeding and why, rather than another one-off per-shape patch. Not scoped or attempted this session. The full 22-table sweep still aborts before A1–A4 or the differential stage ever run — no SAFE/LEAK/UNATTRIBUTABLE counts, no tenant-attribution assessment on real traffic; the differ has still never been reached against `coder`. | Design review of `SeedCanaries`'s seeding strategy with respect to `CHECK` constraints — cross-column and single-column-range — before attempting a fourth per-shape patch. Not scoped this session. |
| `TGD-BL-41` | **closed — fixed this session, correctness bug, prioritized ahead of `TGD-BL-40` on that basis; DB-backed proof written but not executed (no Postgres in this sandbox)** | **A third `isWriteStatement` gap, same class as `TGD-BL-31`'s (leading-comment) fix, found while inspecting `TGD-BL-39`'s vacuous-`SAFE` results.** `DeleteOldWorkspaceBuildOrchestrations` (46 occurrences) and `ExpirePrebuildsAPIKeys` (44 occurrences) are both real `coder` writes shaped `WITH <cte> AS (...) DELETE/UPDATE ...` — a CTE-wrapped write, previously routed through the read-path comparison instead of `diffWrite` regardless of whether it touched a row: a wrong verdict *mechanism*, not merely a wrong flag. **Fixed by classifying the statement's actual kind, not its leading keyword position:** `statementContainsWrite`/`skipWithClause` (`internal/differ/differ.go`) walk past a leading `WITH` (RECURSIVE, column lists, `[NOT] MATERIALIZED` all handled), recursing into every CTE body — reusing `extract.go`'s existing `balancedParenSpan` — so a write nested entirely inside a CTE body (outer statement a plain `SELECT`) is caught too, not just a CTE-then-write outer shape. See §9.7 for the full account. **Proven red before green:** 9 outer-CTE-write cases and 3 nested-in-CTE cases (`TestIsWriteStatement_CTEWrappedWrite`, `TestIsWriteStatement_WriteNestedInsideCTE`) all failed before, pass after; a negative case (`TestIsWriteStatement_PlainCTEReadIsNotAWrite`) guards against over-matching. All four are pure unit tests, run and green in this session with no DB required. An end-to-end DB-backed test (`TestDiff_CTEWrappedWriteRoutesToDiffWrite`) was written in the existing fixture-database style and compiles clean, but this sandbox has no live Postgres (no sudo to install one, no Docker access) — it has not actually been run. **Not yet done: re-running real `coder` traffic to confirm the 90 affected `SAFE`s reclassify** — this needs a `coder` checkout and a capture file, neither present in this environment; see §9.7's closing note. | Re-run `coder` once a real Postgres + `coder` checkout + capture file are available, to confirm the 90 previously-misrouted `SAFE`s now reclassify via `diffWrite`, and to run `TestDiff_CTEWrappedWriteRoutesToDiffWrite` against real Postgres for the first time. |
| `TGD-BL-40` | **closed — implemented this session; DB-backed proof written but not executed (no Postgres in this sandbox)** | **Write-path vacuous detection.** `diffWrite` now re-executes both legs via `execRolledBack`/`execRolledBackAsTenant` (`ExecContext` + `sql.Result.RowsAffected()`) instead of `runRolledBack`'s `QueryContext`, which reported no row data for a write without `RETURNING` regardless of how many rows it actually touched. `RowsAffected()` comes from PostgreSQL's command-complete tag either way (confirmed against `lib/pq`'s `Exec`, which drains `RETURNING` rows itself and still reports the real count), so one code path serves both shapes — no `RETURNING`-vs-not branch needed. A `Safe` write is now flagged `Vacuous` when both legs affected zero rows, mirroring the read path's rule exactly. **Proven with three DB-backed test cases** (`TestDiff_WriteVacuousWhenBothLegsAffectZeroRows` — no `RETURNING`, the exact shape the old mechanism could never observe; `TestDiff_WriteVacuousWithReturningClauseHandledCorrectly` — `RETURNING` present, confirming no regression; `TestDiff_WriteNotVacuousWhenRowsAffected` — the non-vacuous negative case with `RETURNING`) plus the existing `TestDiff_WriteAcceptedByRLSIsSafe`'s no-`RETURNING` negative case. All compile clean against the project's fixture database; none have been run against real Postgres in this session (no live Postgres in this sandbox — no sudo to install one, no Docker access). **Not yet done: re-running real `coder` traffic** — `TGD-BL-39`'s 44 known-vacuous `DeleteOldWorkspaceAgentStats` writes should now be flagged automatically rather than only by hand-inspection, but this needs a `coder` checkout and capture file, neither present here. | Re-run `coder` once real Postgres + a `coder` checkout + capture file are available: confirm the 44 `DeleteOldWorkspaceAgentStats` writes are now flagged `Vacuous`, get the combined (read-path + write-path) vacuous count and the four corrected denominators, and only then set the third `unattributableCeilingRate` baseline (§9.7, §7.1). |
| `TGD-BL-39` | **closed — decided (a flag, not a fourth verdict) and implemented; re-run reveals the current ceiling baseline no longer holds under the corrected denominator** | See §9.7 and §5 for the decision and what was built (read-path `Vacuous` detection, the ceiling's denominator correction) and `TGD-BL-40`/`-41` for what was found but deliberately not built or fixed in the same pass. **Full suite re-verified after implementation:** `go vet`, `gofmt -l` clean, `go test ./... -race -count=1` all green, mutation harness 8/8, fixture corpus exact on both tiers — the new field and denominator formula changed no fixture-tested outcome, only real-traffic classification and the ceiling's own arithmetic. **Re-run against the exact real `coder` capture `TGD-BL-38` baselined the ceiling against, same binary that also carries `TGD-BL-40`'s known gap:** of the run's 427 `SAFE` verdicts, **145 are vacuous** (all read-path — `diffWrite` cannot detect the class yet) — 46 `DeleteOldWorkspaceBuildOrchestrations` and 44 `ExpirePrebuildsAPIKeys` (both `TGD-BL-41`'s CTE-write misrouting, coincidentally caught here), 38 `GetOldUnlinkedChatFileIDs`, 5 `GetChatFileByID`, 5 unnamed, and a handful of others — genuine `SELECT`s that happened to match nothing this run, the same shape `TGD-BL-34` already found for its 36 view-alias queries. **Corrected denominators:** `real_app_sql_touching_any_declared_table` and `row_level_touching_real_app_sql` — identical now, since 0 tables are structural-only — both became **519** (down from 664; `LEAK` unchanged at 106, `UNATTRIBUTABLE` unchanged at 131), for a corrected rate of **25.24%** (131/519). **This exceeds `TGD-BL-38`'s recorded baseline (131/664 ≈ 19.73%) — the exact same capture that produced that baseline now fails it, run against the corrected binary.** Not a regression: `TGD-BL-38`'s number was measured with vacuous `SAFE`s still counted as attributed evidence, which this session's own analysis (§9.7) concluded was the wrong denominator, not merely a smaller population that happened to include some noise. 25.24% is also a **lower bound**, not the true corrected rate: `TGD-BL-40`'s un-built write-path detection would exclude at least 44 more (the `DeleteOldWorkspaceAgentStats` vacuous writes `TGD-BL-37` already showed are vacuous by inspection), and `TGD-BL-41`'s un-fixed routing gap could change some of the 90 CTE-write outcomes once they reach `diffWrite` properly. **`unattributableCeilingRate` was NOT changed in this session** — it remains `131.0/664.0`, the `TGD-BL-38` value, so a real run against this exact capture now correctly exits 3 rather than silently reporting a number that no longer means what it claims. Per the standing "recommend, don't set" rule for the ceiling: this is the **second re-baselining candidate in two sessions**, and unlike `TGD-BL-38`'s, this one would need to move UP, not down — which the down-only ratchet cannot do as a routine action; it needs the deliberate, documented "why a higher rate is acceptable" decision `TGD-US-07` AC-2 requires, and arguably should wait for `TGD-BL-40`/`-41` to land first so the baseline is set once against a fully-corrected number rather than twice in quick succession against a partially-corrected one. | `TGD-BL-40` and `TGD-BL-41` (both filed above) are prerequisites to a clean third baseline measurement. Once both land, re-run and set `unattributableCeilingRate` deliberately, with the "raising it" decision recorded explicitly rather than inferred from a falling-then-rising number across three sessions. |
| `TGD-BL-38` | **closed — `TGD-NFR-03` ratcheted down to 131/664 (≈0.1973), gate re-proven, and the improvement's real cause recorded so it is never misread as better attribution logic** | **Ratcheted, not raised** — `unattributableCeilingRate` (`cmd/tenantguard/ceiling.go`) changed from `130.0/611.0` to `131.0/664.0`, `TGD-US-07` AC-2's only permitted direction. **What the SRS/source now state plainly, per instruction, so a future reader does not credit the wrong thing:** the numerator barely moved (130→131 — one more `TGD-BL-35`-shaped duplicate-key case); the denominator grew (611→664, +53) entirely because `TGD-BL-37` made 9 more tables row-level-provable, and of those 9 only ONE (`workspace_agent_stats`) was touched by any query in this specific capture — the other 8 contributed zero traffic to either side of the fraction, only widening which tables the ceiling's population is even allowed to include. This is a real, reproducible, tool-reported number — not a bad run — but it is coverage-driven, not evidence the extraction/attribution logic itself got better at reading SQL. **The consequence named explicitly, because it is the kind of thing that bites later:** a future run whose policy or captured traffic loses those 9 tables again (a reverted fixture, a policy scoped back down, a target without the feature) would be held to a ceiling this narrower population never actually earned, and a breach in that shape should be read as "coverage regressed," not "attribution regressed." **Proven able to fire on the new value, not assumed to inherit the old proof** — the same four requirements as `TGD-BL-06`, re-run against `131.0/664.0` specifically: (1) above fails — `TestAuditCLI_UnattributableCeilingFailsAboveIt`'s 2/3 scenario, unchanged, still exceeds the lower ceiling. (2) At-or-below passes — `TestAuditCLI_UnattributableCeilingPassesAtOrBelowIt`'s scenario needed rebuilding: its old 1/5=0.20 rate now sits ABOVE the new, lower ceiling (0.1973), so it was rewritten to 1/6≈0.1667 (one more `SAFE` event) to stay a genuine at-or-below case rather than silently becoming a false pass masked by the ceiling's own movement — the same fix applied to `TestAuditCLI_UnattributableRateByDenominator`, whose unrelated population-breakdown scenario had the identical 0.20 rate and would otherwise have started failing on exit code alone. (3) Mutation: the stored constant temporarily set to `0.01`, confirmed to flip the passing CLI test and two `ceiling_test.go` cases red, then restored to `131.0/664.0`. (4) Deletion: the enforcement call in `runGateCommand` temporarily removed, confirmed to flip the breach test to exit 0 instead of 3, then restored. **The exact baselining capture re-run on the new binary reproduces `131/664 = 0.19729` precisely and exits 0** — the boundary case itself, not just a synthetic one. Full suite re-verified: `go vet`, `gofmt -l` clean, `go test ./... -race -count=1` all green, mutation harness 8/8, fixture corpus exact on both tiers. SRS `TGD-NFR-03` row and §7.1 updated with the new value, the same "what this is not" caveat now extended with the coverage-vs-attribution distinction, and the exact rewritten CLI test list so the mutation evidence is traceable. | — |
| `TGD-BL-37` | **closed — all 9 real `coder` structural-only tables proven fixture-constructible; row-level coverage reaches 22/22 for the first time; re-run against real capture; ceiling ratchet-down recommended, not set** | **⚠ SUPERSEDED (`TGD-BL-42`): the `{"safe":427,"leak":106,"unattributable":4173}` figure below, and the `131/664`/`19.73%` denominators derived from it, were measured with a differ that silently discarded the tenant value on any cast positional `INSERT` parameter (`$N::type`, `sqlc`'s default emission shape) — the exact shape most of `coder`'s own `sqlc`-generated INSERTs use. The 106 LEAK count is not trustworthy, and the 19.73% ceiling this entry recommended (and `TGD-BL-38` later set) rests on the same contaminated run. See SRS §7.2/§7.3 for the defect, the fix, and the corrected re-run and baseline. The fixture-authoring work described below (9 tables made row-level-provable) is unaffected — it concerns oracle seeding, not tenant-value extraction.** **Constructibility, per table, checked against real schema (`\d`) before writing anything.** All 9 were genuinely constructible — none blocked. The reasons they were empty on a fresh migration: `chat_organization_model_overrides`/`chat_user_model_overrides`/`mcp_server_configs`/`workspace_agent_port_share`/`workspace_app_stats`/`workspace_app_statuses` are features (chat model overrides, MCP servers, port sharing, app usage stats) nothing in a freshly-migrated, unexercised deployment has ever configured or used; `groups`/`workspace_agent_stats` each hold exactly 1 real row (the default group/one stat row from setup), one short of sampling's 2-row minimum; `jfrog_xray_scans` needs a workspace with a completed vulnerability scan, which a fresh deployment has never run. **What made every one constructible despite real constraints (FKs, CHECKs, enums, NOT NULLs, composite uniques):** (1) `SeedCanaries` sets `session_replication_role = replica` for the whole seeding session (§9.6), which disables FK-enforcing triggers — a fixture's non-tenant FK columns (`model_config_id`, `agent_id`, `app_id`, `user_id`, etc.) never need to reference a real parent row, only the tenant column's own type. (2) Every real CHECK constraint hit was satisfiable: enum-shaped text CHECKs (`chat_organization_model_overrides_context_check`, `mcp_server_configs_auth_type_check`, etc.) by picking a listed value; the one cross-column CHECK found (`chat_user_model_overrides_model_requires_config_check`: `(mode='model') = (model_config_id IS NOT NULL)`) by choosing `mode='chat_default'` with `model_config_id` left `NULL`; the one nullable-guarded CHECK (`groups_chat_spend_limit_micros_check`) by leaving it `NULL`. (3) Every composite unique/PK constraint found (`(organization_id, context)`, `(name, organization_id)`, `(agent_id, workspace_id)`, `(workspace_id, agent_name, port)`) already includes the tenant column, so `columnsNeedingSyntheticValues`'s existing rule (a unique index including the tenant column is auto-satisfied by `CanaryA≠CanaryB` alone, `TGD-BL-30`) meant the fixture's OTHER columns could repeat identically across both rows — no manual per-row distinctness needed. The one exception, `workspace_app_stats`' `(user_id, agent_id, session_id)`, does NOT include the tenant column — but per the same existing rule those three columns are auto-marked `needSynthetic`, so the tool's own per-row synthetic generator supplies them and the fixture never needed to. **Fixtures authored** (`chat_organization_model_overrides`: `context`+`model_config_id`; `chat_user_model_overrides`: `user_id`+`context`+`mode`; `groups`: `name`; `jfrog_xray_scans`: `agent_id`; `mcp_server_configs`: `display_name`+`slug`+`url`; `workspace_agent_port_share`: `agent_name`+`port`+`share_level`; `workspace_agent_stats`: `created_at`+`user_id`+`agent_id`+`template_id`; `workspace_app_stats`: `access_method`+`slug_or_port`+`session_started_at`+`session_ended_at`+`requests`; `workspace_app_statuses`: `agent_id`+`app_id`+`state`+`message`) — every other `NOT NULL` column has a database default (skipped automatically) or is the tenant column itself. All 9 seeded successfully on the first `verify` run against real `coder_target` — no iteration needed, confirming the schema analysis was correct rather than arrived at by trial and error. **Result: `verify` on the full 22-table policy now returns `proven:true` with 22 of 22 row-level, 0 structural-only** (13 `sampled`, 9 `fixture` — `TGD-BL-36`'s new `seed_source` field on every entry). **Differential re-run** on the same `coder_capture_events.jsonl`: `{"safe":427,"leak":106,"unattributable":4173}` (previously 383/98/4225) — `SAFE` **+44**, `LEAK` **+8**, `UNATTRIBUTABLE` **−52**. **Only 1 of the 9 newly-seeded tables was touched by any real query in this capture: `workspace_agent_stats`** — the other 8 (`chat_organization_model_overrides`, `chat_user_model_overrides`, `groups`, `jfrog_xray_scans`, `mcp_server_configs`, `workspace_agent_port_share`, `workspace_app_stats`, `workspace_app_statuses`) gained real oracle coverage (A1/A4 proven on fixture data) but **zero real differential exercise** — `dbpurge` simply never calls the code paths that touch them. Inspected directly: the +8 `LEAK`s are `InsertWorkspaceAgentStats` ×2 (an `INSERT ... SELECT` — not the literal-`VALUES` shape `TGD-BL-34`'s parser fix covers, so it falls through to the WHERE-clause path, finds no `workspace_id` predicate, attributes `AttrNoPredicate`, and is correctly rejected by `WITH CHECK` under `diffWrite` — a genuine, `L1`-shaped Tier-1 finding, same standing caveat as `TGD-BL-32`'s: not a confirmed vulnerability, app-layer authorization elsewhere is not visible to this tool) and `GetWorkspaceAgentStats` ×6 (a `WITH ... SELECT` CTE aggregate with no tenant predicate — same shape). The +1 `UNATTRIBUTABLE` is a third `InsertWorkspaceAgentStats` hitting the exact `TGD-BL-35` duplicate-key pattern (filed, not patched, unchanged) — now also observed on a fixture-seeded table, not only sampled ones. **The +44 `SAFE`s (`DeleteOldWorkspaceAgentStats`, a global retention purge with no tenant predicate at all) are, inspected directly, vacuous**: the purge's `created_at < cutoff` clause does not reach the fixture rows' `now()` timestamps, so both the unrestricted and restricted legs delete 0 rows — `SAFE` because nothing happened on either side, not because correct scoping was demonstrated, the same "0=0" caveat `TGD-BL-34` already named for its 36 view-alias `SAFE`s. **Denominators, from the tool's own report:** `all_captured_queries` 88.67% (4173/4706), `real_app_sql_any_table` 73.64% (1489/2022), `real_app_sql_touching_any_declared_table` **19.73%** (131/664 — now numerically identical to the next row, since 0 tables are structural-only anymore), `row_level_touching_real_app_sql` **19.73%** (131/664). **Ratchet-down candidate, recommended, NOT set, per instruction:** 19.73% < the baselined 21.28% (`TGD-BL-06`) — `TGD-US-07` AC-2's ratchet is down-only, and this is exactly the case it exists for: a real, reproducible, tool-reported improvement, not a bad run. Recommend re-baselining `unattributableCeilingRate` to `131.0/664.0` in a follow-up session, naming this measurement as the reason, rather than doing it inline with this fixture-authoring work. | Re-baseline `TGD-NFR-03` to `131/664` (`TGD-BL-06`-style entry, not done here on instruction). The 8 tables with zero real exercise are real coverage but not real validation — the natural next real-traffic broadening (already named by `TGD-BL-31`/`-32`) is running against a workload that actually calls chat/MCP/port-sharing/xray-scan code paths, not just `dbpurge`. |
| `TGD-BL-36` | **closed — `SeedCanaries`/the CLI report now distinguish sampled-row proof from fixture-row proof; previously conflated** | **The gap, stated as asked.** Before this item, `TablesRowLevel` (`TGD-BL-32`) reported a flat list of table names — a table proven via `seedFromSample` (real target data, tenant column rewritten) and one proven via `seedFromFixture` (data an operator typed into the policy file) were reported identically. That is not the same strength of evidence: a fixture-seeded table's A1/A4 pass proves the oracle mechanism works against a schema shaped that way, not that it has been exercised against anything the target itself produced. **Fixed.** `internal/oracle/probe.go`: new `SeedSource` type (`SeedSourceSampled`/`SeedSourceFixture`) and `SeededTable{Table, Source}`; `SeedCanaries`'s first return value changed from `[]string` to `[]SeededTable`, set per relation based on which path (`seedFromSample`/`seedFromFixture`) actually ran. `cmd/tenantguard/oracle_gate.go`: `tableProof` gained a `SeedSource string` field, populated from a `map[string]oracle.SeedSource` built off `SeedCanaries`'s richer return. `cmd/tenantguard/main.go`: `verifyReport.TablesRowLevel` changed from `[]string` to `[]rowLevelTable{Table, SeedSource}` (JSON: `"seed_source": "sampled"` or `"fixture"`), both output formats. **Low-risk mechanically**: every existing call site of `SeedCanaries` across the test suite checked only `len(seeded)`, never indexed or ranged over its contents, so the type change required zero test-assertion rewrites in `internal/oracle` — confirmed by grep before changing the signature, not assumed. Proven: two new `internal/oracle` tests (`TestSeedCanaries_ReportsSampledSource`, `TestSeedCanaries_ReportsFixtureSource`) pin each path's `Source` directly; two CLI tests (`TestVerifyCLI_OneUnseedableTableDegradesToStructuralOnly` extended, `TestVerifyCLI_FixtureSeededTableReportsFixtureSource` new) prove the report field end-to-end, including a table with BOTH a sampled and a fixture-seeded table in the same run reporting each correctly. Full suite re-verified: `go vet`, `gofmt -l` clean, `go test ./... -race -count=1` all green, mutation harness 8/8, fixture corpus exact on both tiers. | — |
| `TGD-BL-35` | **open — filed, not patched, per explicit instruction** | **Finding F from `TGD-BL-33`'s categorization**, 125 (later re-measured at a smaller count once `TGD-BL-34`'s fixes shrank `row_level_unattributed`) real captured `INSERT`s that attribute correctly and reach `diffWrite`, but whose `unrestricted` re-execution leg itself fails with Postgres `23505` (duplicate key) — `InsertOrganizationMember`, `InsertTemplateVersion`, `InsertTemplate`, others. **Root cause, not yet a design:** the probe is a `TEMPLATE` copy of the *live* target database (§9.6/`TGD-BL-30`), so when a captured `INSERT` carries the exact literal primary key it inserted for real at capture time, replaying it against a probe that already contains that literal row is close to guaranteed to collide — captured write traffic and probe state are drawn from the same underlying history, not independent. This is a genuine limitation of the differential re-execution mechanism on tables with real historical data, invisible to the fixture corpus (which only ever seeds synthetic single/dual-tenant tables with no prior collision risk to replay into). **Explicitly not fixed this session — filed as its own item on instruction, not patched inline with `TGD-BL-34`'s two bounded fixes.** A `23505` is not evidence of a leak (it is UNATTRIBUTABLE, correctly, via `PopulationRowLevelUnattributed`, not silently folded into a verdict) — but it does mean a real slice of real write traffic is currently unattributable for a reason that has nothing to do with attribution logic, and would need its own design pass: candidates include re-executing against a point-in-time snapshot taken before the capture's own writes landed, treating "identical row already present with identical values" as compatible-with-`SAFE` rather than an error, or accepting the gap and reporting it distinctly rather than solving it. None evaluated here. | A design review of write re-execution against a probe seeded with real historical data — the same shape of task `TGD-BL-29`'s design review (§9.6) was for seeding, applied to replay instead. |
| `TGD-BL-34` | **closed — the two bounded attribution gaps `TGD-BL-33` found fixed, red before green, and re-verified against the real `coder` capture** | **⚠ SUPERSEDED (`TGD-BL-42`): this entry's "LEAK unchanged at 98" figure inherits `TGD-BL-32`'s superseded count — see that entry's note. Not trustworthy as a LEAK figure; the view-alias/nested-paren fixes described below are unaffected (they concern table recognition and VALUES-list parsing, not tenant-value cast stripping).** **View-alias resolution (36 queries).** `coder`'s `GetWorkspaceBuildByID`/`GetTemplateVersionByID` query `FROM workspace_build_with_user AS workspace_builds` / `FROM template_version_with_user AS template_versions` — a view, aliased to the exact name of the real declared scoped table, specifically so the rest of the query can refer to it as if it were the base table. `internal/differ/extract.go`'s `tableRef` regex matched only the token immediately after `FROM`/`JOIN`/`INTO`/`UPDATE` (the view's own name), never a trailing alias, so the real table's name never registered as referenced at all — `referencedTables` now also captures an optional `[AS] <alias>` and treats either name as a match, with a documented stopword list (`WHERE`, `ON`, `USING`, `SET`, join keywords, `ORDER`/`GROUP`/`LIMIT`/`OFFSET`/`RETURNING`/`VALUES`/`FOR`) so a real following clause is never mistaken for an alias. Proven red→green: `TestExtractTenant_ViewAliasedToBaseTableNameResolves` failed against the old regex (`Unattributable`, "no declared scoped table"), passes with the fix. **Named as a distinct shape, not folded into U1–U3:** it is not a subquery-computed tenant, a set-returning function, or a cross-join conflict — the tenant comparison itself may be perfectly ordinary; the failure is purely in whether the table is recognised as referenced at all, a step upstream of every U1–U3 check. **ON CONFLICT / nested-paren VALUES bug (9 queries).** `coder`'s `UpsertProvisionerDaemon` (`INSERT ... VALUES (gen_random_uuid(), $1, ...) ON CONFLICT (...) DO UPDATE ...`) tripped `extractFromInsert`'s old `insertShape` regex, whose VALUES capture group (`\(([^)]*)\)`) cannot handle a value that is itself a function call with its own parens: it stopped at `gen_random_uuid()`'s own closing paren, captured a 1-element VALUES list against a 10-element column list, and reported a false "column list and VALUES list have different lengths" — an error dressed up as a parse failure, for a query that actually resolves cleanly. Replaced with `insertPrefix`/`valuesPrefix` locating each opening paren, plus a new `balancedParenSpan` helper that tracks nesting depth manually (the same limitation `splitTopLevel` already names — a `)` inside a quoted string literal is miscounted — is inherited and documented, not solved, since nothing in this corpus or real capture exercises it). Proven red→green: `TestExtractTenant_InsertWithFunctionCallValueResolves` failed pre-fix, passes post-fix; `TestExtractTenant_InsertGenuineLengthMismatchStillUnattributable` (a REAL mismatch, fewer values than columns) confirms the protective intent survives the rewrite unweakened. Full suite re-verified after both fixes: `go vet`, `gofmt -l` clean, `go test ./... -race -count=1` all green, `TestMutationHarnessM1ThroughM8` 8/8, `TestCorpus_ExactSetEquality` exact on both tiers — neither fix touched any fixture-tested behaviour, only two extraction-layer parsing gaps neither the corpus nor the mutation harness had a shape for. **Re-run against the real `coder` capture** (same `coder_capture_events.jsonl` `TGD-BL-31`/`-32`/`-33` used, same `coder_target` database, full 22-table policy, new binary): `UNATTRIBUTABLE` fell from 4,265 to 4,225 (**−40**), `SAFE` rose from 343 to 383 (**+40**), `LEAK` unchanged at 98. All 36 view-alias queries now resolve — inspected directly, all landed `SAFE` (both legs return the identical row set for the captured id; whether that id still exists as a real row in the now-twice-re-run `coder_target` is a fact about this specific probe snapshot, not a property of the fix, and is not claimed as a meaningful correctness validation beyond "the table is now recognised at all"). All 4 real `UpsertProvisionerDaemon` captures now resolve, all `SAFE` with a real tenant value. The other 5 of the original 9 length-mismatch queries were ALSO `gen_random_uuid()`-style parsing failures underneath — once parsed correctly, they turned out to have a genuinely subquery-computed VALUES entry for the tenant column, now correctly reported as `PopulationRowLevelUnattributed`/"tenant value is computed by a subquery" instead of the old, wrong "different lengths" message — a real, distinct, correctly-labelled limitation surfaced by fixing the parser, not hidden by it. | — |
| `TGD-BL-33` | **closed — every `UNATTRIBUTABLE` verdict now carries a tool-computed `Population`; report emits all four candidate denominators, labelled; not baselined (instructed)** | **Continuation of a prior-session chat-only analysis** (not previously committed to this document) that hand-categorized a real `coder` differential run's 4,265 `UNATTRIBUTABLE` verdicts by grepping SQL text outside the tool, finding the reported 90.6% "all captured queries" rate dilutes a real ~27.8% attribution-failure rate with cursor-protocol/session-bookkeeping traffic (`pg_dump`'s own per-table dump mechanism, triggered by `dbtestutil`'s on-failure diagnostic dump — `C-7`/`TGD-BL-13` — over half of ALL captured traffic) and real application SQL against tables the policy never declared scoped. That analysis is now implemented as a reproducible, tool-computed mechanism rather than a one-off script. **What was built.** `internal/differ/population.go`: a new `Population` type with four values — `no_declared_table`, `structural_only`, `row_level_unattributed`, `non_query` — and `ClassifyUnattributable(sqlText, rowLevel, structuralOnly)`, which checks `isNonQuery` (a word-bounded prefix check against `DECLARE`/`FETCH`/`CLOSE`/`LOCK`/`BEGIN`/`COMMIT`/`SET`/`RESET`/`PREPARE`/`EXECUTE`, comment-stripped the same way `TGD-BL-31`'s `isWriteStatement` is) FIRST — a `pg_dump` `DECLARE CURSOR` naming a real scoped table by name is still `non_query`, not `row_level_unattributed` — then reuses `referencedTables` (the same text-matching `ExtractTenant` already does) against the row-level and structural-only relation sets separately, with row-level given priority when a query touches both. `runOracleGate`'s `onProven` callback signature gained a second relation-slice argument, `structuralOnly` (alongside the pre-existing `rowLevel`) — passed to the CLI's classification call only, never to `differ.Diff`/`ExtractTenant`, preserving `TGD-BL-32`'s safety property untouched: a structural-only table still cannot ever be attributed `SAFE`/`LEAK`. `cmd/tenantguard`'s report gained: `queryVerdict.Population` (set only when `Verdict` is `UNATTRIBUTABLE`); `verdictCounts.Population` (four sub-counts summing to `Counts.Unattributable`); `UnattributableRateByDenominator`, a labelled list of `{label, denominator, unattributable, rate}` computed by `unattributableRateBreakdown` for the same four denominators the prior session's chat analysis proposed by hand — `all_captured_queries` (identical to the existing `UnattributableRate` field), `real_app_sql_any_table` (excludes `non_query`), `real_app_sql_touching_any_declared_table` (`SAFE+LEAK+structural_only+row_level_unattributed`), `row_level_touching_real_app_sql` (`SAFE+LEAK+row_level_unattributed` — the one recommended for `TGD-BL-06` to baseline against, since it is the only one isolating "attribution was possible in principle and still failed" from coverage gaps and harness noise). A denominator of 0 omits its entry rather than reporting a fabricated 0/0. Both JSON and text (`--output text`) report formats updated. **Proven**, unit-level (`internal/differ/population_test.go`, 7 cases: each population value, priority when a query touches both a row-level and a structural table, comment-stripped non-query detection, word-boundary prefix safety) and end-to-end through the built binary (`cmd/tenantguard/main_test.go`'s `TestAuditCLI_UnattributableRateByDenominator`: one synthetic event per population plus a `SAFE` and a `LEAK`, asserting the report's own `Population` fields, `Counts.Population`, and all four `UnattributableRateByDenominator` entries reproduce the hand-computed expected values exactly). Full suite re-verified: `go vet`, `gofmt -l` clean, `go test ./... -race -count=1` all green, mutation harness 8/8, fixture corpus exact on both tiers. **Re-run against the real `coder` capture**, tool's own output, no hand-grepping this time: `all_captured_queries` 89.8% (4,225/4,706 — down slightly from the prior session's 90.6%, because `TGD-BL-34`'s fixes, applied in the same binary, resolved 40 previously-unattributable queries to `SAFE`), `real_app_sql_any_table` 76.2% (1,541/2,022), `real_app_sql_touching_any_declared_table` 27.6% (183/664), `row_level_touching_real_app_sql` **21.3%** (130/611) — down from the prior session's hand-derived 27.8%, entirely explained by `TGD-BL-34` closing 40 of the queries that were previously counted in that population's numerator. **Not baselined, per explicit instruction:** `TGD-NFR-03`/`TGD-BL-06` still carry no ceiling. The tool can now reproduce its own numbers on every run — the stated prerequisite for baselining at all — but the operator asked to see that reproduction happen before setting anything, not to have it set here. | `TGD-BL-06` itself: baseline `TGD-NFR-03` against `row_level_touching_real_app_sql`, now that the number is reproducible from the tool's own report rather than a hand-derived one-off. Not attempted this session, on instruction. |
| `TGD-BL-32` | **closed — §9.6's tiered proof-depth model implemented, tested, and re-run against real `coder` data: row-level proof extended from 2 of 22 to 13 of 22 real scoped tables** | **⚠ SUPERSEDED (`TGD-BL-42`): the `{"safe":343,"leak":98,"unattributable":4265}` figure below was measured with a differ that silently discarded the tenant value on any cast positional `INSERT` parameter (`$N::type`, `sqlc`'s default emission shape), turning real, correctly-attributed writes into false `LEAK`s. The 98 LEAK count is not trustworthy; do not cite it as evidence of real findings. See SRS §7.2/§7.3 for the defect and the corrected re-run.** **What changed.** `runOracleGate` (`cmd/tenantguard/oracle_gate.go`) no longer aborts the whole sweep the moment one scoped relation fails to seed. `tableProof` gained `Seeded`/`SkipReason` fields; a relation that seeded gets A1/A4 attempted as before, one that didn't is recorded structural-only (A2/A3 — catalog-only, unaffected by seeding — still proved for it) with its skip reason named. `aggregateProof` now folds only `Seeded==true` entries into the global `ProofState`: an unseeded table contributes neither a pass nor a fail, but if NOTHING seeds at all, `A1Checked`/`A4Checked` stay false and the run still aborts exactly as before (`TestRunOracleGate_NoTableSeedsStillAborts`) — the tiering never turns "the oracle was never demonstrated to see anything" into a passing run. The safety property the design review named — a structural-only table must never be verdicted `SAFE`/`LEAK` — is achieved without touching `differ.Verdict` at all: `onProven` now receives only the row-level-proven relation subset (`rowLevel`, filtered from `scoped`), so a query attributed to a structural-only table falls straight through the differ's pre-existing "no declared scoped table referenced by name" path and comes back `UNATTRIBUTABLE`. Proven directly, red before green: `TestAuditCLI_StructuralOnlyTableNeverProducesSafeOrLeak` was confirmed to genuinely FAIL under the old wiring (a real, correctly-shaped query against an unseeded table came back `LEAK`, not merely `SAFE` — a false finding either way) by temporarily passing `scoped` instead of `rowLevel`, then confirmed passing after reverting to the fix. The report schema gained `tables_row_level`/`tables_structural_only` (with per-table reasons), in both JSON and text output. Two pre-existing tests asserted the OLD all-or-nothing behaviour as correct and were rewritten, not deleted-and-forgotten, with a comment explaining the supersession: `TestRunOracleGate_A2A3RunDespiteOneTableFailingToSeed` → `TestRunOracleGate_A1A4RunOnSeedableTablesDespiteOneTableFailingToSeed`; `TestVerifyCLI_OneBadTableAbortsTheWholeSweep` → `TestVerifyCLI_OneUnseedableTableDegradesToStructuralOnly`. Full suite re-verified: `go vet`, `gofmt -l` clean, `go test ./... -race -count=1` all green, `TestMutationHarnessM1ThroughM8` still 8/8 caught by mapped gate, `TestCorpus_ExactSetEquality` still exact on both expected tiers — this change touched none of the oracle/differ fixture-tested logic itself, only the gate's aggregation and the differ's input set. **Re-run against real `coder`** (same freshly-migrated database `TGD-BL-30`/`-31` used, now with additional real rows from `TGD-BL-31`'s `dbpurge` run through the proxy): `infer` reproduced **22/95/15 a fifth independent time**. `verify` on the FULL 22-table policy — no manual 2-table curation this time — returned `proven:true` with **13 of 22 tables row-level proven** (`audit_logs`, `chat_files`, `chat_model_configs`, `custom_roles`, `organization_members`, `provisioner_daemons`, `provisioner_jobs`, `provisioner_keys`, `template_versions`, `templates`, `workspace_build_orchestrations`, `workspace_builds`, `workspaces`) and **9 structural-only**, each named with the identical, honest reason: "no rows to sample (fewer than 2 exist) and no fixture supplied" (`chat_organization_model_overrides`, `chat_user_model_overrides`, `groups`, `jfrog_xray_scans`, `mcp_server_configs`, `workspace_agent_port_share`, `workspace_agent_stats`, `workspace_app_stats`, `workspace_app_statuses`). **This is a real jump from the 2-table manual curation `TGD-BL-30`/`-31` needed** — driven partly by the tiered mechanism itself (no operator curation required — the full 22-table policy just works, tier-by-tier) and partly by the target database now holding more real rows (from `TGD-BL-31`'s own `dbpurge` run having written real data through the proxy) — both factors are named rather than the credit being assigned to only one. **Differential re-run** on the SAME `coder_capture_events.jsonl` from `TGD-BL-31`, now scoped to the full 22-table policy instead of the 2-table one: `{"safe":343,"leak":98,"unattributable":4265}`, unattributable rate **90.6%** (down from `TGD-BL-31`'s 98.5% on the 2-table policy; more tables in scope means more of the same capture becomes attributable, not less). Spot-checked, not exhaustively: the 98 `LEAK` verdicts are genuine Tier-1 findings by the tool's own defined semantics — a real query shape (`GetProvisionerJobByID`, `InsertChatFile`, `InsertChatModelConfig`, `GetChatFileByID`, `GetProvisionerDaemons`, `GetAuditLogsOffset`, others) against a row-level-proven scoped table, carrying literally no predicate on the declared tenant column at all, so the restricted role (tenant unset) sees fewer rows than unrestricted — exactly `L1`-shaped, not a differ artifact this time (no `isWriteStatement`-style routing bug found on inspection of the `INSERT`-shaped leaks, which correctly went through `diffWrite` and were rejected by RLS's `WITH CHECK`, not the row-content comparison). **Stated plainly, per the standing instruction never to smooth this over: this is NOT a disclosed or confirmed `coder` security finding.** The tool proves what the SQL does, not what `coder`'s application layer does before or after issuing it — several of these query names (`GetProvisionerJobByID` by a bare `id`, `GetAuditLogsOffset`) are plausibly gated by an authorization check elsewhere in Go code that this differential cannot see and does not claim to model (§2's own stated scope boundary). It is real, reproducible Tier-1 signal on real `coder` traffic — the first time this project has produced that at meaningful scale — not a vulnerability report; treating it as the latter without the code-path review §5's `TGD-FR-11` "originating test or code path" gap (`TGD-BL-26`, still open) would provide is exactly the overclaiming this project's own conventions forbid. | The 9 remaining structural-only tables are the same shape TGD-BL-29/-30 already catalogued (empty on a fresh migration, no fixture authored) — closing them is unchanged: author fixtures, or re-run against a `coder` instance with organic usage data. Separately, and now more valuable given 13 tables are in scope: `TGD-BL-26`'s code-path provenance gap is what would let a `LEAK` verdict here be triaged (test/call-site vs. real application code) rather than requiring manual SQL inspection as this session did. |
| `TGD-BL-31` | **closed — differential stage run against real `coder` traffic for the first time; a real defect found in `isWriteStatement`, fixed and proven; result is quantified and its coverage bounded, not presented as an audit of `coder`** | **What ran, end to end, for the first time:** `tenantguard capture` on the real proxy, redirected via `CODER_PG_CONNECTION_URL` alone (no `coder` source changes), against the same `coder/coder` checkout and the same freshly-migrated database `TGD-BL-30` measured (`custom_roles`: 2 rows, `provisioner_keys`: 3 rows — reproduced exactly). `coderd/database/dbpurge`'s suite ran through it: **1,420 Binds, 0 unresolved, 171 connections, 3,286 simple-protocol queries** (some tests failed under the known `C-7` shared-database interference — expected, not investigated further this session). `tenantguard audit --policy <2-table policy, derived from the same `infer` output `TGD-BL-30` used, `custom_roles`+`provisioner_keys` only, the rest demoted to `unscoped`> --events <capture>` then ran the differential for the first time in this project's history. **A real defect surfaced by that first run, not anticipated by any fixture:** `isWriteStatement` (`internal/differ/differ.go`) checked `INSERT`/`UPDATE`/`DELETE` by `TrimSpace`+`HasPrefix` alone. Every `coder` query is `sqlc`-generated with a leading `-- name: X :one` comment, so the check never matched on real traffic — 70 real `UpdateCustomRole` executions were routed through the SELECT-style row-set comparison instead of `diffWrite`, and all 70 came back `LEAK` (`unrestricted returned 1 row(s), restricted returned 1`: same count, different content) — exactly the non-deterministic-output-column false-positive `diffWrite`'s own docstring names as the reason it exists (`updated_at = now()` in the `RETURNING` clause differs by construction between the two re-executions). None of `L1`–`L10` carries a leading comment, so the fixture corpus could not have caught this — found only by running against real, `sqlc`-shaped SQL. **Fixed, red before green:** `TestIsWriteStatement_IgnoresLeadingComment` (`internal/differ/differ_test.go`) reproduces the exact captured SQL shape, confirmed failing pre-fix; `isWriteStatement` now strips leading `--` line comments before the prefix check; the test passes post-fix. Full suite re-run against real PostgreSQL post-fix: all packages green, `TestMutationHarnessM1ThroughM8` still 8/8 caught by mapped gate, `TestCorpus_ExactSetEquality` still exact on both expected sets — the fix changed no fixture or mutation outcome, only real-traffic classification. **Result after the fix, on the real capture:** `{"safe":70,"leak":0,"unattributable":4636}`, unattributable rate 98.5% (reported per `TGD-NFR-03`/`TGD-US-07` AC-3, not enforced — `TGD-BL-06` still open). **The load-bearing caveat, stated rather than smoothed over:** of the 88 queries that named `custom_roles`/`provisioner_keys` at all, **0 were `SELECT`** — 70 were the one `UpdateCustomRole` write, 18 were `DECLARE CURSOR`/`LOCK TABLE` from a test-failure `pg_dump` diagnostic (`UNATTRIBUTABLE`, correctly — no bound tenant predicate to attribute). The row-set differential — the mechanism `L1`–`L10` and the whole design's justification (§2) rest on — was exercised **zero times** by this real capture; every real verdict this session produced came from the write-path RLS-acceptance check, exercised on real traffic for the first time and shown correct once the routing bug was fixed. **This is not an audit of `coder`.** It is a proof that the differential stage runs end to end against a real target and correctly classifies the one query shape that real traffic happened to produce on the 2 of 22 tables the oracle can currently prove — no claim is made, and none should be inferred, about the other 20 tables or about `SELECT`-shaped isolation on any table, real or provable. **Tenant attribution on real traffic, compared to §9.4's probe coverage:** §9.4/`TGD-BL-30` proved `A1`–`A4` — the oracle can see and correctly judge canary-seeded rows on these 2 tables. This session is the first evidence of the separate, downstream question — whether the *proxy's* attribution (`ExtractTenant`, reading the tenant out of the query's own predicate) resolves correctly on real, not synthetic, bound parameters — and on the one write query that produced a verdict, it did: the same `organization_id` param the query itself claimed was the one the restricted role was correctly bound to, and RLS accepted the write. That is one query shape, 70 executions of it, not a general result. | The natural next step is unchanged from `TGD-BL-30`'s: broaden real-traffic coverage beyond `dbpurge`, and beyond `custom_roles`, to actually exercise the row-set (`SELECT`) differential path against real `sqlc`-shaped SQL, which this session did not reach at all. Second: `isWriteStatement`'s comment-stripping is now scoped to leading `--` lines only; a `/* ... */` block comment or a leading blank line before the comment would need a similar audit — not hit by real `coder` traffic this session, not fixed speculatively. Third, unchanged and now sharper: the 20-of-22-table proof-depth gap (`TGD-BL-29`) is what would let a real differential audit make a claim covering more than 2 tables — this session's result is the concrete argument for prioritizing that over further differential runs on the same 2 tables. |
| `TGD-BL-30` | **closed — §9.6's row-sampling mechanism implemented; fourth real `coder` re-run: no fourth defect shape, sweep now PROVEN on real data for the first time** | Implements §9.6's recommendation in full. (1) **Sampling is now the primary mechanism.** `SeedCanaries` copies two existing rows per relation (`sampleRowKeys`, via `ctid` — works even with no `id` column) into two new rows via a single `INSERT ... SELECT ... UNION ALL SELECT ...`, rewriting only the tenant column (`canaryLiteral`, unchanged from `TGD-BL-27`). Every other column — nullable or not — is borrowed verbatim from the sample, satisfying type, cross-column CHECK, and FK constraints by construction, since the row was already valid. (2) **Uniqueness is handled narrowly, not by falling back to full synthesis.** `columnsNeedingSyntheticValues` reads every unique/primary-key index on the relation (`uniqueIndexColumnSets`, `pg_index`) and requires a fresh, `TGD-BL-28`-style per-row-distinct value (`columnSampleValue`, retained exactly as built) only for columns in an index that does **not** already include the tenant column — one that does is auto-satisfied, since `CanaryA ≠ CanaryB` always. Verified directly against both real shapes: `audit_logs`' plain `id uuid` PK needs a synthetic value (borrowing would collide with the very row it was copied from); `organization_members`' composite `(organization_id, user_id)` PK does not (`user_id` is safely borrowed). This is `sampleValue`/`columnSampleValue` surviving narrowly, as a collision-avoidance tool for identity-like columns specifically — not the old unconditional generator reappearing. (3) **FK ordering needs neither parent-copying nor deferral.** `CreateProbeDatabase`'s `TEMPLATE` copy is a full data-and-schema clone, never partial — any parent row a sampled row's FK points to is already present in the probe by construction. Verified directly (`TestSeedCanariesPreservesForeignKeyReferences`): a borrowed FK column's value is confirmed to be one of the two real, pre-existing parent ids, not fabricated. `session_replication_role = replica` is kept as defence in depth (unrelated triggers, not FK resolution). (4) **Fixture fallback, not a silent skip.** A relation with fewer than 2 rows to sample now consults `schema.Relation.Fixture` (a new, additive, `omitempty` JSON field — the same tool-owned, human-reviewed artifact class as the policy file itself); missing both sampling and fixture, or a fixture missing a required column, produces a distinct, named `SkippedTable.Reason` (`seedFromFixture`) — never a raw driver error and never the pre-`TGD-BL-29` generator quietly reappearing. (5) **A2/A3 decoupled from seeding success**, as requested, cheaply: `runOracleGate` now runs `EnableRLS`+`CheckA2`+`CheckA3` for every scoped relation *before* checking whether any relation failed to seed, so one table's seeding gap no longer holds the other 21's structural coverage hostage — proven directly (`TestRunOracleGate_A2A3RunDespiteOneTableFailingToSeed`, mutation-reverted and confirmed red). The A1/A4 tiered-proof-depth question from §9.6 was deliberately left out of scope, as instructed — the AND-fold on seeding success remains unchanged for A1/A4 specifically. Proven: `internal/oracle/sampling_test.go` and updates to `canary_type_test.go` reproduce all three real shapes (`audit_logs`, `organization_members`, and — new this session — `chat_model_configs`'s exact CHECK, both individually-correct choices combining into a violation) confirmed red (the literal real errors: `23505` duplicate key, `42703` missing `id` column, `23514` CHECK violation) before, green after; every existing fixture across the whole suite (`newFixture` et al.) was updated to seed ≥2 baseline rows, since sampling now structurally requires that — a large, mechanical ripple, not a design change. Full suite: `181 tests, 0 skipped, 8/8 mutation-harness subtests, 67.4% coverage`, `gofmt`/`vet` clean. **Fourth real `coder` re-run**, `infer` reproducing **22/95/15 a fourth independent time**: `verify` on the full 22-table sweep now stops on `public.audit_logs` with the NEW, correctly-named fixture-fallback reason — not a new defect shape, but the first time this project has directly measured how empty a truly fresh, never-exercised `coder` migration actually is: of the 22 real scoped tables, only **2** (`custom_roles`: 2 rows, `provisioner_keys`: 3 rows) have ≥2 pre-existing rows at all; the other 20, including `audit_logs` itself, have exactly 0 or 1 — consistent with, and now precisely quantifying, §9.1's original finding that `coder`'s migrations seed almost no data. Supplying a hand-authored fixture for `audit_logs` (its 11 `NOT NULL`, no-default, non-tenant, non-PK columns) let the sweep advance one table further to `public.chat_files` — also empty — confirming the fallback mechanism itself works correctly on the real target, not just in a synthetic fixture. **Isolating the sweep to exactly the 2 naturally-populated tables** (no fixture, pure sampling) produced, for the first time in this project's history, `{"proven":true, "checks":{"a1":true,"a2":true,"a3":true,"a4":true}}` against real `coder` data. The differential stage still never ran — `verify` (not `audit`) was used, and no `--events` capture of real `coder` traffic exists yet (that requires running `coder`'s test suite through the capture proxy per §11.4's `TGD-BL-11` precedent, a materially separate undertaking not attempted this session) — so there is still no tenant-attribution result, no SAFE/LEAK/UNATTRIBUTABLE counts, and no unattributable rate to report from real traffic. No fourth defect *shape* appeared; the standing instruction to file-and-stop on a new shape does not apply. | Full 20-of-22-table fixture coverage for a truly fresh `coder` migration (or, more realistically, re-running against a `coder` instance with real usage data, where most of these tables would already hold ≥2 rows and need no fixture at all) is the natural next step toward exercising A1–A4 on the complete real 22-table sweep. Capturing real `coder` traffic through the proxy (§11.4-style) is the only way to reach the differential stage and answer the tenant-attribution/SAFE-LEAK-UNATTRIBUTABLE questions the task has asked after each of the last two sessions. Neither attempted this session. |

## 12a. `Describe` FIFO correlation — validated against a real driver

Carried over from `TGD-BL-14`: the correlation logic (backend `ParameterDescription`
replies carry no statement name, so attribution rests on FIFO order of outstanding
`Describe('S')` requests) had only ever been proven by synthetic mutation tests. An
earlier attempt to exercise it with `pgx`'s `QueryExecModeExec` sent parameters in
text format and never triggered `Describe` at all — the path went unexercised, and
that was reported plainly rather than left implied.

**This time it worked. [executed]:** `pgx`'s `QueryExecModeDescribeExec` — which never
caches and always describes explicitly — was run through the built `tenantguard`
binary against real PostgreSQL, with two different queries back to back:

```
parse   SELECT $1::int4 + $2::int4
param_description   resolved=true
row_description
bind    params: [oid=23 known=true value="40"] [oid=23 known=true value="2"]

parse   SELECT $1::text || $2::text
param_description   resolved=true
row_description
bind    params: [format=text known=true value="foo"] [format=text known=true value="bar"]
```

Both `ParameterDescription` replies were correctly attributed to their own
statement — the first query's OIDs never leaked into the second's — which is the
FIFO correlation working end to end against a real driver, not a synthetic
harness. The int4 parameters were decoded from binary using the OID captured
from the backend stream, exactly as designed in §9.5.

**A real bug surfaced by this run, not by reading the code:** the captured JSON
showed `"value_known":true` alongside a stale `"unknown_reason":"binary
parameter; type OID not captured"` — `parseBind` sets a placeholder `Reason` on
every binary parameter before its OID is known, and `resolveParams` was not
clearing it after a successful decode. A `Known` parameter could carry a
self-contradictory reason for being unknown in the recorded evidence. Fixed by
clearing `Reason` when decoding succeeds; a red case was observed failing before
the fix (`TestDecodedBinaryParamHasNoStaleReason`), confirmed green after, and
proven by mutation (reverting the clear makes the test fail again).

**What remains unproven:** only one ordering (two statements, described and bound
in the order they were parsed) has been exercised live. A driver that pipelines
`Describe` requests ahead of their matching `Bind`s in a different order, or that
interleaves statements across a shared connection in a way `pgx` does not,
remains validated only by the synthetic mutation tests (`TestDroppedDescribeIsDetected`,
`TestOIDCountMismatchRefusesToDecode`). That gap is `[read-only]`, not `[executed]`, and is not
closed by this result.

## 13. Open items carried into the SRS

- ~~Exact wire-protocol message subset the proxy must implement~~ — **resolved by
  Gate 0**: `Parse` / `Bind` / `Execute` / `Query`, plus `SSLRequest` refusal, with
  per-connection statement scoping (§11.2). Sufficient for both `lib/pq` and `pgx`.
- Whether policy inference should propose or auto-accept inferred scoped tables.
- How the probe database's schema is kept in step with the live target's, given
  §9.4's residual gap.
- ~~Whether Tier 2's driver wrapper shares the differ with Tier 1 or reimplements
  it against in-process state.~~ — **resolved.** Shares `ExtractTenant` (the
  attribution logic) via a new `differ.CheckTenant`; does NOT share `Diff`
  (the RLS re-execution) — reusing that would double every wrapped query's
  DB round trips and require production RLS deployment, disqualifying for a
  hot-path library. Argued in full in `internal/guardrail`'s package doc
  and SRS §7.5, including the measured cost: `CheckTenant` closes `L5`
  (its reason for existing) but not every fixture `Diff`'s re-execution
  closes — see SRS §7.5's fixture-by-fixture accounting (`TGD-US-12`).
- Tier 0's ranking function — no design yet beyond "ranked, never authoritative."
- `L7`'s structural blind spot (`TGD-BL-23`): no design exists yet for detecting a
  leak that flows through a non-`security_invoker` view owned by a privileged
  role — the re-execution method's own baseline (superuser bypass) is
  fundamentally blind to it, at every tier. Not scheduled against any milestone.
- ~~The unattributable ceiling (`TGD-NFR-03`/`TGD-US-07`) is reported but not yet
  enforced~~ — **resolved, `TGD-BL-06` closed.** Baselined at 21.28% against
  `row_level_touching_real_app_sql` and enforced (§9.2, SRS §7.1/`TGD-US-07`
  AC-1–AC-3).
- `TGD-FR-18`'s shared-database header is an operator declaration
  (`--shared-database`), not automatic detection — no design exists for genuine
  automatic detection, and it is unclear one is possible given a single-`--dsn`
  invocation's visibility.
