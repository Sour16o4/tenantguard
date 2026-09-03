# SRS — `TGD` tenantguard: multi-tenant isolation verifier

**Status:** M0. Design accepted (`docs/superpowers/specs/2026-08-29-tenantguard-design.md`).
Gate 0 resolved — the wire proxy holds.
**Date:** 2026-08-29
**Conventions:** `cmd/` + `internal/` layout; `docs/SRS.md` authoritative; ID-tagged
backlog (`TGD-BL-nn`).

---

## 0. Evidence and baseline tags — read before any requirement

This specification distinguishes what has been **run** from what has been
**read**, and separates measured budgets from proposed ones. The distinction is
load-bearing: the tool's entire value rests on not reporting confidence it has
not earned, and the specification is held to the same standard.

| Tag | Meaning |
|-----|---------|
| **[E]** | **Executed.** Observed by running code during Gate 0. Reproducible. |
| **[R]** | **Read-only.** Established by reading source. Plausible, unverified by execution. An acceptance criterion carrying **[R]** rests on a premise nobody has run. |
| **[U]** | **Unbaselined.** A proposed number awaiting first measurement. **Not a requirement.** Must not be cited as a target until baselined, and the story that baselines it is named. |

**Rules that follow from these tags, and are themselves requirements:**

1. An **[R]** premise may not be described as verified anywhere in this document,
   in `README.md`, or in any report the tool emits.
2. A **[U]** budget may not gate a merge, block a release, or appear in published
   results until the baselining story has run. Until then it is a design intent.
3. When an **[R]** claim is later executed, its tag changes to **[E]** in the same
   pull request that runs it — never in advance.

### Open questions this SRS must not assume away

- **`TGD-BL-09` is open and half-answered.** `coder` emits no `SET`/`set_config`
  tenant context **[R]**. **`zitadel`'s session handling has not been inspected** —
  only its `go.mod` was read. **No requirement, acceptance criterion, or NFR in
  this document may assume `zitadel` does or does not set session tenant context.**
  Where behaviour would differ, both branches are specified or the scope is
  restricted to `coder`.

---

## 1. Client Requirement Brief

Build a tool that proves whether a Go application's SQL queries respect
multi-tenant row isolation, by letting PostgreSQL decide what each query should
have been permitted to return, rather than by parsing the query.

The tool must be usable against a repository the operator did not write, must
never write to that repository's database, and must abort rather than report a
clean result when it cannot prove its own oracle is working.

## 2. Problem Statement & Justification

Shared-schema multi-tenancy scopes rows by a tenant column. Isolation depends on
every query carrying a correct tenant predicate, on every code path, forever.
There is no compiler check. The failure is silent, the blast radius is the
customer list, and the discovery channel is usually the customer.

Syntactic tools — parse the SQL, assert a predicate exists — are defeated by
joins, CTEs, views, subqueries, set-returning functions and tautologies, and
cannot detect a query scoped to the *wrong* tenant, which is syntactically
perfect.

**Justification for the chosen approach.** Delegating the verdict to PostgreSQL's
own row-level security makes the database the authority. Joins, views, CTEs and
functions are resolved by the engine rather than reimplemented in a parser, and a
row-set divergence *is* a violation rather than a heuristic suggesting one.

**The cost, stated plainly.** The synthesised RLS becomes the definition of ground
truth. If it is wrong, the tool reports "0 leaks" with total confidence, and that
output is indistinguishable from a clean repository. §6 Epic E-1 and §10's
mutation plan exist entirely to make that failure impossible to reach silently.

## 3. Scope

### 3.1 In scope (v1)

- PostgreSQL only.
- Shared-schema tenancy with a tenant column.
- Three operating tiers: syntactic triage, proxy-based differential audit, and an
  in-process fail-closed guardrail.
- Validation against `coder/coder` as the primary target.

### 3.2 Out of scope (v1)

| Excluded | Reason |
|---|---|
| MySQL, SQLite, CockroachDB | The oracle is PostgreSQL RLS. No equivalent is assumed. |
| Schema-per-tenant, database-per-tenant | A different detection problem. Claiming it would be a lie of scope. |
| Remediation / query rewriting | The tool reports. It does not edit the target. |
| TLS-terminating proxy | `sslmode=require` targets are unsupported at Tier 1 — see `TGD-NFR-12`. |
| Non-Go targets | Tiers 0 and 1 are language-agnostic at the wire level but will only be validated against Go. |

### 3.3 Trust boundary (never cross)

**The tool never writes to the target application's database.** Not in tests, not
behind a flag. There is no flag for this; do not add one. All write access is
confined to a tool-owned probe database created by the tool (`TGD-US-03`).

## 4. Personas & Stakeholders

| Persona | Role | Needs |
|---|---|---|
| **Priya, platform engineer at a B2B SaaS** | Primary user | Wants to know whether her own codebase leaks across tenants, without writing a test per query, and without trusting a linter that produces false positives. |
| **Rahul, security engineer** | Buyer / adopter | Needs a result he can put in front of an auditor. A finding must be reproducible and a clean run must be trustworthy, or it is worthless to him. |
| **An open-source maintainer** | Involuntary subject | The tool may be pointed at their repository. Findings must be private-disclosure-ready, accurate, and never fabricated. |
| **The operator running an audit** | Tier 1 user | Must be able to point the tool at an unfamiliar repository and get a defensible answer, or a clear statement of why one is not available. |

## 5. Functional Requirements

Priority: **M** must, **S** should, **C** could.

| ID | Requirement | Priority | Verification | Tag |
|----|-------------|----------|--------------|-----|
| `TGD-FR-01` | The tool shall synthesise PostgreSQL RLS policies from a declared tenancy policy. | M | Fixture corpus | — |
| `TGD-FR-02` | The tool shall run four oracle self-checks before judging any query, and shall abort with no report if any fails. | M | Mutation harness | — |
| `TGD-FR-03` | The tool shall create a tool-owned probe database, seed two canary tenants into it, and prove row-withholding there, without writing to the target's database. | M | Integration test | — |
| `TGD-FR-04` | The tool shall capture `Parse`/`Bind`/`Execute`/`Query` from the PostgreSQL frontend stream, scoping statement names **per connection**. | M | Protocol test | **[E]** |
| `TGD-FR-05` | The tool shall support the statement-naming schemes of both `lib/pq` (sequential per connection) and `pgx` v5 (SHA of SQL). | M | Protocol test | **[E]** |
| `TGD-FR-06` | The tool shall classify every captured query as exactly one of `SAFE`, `LEAK`, `UNATTRIBUTABLE`. | M | Fixture corpus | — |
| `TGD-FR-07` | The tool shall never classify a query it could not resolve as `SAFE`. | M | Fixture `U4` | — |
| `TGD-FR-08` | The tool shall fail the run when the unattributable rate exceeds the configured ceiling. | M | Integration test | **[E]** `TGD-BL-06` — see `TGD-US-07` AC-1 for the closing evidence. |
| `TGD-FR-09` | The tool shall provide a Tier 0 syntactic pass that requires no database, emits ranked suspicions, and **never exits non-zero**. | M | CLI contract test | **[E]** Built (`internal/triage`, `tenantguard triage`). 12 unit tests (11 written alongside the implementation, 1 more added after a real defect surfaced running against real `coder` source, §7.4) — all pass; `go vet`/`gofmt` clean. Timed clone-to-output against a fresh `coder/coder` clone: 19.5s, well under the 5-minute budget (`TGD-NFR-01`). Run against `coder`, compared query-by-query against the same session's 37 real `LEAK` verdicts (§7.3): 3/7 distinct leaking query names overlap; the other 4 diverge for three distinct, named reasons (§7.4) — the first time this project has measured the tier split empirically rather than asserting it. |
| `TGD-FR-10` | The tool shall infer candidate tenant-scoped tables from `information_schema` and emit a policy file for human review. | M | Inference test | — |
| `TGD-FR-11` | The tool shall report each finding with the query text, the verdict, the originating test or code path, and each parameter **either as a decoded value or explicitly marked undecodable with the reason**. Byte capture and value recovery are distinct and must be reported separately; an undecodable parameter must never be rendered as though its value were known. | M | Integration test | **[E]** for the distinction |
| `TGD-FR-19` | The tool shall capture the **backend** message direction and use `ParameterDescription` / `RowDescription` type OIDs to decode binary-format parameters, marking as undecodable only those whose OID is absent or unsupported. | M | Protocol test | `TGD-BL-14` |
| `TGD-FR-12` | The tool shall detect a target requiring TLS and exit with a distinct code and an actionable message, rather than failing obscurely. | M | Integration test | **[E]** |
| `TGD-FR-13` | The tool shall provide a Go library API for Tier 2 fail-closed guardrail use. | S | API example test | **[E]** Built (`internal/guardrail`, `guardrail.Wrap`/`WithTenant`). Mechanism argued before building (§7.5, `internal/guardrail`'s own package doc): a static predicate-vs-context comparison (`differ.CheckTenant`, sharing `ExtractTenant` with Tier 1, never re-executing anything), chosen over reusing the RLS oracle at runtime on cost and hot-path grounds. 20 unit tests total including two proving zero database contact for a blocked query against an unreachable address. Design §7's 24-fixture corpus run at Tier 2 for the first time (`TestCorpus_Tier2ExactSetEquality`): L5 correctly detected (`LEAK`, the headline requirement); 3 of 23 testable fixtures diverge from §7.4's pre-implementation prediction, each traced to a specific, named cause and reported as a design finding rather than reconciled (§7.5). A fourth apparent divergence, `U2`, was found on review to be a real fail-open bypass rather than a mere mislabelling and was fixed the same session, proven red-before-green plus a mutation (`TGD-BL-43`, §7.6) — it now matches §7.4's original prediction exactly. |
| `TGD-FR-14` | The mutation harness shall assert each mutant is caught **by its mapped gate**, treating a mutant caught by a different gate as a finding. | M | Mutation harness | — |
| `TGD-FR-15` | The tool shall record, per run, whether `A1` was proved on the probe database only or also on the live target, and report which. | M | Integration test | — |
| `TGD-FR-16` | The tool shall support `--output json` for all reporting commands, with human-readable output on stderr only. | S | Schema conformance test | — |
| `TGD-FR-17` | The tool shall detect an unmigrated target database — declared scoped tables absent — and exit with a distinct code naming the missing tables, rather than proceeding and reporting a clean run. | M | Integration test | **[E]** `C-6` |
| `TGD-FR-18` | The tool shall record, in the report header, that the target harness is sharing one database across tests (no per-test isolation), so cross-test interference is not mistaken for application behaviour. **Narrowed:** as an operator declaration (`--shared-database`), not automatic detection — a single `--dsn` invocation carries no signal distinguishing a reused database from a dedicated one. `[E]` `C-7` is evidence for *why this header matters* (observed on `coder`), not evidence the tool detects it unprompted. | S | Integration test | **[E]** `--shared-database` flag, `TGD-BL-24` |

---

## 6. Epics & User Stories

### Epic E-1 — The Oracle
*The foundation. If the synthesised RLS is wrong, every subsequent verdict is confidently wrong, and a clean run is indistinguishable from a blind one.*

---

#### `TGD-US-01` — Synthesise RLS policies from a declared tenancy policy
**[Epic: E-1] [Points: 5] [Priority: M]**

**STORY**
As **Priya**, I want the tool to turn a short declaration of which tables are
tenant-scoped into working RLS policies, so that PostgreSQL can act as the
authority on what each query should have returned.

**DESCRIPTION**
The tenancy policy names scoped tables and their tenant column. The tool
synthesises one policy per table plus a restricted role that the differ connects
as. This is the step that defines ground truth, so it is also the step the
mutation harness (`TGD-US-11`) attacks hardest.

`coder` has **no RLS anywhere in its schema** **[R]** — authorisation is entirely
in application code. Synthesis is therefore *necessary rather than optional* for
the primary target, and has nothing pre-existing to conflict with.

**ACCEPTANCE CRITERIA**

- **AC-1** *Given* a tenancy policy naming tables and tenant columns,
  *When* the tool runs synthesis,
  *Then* each named table has RLS enabled and exactly one matching policy in
  `pg_policies`, and the restricted role is created with no ownership of those tables.

- **AC-2** **[E], via the CLI's flag input rather than a policy file.** *Given* a
  table that does not exist in the schema,
  *When* synthesis runs,
  *Then* the tool exits non-zero naming that table, and creates **no** policies at all.

  No policy-file format exists (§8.1's `--policy <f>` is not implemented — see
  `TGD-BL-18`); this CLI takes `--table` directly. The protective intent is met
  regardless: `EnableRLS`'s `ALTER TABLE` against a nonexistent relation errors,
  and that surfaces as a non-zero exit with stdout asserted empty, checked
  end-to-end against the built binary.

- **AC-3** **[E], via the CLI's flag input rather than a policy file** (same scope
  note as AC-2). *Given* a tenant column that does not exist on its table,
  *When* synthesis runs,
  *Then* the tool exits non-zero, and creates no policies.

  Checked end-to-end against the built binary, distinct from the AC-4/`TGD-BL-15`
  "wrong but existing column" case: `--tenant-column` naming a column absent
  from the table entirely makes `CREATE POLICY` error at the database level,
  surfacing as non-zero exit with empty stdout.

- **AC-4** **[E] Closed.** *Given* successful synthesis,
  *When* the tool inspects its own work,
  *Then* every synthesised policy is confirmed present via `pg_policies` (and
  `relrowsecurity`/`relforcerowsecurity` via `pg_class`) before synthesis
  reports success — the tool does not assume its own DDL took effect.

  Implemented as `verifySynthesis`, called by `EnableRLS` after its DDL. **A
  genuine "DDL succeeded, catalog disagrees" state could not be constructed**:
  under PostgreSQL's autocommit and READ COMMITTED defaults, once `CREATE
  POLICY` returns without error it is durably committed and immediately
  visible to any subsequent query on any connection — a single-writer run with
  no concurrent interference cannot produce the divergence this AC defends
  against. That is stated as fact, not worked around. Two things were proven
  instead, both against real PostgreSQL: (1) `verifySynthesis`'s own detection
  — three deterministically constructed bad catalog states (RLS never
  enabled, RLS enabled but not forced, no policy present), each built directly
  and bypassing `EnableRLS` — 3 mutations against the three checks, all caught.
  (2) The wiring from `EnableRLS` to that detection, via a controlled seam
  (`verifySynthesisFn`, a package variable) rather than an unconstructable
  Postgres state: a stub was substituted that fails unconditionally while
  `EnableRLS`'s real DDL still ran and succeeded, and `EnableRLS`'s returned
  error was confirmed to carry the stub's sentinel. The literal mutation this
  AC's own text calls for was also run: removing the call to the seam from
  `EnableRLS` made the wiring test fail red, for the predicted reason
  (`EnableRLS` returned nil despite the stub reporting failure), then restored.

- **AC-5** *Given* synthesis has run,
  *When* the target's database is inspected,
  *Then* no policy, role, table, or row has been created or modified in it
  (§3.3 trust boundary; synthesis targets only the probe database and, for `A2`/`A3`,
  reads the live one).

---

- **AC-6** **[E]** *Given* a schema with a table carrying two or more tenant-column
  candidates (e.g. both `org_id` and `workspace_id`),
  *When* inference runs,
  *Then* the table is reported `Unclassifiable` with both candidates listed — never
  silently resolved to one, and never defaulted to unscoped.

- **AC-7** **[E]** *Given* a view, materialized view, or foreign table,
  *When* inference runs,
  *Then* it is reported `Unclassifiable` with the reason that RLS cannot attach to
  it, even if it exposes a tenant-shaped column.

#### `TGD-US-02` — Oracle self-check that aborts instead of reporting clean
**[Epic: E-1] [Points: 8] [Priority: M]**

**STORY**
As **Rahul**, I need the tool to prove its oracle can actually see before it tells
me I have no leaks, so that a clean report means something.

**DESCRIPTION**
Four assertions run before any query is judged. All four abort the run with a
distinct exit code and emit no findings report.

`A3` exists because RLS does not apply to a table's owner unless
`FORCE ROW LEVEL SECURITY` is set. A tool connecting as owner finds RLS silently
inert while every indicator stays green — a gate that cannot fire, which is the
exact defect class this project exists to detect in other people's systems.

**ACCEPTANCE CRITERIA**

- **AC-1** (`A1`, negative control) *Given* a synthesised policy set,
  *When* a deliberately unscoped probe query runs as the restricted role and as an
  unrestricted role,
  *Then* the restricted role returns **strictly fewer** rows; if the counts are
  equal, the tool aborts with exit code 10 and prints no findings.

- **AC-2** (`A2`, coverage) *Given* the declared scoped tables,
  *When* the tool queries `pg_policies` and `pg_class.relrowsecurity` on the live target,
  *Then* every declared table has an enabled matching policy, or the tool aborts
  with exit code 11 listing the tables that do not.

- **AC-3** (`A3`, privilege) *Given* the connecting role,
  *When* the tool inspects `pg_roles.rolsuper`, `rolbypassrls`, and table ownership,
  *Then* the run aborts with exit code 12 if the role is a superuser, holds
  `BYPASSRLS`, or owns a scoped table **without** `relforcerowsecurity` set on it.

- **AC-4** (`A4`, positive control) *Given* a known-correctly-scoped query,
  *When* it runs under both roles,
  *Then* the row sets are identical; if the restricted role returns fewer rows,
  the tool aborts with exit code 13, because an over-restrictive oracle reports
  every query as a leak.

- **AC-5** **[E] Closed via the CLI, end-to-end.** *Given* any self-check failure,
  *When* the run aborts,
  *Then* stdout contains **no** findings report and **no** leak count, in any
  output format, and the exit code identifies which assertion failed.

  Proven by invoking the **built binary**, not by calling Go functions
  directly: `tenantguard verify` and `tenantguard audit`, run against real
  PostgreSQL with a `--tenant-column` naming a non-text column, both exit `10`
  (`ExitA1Failed`) with **stdout asserted byte-empty** and a reason on stderr.
  `audit` was checked specifically, since this AC names it, not only `verify`.
  A clean run was proven on the same binary to exit `0` with a real JSON report
  on stdout — a build that can only abort would be as uninformative as one that
  can only pass. Mutation-proven: report-on-abort, exit-code mismapping (both
  directions, `A1`↔`A4`), and the probe database not being dropped were all
  caught.

- **AC-6** *Given* all four self-checks pass,
  *When* the run proceeds,
  *Then* the report header records which assertions were proved against the probe
  database and which against the live target (`TGD-FR-15`).

**ADDITIONAL NOTES**
- Exit codes 10–13 are reserved for `A1`–`A4` respectively and are part of the
  CLI contract (§8.2).

---

#### `TGD-US-03` — Probe database, so `A1` does not depend on the target's seed data
**[Epic: E-1] [Points: 5] [Priority: M]**

**STORY**
As **the operator**, I want `A1` to work against a repository that seeds a single
tenant, so that the tool is useful on real targets instead of correctly aborting on
all of them.

**DESCRIPTION**
`A1` needs two tenants with rows in one scoped table. `coder`'s template database
is schema-only and `dbtestutil` seeds zero organisations **[R]**, so `A1` would
abort on a healthy repository.

The resolution is that `A1` was mis-sited: it is a self-check on the synthesised
RLS, not on the application. Whether RLS withholds rows is a property of policies
and schema, not of seed data. The tool therefore builds its **own** probe database
from the target's schema, seeds two canary tenants there, and proves withholding
against that.

For `coder` this uses the mechanism `coder` already uses —
`CREATE DATABASE … WITH TEMPLATE tpl_<migrations-hash>` **[R]**. Generally it is the
target's migrations, or `pg_dump --schema-only`.

**Residual gap, specified rather than hidden:** `A1` then proves withholding on the
probe. If the live schema has diverged, the oracle could still be blind where it
matters. `A2`/`A3` continue to run against the live database, and `AC-5` below
upgrades confidence when the live database happens to permit it.

**ACCEPTANCE CRITERIA**

- **AC-1** *Given* a target schema,
  *When* the tool prepares a run,
  *Then* it creates a probe database from that schema and seeds at least two
  canary tenants with rows in every declared scoped table.

- **AC-2** *Given* the probe database exists,
  *When* the run completes or aborts for any reason,
  *Then* the probe database is dropped, including on panic and on `SIGINT`.

- **AC-3** *Given* the connecting role lacks `CREATE DATABASE`,
  *When* the tool prepares a run,
  *Then* it exits with a distinct code and states the missing privilege, rather
  than falling back to seeding the target.

- **AC-4** *Given* any run,
  *When* the target's database is inspected afterwards,
  *Then* row counts in every target table are unchanged (§3.3).

- **AC-5** *Given* the live target database happens to contain two or more tenants
  with rows in one scoped table,
  *When* the run proceeds,
  *Then* `A1` is additionally executed against the live database, and the report
  records the result as target-verified rather than probe-verified.

---

### Epic E-2 — Capture
*Gate 0 proved the protocol behaviour. These stories turn a throwaway spike into a component.*

---

- **AC-6** **[E]** `A1` alone cannot detect a policy attached to the wrong tenant
  column: a policy comparing `id::text` to the tenant setting still satisfies
  `A1`'s "strictly fewer rows" check, because it excludes *some* rows — just not
  the correct ones. This was found by mutation testing against `oracle.go`, not
  anticipated in the original design. **`A1` passing is necessary but not
  sufficient**; `A4` (`TGD-US-02` AC-4) is required to catch this class of error.

  **[E] Closed as an executable gate, not only a documentation rule.** `CheckA4`
  is implemented and proven against real PostgreSQL: it passes on a correct
  policy and aborts — wrapping `ErrA4`, exit code 13, distinct from `ErrA1`'s
  exit code 10 — on both the exact `id::text` policy that survived `A1` and a
  separate `USING(false)` policy. Non-redundancy is demonstrated directly rather
  than assumed: a correct policy on a single-tenant probe makes `A1` abort while
  `A4` passes; the wrong-column policy on a two-tenant probe makes `A1` pass
  (deceptively) while `A4` aborts. `ProofState.PolicyProven()` enforces that no
  report may be generated on `A1` success alone: it is tested directly across six
  combinations of checked/passed, including "`A1` passed, `A4` never run", which
  must still refuse.

#### `TGD-US-04` — Wire proxy with per-connection statement scoping
**[Epic: E-2] [Points: 8] [Priority: M]**

**STORY**
As **the operator**, I want to point the target's connection URL at a proxy and run
its test suite unchanged, so that auditing needs no code changes in a repository I
do not own.

**DESCRIPTION**
Gate 0 established the required message subset and the one non-negotiable design
constraint **[E]**: statement names are scoped **per connection**, not globally.

`lib/pq` names prepared statements sequentially per connection — `"1"`, `"2"` — and
four connections preparing four *different* SQL texts all received the name `"1"`
**[E]**. A globally-keyed proxy resolves that name to whichever SQL it saw last and
then judges a query the application never ran, emitting a confident verdict about
the wrong statement. That is strictly worse than failing.

`pgx` v5 names by SHA of the SQL text, so identical names imply identical SQL and
the collision cannot occur **[E]**. The tool supports both and assumes neither.

**Driver facts:** `coder` uses `lib/pq v1.10.9` via their `coder/pq` fork, with no
`jackc/pgx` in `go.mod` **[R]**. `zitadel` uses `pgx/v5 v5.9.2` **[R]**.

**ACCEPTANCE CRITERIA**

- **AC-1** *Given* a target configured to connect through the proxy,
  *When* its workload runs,
  *Then* every `Bind` is resolved to the SQL text of its `Parse`, scoped to the
  connection the `Bind` arrived on.

- **AC-2** **[R]** *Given* two connections that each `Parse` different SQL under the
  same statement name,
  *When* both issue `Bind` against that name,
  *Then* each resolves to its own connection's SQL, and no cross-connection
  resolution occurs. **This test must fail if statement state is made global.**

  **Rationale is unproven against the primary target.** `lib/pq` names statements
  sequentially per connection *in general*, so two connections can hold different SQL
  under the name `"1"` — reproduced in a synthetic harness **[E]**. But `coder` never
  produces it: `TGD-BL-11` observed **1,317/1,317 statements unnamed**, because
  `database/sql` with arguments takes `lib/pq`'s unnamed path and never calls
  `db.Prepare`. The collision requires explicit preparation, which `coder`'s tested
  code does not use.

  The AC is **retained**, because the risk is real for any target that does prepare
  explicitly, and `zitadel` uses `pgx` whose behaviour here is separately measured.
  It is tagged **[R]** so nobody reads it as something `coder` demonstrated.

- **AC-3** *Given* a workload exceeding the driver's statement-cache capacity,
  *When* eviction forces re-`Parse`,
  *Then* resolution continues with no unresolved `Bind`. **[E]** — 601 distinct
  statements against a 512 capacity produced 0 unresolved.

- **AC-4** *Given* messages split across TCP reads and several messages arriving in
  one read,
  *When* the framer processes the stream,
  *Then* all messages are recovered intact. Verified with a deliberately small read
  buffer. **[E]**

- **AC-5** *Given* a client using the simple query protocol,
  *When* queries arrive,
  *Then* the fully-interpolated SQL is captured verbatim. **[E]**

- **AC-6** *Given* any captured traffic,
  *When* the proxy forwards it,
  *Then* the byte stream to the server and back to the client is **unmodified**;
  the proxy observes and never rewrites.

- **AC-7** *Given* a client that requires TLS,
  *When* it connects,
  *Then* the tool exits with the code reserved for `TGD-FR-12` and states that
  Tier 1 requires a non-TLS test DSN. **[E]** — `sslmode=require` currently fails
  at connection time.

- **AC-8** **[E]** *Given* a client that closes a prepared statement and later issues
  a `Bind` naming it without re-preparing,
  *When* the verdict is assigned,
  *Then* the `Bind` is **unresolvable**, never resolved to the closed statement's SQL.

  Statement state is removed on `Close` (`'C'`), scoped per connection. Without this,
  the proxy resolves to stale SQL and judges a query the application never ran — the
  `R-2` failure class. Found and fixed as `TGD-BL-12`; the red case was observed
  failing before the change, and a regression run exercised **101 real `Close`
  messages** with 821/821 still resolved. **This AC must fail if `Close` handling is
  removed.**

---

#### `TGD-US-05` — Degrade to `UNATTRIBUTABLE`, never to `SAFE`
**[Epic: E-2] [Points: 3] [Priority: M]**

**STORY**
As **Rahul**, I need a query the tool could not analyse to be reported as
unanalysed, so that coverage gaps are visible instead of being counted as passes.

**ACCEPTANCE CRITERIA**

- **AC-1** **[E]** *Given* a `Bind` whose statement name has no `Parse` on that connection,
  *When* the verdict is assigned,
  *Then* it is `UNATTRIBUTABLE`, never `SAFE`. `differ.go`'s `!ev.Resolved` check.

- **AC-2** **[E]** *Given* a `Bind` whose parameters cannot be fully decoded,
  *When* the verdict is assigned,
  *Then* it is `UNATTRIBUTABLE` and the report states which parameter positions
  were unrecoverable. Previously the reason said only "at least one parameter"
  — closed this session: `ExtractTenant` now names every unrecoverable `$N`
  position, proven by `TestExtractTenant_UnattributableReasonNamesParameterPositions`
  (single and multi-position cases) and mutation-tested (reverting to the old
  generic message fails the test). See `TGD-BL-26`.

- **AC-3** **[E]** *Given* any run,
  *When* the summary is printed,
  *Then* `SAFE`, `LEAK` and `UNATTRIBUTABLE` counts are all shown, and the three
  sum to the number of captured queries. Closed this session: `verifyReport.Counts`
  (`safe`/`leak`/`unattributable`), set whenever `--events` produced at least one
  verdict, proven by `TestAuditCLI_WithEventsProducesPerQueryVerdicts` (asserts
  the three counts individually and that they sum to `len(Queries)`) and
  mutation-tested. See `TGD-BL-26`.

---
### Epic E-3 — Verdict Engine
*The differential itself, and the honesty constraints on what it may claim.*

---

#### `TGD-US-06` — Differential verdict per captured query
**[Epic: E-3] [Points: 8] [Priority: M]**

**STORY**
As **Priya**, I want each query re-executed under RLS and its rows compared, so that
a reported leak is proven rather than suspected.

**DESCRIPTION**
For a captured query `Q` that returned row set `R` unrestricted, the tool determines
the intended tenant `T`, executes `Q` under the restricted role bound to `T`
yielding `R_rls`, and compares. Any row in `R` and not in `R_rls` is a proven leak.

**Tier-dependent detection, specified because it changes what a clean run means.**
At Tier 1 the proxy's only source for "which tenant did this intend?" is the value
in the query's own predicate. That is sufficient for fixtures `L1`–`L4` and
`L6`–`L10`, whose predicates are absent, tautological or structurally defeated. It
is **not** sufficient for `L5` — a correct predicate naming the wrong tenant is
self-consistent and passes.

`coder` emits no `SET`/`set_config` tenant context **[R]**, so **`L5` is a
confirmed Tier 1 blind spot for the primary target** and must appear as such in
`LIMITATIONS.md`. **`zitadel`'s session handling is unknown (`TGD-BL-09` open) and
this story assumes nothing about it in either direction.**

**ACCEPTANCE CRITERIA**

- **AC-1** **[E]** *Given* a captured query with a determinable intended tenant,
  *When* the differ runs,
  *Then* the query is re-executed under the restricted role and the row sets compared.

- **AC-2** **[E]** *Given* the unrestricted result contains rows absent from the RLS result,
  *When* the verdict is assigned,
  *Then* it is `LEAK`, and the report includes the withheld row count.

- **AC-3** **[E]** *Given* an aggregate query with no tenant predicate — e.g. `SELECT count(*)`,
  *When* the differ runs,
  *Then* a differing scalar result is reported as `LEAK`, because information crossed
  the boundary even though no row did.

- **AC-4** **[E]** *Given* the corpus is run at **Tier 1**,
  *When* verdicts are compared to the expected Tier 1 set,
  *Then* they match exactly, **with `L5` expected as `SAFE`** — the documented blind spot.
  A second, structural blind spot (`L7`, undetectable at every tier) was found while
  building the corpus and is documented in `LIMITATIONS.md` and `TGD-BL-23`; the
  Tier 1 expected set includes `L7` as `SAFE` for that reason, not as an oversight.

- **AC-5** *When* verdicts are compared to the expected Tier 2 set,
  *Then* they match exactly, **with `L5` expected as `LEAK`**. **[E]** Closed
  (§7.5, `TGD-US-12`, `M4`): `TestCorpus_Tier2ExactSetEquality` runs `CheckTenant`
  against every Tier-2-applicable fixture and asserts exact-set equality —
  `L5` lands `LEAK`, confirming the AC's own headline case. The literal
  original phrasing ("match exactly" against §7.4's table) does NOT hold
  unmodified: 3 of 23 fixtures (`L3`, `L7`, `L10`) diverge from that
  table's pre-implementation prediction for reasons specific to the chosen
  mechanism (§7.5) — the test asserts against `checkTenantExpected`, the
  actual, understood, argued-correct set, not against `corpusFixtures`'
  (`U2` diverged too until `TGD-BL-43` (§7.6) fixed the real fail-open
  bypass it had found; it now matches §7.4's prediction exactly.)
  unmodified `tier2` column, and logs every one of the four divergences
  explicitly on every run rather than silently passing against a relaxed
  target.

- **AC-6** **[E]** *Given* re-execution of a captured query,
  *When* it runs against the probe or a read-only snapshot,
  *Then* no write occurs to the target database, including for captured `INSERT`,
  `UPDATE` and `DELETE` statements (§3.3). Proven via `diffWrite`'s always-rolled-
  back transaction; `TestDiff_InsertReturningNeverWrites` asserts row counts are
  unchanged after a captured `INSERT` is re-executed. See `TGD-BL-21` for the
  non-transactional-sequence finding that shaped this mechanism.

**ADDITIONAL NOTES**
- `AC-4` and `AC-5` must use **two distinct expected sets**. A single expected set
  silently passes whichever tier it was not written for. **[E]** — enforced by
  `TestCorpus_TierExpectationsAreConsistent`, itself mutation-tested.
- `AC-1`–`AC-4` and `AC-6` closed an earlier session (`TGD-BL-21`, `TGD-BL-22`,
  `TGD-BL-23`, `TGD-BL-24`); `AC-5` closed this session (§7.5, `TGD-US-12`).

---

#### `TGD-US-07` — Unattributable ceiling fails the run
**[Epic: E-3] [Points: 3] [Priority: M]**

**STORY**
As **Rahul**, I want a run that could not analyse most of its traffic to fail, so
that `UNATTRIBUTABLE` cannot become a way to pass by not looking.

**DESCRIPTION**
A run marking everything `UNATTRIBUTABLE` is never wrong and never useful. The
ceiling makes that outcome a failure rather than a silent non-result.

**ACCEPTANCE CRITERIA**

- **AC-1** **[E] Closed — `TGD-BL-06`.** *Given* an unattributable rate above the
  configured ceiling, *When* the run completes, *Then* it exits non-zero and the
  summary states the rate and the ceiling.

  Implemented as `checkUnattributableCeiling` (`cmd/tenantguard/ceiling.go`),
  called after the report is written to stdout and before the process exits:
  a rate strictly greater than `unattributableCeilingRate` against
  `unattributableCeilingDenominator`'s own entry in
  `UnattributableRateByDenominator` (`TGD-BL-33`) exits
  `ExitUnattributableCeilingExceeded` (3, reserved in §8.2 since this
  document was written and unused until now) with a stderr message naming
  the denominator, the measured rate, the raw counts, and the ceiling —
  proven by `TestCheckUnattributableCeiling_MessageNamesRateAndDenominator`.
  The full report still reaches stdout on a breach (a completed, proven run
  with real per-query verdicts, unlike codes 10–13's oracle-self-check
  aborts, which correctly suppress it) — proven end-to-end by
  `TestAuditCLI_UnattributableCeilingFailsAboveIt`, alongside its companion
  `TestAuditCLI_UnattributableCeilingPassesAtOrBelowIt` for the "at or below
  passes" half. **The gate's ability to actually fire was proven, not
  assumed** — `TGD-NFR-03` had been unbaselined for this project's entire
  life before this session, so nothing had ever exercised it. Two mutations
  run and reverted: (1) the stored baseline temporarily lowered to `0.01`,
  which flipped `TestAuditCLI_UnattributableCeilingPassesAtOrBelowIt` and two
  `ceiling_test.go` cases to failing — the pass/fail boundary is read from
  the recorded constant, not hardcoded; (2) the enforcement call in
  `runGateCommand` temporarily removed, which flipped
  `TestAuditCLI_UnattributableCeilingFailsAboveIt` to failing (exit 0 instead
  of 3) — the check is load-bearing, not dead code a deletion could pass
  through silently. Both reverted after observing red.

- **AC-2** **[E, narrowed].** *Given* a configured ceiling higher than the
  current committed value, *When* the tool loads configuration, *Then* it
  refuses the value — the ceiling ratchets **down only**, and raising it
  requires a recorded backlog decision, not a config edit.

  **There is no `--ceiling` flag or config file, so there is nothing at
  runtime to "refuse."** `unattributableCeilingRate` is a Go constant in
  source (`cmd/tenantguard/ceiling.go`), not an operator-supplied value —
  raising it is only possible by editing that source line, which is itself
  the enforcement mechanism the literal AC describes a runtime check for:
  changing it requires a code change, a PR, and (per this document's own
  discipline) a recorded backlog entry naming why, exactly as AC-2 asks, just
  enforced by process and code review rather than by a load-time validation
  routine, since there is no configuration surface for one to validate. This
  is a real, permanent narrowing, not a temporary gap: no `--ceiling` flag is
  planned, so this AC's literal "when the tool loads configuration" premise
  will not be satisfied by a future build either, unless a configurable
  ceiling is added for some other reason.

- **AC-3** **[E] Closed — `TGD-BL-06`.** *Given* the ceiling has been baselined,
  *When* a run's measured rate against the baselined denominator exceeds it,
  *Then* the run fails per AC-1.

  **Superseded from its original text**, which guarded the opposite: "given
  the ceiling has not yet been baselined... the ceiling does not fail the
  run." That guard did its job — `TGD-NFR-03` stayed **[U]** and unenforced
  from this document's first draft until `TGD-BL-33`/`-34` built a
  tool-computed, reproducible denominator and `TGD-BL-06` measured a real
  baseline against it (`unattributableCeilingRate = 130.0 / 611.0 ≈
  0.2128`, `row_level_touching_real_app_sql`, measured
  2026-09-01 against `coder`'s `dbpurge` suite captured through the proxy —
  see `TGD-NFR-03`'s own SRS row for the full provenance and the ratchet
  rule). The pre-baseline behavior this AC used to require is preserved as a
  *degenerate case*, not deleted: a run whose traffic touches nothing in the
  baselined population (no `--events`, or `--events` producing zero
  `row_level_touching_real_app_sql` queries) still reports the rate and
  exits 0 — proven by
  `TestAuditCLI_UnattributableRateReportedEvenWhenCeilingHasNothingToCheck`
  (renamed from `...NeverEnforced`, whose old name and rationale were no
  longer true once this AC closed) and directly by
  `TestCheckUnattributableCeiling_NoMatchingEntryPasses`. `TGD-BL-24` remains
  the reason `unattributable_rate` is reported at all; `TGD-BL-33`/`TGD-BL-06`
  are what this AC now closes on.

---

### Epic E-4 — Tier 0 Triage

---

#### `TGD-US-08` — Syntactic pass that is never authoritative
**[Epic: E-4] [Points: 5] [Priority: M]**

**STORY**
As **the operator**, I want a zero-setup pass over any Go repository, so that I can
rank what to investigate before committing to a Tier 1 run.

**DESCRIPTION**
Tier 0 is the only tier that keeps the "point at any repo, no setup" promise, and it
must never be allowed to imply more than it proves. Fixtures `L6` (`WHERE tenant_id
= tenant_id`) and `L10` (`OR status = 'public'`) both pass a naive
predicate-presence check, which is the empirical argument for this tier being
advisory only.

**ACCEPTANCE CRITERIA**

- **AC-1** *Given* any Go repository and no database,
  *When* Tier 0 runs,
  *Then* it emits ranked suspicions and **exits zero regardless of findings**.
  **[E]** `runTriage` (`cmd/tenantguard/main.go`) exits 0 unconditionally once
  `triage.Run` starts walking real files — see its own doc comment for the
  one, deliberate carve-out (a bad `--output` value or a nonexistent/non-
  directory path is a usage error, exit 2, identical to every other
  subcommand's `--dsn`/`--policy` validation — never a finding about the
  scanned repo). §7.4 for the full run against `coder`.

- **AC-2** *Given* any Tier 0 output,
  *When* it is printed in any format,
  *Then* every finding is labelled unverified, and the words "leak", "proven" and
  "violation" do not appear.
  **[E]** `Report.Unverified` is always `true`, `Report.Label` is the fixed
  `unverifiedLabel` constant, and `TestRun_OutputNeverContainsForbiddenWords`
  greps every string the package actually emits (not just the ones assumed
  safe) for all three words.

- **AC-3** *Given* fixtures `L6` and `L10`,
  *When* Tier 0 runs,
  *Then* it does **not** flag them — recorded as a known limitation of the tier, and
  the reason Tier 1 exists.
  **[E]** `TestRun_L6TautologyNotFlagged`/`TestRun_L10ORDefeatNotFlagged`,
  design §7's own L6/L10 SQL text verbatim. §7.4 found, empirically against
  real `coder` traffic, that AC-3's naive-presence blind spot is broader
  than L6/L10 alone: a tenant column merely present in a `SELECT`/`RETURNING`
  list or an `INSERT` column list — never as an actual filter — passes the
  same check, and neither fixture names that shape.

---

### Epic E-5 — Policy Inference

---

#### `TGD-US-09` — Infer the tenancy policy from the schema
**[Epic: E-5] [Points: 5] [Priority: M]**

**STORY**
As **the operator**, I want the tool to propose which tables are tenant-scoped, so
that I review roughly ten lines instead of authoring a policy for an unfamiliar
schema.

**DESCRIPTION**
Inference reads `information_schema` and proposes scoped tables from column names:
`tenant_id`, `org_id`, `organization_id`, `workspace_id`, `account_id`, `owner`.

For `coder`, `organization_id` is the candidate — 252 occurrences across 118
migration files **[R]**. For `casdoor`, `owner` is part of the composite primary key
and is consistently filtered **[R]**.

**ACCEPTANCE CRITERIA**

- **AC-1** **[E]** *Given* a reachable target schema,
  *When* inference runs,
  *Then* it emits a policy file listing candidate scoped tables and their tenant column.
  Closed this session: `tenantguard infer --dsn URL --out FILE`, proven by
  `TestInferCLI_WritesPolicyFileVerifyCanConsume` (writes a real policy file,
  reads it back, asserts the classification, then feeds it unmodified into
  `verify --policy` and gets a proven run). Previously `schema.Infer` existed
  only as an internal library with zero CLI callers — the operator-facing
  surface this AC actually asks for did not exist. See `TGD-BL-26`.

- **AC-2** **[E]** *Given* inference output,
  *When* it is written,
  *Then* every entry is marked unconfirmed and requires explicit human acceptance;
  the tool does not proceed to a differential run on inferred-but-unreviewed policy.
  Closed this session, narrowly: `verify`/`audit` now take `--policy FILE`
  instead of a hand-supplied `--table`/`--tenant-column` pair, and refuse to run
  without one (`TestVerifyCLI_MissingPolicyFileExitsUsageError`) or infer on the
  fly — there is no code path from `--dsn` straight to a differential run. The
  file's existence is the acceptance gate; this build does not track a
  per-entry "reviewed" checkbox inside the file itself, which is a real,
  narrower reading of "explicit human acceptance" than a future build might
  want. See `TGD-BL-26`.

- **AC-3** **[E]** *Given* a table with more than one candidate tenant column,
  *When* inference runs,
  *Then* both are listed and the entry is marked ambiguous rather than one being
  chosen silently. `schema.Classify`'s `Unclassifiable`-with-`Candidates` path
  (pre-existing, unit-tested) is now reachable from the CLI via `infer`, closing
  the product-surface gap AC-1 had.

- **AC-4** *Given* `zitadel`'s versioned tables and split schemas,
  *When* inference runs,
  *Then* it completes without crashing and reports what it could not classify.
  Gated by `TGD-BL-07`; **[U]** — no accuracy target until measured.
  **Formally descoped from M2, not silently left dangling:** `zitadel` is a
  third-target integration effort — the design's own validation-targets
  table (§10) already calls it out as "valuable *because* it is the hard
  case; third target, not first." `M4` already carries `zitadel`-specific
  work as its own named exit-criterion gap (`TGD-BL-09`, session-handling
  inspection). Splitting `zitadel` integration work across both M2 and M4
  duplicates the same third-target burden under two different milestones
  for no reason tied to either milestone's own definition — `M2`'s name is
  "Tier 1 usable," already proven on `coder` (§7.3, 37 real `LEAK`
  verdicts), not "Tier 1 usable against every target." This AC moves to
  `M4`, alongside `TGD-BL-09`, where `zitadel`-specific work already lives.

---

### Epic E-6 — Reporting, CI, and Self-Attack

---

#### `TGD-US-10` — Findings report and CI contract
**[Epic: E-6] [Points: 5] [Priority: M]**

**STORY**
As **Priya**, I want a finding I can act on and hand to a colleague, so that a
report becomes a fix rather than an argument.

**ACCEPTANCE CRITERIA**

- **AC-1** **[E, partial]** *Given* a `LEAK`, *When* it is reported, *Then* the output
  includes the SQL text, resolved parameters, withheld row count, and the test or
  code path it was reached from. The first three are closed this session:
  `queryVerdict.Params` now carries the FULL bound parameter list (not just the
  single value used for tenant attribution), proven by
  `TestAuditCLI_WithEventsProducesPerQueryVerdicts`. "The test or code path it was
  reached from" is **not implemented and not claimed** — capture records only a
  connection id, with no provenance back to a test name or call site. Tracked as
  `TGD-BL-26`, deliberately left open rather than faked.
  **This remaining clause is formally descoped from M2, not silently left
  dangling:** it is a findings-report triage enhancement (letting a `LEAK`
  be traced to the test or call site that produced it) — valuable, named
  explicitly by `TGD-BL-32`'s own real-`coder`-run notes as what would let
  a `LEAK` be triaged without manual SQL inspection, but not required to
  demonstrate that Tier 1 itself produces real, actionable findings, which
  §7.3 already has (37 real `LEAK` verdicts, inspected directly by hand
  this project's own way, without this feature). It is a capture-layer
  change (threading provenance from `tenantguard capture` through to the
  report), cross-cutting all tiers rather than specific to any one
  milestone's own theme — it moves to `M5` (v1.0.0 hardening), where "every
  `[U]`/open item either resolved or explicitly withdrawn" already governs.

- **AC-2** **[E]** *Given* `--output json`, *When* the command completes, *Then* stdout
  contains a single valid JSON document conforming to the published schema, and all
  human-readable output goes to stderr. Previously there was no `--output` flag at
  all — the behaviour existed by default but nothing let a test select or exercise
  it. Closed this session: `--output json|text`, proven by
  `TestVerifyCLI_TextOutputIsSelectable` (both formats) and
  `TestVerifyCLI_BadOutputFlagExitsUsageError`.

- **AC-3** **[E]** *Given* any report, *When* the header is written, *Then* it records the
  tier, whether `A1` was probe-verified or target-verified, and the
  `SAFE`/`LEAK`/`UNATTRIBUTABLE` counts. Closed this session: `verifyReport.Tier`
  (always `1`), `.ProofSource` (always `"probe"` — every check runs against the
  disposable probe, never `--dsn`'s own data), and `.Counts`, proven by
  `TestVerifyCLI_CleanRunExitsZeroWithReport` (tier/proof_source) and
  `TestAuditCLI_WithEventsProducesPerQueryVerdicts` (counts). See `TGD-BL-26`.

- **AC-4** **[E]** *Given* an aborted oracle self-check, *When* output is produced, *Then*
  it contains no findings section in any format (reinforces `TGD-US-02` AC-5).
  Unaffected by this session's changes; still proven by the existing abort-path
  CLI tests (stdout empty on every non-zero exit).

---

#### `TGD-US-11` — Mutation harness that attacks the oracle
**[Epic: E-6] [Points: 8] [Priority: M]**

**STORY**
As **Rahul**, I want proof that each gate can actually fire, so that a passing gate
is distinguishable from a gate that is incapable of failing.

**DESCRIPTION**
A gate that cannot fire looks exactly like a gate that passes, and only a
deliberate mutant tells them apart.

**[E] `M1`–`M8` implemented as `TestMutationHarnessM1ThroughM8`
(`internal/oracle/mutation_harness_test.go`), run against real PostgreSQL. Running
it corrected the design's own §8 mapping: `M3` and `M7` were originally mapped to
`A1`, but both are "exclude every row" defects, and `A1` is a row-count check that
cannot distinguish that from correct filtering — only `A4`'s content comparison
catches them. `M8` was added specifically to confirm `A1` still has coverage `A4`
does not: on a single-tenant probe, `USING (true)` is indistinguishable from a
correct policy to `A4`, but `A1` fires regardless, because it never inspects
content. The corrected mapping and the general property behind it are recorded in
§8. AC-1 through AC-4 below are written against that corrected mapping, not the
original one.**

**ACCEPTANCE CRITERIA**

- **AC-1** *Given* each mutant `M1`–`M8`, *When* the harness runs, *Then* the mutant
  is caught **by the gate it is mapped to** in the design's §8 table (as corrected).

- **AC-2** *Given* a mutant that also fires a gate **other than** its mapped one,
  *When* the harness evaluates the result, *Then* this is **not** a failure — several
  mutants are legitimately caught by more than one gate (e.g. `BYPASSRLS` trips both
  `A1` and `A3`), and that overlap is defence-in-depth, not noise. The failure
  condition is specifically the mapped gate staying **silent** while a different gate
  fires instead.

- **AC-3** *Given* a mutant caught by a gate other than its mapped one, **with the
  mapped gate silent**, *When* the harness evaluates the result, *Then* it is reported
  as a **finding**, not a pass — the mapping is wrong or the mapped gate cannot fire.

- **AC-4** *Given* a mutant caught by no gate, *When* the harness completes, *Then*
  the run fails and the blind spot is recorded with a backlog ID.

- **AC-5** *Given* the harness itself, *When* it was run with `A1`'s check disabled,
  *Then* `M8` failed as a blind spot and `M2` failed as a wrong-gate finding — proving
  the harness can fail rather than passing vacuously. **[E]**

- **AC-6** **[E] Closed — proven executed, not only written.** *Given* the harness
  itself, *When* CI runs, *Then* it is merge-blocking.

  `.github/workflows/ci.yml` now exists: three jobs — `build-vet-fmt`,
  `test-race-coverage-harness` (Postgres service, `go test -race`, coverage
  floor, the `M1`–`M8` harness), `workflow-lint`. The harness job runs as an
  ordinary step with a non-zero exit failing the job like any other; nothing
  marks it "advisory."

  **Actually executed, not merely written — a distinction this document has
  insisted on throughout, applied to itself.** At the time this was written,
  the repository had no git history and no remote, so GitHub had never
  run this workflow. That gap was not left unaddressed: `actionlint` and `act`
  (both real, independent tools, `go install`ed fresh in this session — not
  hand-rolled substitutes) were used to lint and then **actually execute** the
  workflow locally against real Docker containers, including a genuine
  PostgreSQL service container.

  **The core requirement — CI must go red — was proven, not asserted.** `A1`'s
  inequality check was disabled (the same mutation that turns `M8` into a
  blind spot) and the full pipeline was re-run end to end: the job **failed**,
  naming the exact broken tests (`M2`, `M8`, and the standalone `A1` abort
  tests) via `scripts/assert_tests_ran.py`'s output. The change was reverted
  and the pipeline **passed green** again — both directions demonstrated, not
  assumed.

  **The "silent skip" failure mode named by this AC's own rationale was also
  reproduced.** The Postgres `services:` block was removed to simulate the
  exact mistake this AC exists to catch — no database, tests skip, job goes
  green having proven nothing. Instead: the `wait for postgres` step timed out
  and failed the job outright, because it never got as far as running any Go
  test at all. Separately, `scripts/assert_tests_ran.py` provides the second
  layer: if Postgres were merely *slow* rather than absent and tests ran and
  skipped anyway, it fails the job by name-checking that all eight `M1`–`M8`
  subtests reported `pass`, not skip.

  **Four real defects were found and fixed by actually running the pipeline,
  each one a thing that would have passed a syntax check but failed for real:**
  `pg_isready` is absent from at least one real runner image (replaced with a
  pure-bash `/dev/tcp` check); `actionlint`'s default discovery requires a git
  repository to locate `.github/workflows/`, which this checkout does not
  reliably have (fixed by passing the file path explicitly); `PyYAML` is not
  preinstalled on the runner (added an explicit install step); and that
  runner's system Python is PEP 668 "externally managed" and refuses a plain
  `pip install` (fixed with `--break-system-packages`, safe specifically
  because CI runners are ephemeral). None of these would have been caught by
  writing the YAML carefully and trusting it — only by running it.

  **`[R]` resolved to `[E]`: GitHub's hosted runner has now actually run
  this workflow, all three jobs, and gone fully green.** This repository's
  first push (`ac01136`) surfaced two real local-vs-hosted divergences,
  fixed in commit `fdccf50` (`TGD-BL-44`, §7.7) — a genuine `gofmt` defect
  and two genuine `shellcheck`-via-`actionlint` findings, neither caught by
  any prior local or `act`-based session because `shellcheck` had never
  been installed in any of them. The next push, same commit `fdccf50`, ran
  as GitHub Actions run `33716947618` (`https://github.com/Sour16o4/tenantguard/actions/runs/33716947618`,
  2026-09-03) and **all three jobs succeeded**, confirmed from the run's
  own job metadata: `build, vet, gofmt`, `lint this workflow file`, and —
  for the first time — `test (-race), coverage floor, M1-M8 mutation
  harness`, on GitHub's real hosted infrastructure with a real Postgres
  service container, not `act`'s local Docker. That third job's own
  output, `scripts/assert_tests_ran.py`: **298 tests passed, 0 skipped,
  all 8 mutation-harness subtests confirmed present** — the exact
  by-name check this AC's own mechanism requires (the script only reaches
  that summary line after individually matching all eight `M1`–`M8`
  substrings against passed tests; it enumerates them by name only on the
  failure path) — and coverage 70.0%, above the 60% floor. This closes the
  gap this AC's evidence previously stated plainly: GitHub itself has now
  run this workflow, and it passed, not merely `act`'s local approximation
  of it.

  **What remains explicitly `[R]`, not folded into this closure:** the
  branch-protection **required status check** setting — the literal
  GitHub mechanism that makes a green run merge-blocking rather than
  merely informative — is a repository administration setting requiring
  push access, and remains **unconfigured**. The workflow is *structured*
  to support it (three independent jobs, a `needs:` dependency, a
  non-zero exit failing the job like any other) and has now been proven
  to actually run and pass on GitHub's own infrastructure, but nothing yet
  stops a pull request from merging while this pipeline is red — that is
  a distinct, still-open gap, not resolved by a workflow file existing and
  passing.

---

### Epic E-7 — Tier 2 Guardrail

---

#### `TGD-US-12` — Fail-closed driver wrapper
**[Epic: E-7] [Points: 8] [Priority: S]**

**STORY**
As **Priya**, I want unscoped queries to error in my own application rather than
return another tenant's rows, so that isolation is enforced and not merely audited.

**DESCRIPTION**
Tier 2 requires application changes — tenant context propagation and a driver
wrapper — and is documented as such. It is also the only tier that can detect `L5`,
which is the substantive argument for its existence rather than a convenience.

**ACCEPTANCE CRITERIA**

- **AC-1** *Given* a wrapped connection and a query on a scoped table with no tenant
  predicate, *When* it executes, *Then* it returns an error and no rows.
  **[E]** `TestDB_BlocksUnscopedQuery`/`TestDB_BlocksUnscopedExec` (§7.5) —
  the latter also confirms directly, by re-querying, that the blocked write
  never executed.

- **AC-2** *Given* a query whose tenant predicate names a tenant other than the
  request context's, *When* it executes, *Then* it returns an error (`L5` detection).
  **[E]** `TestDB_BlocksWrongTenant` (§7.5) — a real wrapped connection, `L5`'s
  exact shape (query claims "acme", context says "globex"), against real
  PostgreSQL. Also proven at the corpus level: `TestCorpus_Tier2ExactSetEquality`'s
  `L5` case is the one fixture design §7.4 predicted would differ by tier, and it does.

- **AC-3** *Given* the wrapper is active and context is absent, *When* a query on a
  scoped table runs, *Then* it fails closed — absence of context is never treated as
  permission.
  **[E]** `TestDB_BlocksWhenContextTenantAbsent`, `TestTenantFromContext_EmptyStringTreatedAsAbsent`
  (an empty-but-present tenant must not defeat the rule either), and
  `TestDB_BlockedQueryNeverReachesDatabase` (§7.5) — proven against an
  unreachable database address, so a dial attempt would surface as a
  connection error rather than the guardrail's own sentinel error if the
  check were not actually gating execution.

- **AC-4** *Given* the Tier 2 corpus run, *When* verdicts are compared, *Then* they
  match the Tier 2 expected set exactly (`TGD-US-06` AC-5).
  **[E]** Run for the first time this session (§7.5): `TestCorpus_Tier2ExactSetEquality`
  asserts against `checkTenantExpected` — `CheckTenant`'s actual, understood,
  argued-correct verdict per fixture — which agrees with design §7.4's
  original prediction at 20 of 23 testable fixtures and diverges, each for a
  specific named reason logged on every run, at 3 (`L3`, `L7`, `L10`).
  Reported as design findings (§7.5), not reconciled by adjusting `fx.tier2`
  or by building a second enforcement mechanism to force a match. (A fourth,
  `U2`, diverged too until `TGD-BL-43` (§7.6) found it was a real fail-open
  bypass, not a labelling gap, and fixed it — proven red-before-green plus
  a mutation, same session.)

---
## 7. Non-Functional Requirements

**Baseline discipline.** A row tagged **[U]** is a *proposal awaiting measurement*,
not a requirement. It must not gate a merge, block a release, or be quoted in
`README.md` or `docs/BENCHMARKS.md` until the named story has baselined it. Only
`TGD-NFR-04` (measured on a real suite by `TGD-BL-11`) and `TGD-NFR-12` (measured by
Gate 0) are the only rows carrying numbers that were actually run.

| ID | Requirement | Target | Verification | Status |
|----|-------------|--------|--------------|--------|
| `TGD-NFR-01` | Tier 0 time from clone to ranked output | < 5 min | Timed test | **[E]** Measured, §7.4: `19.5s` clone-to-output against a fresh `coder/coder` clone (`git clone --depth 1` + `tenantguard triage`), dominated entirely by the clone itself — the scan phase alone is `~2.5s` against `coder`'s ~2,800 scanned files. |
| `TGD-NFR-02` | Tier 1 time to first finding on an unfamiliar repository | **PARTIALLY BASELINED** | Timed test | **[E]** for the tool-mechanical setup phases only: `infer` + `verify` together measured at **under 1 second** (0.059s + 0.889s) against a real 132-relation/22-scoped-table schema, §7.9. **[U]** for the full metric — human policy-review time and the live-capture-to-first-`LEAK` phase remain unmeasured; the 90-minute design figure stays **withdrawn**. §7.9 records the measurement protocol a fair full baseline requires (`TGD-BL-45`, a genuinely unfamiliar target, filed). No single number is in force for the full metric. |
| `TGD-NFR-03` | Unattributable rate ceiling | **Stored value: `122.0/378.0` (≈32.28%)**, against the `row_level_touching_real_app_sql` denominator (`TGD-BL-33`), corrected by `TGD-BL-39` to exclude vacuous `SAFE`s (§9.7), measured on a differ with `TGD-BL-42` fixed (§7.3) — the third baseline set, and the first RAISE. | Integration test | **[E]** Enforced and re-provable. History: `130/611` (`TGD-BL-06`) → `131/664` (`TGD-BL-38`, down) → **`122/378`** (`TGD-BL-42`, up — §7.3 in full, and `ceiling.go`'s own comment, for why the raise does not violate `TGD-US-07` AC-2's down-only ratchet: every prior number was measured by a differ later found to silently discard real tenant values on cast `INSERT` parameters, `$N::type` — `sqlc`'s default codegen shape — so those were never a trustworthy floor). §7.1/§7.2 (superseded) record the intermediate, contaminated measurements (`25.24%`/`131/519`, then `31.41%`/`468/147`) for provenance; neither was ever set as the stored constant. A real `audit` run against the exact capture and binary this baseline was measured on exits 0. |
| `TGD-NFR-04` | **`Bind` resolution rate** — a captured `Bind` is matched to the SQL of an open statement on its own connection | 100% | Protocol test + real-suite run | **[E]** `TGD-BL-11`: **1,314/1,314** across 163 connections of `coder`'s `dbpurge` suite, plus 2,266 simple-protocol queries. **[E]** ported implementation: 821/821 against live `pgx` and `lib/pq`. **Scope caveat:** `coder` exercises **no statement caching** (Parse:Bind 1:1) and **only unnamed statements**, so the eviction and named-statement paths rest on synthetic evidence alone. |
| `TGD-NFR-17` | **Parameter value recovery rate** — a captured parameter is readable as a *value*, not merely as bytes | Report; threshold deferred | Protocol test + real-driver run | **[E] before backend capture:** 861/961 (89.6%) known — all text; **100/961 (10.4%) binary and undecodable**. **[E] after backend capture (`TGD-BL-14`):** **961/961 (100%)** — the same workload set, all 100 binary parameters decoded from `ParameterDescription` OIDs. **Distinct from `TGD-NFR-04`; never report the two as one figure.** `coder`'s 1,314 remain **[R]** for this metric: the spike recorded no format codes, so **that run's text/binary breakdown is unrecoverable**. **Residual, unmeasured:** the decoder covers a fixed type set; `zitadel` is untested and may bind types with no decoder (`numeric`, `jsonb`, arrays), which stay undecodable by design. **[E] `Describe` FIFO correlation, validated against a real driver:** `pgx`'s `QueryExecModeDescribeExec` (never caches, always describes explicitly) was run through the built binary against real PostgreSQL with two distinct queries back to back; both `ParameterDescription` replies were attributed to the correct statement, with no cross-contamination. Previously this logic was proven only by synthetic mutation tests — an earlier attempt with `QueryExecModeExec` sent parameters as text and never triggered `Describe` at all, which was reported rather than left implied. **A real bug surfaced by this run**: a successfully decoded binary parameter carried a stale `unknown_reason` alongside `value_known:true` — `parseBind`'s placeholder reason was never cleared on successful decode. Fixed, red/green proven, mutation-caught. **Still unproven:** only one `Describe`/`Bind` ordering has been exercised live; out-of-order or interleaved pipelining remains validated by the synthetic tests alone (`[R]`). |
| `TGD-NFR-05` | Proxy added latency per query | **UNBASELINED** | Benchmark | **[U]** — never measured; Gate 0 measured correctness, not overhead. |
| `TGD-NFR-06` | Memory during a 100k-query capture | **UNBASELINED** | Benchmark | **[U]** — statement maps grow per connection; unmeasured. |
| `TGD-NFR-07` | Writes to the target application's database | **Zero** | Row-count diff test | Absolute (§3.3) |
| `TGD-NFR-08` | A findings report is emitted only when all four oracle self-checks passed | **Absolute** | Mutation harness | Absolute |
| `TGD-NFR-09` | Fixture corpus exact-set equality, per tier, across all three verdict classes | **Absolute** | Corpus test | Absolute |
| `TGD-NFR-10` | Every mutant `M1`–`M8` caught by its **mapped** gate | **Absolute** | Mutation harness | **[E]** all 8 pass against real PostgreSQL. `M3`/`M7` corrected from `A1` to `A4` during this run — see design §8 — and `M8` added to confirm `A1` retains independent coverage `A4` does not subsume. Previously proven only locally/under `act`; now also confirmed on GitHub's own hosted runner, by name, in GitHub Actions run `33716947618` (commit `fdccf50`) — see `TGD-US-11` AC-6 and §7.7 (`TGD-BL-44`). |
| `TGD-NFR-11` | Probe database dropped on every exit path including panic and `SIGINT` | **Absolute** | Integration test | Absolute |
| `TGD-NFR-12` | Behaviour against a TLS-requiring target | Distinct exit code + actionable message | Integration test | **[E]** — `sslmode=require` fails at connect today; the message is the work. |
| `TGD-NFR-13` | Coverage on `internal/oracle` and `internal/differ` | ≥ 90% | CI gate | — |
| `TGD-NFR-14` | Overall `internal/` coverage | ≥ 75% | CI gate | — |
| `TGD-NFR-15` | Race detector clean across the full suite | **Absolute** | `go test -race` | Absolute |
| `TGD-NFR-16` | Supported Go versions | Current + 1 prior minor | CI matrix | — |

### 7.1 `TGD-NFR-03`'s baseline, in full (`TGD-BL-06`, ratcheted by `TGD-BL-38`)

**⚠ The stored constant this section discusses (`131.0/664.0`) and the
`25.24%`/`131/519` figure below are both superseded — see §7.3.** Kept
verbatim as the historical record of how the ratchet-down and the
denominator correction were reasoned about at the time; the table row above
now states the current (`122.0/378.0`) value.

The table row above stated the number as of `TGD-BL-38`; this records what
it was measured on, exactly, so a future re-baseline has something concrete
to compare against rather than a bare percentage.

**Exact value:** `unattributableCeilingRate = 131.0 / 664.0` (`≈ 0.19728915662650603`),
stored as that literal fraction in `cmd/tenantguard/ceiling.go`, not a rounded
decimal — the source line itself carries the provenance. **Ratchet history:**
originally baselined at `130.0 / 611.0` (`≈ 0.21276595744680850`, `TGD-BL-06`,
2026-09-01); lowered to the current value by `TGD-BL-38` (2026-09-02) once
`TGD-BL-37` made all 22 declared `coder` tables row-level-provable, up from 13.

**Denominator:** `row_level_touching_real_app_sql` (one of `TGD-BL-33`'s four
tool-computed candidates) — real application SQL (cursor-protocol and session
bookkeeping excluded) touching at least one table the oracle has proven at the
row level (`A1` and `A4` both passed for it). Chosen over the other three
because it is the only one isolating "attribution was possible in principle
and still failed" from testing-harness noise, traffic about tables the policy
never declared tenant-scoped, and coverage gaps in tables the tool was never
shown it can see — none of which say anything about whether the attribution
logic itself is trustworthy, which is what `TGD-FR-08` exists to gate.

**Measured on:** `coder/coder`'s `coderd/database/dbpurge` test suite, run
through `tenantguard capture` (`TGD-BL-31`'s capture: 1,420 Binds, 171
connections, `coder_capture_events.jsonl` — the SAME capture both the original
baseline and this ratchet were measured against; only the policy and the
target database's seeded state changed between the two measurements), audited
with the full 22-table policy `infer` produced against the same
freshly-migrated `coder_target` database `TGD-BL-30`–`-32`/`-37` used. At the
`TGD-BL-38` measurement, all 22 of 22 tables were row-level-proven (13 sampled,
9 fixture-seeded — `TGD-BL-37`), up from 13 of 22 at the original `TGD-BL-06`
measurement. 131 of 664 queries in the denominator came back `UNATTRIBUTABLE`
(previously 130 of 611).

**Why the rate moved, stated plainly rather than read as improved attribution
— this is the load-bearing caveat on this specific ratchet.** The numerator
barely changed: 130 → 131, one additional instance of the same
`TGD-BL-35`-shaped duplicate-key re-execution collision (filed, not fixed).
The denominator grew by 53 (611 → 664) because 9 more tables became
row-level-provable and their real captured traffic joined the measured
population — but of those 9 newly-provable tables, real traffic in this
specific capture only ever touched **one**, `workspace_agent_stats`; the
other 8 (`chat_organization_model_overrides`, `chat_user_model_overrides`,
`groups`, `jfrog_xray_scans`, `mcp_server_configs`,
`workspace_agent_port_share`, `workspace_app_stats`,
`workspace_app_statuses`) contributed zero queries to either side of the
fraction — they only widened which tables the ceiling's population is
permitted to include, because `dbpurge` never calls the code paths that
touch them. **So this is a real, reproducible, tool-reported number — not a
bad run — but it is coverage-driven, not evidence that
`ExtractTenant`/the differ's attribution logic itself got better at reading
SQL.** The practical consequence: a future run whose policy or captured
traffic loses those 9 tables again (a reverted fixture, a policy scoped back
down, a target lacking the feature) would be held to a ceiling this narrower
population never actually earned. A breach shaped like that should be read
as "coverage regressed," not "attribution regressed" — the two are
distinguishable by checking `tables_row_level`'s length and `seed_source`
mix (`TGD-BL-36`) in the report that produced the breach, not by the exit
code alone.

**Ratchet rule (`TGD-US-07` AC-2):** this value may only ever be **lowered** by
a later measurement. Raising it is never a config edit — there is no
`--ceiling` flag to edit; `unattributableCeilingRate` is a source constant, and
changing it requires a code change carrying a recorded backlog entry that
states why a higher rate is being accepted, the same discipline this whole
document applies to every other reversal. A *lowering*, as `TGD-BL-38` did, is
always permitted by the rule itself, but is still recorded here with its own
backlog entry and its own re-proof of the gate (§7.1 above; `TGD-BL-38`) rather
than being treated as a free action — the ratchet direction being unrestricted
is not the same as the change being unexamined.

**What this number is not, stated plainly rather than left to be assumed:**
**≈19.73% is a `coder`-specific baseline from one capture of one target's test
suite run once.** It is not a general claim about this tool's attribution
quality, and it is not a claim that `coder`'s own code quality sits at this
number either — plenty of it is `coder`-shape-specific (its `sqlc`-generated
SQL, its particular mix of query forms) rather than a property of extraction
logic that would transfer to a different target untouched. **A second target
measuring a meaningfully different rate does not by itself mean this baseline
is wrong** — it would mean the two targets differ, which is expected, not an
error to reconcile toward one number. What would justify *revising* this
baseline: a second target's own measurement, taken the same way (a real
capture, the same denominator, reported by the tool itself, not hand-derived)
— never a single bad run against `coder` (a flaky test suite run, a
differently-migrated database, a smaller or larger capture) and never a desire
to make a specific run pass. A bad run is noise around this number; a second
target is a second data point, and only the latter is evidence the baseline
itself needs to move. The same rule applies to a *lowering* driven by
broadened coverage on the same target, as `TGD-BL-38` was: real and
tool-reported, but not to be read as "the tool got better at attribution,"
for the reasons stated above.

**`TGD-BL-39` addendum — the denominator itself was corrected, and the
stored value is now known to understate the true rate.** §9.7 found that a
`SAFE` where both comparison legs matched/affected zero rows (`∅=∅`) proves
nothing about isolation, yet was being counted identically to a real,
informative `SAFE` in `attributed = Safe + Leak` — inflating the
denominator every rate above rests on. `cmd/tenantguard`'s
`unattributableRateBreakdown` was corrected to `attributed = (Safe -
VacuousSafe) + Leak`, and the exact capture `TGD-BL-38` measured `131/664`
against was re-run on the corrected binary: **131/519 = 25.24%.** That
capture, unchanged, now measures a rate above the recorded ceiling — not
because anything got worse, but because the recorded ceiling was computed
with a denominator this session's own analysis concluded was wrong.

**25.24% is itself a lower bound, not the corrected number — and, as of a
later session, is now known to be stale for a second reason.** Two gaps
understated it further at the time it was measured: `TGD-BL-40` (write-path
vacuous detection — `diffWrite` could not yet tell "affected zero rows" from
"affected some," so at least 44 more `SAFE`s known-vacuous by direct
inspection stayed counted as attributed) and `TGD-BL-41` (a third
`isWriteStatement` gap — CTE-wrapped writes, `WITH ... AS (...) DELETE/UPDATE`,
routed through the read-path comparison instead of `diffWrite`; 90 of the 145
detected-vacuous `SAFE`s in that re-run were this misrouting, not confirmed
to be irreducibly vacuous once routed correctly). **Both are now fixed at
the code level** (`TGD-BL-41` first, as a correctness bug — a write compared
as a read is the wrong verdict mechanism, not just a wrong flag — then
`TGD-BL-40`), proven by unit tests plus DB-backed tests written in the
existing style, but **neither has been run against real Postgres or real
`coder` traffic**: the session that implemented them had no live Postgres,
no `coder` checkout, and no saved capture file available. 25.24% therefore
remains the last *actually measured* number; the true corrected rate is
still open, now for a documented infrastructure reason rather than an
unbuilt-gap reason.

**The stored constant (`131.0/664.0`) was deliberately NOT changed by
either session.** `TGD-US-07` AC-2's ratchet is down-only; this correction
would need to move the value UP, which is never a routine action under that
rule — it requires the same "recorded decision naming why a higher rate is
acceptable" any raise does. The first session's instructions were to
implement the vacuous-verdict decision and report the consequence, not to
also make that raise decision unilaterally; the second session's
instructions were to fix `TGD-BL-40`/`-41` and only then set a third
baseline once, against the fully-corrected denominator — but "fully
corrected" requires an actual re-run this environment could not perform, so
that baseline still has not been set. **Practical effect, stated plainly:**
a real `audit` run against the `TGD-BL-38` capture still correctly exits 3
(the ceiling fires) against the recorded baseline — this is the tool being
more honest with the same recorded baseline, not a new failure in the tool.
Still recommended, still not done: obtain a real Postgres instance, a
`coder` checkout, and a capture file; re-run; confirm the 90 CTE-misrouted
`SAFE`s reclassify and the 44 write-path vacuous writes get flagged; then
set a third baseline once against that fully-corrected number, rather than
baselining twice in quick succession against a partially-corrected one —
this would be the **second re-baselining event in two sessions**, and unlike
the first (a real, tool-reported down-ratchet from broadened coverage),
this one is a measurement-methodology correction and should be named as
such, not read as either progress or regression.

### 7.2 ⚠ SUPERSEDED BY §7.3 — this session's re-run, `TGD-BL-42`'s discovery, and why the third baseline was NOT set at the time this section was written

**Every number in this section was measured by a differ later found, in this
same session, to be wrong (`TGD-BL-42`, filed below, fixed and re-run in
§7.3).** `extractFromInsert`'s positional-VALUES matcher silently discarded
the tenant value on any cast parameter (`$N::type` — `sqlc`'s default
codegen shape, and the shape most of `coder`'s own `INSERT`s use), turning
real, correctly-captured writes into false `LEAK`s and false
`UNATTRIBUTABLE`s. The 96 `LEAK` count, the 468/147 = 31.41% rate, and the
vacuous/CTE breakdown below are all downstream of that bug and are kept
here only as the historical record of how the defect was found — **do not
cite any number in this section as a current or trustworthy measurement.**
§7.3 has the corrected fix, the proof it is load-bearing, and the re-run
against a fixed binary.

This session had what the previous two lacked: a live Postgres (Docker,
brought up after the operator's docker-group grant took effect), a fresh
`coder/coder` clone, and the ability to run the whole pipeline end to end.
It executed the full recovery plan §7.1 laid out — with one deliberate
substitution and one hard stop, both recorded here rather than left for a
reader to infer.

**DB-backed test suite, run with `TGD_TEST_DSN` set.** All 86 tests
previously reported `SKIP` (across `internal/oracle`, `internal/differ`,
`internal/capture`'s indirect dependents, and `cmd/tenantguard`) ran and
passed, including the three `TGD-BL-40` write-path vacuous tests
(`TestDiff_WriteVacuousWhenBothLegsAffectZeroRows`,
`TestDiff_WriteVacuousWithReturningClauseHandledCorrectly`,
`TestDiff_WriteNotVacuousWhenRowsAffected`) and the `TGD-BL-41` CTE-routing
test (`TestDiff_CTEWrappedWriteRoutesToDiffWrite`). Zero failures, zero
remaining skips, full suite (`go test ./...` with the DSN set): **ok** on
all five packages.

**The `lib/pq` claim, proven rather than reasoned about.** A standalone
program against the same real Postgres instance ran `ExecContext` for an
`UPDATE ... WHERE tenant_id = $1` and the same statement with `RETURNING
id` appended, at both zero-row and multi-row affected counts, plus a
`DELETE ... RETURNING`. In every case `sql.Result.RowsAffected()` matched
the true affected-row count exactly (3, 0, 2, 0, 3) — confirming
`diffWrite`'s comment (`internal/differ/differ.go:213`) that `lib/pq`
drains any `RETURNING` rows internally and still reports the correct
command-complete count, not a claim taken on faith.

**Capture: a fresh one, not the saved `TGD-BL-31` file — because no saved
file exists in this environment.** `coder_capture_events.jsonl` and the
`coder` checkout that `TGD-BL-31`/`-37`/`-38` used were session-local
artifacts of an earlier sandbox; neither survived into this one (confirmed
by search — the closest match was an unrelated `TGD-BL-11`-era capture from
2026-08-29, wrong shape and wrong date to be the same file). This session
therefore did what §7.1's own recovery plan called for: cloned `coder/coder`
fresh (`main` at clone time), migrated a new `coder_target` database,
proxied `coderd/database/dbpurge` through `tenantguard capture`, and
`infer`red a policy against the freshly-migrated schema — 22 scoped tables,
identical to `TGD-BL-37`'s set. The four `dbpurge` test failures
(`TestDeleteExpiredAPIKeys`, `TestDeleteOldBoundarySessions`,
`TestDeleteOldAuditLogs`, `TestDeleteOldWorkspaceAgentLogsRetention`) again
reproduced, consistent with `C-7`'s shared-database interference, not a new
problem. **All 22 tables were made row-level-provable on the first attempt**
(13 sampled, 9 fixture-seeded — `chat_organization_model_overrides`,
`chat_user_model_overrides`, `groups`, `jfrog_xray_scans`,
`mcp_server_configs`, `workspace_agent_port_share`, `workspace_agent_stats`,
`workspace_app_stats`, `workspace_app_statuses`), using the same
FK-checks-disabled fixture mechanism `TGD-BL-37` relied on (`probe.go`'s
documented behaviour: FKs are enforced for sampled rows by construction, and
deliberately disabled for the fixture/synthetic paths, so a fixture's
non-tenant foreign-key-shaped columns need only be type-correct, not
real). This is a **different capture from a different point in `coder`'s
history than `TGD-BL-38`'s** — its raw counts are not comparable line-for-
line to the ones recorded in `ceiling.go`, and are not claimed to be; it is
a new, independent measurement of the same corrected code, which is what
`TGD-BL-42` below turned out to require anyway.

**Audit against the fresh capture, with `TGD-BL-40`/`-41` both live in the
binary under test.** Four denominators (`unattributable_rate_by_denominator`):

| label | denominator | unattributable | rate |
|---|---|---|---|
| `all_captured_queries` | 4865 | 4301 | 88.41% |
| `real_app_sql_any_table` | 2158 | 1594 | 73.86% |
| `real_app_sql_touching_any_declared_table` | 468 | 147 | 31.41% |
| `row_level_touching_real_app_sql` | 468 | 147 | 31.41% |

The last two are equal in this run only because every declared table is now
row-level-proven (no `structural_only` remainder to separate them) —
coincidence of this capture's coverage, not a rule.

**Vacuous, both paths, and the CTE reclassification.** `counts.vacuous_safe
= 243` (already `TGD-BL-39`-corrected out of `attributed`). Classifying the
underlying SQL text (mirroring, not calling, `isWriteStatement`'s own
CTE-skip parser): 296 captured queries are `WITH ...`-wrapped writes: 198
`UNATTRIBUTABLE`, 98 `SAFE`, 0 `LEAK`; of the 98 `SAFE`, 93 are flagged
`Vacuous` — i.e. correctly routed to `diffWrite` and correctly found to have
affected zero rows on both legs, which is what `TGD-BL-41`'s routing fix and
`TGD-BL-40`'s detection fix together are supposed to produce, and did. Of
the 243 total vacuous `SAFE`s: 93 are these CTE-wrapped writes, 86 are
plain (non-CTE) `INSERT`/`UPDATE`/`DELETE`, and 64 are read-shaped `SELECT`
comparisons that matched nothing on both legs (the original, pre-`TGD-BL-40`
vacuous case). This capture has no directly-comparable "90 CTE-misrouted"
figure from `TGD-BL-38`'s own capture to reclassify against — that capture
no longer exists to re-run — but the mechanism it was reporting on is
exercised here, on fresh traffic, and behaves as designed.

**`TGD-BL-42` — new defect, filed here, and why the third baseline is not
being set this session.** Auditing this fresh capture surfaced 96 `LEAK`
verdicts, a population size worth inspecting before trusting the rate above.
A sample showed real, unambiguous single-tenant `INSERT`s — e.g.
`InsertChatModelConfig`, binding `organization_id` at `$11::uuid` in the
`VALUES` list with the real organization UUID plainly visible in the
captured params — being reported as `LEAK` with reason `... rejected by
row-level security for tenant ""`: an **empty tenant**, not the real one
visible three lines above it in the same JSON. Traced to
`internal/differ/extract.go`: `extractFromInsert`'s per-column `VALUES`
matcher (`extract.go:~300`) runs `paramRef.FindStringSubmatch(rhs)` against
the *raw, untrimmed-of-cast* `VALUES` entry, and `paramRef` is anchored
(`^\$(\d+)$`, `extract.go:53`) — so an entry that is textually `$11` matches,
but `$11::uuid` does not, because of the trailing cast. On no match,
`extractFromInsert` falls through to `AttrNoPredicate` (`extract.go:326`) —
the *same* "no tenant claimed" bucket a genuinely unscoped `INSERT` would
produce — silently discarding a real, present tenant value rather than
reporting `Unattributable` (which at least would not fabricate a false
`LEAK`). The WHERE-clause path (`tenantCompareRegex`, used for `SELECT`/
`UPDATE`/`DELETE` predicates) does **not** share this bug: its right-hand-
side alternation `(\$\d+|\w+|\([^)]*)` matches only the `$N` prefix of
`$1::uuid` and stops there, leaving the cast unconsumed and outside the
match — `paramRef` is then applied to the already-cast-free `"$1"` and
succeeds. The defect is specific to `extractFromInsert`'s positional-VALUES
path.

Reproduced minimally and in isolation, independent of `coder` or this
capture: `ExtractTenant` on `INSERT INTO widgets (name, organization_id)
VALUES ($1::text, $2::uuid)` with both parameters `Known` returns
`AttrNoPredicate` (empty value), not `AttrResolved` with the real bound
UUID. `$N::type` on a positional `INSERT` value is not a `coder` idiosyncrasy
— it is `sqlc`'s (and more generally, most Go Postgres codegen's) default
output shape, visible in essentially every `INSERT` in this capture's
sample, `InsertWorkspaceAgentStats` and `InsertChatModelConfig` included —
so this is not a narrow edge case; it is a **systemic hole in the INSERT
positional-value path** that this session's LEAK sample happened to make
visible, not something specific to the 9 newly fixture-seeded tables or to
this capture.

**Consequence for the numbers above and for baselining:** every `LEAK` (and
plausibly some fraction of the `SAFE`/vacuous write counts too, wherever a
cast-parameterized `INSERT`'s tenant column happened to still compare
correctly by accident) in this run's write-path population is suspect until
this is fixed — the 96-`LEAK` figure is not trustworthy evidence of real
cross-tenant leakage, and the 468/147 = 31.41% `row_level_touching_
real_app_sql` rate is not a clean, fully-corrected number either, since an
unknown share of its `147` unattributable-or-misclassified population is
this bug's artifact rather than genuine attribution failure or genuine risk.
**No third baseline is being set this session.** Setting one now would
memorialise a rate this session's own investigation just showed is
contaminated by a newly-discovered tool defect — exactly the "don't
baseline against a number you already know is wrong" discipline `TGD-BL-39`
was named for. This is filed, not fixed: fixing `extractFromInsert` to
strip (or tolerantly match through) a trailing `::type` cast the same way
the WHERE-clause path already does, then re-running this exact capture, is
what "fully corrected" now requires before a third baseline is defensible —
one more precondition than `TGD-BL-40`/`-41` alone turned out to be.

### 7.3 `TGD-BL-42` fixed, proven, and the third baseline set

**Also superseded, historically, and why it is quoted rather than deleted:**
the `TGD-BL-32` and `TGD-BL-37` entries in `docs/superpowers/specs/2026-08-29-
tenantguard-design.md` quote `{"safe":343,"leak":98,"unattributable":4265}`
and `{"safe":427,"leak":106,"unattributable":4173}` respectively — both LEAK
counts (98, 106) from the same pre-`TGD-BL-42` differ, predating even this
session. Both entries in that document now carry an explicit ⚠ SUPERSEDED
note pointing here rather than being edited to remove the numbers — the
figures are historically real (that is what the tool reported, on that
capture, on that binary, at that time) but not trustworthy as LEAK evidence.
`TGD-BL-31`'s `{"safe":70,"leak":0,"unattributable":4636}` is unaffected: its
one real verdict came from the write-path RLS-acceptance check on a query
whose only bound parameter was uncast, never reaching the buggy code path.

**The fix.** `internal/differ/extract.go`: `extractFromInsert`'s per-column
`VALUES` matcher used `paramRef` (`^\$(\d+)$`), anchored at both ends —
matching a bare `$11` but not `$11::uuid`. Replaced the call site (and the
WHERE-clause path's equivalent call site, for uniformity and to add
`CAST(...)` coverage there too — see below) with a new `resolveParamRef`,
backed by two regexes: `paramRefCastChain` (`^\$(\d+)(?:\s*::\s*<ident>
(?:\s+<ident>)*(?:\s*\[\s*\])?)*$` — `$N` followed by zero or more `::type`
casts, each tolerating whitespace around `::`, a multi-word type name
`timestamp with time zone`/`double precision`, and an array suffix `[]`,
so chained casts like `$1::uuid::text` also resolve) and
`paramRefCastFunc` (`CAST($N AS type)`, case-insensitive, same type-name
tolerance). `tenantCompareRegex`'s right-hand-side alternation gained an
explicit `CAST\s*\([^)]*\)` alternative before its `\w+` fallback, so a
`CAST(...)` expression is captured whole instead of truncating to the bare
word "CAST" — the WHERE-clause path's `$N::type` handling needed no change
at all (see below).

**Checked the same class of anchoring elsewhere, empirically, not by
inspection alone — per instruction:**
- **Whitespace around `::`** (`$1 :: uuid`, seen in real `coder` traffic
  alongside the compact `$1::uuid` form): now handled in both paths, proven
  by `TestExtractTenant_InsertSpacedCastResolves` and
  `TestExtractTenant_WhereClauseSpacedCastRHSResolves`.
- **Array casts** (`$1::text[]`, `$10::bigint[]`, real shapes in this
  capture): handled, proven by `TestExtractTenant_InsertArrayCastResolves`
  and `TestExtractTenant_WhereClauseArrayCastRHSResolves`.
- **Multi-word type names** (`timestamp with time zone`, `double precision`,
  real shapes in this capture, on sibling columns): handled, proven by
  `TestExtractTenant_InsertMultiWordTypeCastResolves`.
- **Nested/chained casts** (`$1::uuid::text`): handled, proven by
  `TestExtractTenant_InsertNestedCastResolves` — not observed in this
  capture, a legal PostgreSQL shape a future one could contain.
- **`CAST($N AS type)`** — the SQL-standard alternative spelling: one
  occurrence in this capture's raw traffic (`CAST($1 AS integer)`, not on a
  tenant column), but the same defect class, so fixed in both paths, proven
  by `TestExtractTenant_InsertCastFuncFormResolves` and
  `TestExtractTenant_WhereClauseCastFuncRHSResolves`.
- **Whether the WHERE-clause path was genuinely clean or just untested:**
  genuinely clean, but untested — confirmed empirically, not by re-reading
  the regex. `tenantCompareRegex`'s right-hand-side alternation
  `(\$\d+|\w+|\([^)]*)` captures only the `\$\d+` prefix of `tenant_id =
  $1::uuid`, leaving `::uuid` outside the match; `paramRef` (now
  `resolveParamRef`) then matches the already-cast-free `"$1"` and always
  succeeded, before and after this fix. `TestExtractTenant_
  WhereClauseCastRHSResolves`/`_SpacedCastRHSResolves`/
  `_ArrayCastRHSResolves` now name this explicitly, closing the "clean by
  luck, not by test" gap.

**Red before green, plus a mutation proving the tests are load-bearing.**
The minimal reproduction from this session's earlier investigation
(`ExtractTenant` on `INSERT INTO widgets (name, organization_id) VALUES
($1::text, $2::uuid)` returning `AttrNoPredicate` instead of the real bound
UUID) was written up as `TestExtractTenant_InsertCastPositionalValueResolves`
alongside 13 sibling tests (§ above). All 14 confirmed failing against the
pre-fix code (a temporary revert of both call sites and the
`tenantCompareRegex` pattern back to their original anchored form): **7
failed** (the `Insert*` cast/nested/`CAST(...)` tests and
`WhereClauseCastFuncRHSResolves` — the WHERE path's `::type`-shorthand
tests correctly stayed green even pre-fix, matching the "genuinely clean"
finding above), confirming the tests actually exercise the bug rather than
being vacuously true. Restored the fix: all 14 pass, and the full
`internal/differ` suite (38 tests) and the full DB-backed suite (`go test
-p 1 ./...` with `TGD_TEST_DSN` set — serial, not parallel, to avoid an
unrelated, pre-existing cross-package probe-database-count flake in
`TestVerifyCLI_ProbeDatabaseIsAlwaysDropped` that reproduces under `go test
./...`'s default cross-package parallelism and passes reliably standalone
and under `-p 1`, confirmed 3/3 and unrelated to this fix) pass, zero
regressions.

**Re-run, end to end, against the fixed binary.** A second fresh
`coder/coder` clone this session (the first clone and its
`coder_target`/capture were superseded along with §7.2's numbers, so a
clean re-run needed a clean target rather than reusing state a buggy binary
had already touched), freshly migrated, `coderd/database/dbpurge` run
through `tenantguard capture` a second time (the same `C-7` shared-database
test interference reproduced — expected, unrelated to this fix), `infer`
producing the same 22 scoped tables, all 22 made row-level-provable (13
sampled, 9 fixture-seeded as `TGD-BL-37`'s originals, plus a 10th fixture
this specific fresh clone needed — `workspace_build_orchestrations`, empty
here though it had organic rows in the previous clone's capture-polluted
database; authored the same way, FK-checks-disabled-during-seeding making
its dependent columns type-correct-only, not real).

**Result:** `{"safe":498,"leak":37,"unattributable":3804}` (4339 total).
Four denominators:

| label | denominator | unattributable | rate |
|---|---|---|---|
| `all_captured_queries` | 4339 | 3804 | 87.67% |
| `real_app_sql_any_table` | 2067 | 1532 | 74.12% |
| `real_app_sql_touching_any_declared_table` | 378 | 122 | 32.28% |
| `row_level_touching_real_app_sql` | 378 | 122 | 32.28% |

`counts.vacuous_safe = 279`: 97 CTE-wrapped writes (all of the 97 `SAFE`
CTE-writes — 304 total `WITH...`-wrapped writes: 207 `UNATTRIBUTABLE`, 97
`SAFE`, 0 `LEAK`), 121 plain (non-CTE) `INSERT`/`UPDATE`/`DELETE` (of 298
`SAFE` plain writes; the other 177 affected real rows, not vacuous), and 61
read-shaped `SELECT` comparisons that matched nothing on both legs — the
`TGD-BL-40`/`-41` mechanisms exercised again on fresh traffic, same
behaviour as §7.2's (buggy-differ) run, unaffected by this fix since they
concern a different code path (`diffWrite`'s own vacuous detection and
`isWriteStatement`'s CTE routing, not `ExtractTenant`).

**The 37 remaining `LEAK`s, inspected individually, not sampled.** All 37
trace to one of two already-documented shapes, neither a new instance of
`TGD-BL-42` or any other defect: (1) a query filtered only by a bare `id`
(primary key), never the declared tenant column at all —
`GetChatFileByID`, `GetProvisionerJobByID`, `GetProvisionerDaemons`,
`GetChatFileMetadataByChatID` — correctly `AttrNoPredicate`, the same L1
shape `TGD-BL-32` originally named and cautioned is Tier-1 tool signal, not
a confirmed `coder` vulnerability (app-layer authorization elsewhere is not
visible to this tool); (2) `InsertWorkspaceAgentStats`/
`GetWorkspaceAgentStats`, an `INSERT ... SELECT unnest(...)`/CTE-aggregate
shape `extractFromInsert`'s literal-`VALUES` path was never built to cover
(it recognises `INSERT INTO t (cols) VALUES (...)`, not `INSERT INTO t
(cols) SELECT ...`) — the exact shape `TGD-BL-37` already found and named,
unchanged by this session. Two of the 37 resolved a real, non-empty tenant
(the sentinel all-zero UUID `00000000-0000-0000-0000-000000000000`, bound
by `GetAuditLogsOffset`'s own "no filter" default) and correctly leaked
under that value — evidence `resolveParamRef`'s fix is exercised on a real
leak, not only on eliminating false ones.

**No new defect shape found this session**, per the standing "file and
stop" instruction — nothing here is filed as a new `TGD-BL` entry.

**Third baseline, set.** `unattributableCeilingRate` (`cmd/tenantguard/
ceiling.go`) is now `122.0/378.0` (≈0.32275, 61/189 simplified) — the third
value this project has ever set, and the first RAISE. Full reasoning,
including why `TGD-US-07` AC-2's down-only ratchet is not being violated
by a raise, is recorded in `ceiling.go`'s own comment (not duplicated here
— the source is the citable location, per this project's own convention
for every prior baseline) — summarised: every prior number (`130/611`,
`131/664`, the unset `131/519`) was measured by a differ now known to have
silently discarded real tenant values on cast `INSERT` parameters, so those
were never a trustworthy floor; a raise driven by fixing the measuring
instrument and re-measuring correctly is the ratchet rule's own named
exception (`TGD-US-07` AC-2's "requires a recorded decision naming why a
higher rate is acceptable"), not a routine or silent one. The rate also
reflects a different, freely-varying capture (a fresh clone, not the same
traffic `TGD-BL-06`/`-38` measured, which no longer exists to re-run) of a
different size — named plainly rather than the whole movement being
attributed to the fix alone. A real `audit` run against this exact capture
and binary now correctly exits 0.

### 7.4 `TGD-FR-09`/`TGD-US-08` — Tier 0 built, tested, and measured against `coder`

This session stopped further work on the differ (a clean run against a
real target, four consecutive sessions of find-defect/fix/re-run/re-
baseline) and built the tier of the SRS that had zero lines written:
Tier 0, the syntactic pass design §3/§9 describe and `TGD-FR-09`/
`TGD-US-08` specify but no prior session had implemented.

**Table-list decision, made and recorded, not left implicit.** Two
mechanisms were possible: read the policy file `infer` already emits (more
accurate — it reflects a live schema), or infer candidate tenant-scoped
tables from column naming in the scanned SQL text alone, no database. The
second was chosen, and the first was not built alongside it (the
instruction was explicit: don't build both). Reasoning, in full, lives in
`internal/triage`'s own package doc rather than duplicated here, but
summarised: requiring a policy file makes Tier 0 depend on a prior Tier 1
database run having already happened, which is circular for a tier whose
entire story (`TGD-US-08`) is running *before* that investment is made —
and design §9's own precondition table states Tier 0's precondition as
"None. Any Go repo," which a policy-file dependency, even an indirect one,
is not. The cost is real and named, not hidden: a table is only "known
scoped" if the corpus's own SQL text happens to mention a tenant-shaped
column somewhere — undercounting relative to a live schema, which is the
safe failure direction for a tier that must never overclaim.

**What was built.** `internal/triage` (new package, ~460 lines) + `tenantguard
triage PATH [--output json|text]` (`cmd/tenantguard/main.go`). Walks a
repository for `*.sql` query files (skipping `migrations/` directories —
schema, not application queries) and non-generated `*.go` source (skipping
files carrying Go's standard `// Code generated ... DO NOT EDIT` header —
without this, a sqlc-shaped repo like `coder` would have every statement
counted twice, once from its `*.sql` source and once from the generated
`*.sql.go` file sqlc emits with the identical SQL text as a string
constant). Extracts candidate statements (via `go/parser`'s AST for Go
files — real tokenisation, not a text regex reaching into a string
literal inside a comment — and sqlc's `-- name:` convention for `*.sql`
files), classifies each as `READ`/`WRITE` and finds its primary table,
builds a corpus-wide "table → its evidenced tenant column" map from the
naming heuristic (`tenant_id`, `org_id`, `organization_id`, `workspace_id`,
`account_id`, `customer_id`, `owner_id`, `owner` — design §9's own named
list, widened with the variants `coder`'s and common convention actually
use), then flags any statement against a known-scoped table that never
mentions that column anywhere in its own text — `AC-3`'s naive
presence-only check, which by construction does not catch `L6`
(`tenant_id = tenant_id`) or `L10` (`... OR status = 'public'`): both
mention the column, so both pass, which is the acceptance criterion.
Ranking: write-shaped statements before read-shaped, then by the table's
own scoped-confidence (how much of its OTHER traffic in the corpus does
scope it) descending, then file/line for determinism.

**Two real defects found and fixed while testing against real `coder`
source, in this same session — proof this needed real traffic, not just
fixtures, the same lesson `TGD-BL-31` first taught this project.** A first
pass, gated only on a leading SQL-verb keyword plus a bare structural
keyword's presence anywhere in the text (`FROM` for `SELECT`/`DELETE`,
`SET` for `UPDATE`, `INTO` for `INSERT`), misclassified plain English CLI
help strings as SQL: `"Update the coder section in %s"`, `"Delete all
encrypted data from the database. THIS IS A DESTRUCTIVE OPERATION."` — both
real `coder` source strings — satisfied the keyword gate (English "from"
and prose containing "the" after `UPDATE`/`DELETE`) and produced "the" as
a fabricated table name, appearing in the known-scoped-tables list and
(rank 140+) the suspicions list. Fixed with a second, load-bearing gate
(`matchTable`/`sqlClauseWords`, `internal/triage/triage.go`): validate that
what immediately follows the matched table token (allowing one alias word)
is a real SQL clause keyword (`WHERE`, `SET`, `VALUES`, `ON`, …) or
punctuation/end-of-statement — real SQL always has one there; the English
sentences did not. `TestRun_PlainEnglishCLIHelpTextNotMisclassifiedAsSQL`
proven failing pre-fix (`statements_found = 2`, want `0`) before the fix
landed, passing after. A related second issue, found while writing that
same test: a candidate whose verb-plus-keyword gate passed but whose table
match then correctly failed was still being counted as a "Statement" (an
empty-table entry inflating `statements_found` silently, though never
producing a false `Suspicion` since a table-less statement carries no
tenant-column evidence to check) — tightened so a failed table match
disqualifies the candidate entirely, matching the "not SQL at all" contract
`classifyOne`'s other callers already assume. Full `internal/triage` suite
(12 tests) green after both fixes; `go vet`/`gofmt` clean; the full
repository's DB-backed suite (`go test -p 1 ./...` with `TGD_TEST_DSN` set)
still green, zero regressions elsewhere.

**Budget, measured, not asserted (`TGD-NFR-01`).** A genuinely fresh,
timed `git clone --depth 1` of `coder/coder` followed immediately by
`tenantguard triage --output json` against it: **19.5 seconds total**,
clone to output — the clone itself accounts for nearly all of it; the scan
phase alone, timed separately against an already-cloned checkout, is
**~2.5 seconds** across 2,795 scanned files (985 candidate statements
found after both fixes). Comfortably under the 5-minute budget, with wide
margin even for a much larger repository.

**Run against `coder`, compared query-by-query against this same session's
37 real `LEAK` verdicts (§7.3, the fixed differ's own corrected re-run) —
the first time this project has measured the tier split empirically rather
than asserting it exists.** 339 suspicions, 182 distinct flagged query
names. The 37 `LEAK` verdicts name 7 distinct queries
(`InsertWorkspaceAgentStats` ×2, `GetWorkspaceAgentStats` ×6,
`GetProvisionerJobByID` ×18, `GetProvisionerDaemons` ×2,
`GetAuditLogsOffset` ×2, `GetChatFileByID` ×4,
`GetChatFileMetadataByChatID` ×3).

**Overlap: 3 of 7** (`GetChatFileByID`, `GetProvisionerDaemons`,
`GetProvisionerJobByID`) — Tier 0 flagged all three, and inspection
confirms why: their `*.sql` *source* text is `SELECT * FROM <table> WHERE
id = ...`/`SELECT * FROM <table>` with no tenant-column mention at all.
Notably, `coder`'s own *captured, wire-level* SQL for these same queries
(§7.3's differ input) shows an explicit, expanded column list including
`organization_id` — `sqlc` expands `SELECT *` into real column names at
code-generation time, so the source Tier 0 reads and the wire traffic
Tier 1 captures are, for `SELECT *` queries, genuinely different text for
the same logical query. Tier 0 happened to flag these correctly regardless
(the source's `SELECT *` mentions no column at all, tenant or otherwise),
but this is worth naming as a real, previously unremarked property of
auditing a `sqlc` repo at the source level versus the wire level.

**Divergence: the other 4, each for a distinct, verified reason — not one
blind spot but three:**

1. **A predicate that legitimately mentions the column but is
   value-conditional at runtime** (`GetAuditLogsOffset`). Inspected the
   source directly: `CASE WHEN @organization_id::uuid !=
   '00000000-...-000000000000'::uuid THEN audit_logs.organization_id =
   @organization_id ELSE true END` — a real, intentional "optional filter,
   sentinel bypasses it" feature (an unfiltered admin view across all
   organisations), not a bug. The column is textually present as a real
   comparison, so Tier 0's presence check correctly does not flag it —
   this is design §7's own `L5` shape (`TGD-US-08`'s
   `AC-3` names L6/L10, not L5, but the mechanism is the same: a
   syntactically genuine predicate whose *value* at runtime is what
   determines the outcome). Undetectable without executing the query,
   which is exactly why Tier 1 exists.

2. **The tenant column appears only in the output, never as a filter**
   (`GetWorkspaceAgentStats`, `InsertWorkspaceAgentStats`) — a shape
   `AC-3`'s two named fixtures do not cover and this session's run is the
   first evidence of. `GetWorkspaceAgentStats` `SELECT`s and `GROUP BY`s
   `workspace_id` in a CTE aggregate whose only `WHERE` clause filters
   `created_at`/`connection_median_latency_ms` — genuinely no tenant
   predicate at all, and it leaked as `AttrNoPredicate` at Tier 1 for
   exactly that reason. `InsertWorkspaceAgentStats` lists `workspace_id`
   in its `INSERT` column list (via `INSERT ... SELECT unnest(...)`, the
   same shape `TGD-BL-37` first found) with no `WHERE` at all. Both pass
   Tier 0's naive check because the column name is textually present
   *somewhere* — the check has no notion of "present as an actual filter"
   versus "present because it's part of what's selected or inserted."

3. **Scoped indirectly, through a join to a different table's own
   tenant column, invisible in this statement's predicate on the target
   table** (`GetChatFileMetadataByChatID`): `SELECT cf.organization_id, ...
   FROM chat_files cf JOIN chat_file_links cfl ON cfl.file_id = cf.id
   WHERE cfl.chat_id = $1`. `organization_id` is textually present (in the
   `SELECT` list), so Tier 0 doesn't flag it — but the actual filter is on
   `chat_id`, a foreign key into a different table entirely, whose own
   scoping (if any) this single statement's text cannot speak to at all.
   PostgreSQL resolved this correctly at Tier 1 (RLS applies to the base
   table regardless of how it's reached); a syntactic reading of one
   statement's own text structurally cannot.

**What this empirically argues, stated as the measurement it is, not a
restatement of the design doc's prior claim.** Design §3/§9 asserted a
tier split was necessary; this session is the first time the project has
put a number on it. 3/7 overlap is not "Tier 0 catches most of what Tier 1
catches" — it is under half, on this one real capture, and the 4 misses
are not one narrow, already-catalogued edge case (`L5`) but three
qualitatively different reasons, one of which (`#2`, column-present-as-
output-not-filter) had never been named in this project's fixture corpus
before this run surfaced it. That is the argument for `AC-3`'s own
"recorded as a known limitation... the reason Tier 1 exists" — now with a
real number and three named shapes behind it, not just L6/L10.

**No new defect shape found in this session's final state** (the two
found during triage's own development were fixed, proven red-before-green,
and are reflected above) — nothing further filed.

### 7.5 `TGD-FR-13`/`TGD-US-12` — Tier 2 built, mechanism argued before building, corpus run at Tier 2 for the first time

Two tiers of the SRS had zero lines written before this session's
predecessor built Tier 0; this session built the bigger of the two
remaining, and the only one that fixes `L5`: Tier 2, the fail-closed
guardrail library (`TGD-FR-13`/`TGD-US-12`). Scope, held to deliberately:
the wrapper, tenant context propagation, fail-closed enforcement, and the
fixture corpus run at Tier 2. **Not a `coder` integration** — wiring this
into `coder`'s own source is a separate question needing changes in their
repo, out of scope for this slice.

**Enforcement mechanism, argued before any code was written, per
instruction — full text lives in `internal/guardrail`'s own package doc
(the citable location, this project's own convention for every prior
design decision), summarised here.** Three candidates: (1) reuse the RLS
oracle at runtime — re-execute every query a second time under a
synthesised, RLS-enforcing role, the same mechanism Tier 1's `Diff` already
uses, called from the hot path instead of offline; (2) a predicate check
before execution — extract the query's own claimed tenant
(`differ.ExtractTenant`, already built, already `TGD-BL-42`-hardened) and
compare it against the context's tenant, with no re-execution and no
second query; (3) something else — e.g. a middleware that only sets the
RLS session variable per request and leans on RLS itself, already deployed
in production, to filter rows.

(3) was rejected first, on the ACs' own wording: `TGD-US-12` AC-1 requires
a blocked query to "return an error and no rows," and real RLS enforcement
does not error — a wrongly-scoped `SELECT` under RLS just silently returns
fewer or zero rows, indistinguishable from "no matching record," and it
requires the SAME production RLS deployment Tier 1's whole `A1`–`A4` proof
burden exists to justify, paid a second time rather than the "days, not
point-and-shoot" simplification design §9 frames Tier 2 as. (2) was chosen
over (1) on cost (a single in-process regex pass per query vs. doubling
every wrapped query's database round trips — disqualifying for a library
meant to sit in front of an application's *entire* query volume, not an
offline batch run against one captured file), on the hot-path constraint,
and because AC-1/AC-2's wording ("returns an error... instead of" the
query running at all) reads as a pre-execution check, not "run it, then
diff two executions after the fact." (2) also directly answers design §13's
open question ("whether Tier 2's driver wrapper shares the differ with
Tier 1 or reimplements it") concretely: shares `ExtractTenant`, does **not**
share `Diff`/the RLS re-execution — now resolved in the design doc itself,
not left open.

**What was built.** `internal/differ/tier2.go`: `CheckTenant(sqlText,
relations, params, intendedTenant) Result` — `ReferencesScopedTable` first
(nothing declared Scoped is named -> `Safe`, nothing to enforce), then
`ExtractTenant`, mapped to Tier 2's decision:
`AttrUnattributable`/`AttrNoPredicate` -> `Leak` (fail closed — an audit
tool can afford to report "couldn't determine" and move on; a guardrail
cannot afford to guess), `AttrResolved` with a mismatched value -> `Leak`
(`L5`, exactly), a match -> `Safe`. `internal/guardrail` (new package):
`WithTenant`/`TenantFromContext` (context propagation, an empty string
deliberately treated as absent so it cannot defeat AC-3), and `DB` — a
wrapper that does **not** embed `*sql.DB` (embedding would promote every
original method unchanged, letting a caller reach `Query`/`Exec` directly
and bypass the guardrail by accident — the single most important thing to
get right) — exposing `QueryContext`/`ExecContext`/`QueryRowContext` only,
each gated by a `check` that returns before `db.inner` is ever touched on
anything but a `Safe` verdict. `QueryRowContext` needed its own `Row` type
(`*sql.Row` has no exported constructor for a preset error) mirroring
`*sql.Row`'s own defer-the-error-to-`Scan` contract. `PrepareContext`/
`Prepare` and the non-`Context` `Query`/`Exec`/`QueryRow` are named, not
wrapped — a documented gap (`internal/guardrail/wrap.go`'s own doc comment
says why), not silently unsupported.

**Tested, including two tests proving the "zero database contact" claim
concretely rather than by absence of a returned row.** 12 unit tests: 3
pure (context propagation, including the empty-string case), 9 against
real PostgreSQL (`TGD_TEST_DSN`) — allow/block for `QueryContext`,
`ExecContext`, `QueryRowContext`; `L5` detection through a real wrapped
connection (`TestDB_BlocksWrongTenant`); a blocked write proven not to have
run by re-querying afterward (`TestDB_BlocksUnscopedExec`); and
`TestDB_BlockedQueryNeverReachesDatabase`, which points the wrapped `*DB`
at an address nothing listens on and confirms the guardrail's own sentinel
errors come back rather than a connection error — if the check were not
actually gating execution before dialing, this test would see a dial
failure instead. All pass; full repository suite (`go test -p 1 ./...`
with `TGD_TEST_DSN` set) still green, zero regressions; `go vet`/`gofmt`
clean.

**The corpus run at Tier 2, for the first time — `TGD-US-06` AC-5 /
`TGD-US-12` AC-4.** `TestCorpus_Tier2ExactSetEquality`
(`internal/differ/corpus_test.go`) runs `CheckTenant` against all 23
Tier-2-applicable fixtures (`U4` excluded — design §7.4's own table marks
it "n/a (no proxy)": a capture-layer artifact with no Tier 2 equivalent,
since `CheckTenant` operates on already-known, in-process Go values and
has no concept of an unresolved wire parameter to begin with). **`L5`
lands `LEAK`** — the headline requirement, proven both at the corpus level
and, independently, by a real wrapped-connection test above.

**As first measured this session: 19 of 23 matched design §7.4's original,
pre-implementation prediction exactly, and 4 diverged — reported as design
findings, per instruction, not reconciled by adjusting the corpus or by
building a second mechanism to force a match. `U2` (below) was
subsequently found, on review, to be a real fail-open bypass rather than a
mere mislabelling, and was fixed the same session — see §7.6. The count
after that fix is 20 of 23 matching, 3 diverging; `U2`'s own entry below is
kept as the historical record of what the divergence was and why, exactly
as it read before the fix, per this project's own convention of not
editing history quietly:**

- **`L3`, `L10` — predicted `LEAK`, actually `SAFE`.** Both are caught at
  Tier 1 through `Diff`'s real RLS row-set re-execution, not through
  attribution: `L10`'s `OR` clause still resolves a correct-looking
  `tenant_id = $1` textually (the regex finds it regardless of the
  trailing `OR status = $2`, the same mechanic `TGD-BL-42`'s fix relied
  on, here working against detection instead of for it), and the actual
  leak is that RLS-restricted re-execution returns FEWER rows than
  unrestricted once the `OR` branch is intersected with the policy — a
  row-set property, not an attribution failure. `L3`'s join scopes
  `audit_log` (`a.tenant_id = $1`) but not `invoices` (aliased `i`) at
  all; `ExtractTenant`'s per-relation loop is alias-blind (traced directly
  in `extract.go`: the same regex, searching the whole SQL text, is run
  once per declared-Scoped relation with no way to bind a match to the
  specific alias it followed FROM/JOIN), so the `invoices` iteration finds
  the SAME `a.tenant_id = $1` text the `audit_log` iteration did and
  resolves it too — `CheckTenant` sees an apparently-correct value for
  BOTH tables and passes. Tier 1 catches the real leak (the seeded
  `audit_log` row deliberately points at a DIFFERENT tenant's invoice) via
  the join's actual row-level behavior under RLS. `CheckTenant` has no
  equivalent of either mechanism — this is the exact, predicted cost of
  choosing (2) over (1), now measured rather than merely argued.
- **`L7` — predicted `SAFE` ("a blind spot at every tier", `TGD-BL-23`),
  actually `LEAK`.** A genuinely positive finding, not a gap: `L7`'s query
  (`SELECT * FROM invoices_view_no_invoker`) has zero predicate at all, and
  `CheckTenant` blocks any scoped-table query with no comparison
  unconditionally — it has no notion of "RLS would have been bypassed
  anyway by view ownership," which is Tier 1's SPECIFIC reason for missing
  this fixture. That reason does not apply to a mechanism that never
  reaches RLS at all. `TGD-BL-23`'s "blind spot at every tier" framing is
  refuted for THIS Tier 2 implementation specifically — though not for one
  built on mechanism (1), which would inherit Tier 1's exact blindness by
  construction, worth naming so a future re-implementation on (1) does not
  assume this finding still holds.
- **`U2` — predicted `UNATTRIBUTABLE`, actually (at the time this was
  written) `SAFE`. ⚠ FIXED — see §7.6 (`TGD-BL-43`); `U2` now correctly
  lands `UNATTRIBUTABLE`, matching this row's own original prediction. Kept
  below verbatim as the record of what the bug was.** The most severe of
  the four, and named as more severe rather than grouped with the other
  three.** `L3`/`L10`/`L7` are all cases where the classification changed
  but the underlying query was still INSPECTED. `U2`
  (`SELECT * FROM scoped_summary()`) is not: `ReferencesScopedTable` finds
  no declared-Scoped table named in the text at all (the function itself
  is not a declared relation), so `CheckTenant`'s first check short-
  circuits straight to `Safe` before `ExtractTenant` ever runs — the query
  is allowed through, uninspected, not merely mis-labelled. This is a real
  coverage gap: a function or view NOT itself declared in the policy, that
  internally touches a real Scoped table, is invisible to this mechanism.
  Chosen deliberately, not accidentally — the alternative (block every
  query that fails to explicitly name a declared-Scoped table) would also
  block `SELECT now()`, a health check, or any query against a
  legitimately Unscoped table, which is disqualifying for a library meant
  to wrap an application's ENTIRE `*sql.DB`. Named here as the sharpest
  edge of mechanism (2)'s cost, and the strongest argument for treating
  mechanism (1) (RLS reuse) as a distinct, heavier-weight, OPT-IN addition
  for the highest-risk deployments, rather than evidence this session's
  choice of (2) was wrong for its stated, narrower purpose (closing `L5`
  at hot-path cost).

**What this settles, stated as the measurement it is.** `L5` is
detectable, exactly as `TGD-US-06`/`TGD-US-12` require, by a mechanism
cheap enough to run on every query — the substantive argument for Tier 2
existing at all, now demonstrated rather than asserted. The corpus run
also demonstrates, with the same rigor, that "Tier 2 fixes `L5`" is not
the same claim as "Tier 2 subsumes everything Tier 1 catches" — three
fixtures Tier 1 catches through row-set re-execution are outside this
specific mechanism's reach, and (as of this section's writing) one (`U2`)
was outside it in a materially more concerning way than the other two —
**`U2` specifically was fixed the same session, not merely reported; see
§7.6.**

### 7.6 `TGD-BL-43` — the `U2` finding was a fail-open bypass, not a labelling gap; fixed, proven red-before-green plus a mutation

§7.5 named `U2` as the most severe of the four Tier 2 corpus divergences —
not a mis-classification but an actual bypass: a function or view not
itself declared in the policy, that internally touches a real Scoped
table, was allowed through completely uninspected. On review this was
correctly judged not acceptable to leave as a reported limitation: a
fail-open path in a library whose entire contract is fail-closed is a
defect, not a cost/coverage tradeoff, and closing it was this session's
first priority.

**The fix.** `CheckTenant`'s old gate (`ReferencesScopedTable`) asked one
question — "does this query name a table the policy declared Scoped?" —
and defaulted to `Safe` on no. That default was the bug: "not found among
the tables the function was told to enforce" and "genuinely irrelevant" are
not the same fact, and the function only ever had the Scoped subset to check
against in the first place (`guardrail.Wrap` passed `policy.Scoped()`, not
the full policy). Fixed in two parts:

1. `guardrail.Wrap` now stores the FULL policy (`policy.Relations` —
   Scoped, Unscoped, and Unclassifiable alike), not just the Scoped
   subset.
2. `internal/differ/tier2.go`'s new `resolveReferences` requires every
   relation-shaped name a query mentions (`referencedRelationNames` — a
   new helper returning real names only, never an alias, distinct from
   `referencedTables`, whose flat name-or-alias map would have
   misresolved every JOIN's alias as "unknown") to resolve against that
   full policy: not found at all → fails closed (`Unattributable`); found
   but `Unclassifiable` (every view, unconditionally, per `Classify`'s own
   rule — `schema.go`'s package doc: "Unclassifiable... is never
   equivalent to Unscoped," now extended to Tier 2 enforcement) → fails
   closed identically; found and `Scoped` or `Unscoped` → resolved,
   proceeds as before. A query naming no relation-shaped target at all
   (`SELECT now()`) still passes — there is nothing to resolve, a
   different case from resolving to something unrecognised.

**A necessary second fix, found while proving the first one didn't break
ordinary usage.** The first pass of `resolveReferences`, run against the
corpus, correctly closed `U2` but also newly blocked `S4` and `L4` — both
`WITH`-shaped fixtures whose outer `SELECT` reads from its own CTE's name
(`WITH scoped AS (...) SELECT * FROM scoped`). A CTE's own name can never
appear in any policy (it is not a real relation `infer`'s `pg_class` query
could ever see), so requiring it to resolve would have failed closed on
ordinary, harmless CTE usage — a real-world-common shape, not an edge
case, and disqualifying on its own for a library meant to wrap an
application's whole query volume. Fixed with `cteNames` (`tier2.go`),
reusing `differ.go`'s own balanced-paren `WITH`-list parser
(`skipIdentifier`/`balancedParenSpan`/`hasCaseInsensitivePrefix`/
`skipSpaceFrom` — the same helpers `skipWithClause` already uses for the
identical grammar, not a second, divergent parser) to collect declared CTE
names, including recursively inside a CTE body that itself begins with a
nested `WITH`, and exclude them from resolution. The corpus fixture's own
`relations` list also needed `users`/`tenants` added (both genuinely carry
a `tenant_id` column in the fixture schema; a real `infer` run would
classify both `Scoped`) — a fixture-completeness gap `U1`/`L8` exposed
once every referenced name had to resolve, not a mechanism defect.

**Proven red before green, with U2's exact shape, plus a mutation — per
instruction.** `TestDB_BlocksFunctionWrappingScopedTable`
(`internal/guardrail/guardrail_test.go`): a real PostgreSQL function
wrapping `SELECT * FROM widgets` (the fixture's own scoped table), called
`SELECT * FROM all_widgets()` through a wrapped connection — `U2`'s exact
shape, against real Postgres, not a synthetic string. Confirmed genuinely
red: `tier2.go` was reverted to the pre-fix gate (`CheckTenant` calling
only `ReferencesScopedTable`) and the test failed exactly as the bypass
predicts — rows returned, no error. `TestCorpus_Tier2ExactSetEquality`'s
own `U2` case failed identically under the same revert (`SAFE`, want
`UNATTRIBUTABLE`). Restored the fix: both pass. A companion test,
`TestDB_AllowsCTEOverScopedTable`, confirms the necessary second fix
didn't overcorrect — a CTE-shaped query still returns its real row through
a wrapped connection. 8 further pure unit tests
(`internal/differ/tier2_test.go`) cover `resolveReferences`/`cteNames`
directly: unresolvable blocks, `Unclassifiable` blocks, `Unscoped` allows,
no-reference-at-all allows, a CTE name is excluded (including a nested
CTE inside a CTE body). Full repository suite (`go test -p 1 ./...` with
`TGD_TEST_DSN` set) green throughout; `go vet`/`gofmt` clean.

**Corpus, re-measured.** `TestCorpus_Tier2ExactSetEquality`'s
`checkTenantExpected` now has `U2: Unattributable` — matching design
§7.4's ORIGINAL prediction exactly, no longer a divergence. **The Tier 2
corpus now stands at 20 of 23 fixtures matching §7.4's original
prediction, not 19 of 23** — `L3`, `L7`, `L10` remain the three genuine,
argued divergences §7.5 already accounted for (Tier 1's row-set
re-execution catching what a static predicate check structurally cannot);
`U2` is no longer among them. `ceiling.go`/`TGD-NFR-03` is unaffected —
this fix touches only Tier 2's guardrail path, not the differential audit
Tier 1's ceiling is measured against.

**No new defect shape found while proving this fix** (the CTE
false-positive was found and fixed within the same change, not left for a
future session) — nothing further filed.

### 7.7 `TGD-BL-44` — CI's first real GitHub run failed on two findings local
tooling never surfaced; the `[R]` gap between `act`'s image and GitHub's
hosted runner is now a measured divergence, not an open question

**What happened.** The first push to GitHub failed two of the three CI
jobs. `build/vet/gofmt`: `gofmt would reformat: internal/capture/backend_test.go`
— a real, one-line indentation defect (`return` mis-indented inside
`paramDescMsg`), present since the file was written, never caught locally
because prior sessions' own `gofmt -l` runs and this session's re-runs were
treated as authoritative without querying CI's actual gate. **Fixed:**
`gofmt -w` applied; `gofmt -l .` now reports nothing repository-wide.

`workflow-lint`: `actionlint` (via its `shellcheck` integration) reported
two real `shellcheck` findings in `.github/workflows/ci.yml` — `SC2034`
("`i` appears unused") on the postgres-wait loop's `for i in $(seq 1 30)`
(`i` is never read in the loop body — fixed to `for _ in ...`), and
`SC2046` ("quote this to prevent word splitting") on the actionlint
invocation itself, `run: "$(go env GOPATH)/bin/actionlint .github/workflows/ci.yml"`
— the whole line was inside one pair of double quotes, so `$(go env GOPATH)`
was still subject to word-splitting/globbing as an unquoted expansion once
YAML parsed the scalar; fixed to quote only the substitution's own span:
`run: '"$(go env GOPATH)/bin/actionlint" .github/workflows/ci.yml'` (the
outer single quotes are YAML's, the inner double quotes are the shell's).

**Why local runs never caught either.** `gofmt`: earlier sessions'
`gofmt -l` output was treated as an established baseline ("clean except
this one file") without re-querying it fresh each session — a manual
exemption a real gate does not honour, exactly the "gate routed around by
hand" shape this project exists to catch, now caught in its own tooling.

`actionlint`/`shellcheck`, root-caused directly from `actionlint`
v1.7.12's own source (`linter.go`, around its rule-construction list):
`NewRuleShellcheck` is only invoked if a `shellcheck` executable resolves
on `$PATH`; if it does not, the shellcheck-backed rule is silently
dropped — logged only through `actionlint`'s internal debug logger (which
nothing in this project's invocation enables), not surfaced as a warning,
and does not affect the exit code. **`shellcheck` was never installed in
any prior local or `act`-based verification session for this project** —
confirmed by grepping both `docs/SRS.md` and the design doc for the string
`shellcheck`: zero hits anywhere, versus repeated, explicit mentions of
installing `actionlint` and `act` themselves. **Reproduced directly, same
binary, same file, this session:** `actionlint` run against the
pre-fix `.github/workflows/ci.yml` with `shellcheck` absent from `$PATH`
exits 0, no output; the identical invocation with a freshly downloaded
`shellcheck` v0.11.0 placed on `$PATH` exits 1, printing the exact two
findings GitHub reported, byte-for-byte matching line numbers and rule
IDs. This is not `act` vs. GitHub's hosted image diverging — it is a
locally-absent dependency the tool silently degrades around, on both the
bare-host and (as far as this project's own docs record) the `act` path
alike.

**What this means for §11's standing `[R]` tag** ("whether GitHub's actual
hosted `ubuntu-latest` behaves identically to `act`'s image... is `[R]`"):
this run is the first actual measurement, and it found a real, reproduced
divergence between local verification and GitHub's runner — but the cause
is confirmed to be the absent `shellcheck` binary locally, not an
`act`-vs-hosted-image behavioral difference as such. The `[R]` tag is
**not resolved** by this finding: whether `act`'s image and GitHub's
hosted image would still agree if both had `shellcheck` present is still
unmeasured. What is now known, not merely asserted: local-only
verification of this pipeline was insufficient on its own, independent of
that open question, and a real GitHub run is required to trust this gate
— exactly the position `TGD-US-11` AC-6's own closure note already took
about branch protection, extended here to cover CI itself.

**Re-verified after both fixes**, `shellcheck` present on `$PATH` for the
first time in this project's history: `actionlint .github/workflows/ci.yml`
exits 0; `scripts/check_workflow_yaml.py` still parses the file
structurally sane; `bash -n` on both hand-written shell scripts clean.
Full repository suite (`go test -p 1 -count=1 ./...` with `TGD_TEST_DSN`
set against a real Postgres) green across all 7 packages, fresh (test
cache cleared first, not relying on a cached prior pass); `go vet`/`gofmt -l`
clean repository-wide; `TestMutationHarnessM1ThroughM8` still 8/8;
`TestCorpus_ExactSetEquality`/`TestCorpus_Tier2ExactSetEquality` still
exact, unchanged from §7.6 (24/24 and 20/23 respectively) — this fix
touched only formatting and CI YAML, no Go logic.

**Now proven.** The third CI job (`test`/`-race`/coverage floor/the
`M1`–`M8` mutation harness) had skipped on the first run because GitHub
Actions' `needs:` held it back when the first two jobs failed. After these
two fixes landed (commit `fdccf50`), the pipeline ran again as GitHub
Actions run `33716947618` and **all three jobs succeeded**, including this
one: a real Postgres service container on GitHub's own hosted runner, and
`scripts/assert_tests_ran.py` reporting 298 tests passed, 0 skipped, all 8
`M1`–`M8` mutation-harness subtests confirmed present by name, coverage
70.0% (floor 60%). This closes the question this section originally left
open for this job specifically — see `TGD-US-11` AC-6 for the full
account, including what stays `[R]` (branch protection, still
unconfigured).

### 7.8 `TGD-BL-01` — name availability check, actually run; a real
collision found, not a clean pass

M0's own exit criterion (§11) is "`TGD-BL-01` (name availability) closed,"
gated on "Repo creation" per the design doc's backlog table. The repo now
exists and is pushed (`github.com/Sour16o4/tenantguard`), so this check
was run for real rather than assumed moot by that fact.

**Checked, this session, against public registries:** GitHub's own search
API (`api.github.com/search/repositories?q=tenantguard+in:name`) returns
**36 existing repositories** named `tenantguard`/`TenantGuard`, including
`chrispl89/tenantguard` — described by its own author as "a small CLI tool
for testing tenant isolation and authorization boundaries in SaaS
applications," the same problem space as this project, not merely a name
collision in an unrelated domain. `pkg.go.dev`'s search surfaces an
existing Go module, `github.com/RudrenduPaul/TenantGuard`, with its own
`cmd/tenantguard` binary path — the identical command name this project
uses, under a different import path (Go's per-repository module
namespacing means no actual build collision is possible, but an operator
searching `pkg.go.dev` or a package index for "tenantguard" will find
multiple unrelated tools). The npm registry lists a published,
actively-distributed `tenantguard-cli` package with prebuilt binaries for
six platform targets, described as "CLI security scanner for self-hosted
multi-tenant AI-agent platforms... fail-closed... tenant-isolation
defects" — the closest match found, describing a tool aimed at
substantially the same problem this project addresses.

**Finding, stated plainly rather than smoothed over:** the name
`tenantguard` is **not uncontested**. It is not merely used elsewhere in
an unrelated sense (which would be a non-issue); at least three
independent projects (`chrispl89/tenantguard`, `RudrenduPaul/TenantGuard`,
and the `tenantguard-cli` npm package) already use this name or a close
variant for tools in the same tenant-isolation problem space. This is a
genuine naming-collision risk for discoverability and for anyone
searching to confirm they have the right tool, not a trademark or legal
claim this project is positioned to assess.

**`TGD-BL-01` is CLOSED on the basis that the check was performed and its
result recorded** — the backlog item was "run a name-availability check,"
not "confirm the name is available," and a contested result is still a
result. No rename is performed here: the repository already exists and is
pushed under this name, and choosing whether to rename, add a
disambiguating description, or accept the collision is a decision for
whoever owns this project, not one this session is positioned to make
unilaterally. Recorded so the decision is informed, not deferred by
omission.

### 7.9 `TGD-BL-05`/`TGD-NFR-02` — the tool-mechanical portion of Tier 1
setup timed for the first time; the full metric still needs a genuinely
unfamiliar target this session does not have

`TGD-BL-05` ("re-baseline Tier 1 time-to-first-finding — now gated on
`TGD-BL-10`, not `TGD-BL-03`") has been open since the design's original
90-minute estimate was withdrawn as built on the wrong bottleneck (policy
review, not seeding). `TGD-BL-10` (probe seeding) closed under M1. This
session re-measured what closing it actually unblocked.

**Measured, this session, real Postgres, real schema:** the only target
with a live migrated database in this environment is `coder` (95 tables,
via the same `coder_target` database prior sessions built) — not an
unfamiliar repository, a limitation stated plainly below, not hidden.
Freshly built binary, timed end to end:

- `tenantguard infer --dsn ... --out ...` against the full 132-relation
  schema: **0.059s** wall-clock (`22 scoped, 95 unscoped, 15
  unclassifiable`).
- `tenantguard verify --dsn ... --policy ...` against the resulting
  22-scoped-table policy — real probe creation, canary seeding, RLS
  synthesis, all four oracle checks, on every scoped table: **0.889s**
  wall-clock, `proven: true`, `a1=true a2=true a3=true a4=true`.

**Combined tool-mechanical time for both fully-automated Tier 1 setup
phases: well under one second**, on a real, large, production-derived
schema. This confirms directly, not by inference, what the withdrawn
90-minute figure's own post-mortem already suspected: the seeding/proof
machinery `TGD-BL-10` built is not the bottleneck. Whatever dominates a
real "time to first finding" is either human policy review (an operator
reading `infer`'s output before accepting it, AC-2's own explicit human
step, `TGD-US-09`) or the audit step's need for actual captured traffic —
neither of which this measurement covers.

**What this measurement does NOT cover, stated rather than glossed:** the
audit step itself — capturing real traffic through the proxy against a
target and reaching a real first `LEAK`/`SAFE`/`UNATTRIBUTABLE` verdict —
requires a live running application driving real queries through
`tenantguard capture`. No such live application is running in this
environment this session (the `coder_capture_events.jsonl` file prior
sessions used no longer exists on disk), and standing one up fresh was
judged out of scope for this pass rather than rushed into an unreliable
number. Nor is `coder` an "unfamiliar" target by this point in the
project's own history — it is the single most-measured repository in this
SRS.

**What a fair full `TGD-NFR-02` measurement requires, stated precisely
rather than left implicit:** a target neither this tool's authors nor this
project's own prior sessions have profiled before, measured wall-clock
from a cold `git clone` through `infer` → a genuinely first-time,
timed human policy review → `verify` → `tenantguard capture` run against
that target's own real test suite or live traffic → `audit` → the first
non-`UNATTRIBUTABLE` verdict. Every phase but the human-review one is now
proven fast in isolation (this section); the review and live-capture
phases are the two genuinely unmeasured costs, and only a first encounter
with a new target measures the review phase honestly at all — a second
run against `coder` cannot, no matter how carefully timed.

**`TGD-NFR-02`'s row is updated to reflect exactly this, not silently
set:** a real, `[E]` sub-measurement now exists for the tool-mechanical
setup phases (recorded, not withdrawn), and the full metric remains
explicitly `[U]`, with the measurement protocol above recorded as the
condition for closing it — the same "recommend, do not silently set"
discipline `TGD-NFR-03`'s ratchet already established. `TGD-BL-05` is
CLOSED on the basis that the gate it names (`TGD-BL-10` landing) has
been acted on and measured as far as this environment honestly allows;
a new backlog entry, `TGD-BL-45`, is filed for the remaining, genuinely
unfamiliar-target measurement.

## 8. External Interfaces

### 8.1 Commands

| Command | Tier | Purpose |
|---|---|---|
| `tenantguard triage <pkg>` | 0 | Syntactic pass. Never exits non-zero. |
| `tenantguard infer --dsn <url>` | 1 | Emit a candidate tenancy policy for review. |
| `tenantguard audit --policy <f> --dsn <url> -- <cmd>` | 1 | Run `<cmd>` with its connection URL pointed at the proxy; report verdicts. |
| `tenantguard verify --policy <f> --dsn <url>` | 1 | Run the four oracle self-checks and exit. |

**The target's connection-URL variable is target-specific and must not be
assumed.** For `coder` it is `CODER_PG_CONNECTION_URL`, **not** `DATABASE_URL`
**[R]**. `audit` takes the variable name as configuration.

### 8.2 Exit codes

| Code | Meaning |
|---|---|
| 0 | Run completed; no `LEAK` (Tier 0 always exits 0) |
| 1 | Run completed; at least one `LEAK` |
| 2 | Usage or configuration error |
| 3 | Unattributable rate above the baselined ceiling (`TGD-NFR-03`/`TGD-BL-06`, in force) — report still written to stdout |
| 4 | Target requires TLS — unsupported at Tier 1 (`TGD-FR-12`) |
| 5 | Missing privilege (e.g. no `CREATE DATABASE` for the probe) |
| 10 | Oracle self-check `A1` failed — negative control did not withhold |
| 11 | Oracle self-check `A2` failed — policy coverage incomplete |
| 12 | Oracle self-check `A3` failed — role bypasses RLS |
| 13 | Oracle self-check `A4` failed — oracle over-restrictive |

Codes 10–13 emit **no findings report** in any output format.

## 9. Constraints, Assumptions & Risks

### 9.1 Constraints

| ID | Constraint | Tag |
|---|---|---|
| `C-1` | PostgreSQL RLS is the oracle. No non-PostgreSQL target is supported. | — |
| `C-2` | Tier 1 requires a target whose test DSN does not require TLS. | **[E]** |
| `C-3` | Tier 1 requires `CREATE DATABASE` privilege for the probe. | — |
| `C-4` | The tool never writes to the target's database (§3.3). | — |
| `C-5` | Both `lib/pq` and `pgx` statement-naming schemes must be supported. | **[E]** |
| `C-6` | **The target's database must be migrated before the run.** `dbtestutil` does **not** migrate an externally-supplied connection URL — supplying one bypasses the template path entirely, so the operator must apply the target's migrations first. | **[E]** `TGD-BL-11` |
| `C-7` | **Supplying a connection URL disables per-test database isolation.** With `CODER_PG_CONNECTION_URL` set, `dbtestutil` skips per-test database creation, so every test shares one database and they interfere. Auditing this way requires either accepting interference or driving the suite one test at a time. Four `dbpurge` tests failed under `TGD-BL-11` and this is the likely cause — but the evidence is **a single isolated re-run**, not a controlled comparison against an unproxied baseline. Treat the attribution as plausible and unconfirmed. | **[E]** for the mechanism `TGD-BL-11`; the failure attribution is **weakly supported** |

### 9.2 Assumptions, and what happens if each is wrong

| ID | Assumption | If wrong |
|---|---|---|
| `A-1` | The target's schema can be reproduced into a probe database. | Tier 1 unavailable for that target; `A1` cannot be proved; the tool aborts rather than degrading. |
| `A-2` | Column-name heuristics identify tenant columns usefully often. | `infer` becomes advisory only and the operator authors the policy; Tier 1 still works. |
| `A-3` | A synthesised policy set is semantically equivalent to the app's intended scoping. | The core risk of the whole approach. Mitigated only by `TGD-US-11`; a surviving mutant means this assumption is unverified. |

### 9.3 Risks

| ID | Risk | Severity | Mitigation |
|---|---|---|---|
| `R-1` | Synthesised RLS is wrong and the tool reports "0 leaks" confidently. | **Critical** | `TGD-US-02` aborts rather than reporting; `TGD-US-11` proves each gate can fire. |
| `R-2` | Proxy resolves a `Bind` to the wrong SQL and judges a query the app never ran. | **Critical** | Per-connection scoping (`TGD-US-04` AC-2), which fails if state is made global. **[E]** |
| `R-3` | `A1` proved only on the probe while the live schema has diverged. | High | `A2`/`A3` run live; `TGD-US-03` AC-5 upgrades to target-verified when possible; the report always states which. |
| `R-4` | `L5` undetectable at Tier 1 gives false assurance. | High | Documented in `LIMITATIONS.md`; two distinct expected corpus sets; **confirmed for `coder` [R]**, **unknown for `zitadel` (`TGD-BL-09` open)**. |
| `R-5` | End-to-end behaviour differs from the synthetic Gate 0 workloads. | **Partly discharged** | `TGD-BL-11` executed: 1,314/1,314 on a real suite. It also **found a real defect** (`TGD-BL-12`, stale resolution on closed statements) that no synthetic workload had triggered, which is evidence the risk was correctly rated. **Residual:** `coder` exercises neither caching nor named statements, so those paths are still synthetic-only. |
| `R-6` | Proxy overhead makes a real suite too slow to audit. | Medium | Unmeasured (`TGD-NFR-05` **[U]**); measured during `TGD-BL-11`. |
| `R-7` | **`A1` passing is mistaken for full policy correctness.** Found by mutation during `TGD-BL-10`: `A1` cannot detect a policy on the wrong tenant column, because a wrong predicate still excludes rows and satisfies `A1`'s inequality. | **CLOSED** | `TGD-BL-15` — done. `CheckA4` implemented and proven **[E]** against real PostgreSQL: passes on a correct policy, aborts (`ErrA4`, exit 13, distinct from `ErrA1`/exit 10) on the exact wrong-column policy that survived `A1`, and separately on a `USING(false)` policy. Non-redundancy shown directly: a correct single-tenant policy makes `A1` abort while `A4` passes; the wrong-column two-tenant policy makes `A1` pass while `A4` aborts. `ProofState.PolicyProven()` makes `TGD-US-03` AC-6 an executable gate (6 cases tested, including "A1 passed, A4 never run" → refused), not only a documentation claim. |

## 10. Verification & Test Plan

| Layer | Method | Gate |
|-------|--------|------|
| Oracle self-check | Mutation harness `M1`–`M8`, each asserted against its **mapped** gate (extra gates firing are not a failure; the mapped gate staying silent is) | Merge-blocking |
| Fixture corpus | Exact-set equality across `SAFE`/`LEAK`/`UNATTRIBUTABLE`, **two expected sets** (Tier 1, Tier 2) | Merge-blocking |
| Statement scoping | Cross-connection collision test; must fail if state is made global | Merge-blocking |
| Protocol | `lib/pq` and `pgx` matrix: cache on, eviction past capacity, simple protocol, split reads | Merge-blocking |
| Trust boundary | Row-count diff of every target table before and after a run; must be identical | Merge-blocking |
| Probe lifecycle | Probe dropped on success, failure, panic, `SIGINT` | Merge-blocking |
| CLI contract | Exit codes §8.2; no findings section on codes 10–13, in any format | Merge-blocking |
| Tier 0 discipline | Asserts exit 0 always, and that "leak"/"proven"/"violation" never appear | Merge-blocking — **[E]** `TestRun_OutputNeverContainsForbiddenWords` (§7.4); exit-0 is structural (`runTriage` cannot reach a non-zero return once `triage.Run` starts), not merely tested |
| Concurrency | `-race` across the full suite | Merge-blocking |
| Performance | `TGD-NFR-05`, `TGD-NFR-06` | **Non-blocking until baselined** |
| End-to-end | `coder`'s suite through the proxy (`TGD-BL-11`) | Blocking for M2, not for M1 |
| Supply chain | `govulncheck`, `gitleaks`, `gosec`, SBOM | Merge-blocking |

### Published results (`docs/BENCHMARKS.md`)

Every published number carries its tag. **[U]** figures may not be published at all.

- `Bind` resolution rate, with workload matrix and whether synthetic or real
- Fixture corpus results, per tier, both expected sets
- Mutation harness: mutants, mapped gates, and which gate actually caught each
- Whether `A1` was probe-verified or target-verified
- **Explicitly stated:** which claims are **[E]** and which remain **[R]**

## 11. Release Plan

**Ordering criterion, stated because it was previously implicit.** Milestones are
ordered by **risk**, not by dependency. M1 leads with the oracle because it is the
component whose failure is silent and total, not because later milestones depend on
it. Capture (`US-04`, `US-05`) sits in M2 but has **no dependency on M1** and can be
built first; doing so was a deliberate, recorded choice. Do not read this table as a
dependency chain.

| Milestone | Exit criteria |
|-----------|---------------|
| **M0 — SRS accepted — `CLOSED`** | This document reviewed. `TGD-BL-01` (name availability) **closed** (§7.8): the check was actually run against GitHub, `pkg.go.dev`, and npm — it found a real naming collision (36 existing GitHub repos, an existing Go module with the identical `cmd/tenantguard` binary name, and a published `tenantguard-cli` npm package in the same problem space), recorded rather than smoothed over. The backlog item was to run the check, not to guarantee a clean result; no rename performed here — that decision belongs to whoever owns this project. |
| **M1 — Oracle proven — `CLOSED`** | `US-01`, `US-02`, `US-03`, `US-11` all closed. `TGD-BL-10`, `TGD-BL-15`, `TGD-BL-16`, `TGD-BL-17`, `TGD-BL-18`, `TGD-BL-19`, `TGD-BL-20` — done. `A1`–`A4` implemented and proven individually and together (`M1`–`M8` harness, all mutants caught by their mapped gate); `TGD-US-01` AC-2/AC-3/AC-4 closed; the CLI wires `A1`–`A4` and `ProofState` behind a real, user-invocable path, proven end-to-end against the built binary, closing `TGD-US-02` AC-5; `TGD-BL-19` fixed — a skipped canary insert now fails immediately and distinctly (exit 2) rather than surfacing as a misleading `A1` abort. **`TGD-US-11` AC-6 closed last**: a CI pipeline exists and was *executed*, not only written — `actionlint` and `act` (both real tools, freshly installed, not hand-rolled) linted and then ran the workflow against real Docker and a real Postgres service container. Both directions of the core requirement were demonstrated: disabling `A1`'s inequality check (the same mutation that blinds `M8`) turned the pipeline red, naming the exact broken tests; reverting turned it green again. The silent-skip failure mode this AC exists to prevent was independently reproduced by removing the Postgres service entirely — the pipeline failed outright rather than passing having tested nothing. Four real defects surfaced by *running* the workflow, not by reading it, were found and fixed in the same pass (a missing `pg_isready` binary, `actionlint`'s git-repository-dependent discovery, missing PyYAML, and PEP 668's package-install restriction) — none would have been caught by a syntax check alone. **What remains explicitly `[R]`, not folded into this closure:** whether GitHub's actual hosted runner behaves identically to `act`'s local image in every respect, and the branch-protection "required status check" setting that is the literal GitHub mechanism for merge-blocking — a repository administration setting requiring push access, unconfigured and unverifiable from here. At the time this closure was written, the repository had no git history and no remote, so GitHub itself had never run this workflow — that fact was stated, not hidden behind the local proof. |
| **M2 — Tier 1 usable — `CLOSED`** | ~~`US-04`~~ — done (AC-1, AC-3–AC-8 `[E]`; AC-2 stays `[R]`, deliberately retained rather than closed — no target exercising explicit statement preparation has been measured). ~~`US-05`~~ — done, AC-1–AC-3 all `[E]`. ~~`US-06`~~ — done, AC-1–AC-6 all `[E]` (AC-5 closed this session, §7.5). ~~`US-07`~~ — done, AC-1–AC-3 `[E]`/narrowed-and-accepted. ~~`US-09`~~ — AC-1–AC-3 `[E]`; **AC-4 formally descoped to `M4`**, not closed here — `zitadel` third-target work belongs alongside `TGD-BL-09`, not duplicated across two milestones (rationale in `TGD-US-09` AC-4's own entry). ~~`US-10`~~ — AC-2–AC-4 `[E]`; **AC-1's "test or code path" clause formally descoped to `M5`**, not closed here — a cross-cutting capture-layer feature, not required to demonstrate Tier 1 produces real findings (rationale in `TGD-US-10` AC-1's own entry). ~~First real finding on `coder`~~ — done: 37 real `LEAK` verdicts, the corrected differential run (§7.3). ~~`TGD-NFR-02`~~ — **partially baselined** (§7.9): the tool-mechanical setup phases (`infer`+`verify`) measured at under 1 second against a real 132-relation schema, `[E]`; the full metric (human review + live-capture time) stays `[U]`, with a measurement protocol recorded and a new backlog entry (`TGD-BL-45`) filed for the genuinely-unfamiliar-target run it requires — not silently set, per the same discipline `TGD-NFR-03`'s ratchet uses. **Closed on this basis:** every item M2's own exit criterion names is now either done, narrowly-accepted-and-closed, or explicitly and reasoned descoped to a later milestone — nothing here is closed by asserting more than what was actually measured or built. |
| **M3 — Tier 0 + second target** | `US-08`. ~~`TGD-BL-06` ceiling baselined~~ — **done** (§7.1, ratcheted §7.3). ~~`US-08`~~ — **done** (§7.4: `internal/triage`, `tenantguard triage`, measured against `coder`). `TGD-BL-04` (`casdoor` PostgreSQL, a second target) remains open, so M3 as a whole is not yet closed. |
| **M4 — Tier 2** | ~~`US-12`. `L5` detection demonstrated.~~ — **done** (§7.5: `internal/guardrail`, `TestDB_BlocksWrongTenant`, `TestCorpus_Tier2ExactSetEquality`'s `L5` case). Not a `coder` integration — that needs code changes in their repo and was explicitly out of scope for this slice. **`TGD-BL-09` closed** — `zitadel` session handling inspected — or `zitadel` formally dropped from v1 scope — remains open; M4 as a whole is not yet closed on that account. **`TGD-US-09` AC-4 (`zitadel` policy inference, `TGD-BL-07`) now also tracked here**, moved from `M2` this session — both `zitadel`-specific gaps (session handling and policy inference) live under one milestone rather than split across two. |
| **M5 — v1.0.0** | All `M` stories. Every **[U]** either baselined or withdrawn. `LIMITATIONS.md` complete. Any third-party finding disclosed and resolved. **`TGD-US-10` AC-1's "test or code path" clause (`TGD-BL-26`) now also tracked here**, moved from `M2` this session — a cross-cutting capture-layer feature, not required to prove Tier 1 works, but still owed before v1.0.0 per this milestone's own "every open item resolved or withdrawn" rule. `TGD-BL-45` (a full `TGD-NFR-02` measurement against a genuinely unfamiliar target, §7.9) also belongs here if not closed sooner. |

**Release gate on evidence tags:** v1.0.0 may not ship while any **[R]** claim is
presented as verified, or any **[U]** number is quoted as a requirement.

## 12. Definition of Done (per story)

- [ ] All acceptance criteria have automated tests, or a documented manual procedure
- [ ] Every AC resting on an **[R]** premise is tagged in the test that covers it
- [ ] Coverage thresholds met; race detector clean
- [ ] Negative-path and abort-path criteria covered, including "emits no report"
- [ ] Trust-boundary test passes: target row counts unchanged
- [ ] Probe database cleanup verified on every exit path
- [ ] `--output json` supported where the story produces output
- [ ] Exit codes conform to §8.2
- [ ] `LIMITATIONS.md` updated if the story adds or narrows a limitation
- [ ] `CHANGELOG.md` entry added
- [ ] No **[U]** number promoted to a requirement without its baselining story
- [ ] Benchmarks re-run if the story touches a measured path

## 13. Success Metrics (measured at GA + 60 days)

| Metric | Target |
|--------|--------|
| Real isolation findings reported to third-party projects, disclosed responsibly | ≥ 1 |
| Repositories other than `coder` audited at Tier 1 | ≥ 3 |
| Mutants surviving the harness | 0 |
| Published claims tagged **[R]** but presented as verified | 0 |
| External issues or PRs from non-authors | ≥ 5 |
