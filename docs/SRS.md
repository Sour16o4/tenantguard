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
| **Access-control applications** — a tenant-shaped column exists (`owner_id`, `TeamId`) but visibility is computed in application code as roles/membership/admin-override, not a database-enforceable partition; legitimate cross-"tenant" reads are the normal, correct case (a public repo, an admin console), not a violation | **A different detection problem, found by trying `go-gitea/gitea` and `mattermost/mattermost` (design §10, SRS §7.10/§7.11) — named here so a third candidate isn't screened for it by trial.** This is not schema-per-tenant/database-per-tenant (those are about *where* rows physically live); here the rows sit in one shared table with a tenant-shaped column, but a synthesized RLS policy on that column would be actively **wrong**, not merely unverified — it would deny every legitimate cross-boundary read (a public resource, an admin/superuser role, a collaborator grant) and this tool would report each denial as a finding: false `LEAK`s generated by a wrong model of the domain, not real tenant-isolation defects. The screening checklist (design §10.1) exists to catch this **before** any pipeline work goes in, not after. |
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
| `TGD-FR-05` | The tool shall support the statement-naming schemes of both `lib/pq` (sequential per connection) and `pgx` v5 (SHA of SQL). | M | Protocol test | **[E]** — confirmed against real `pgx`/`pgxpool` traffic for the first time, §7.15: 2,952+ real `Bind` events against `zitadel`, 0 unresolved. The exact scheme observed in real production-shaped traffic is `pgxpool`'s own `stmtcache_<sha256-hex>` (`internal/stmtcache`), not the bare-`*pgx.Conn` `stmt_<sha256-hex>` path (`conn.go`) this row's evidence previously cited only from source reading — both are genuine SHA-of-SQL schemes, this refines which of `pgx`'s two internal paths real traffic actually exercises. |
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
  Gated by `TGD-BL-07`. **Formally descoped from M2 to M4, not silently
  left dangling:** `zitadel` is a third-target integration effort — the
  design's own validation-targets table (§10) already calls it out as
  "valuable *because* it is the hard case; third target, not first." `M4`
  already carried `zitadel`-specific work as its own named exit-criterion
  gap (`TGD-BL-09`, session-handling inspection). Splitting `zitadel`
  integration work across both M2 and M4 would have duplicated the same
  third-target burden under two different milestones for no reason tied
  to either milestone's own definition. **`TGD-BL-07` closed in M4
  (§7.15):** policy inference against `zitadel`'s real schema completed
  without crashing and correctly classified the composite
  `instance_id`/`org_id` tenancy shape — the real, narrow blocker
  (`instance_id` missing from the candidate list) was fixed, red-before-
  green plus a mutation, then re-verified against the live database
  (121/144 relations scoped correctly). No longer `[U]`.

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
| `TGD-NFR-02` | Tier 1 time to first finding on an unfamiliar repository | **PARTIALLY BASELINED, remainder WITHDRAWN for v1.0.0** | Timed test | **[E]** for the tool-mechanical setup phases only: `infer` + `verify` together measured at **under 1 second** (0.059s + 0.889s) against a real 132-relation/22-scoped-table schema, §7.9. **[U]**, formally withdrawn from `v1.0.0`'s exit bar (§7.20), for the full metric: the live-capture phase needs a third, genuinely unfamiliar target not yet selected (a real, boundable `v1.1+` cost); the human policy-review phase needs an actual human operator, which no agent session can honestly substitute for measuring (a structural limitation, not a scheduling one). The 90-minute design figure stays **withdrawn**. No single number is in force for the full metric. |
| `TGD-NFR-03` | Unattributable rate ceiling | **Stored value: `18.0/762.0` (≈2.3622%)**, against the `row_level_touching_real_app_sql` denominator (`TGD-BL-33`), measured on `coder` post-`TGD-BL-35` fix (§7.18/§7.19) — the fourth baseline set, and the first ratchet driven by a real capability improvement rather than a measurement correction or a coverage change. | Integration test | **[E]** Enforced and re-provable, gate proven to fire on the new number specifically (§7.19: above fails, at-or-below passes, mutating the constant moves the boundary, deleting the check goes red). History: `130/611` (`TGD-BL-06`) → `131/664` (`TGD-BL-38`, down, coverage change) → `122/378` (`TGD-BL-42`, up, measurement correction) → **`18/762`** (`TGD-BL-35`'s fix, down, §7.19 — this move needed no down-only-ratchet exception; it satisfies `TGD-US-07` AC-2 outright). `zitadel`'s own post-fix measurement (17.19%, §7.18) clears the prior ceiling but breaches this one — expected, not a regression: `zitadel`'s capture is bootstrap-heavy by construction, not the steady-state traffic this coder-calibrated ceiling has always measured (§7.19). §7.1/§7.2/§7.3 (all superseded) record earlier, contaminated or since-superseded measurements for provenance; none is the current stored constant. A real `audit` run against the exact capture and binary this baseline was measured on exits 0. |
| `TGD-NFR-04` | **`Bind` resolution rate** — a captured `Bind` is matched to the SQL of an open statement on its own connection | 100% | Protocol test + real-suite run | **[E]** `TGD-BL-11`: **1,314/1,314** across 163 connections of `coder`'s `dbpurge` suite, plus 2,266 simple-protocol queries. **[E]** ported implementation: 821/821 against live `pgx` and `lib/pq`. **Scope caveat:** `coder` exercises **no statement caching** (Parse:Bind 1:1) and **only unnamed statements**, so the eviction and named-statement paths rest on synthetic evidence alone. |
| `TGD-NFR-17` | **Parameter value recovery rate** — a captured parameter is readable as a *value*, not merely as bytes | Report; threshold deferred | Protocol test + real-driver run | **[E] before backend capture:** 861/961 (89.6%) known — all text; **100/961 (10.4%) binary and undecodable**. **[E] after backend capture (`TGD-BL-14`):** **961/961 (100%)** — the same workload set, all 100 binary parameters decoded from `ParameterDescription` OIDs. **Distinct from `TGD-NFR-04`; never report the two as one figure.** `coder`'s 1,314 remain **[R]** for this metric: the spike recorded no format codes, so **that run's text/binary breakdown is unrecoverable**. **Residual, unmeasured:** the decoder covers a fixed type set; `zitadel` is untested and may bind types with no decoder (`numeric`, `jsonb`, arrays), which stay undecodable by design. **[E] `Describe` FIFO correlation, validated against a real driver:** `pgx`'s `QueryExecModeDescribeExec` (never caches, always describes explicitly) was run through the built binary against real PostgreSQL with two distinct queries back to back; both `ParameterDescription` replies were attributed to the correct statement, with no cross-contamination. Previously this logic was proven only by synthetic mutation tests — an earlier attempt with `QueryExecModeExec` sent parameters as text and never triggered `Describe` at all, which was reported rather than left implied. **A real bug surfaced by this run**: a successfully decoded binary parameter carried a stale `unknown_reason` alongside `value_known:true` — `parseBind`'s placeholder reason was never cleared on successful decode. Fixed, red/green proven, mutation-caught. **Still unproven:** only one `Describe`/`Bind` ordering has been exercised live; out-of-order or interleaved pipelining remains validated by the synthetic tests alone (`[R]`). |
| `TGD-NFR-05` | Proxy added latency per query | **Measured: ≈45µs/query added overhead** (single connection, loopback, `SELECT 1`, §7.20) | Benchmark | **[E]** — direct-vs-proxied round-trip timing against the real proxy and real PostgreSQL: direct ≈100-112µs/query, proxied ≈144-163µs/query, across four runs of 2,000-3,000 queries each. No target/ceiling set — this is a first measurement, recorded as a number, not a requirement (§7.20). |
| `TGD-NFR-06` | Memory during a 100k-query capture | **Measured: no growth trend observed** (single sustained connection, §7.20) | Benchmark | **[E]** for the single-connection case: `VmRSS` sampled every second across 111,005 real captured queries (100,000-query bench run plus 11,000 from the latency runs, same process) stayed flat at 12.2-12.6MB, no upward trend. **[U]** remains for the many-short-lived-connections case — this run never tested `Session` cleanup on connection close, only sustained per-connection growth, and "statement maps grow per connection" (the original concern this row named) is about the multi-connection case specifically, not measured here. |
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

**Also fixed while in this file, unrelated to `TGD-BL-44` itself:** GitHub
flagged `actions/checkout@v4` and `actions/setup-go@v5` as declaring a
deprecated Node 20 runtime. Both bumped to `@v7` — verified as real,
current major-version tags (not guessed) via each action's own
`git/refs/tags/v7` and confirmed to declare `using: node24` in their
`action.yml`, and re-verified clean against `actionlint`/`shellcheck` and
the structural YAML check.

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

**Decided: keeping the name `tenantguard`.** Weighed explicitly, not by
default. The GitHub/`pkg.go.dev` collisions are namespace-only — Go's
module system is namespaced per repository path, so no build ever
resolves ambiguously regardless of how many other repos are named
`tenantguard`. The `tenantguard-cli` npm package is the one collision in
the *same problem space*, not merely the same word — but it is a
different ecosystem (npm, not the Go module graph), creates no possible
build collision, and no legal or trademark issue was found in checking
for one. The only real, remaining cost is search-result confusion for a
human evaluating both tools side by side — a real cost, but not one that
justifies cascading a rename through the `TGD-*` prefix across both this
document and the design doc, which is what a rename actually costs here
(every requirement ID, not just one string). Weighed against that: doing
this before v1.0.0, before any external consumer's `go.mod` references
this module path, and while git history is three commits deep, is the
cheapest this decision will ever be — and it was still not judged worth
it, on the discoverability cost alone.

**`TGD-BL-01` is CLOSED on the decision, not merely on the check having
been run.** The check surfaced a real, non-trivial finding (§ above); the
decision that finding fed into was made deliberately, with the tradeoff
stated in the open, not smoothed over by silence. No rename performed.

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

### 7.10 `go-gitea/gitea` vetted as M3's second target, to the same rigor
`casdoor` got — disqualified, a different way

`casdoor` failed on database engine (§ above: MySQL, not PostgreSQL).
`gitea` was checked next, against the same five questions, reading its
actual test harness and source rather than its dependency list.

**1. Does the Postgres job actually run, and what does it exercise?**
Yes, and it is the heaviest-loaded path in the repository, not a token
job. `.github/workflows/pull-db-tests.yml`'s `test-pgsql-shard-1` and
`test-pgsql-shard-2` are gated on `needs.files-changed.outputs.backend
== 'true'` — they run on any pull request touching backend code, not
conditionally skipped in practice. Each shard runs
`.github/actions/pgsql-shard`, which runs `GITEA_TEST_DATABASE=pgsql
make test-integration` with `-race` and `-timeout=40m`, sharded across
two runners; shard 1 additionally runs `make test-migration`. This is
real integration-test execution against a live `postgres:14` service
container, confirmed by reading the composite action's own steps, not
inferred from the workflow's job name.

**2. Which driver — `pgx` or `lib/pq`?** `lib/pq` — confirmed twice:
`go.mod` lists only `github.com/lib/pq`, no `pgx`/`jackc` dependency at
all, and `models/db/engine.go` blank-imports it directly ("`_
"github.com/lib/pq" // Needed for the Postgresql driver`"). This
matters exactly as much as expected and no more: `TGD-NFR-`/`C-5`
already requires and has `[E]`-proven support for both `lib/pq` and
`pgx` statement-naming schemes, so this introduces no new capture-layer
risk.

**3. Is there a real shared-schema tenant column, or is authorization
enforced entirely in application code?** **Application code, and this is
the disqualifying finding.** `repository.owner_id` exists and looks
plausible from outside, the same way it did before `casdoor` was
checked — but `models/perm/access/repo_permission.go`'s
`GetUserRepoPermission` computes visibility as a Go value, not a SQL
predicate: `minAccessMode := util.Iif(!repo.IsPrivate &&
!user.IsRestricted, AccessModeRead, AccessModeNone)` grants read access
to **every** non-restricted user for **every** non-private repository,
regardless of `owner_id` — public-repo visibility crossing "tenant"
boundaries is gitea's normal, correct, load-bearing behavior, not an
edge case. Private-repo access layers team membership
(`organization.GetUserRepoTeams`) and per-unit collaborator grants on
top of that, walked in Go across `team`/`team_user`/`collaboration`
tables, admin override included. There is no single `WHERE owner_id =
X` this tool's oracle could turn into an RLS policy without breaking
the product's own intended behavior: a synthesized `USING (owner_id =
current_setting('tenant'))` policy would deny every legitimate public-
repo read and every legitimate collaborator access, and this tool would
report each denial as a difference worth flagging — noise indistinguishable from a genuine finding, generated by a wrong model of
the domain, not a security defect. This is the same shape §3.2 already
excludes for schema-per-tenant ("a different detection problem; claiming
it would be a lie of scope") — `gitea`'s ACL model is a different
detection problem for a different structural reason, but the exclusion
lands the same way.

**4. Fixture/sampling density — would `coder`'s 2-of-22 problem
recur?** No, and this was checked directly:
`models/fixtures/repository.yml` spreads its rows across more than ten
distinct `owner_id` values (14 repos under one owner, 4 each under
several others, down to 2 and 3), all in the same table — nothing like
`coder`'s empty-tables-on-fresh-migration problem would occur here. This
question is answered for the record; it does not rescue the target
given finding 3.

**5. Connection-URL injection point and TLS.** `tests/pgsql.ini.tmpl`
sets `SSL_MODE = disable` — no TLS obstacle, had this gone further.
Connection is configured via discrete INI fields (`HOST`, `NAME`,
`USER`, `PASSWD`, `SCHEMA`), templated from `TEST_PGSQL_*` values at
test-run time, not a single environment-variable DSN the way `coder`'s
`CODER_PG_CONNECTION_URL` is — redirecting it through the proxy would
mean overriding the templated `HOST` value rather than one env var, a
different mechanism, plausibly still "no code change" but genuinely not
chased further once finding 3 made the target moot.

**Go/no-go: NO-GO.** Every mechanical precondition this tool actually
needs is either satisfied (`lib/pq`, no TLS, fixture density) or
plausibly satisfiable with more digging (connection injection) — `gitea`
is disqualified on the one precondition no amount of engineering effort
here can fix: it is not a shared-schema tenant-isolation application.
Its authorization model is general multi-tenant-*user* access control
(who may see which repository), not multi-tenant-*organization* data
partitioning (which organization's rows these are) — a category this
tool was never built to model, named explicitly rather than discovered
by trying and failing to build against it.

**This finding generalized before the next candidate was vetted, not
after.** Two things came out of `gitea`'s elimination: a named §3.2
exclusion ("Access-control applications") so a third candidate isn't
screened for the same failure mode by trial, and the five questions
above turned into an explicit, ordered screening checklist (design
§10.1) — the tenant-column question first, since it is disqualifying on
its own and makes the other four moot if it fails.

### 7.11 `mattermost/mattermost` vetted against the checklist — disqualified
at question 1, the rest not spent

Checklist question 1 only, per the checklist's own ordering rule: if the
first question fails, the remaining four are not spent on an already-
eliminated target.

**Is `TeamId` a real row-scoped tenant column, or app-computed
visibility?** Read `server/channels/app/authorization.go` directly, the
actual authorization code path, not the schema. `SessionHasPermissionToTeam`:
access is granted if the session holds *unrestricted* status (`session.IsUnrestricted()`,
used by system integrations and bot sessions — an unconditional
bypass), **or** if the user is a member of that specific team with roles
granting the permission, **or**, independently, if the user's own
*session-wide* roles grant the permission at all — the same
system-admin-style blanket-access path `gitea`'s `IsAdmin`/team-owner
checks used. `SessionHasPermissionToChannel` layers the identical
pattern one level down: membership check first, but falling through to
`SessionHasPermissionToTeam` (which itself has the same three-way
escape) when the user isn't a channel member. This is application-code
role/membership evaluation, not a database-enforceable partition — the
identical shape that disqualified `gitea`, differing only in which
specific roles and membership tables are walked. A synthesized RLS
policy on `TeamId` would have the same failure mode `gitea`'s would
have: denying legitimate system-admin and cross-team-permission access
and reporting each denial as a finding.

**Go/no-go: NO-GO, at question 1.** Per the checklist's own point, the
remaining four questions (Postgres-in-CI reality, driver, fixture
density, injection point/TLS) were not investigated — a real,
possibly-favorable answer to any of them would not change this
disqualification, so spending the session on them would answer
questions that no longer matter.

### 7.12 Three eliminations, not three unlucky picks — what this says
about the target population, and what M3 actually needs reconsidered

`casdoor` (wrong database engine), `gitea` (access-control, not
partitioning), `mattermost` (the identical access-control shape) is not
three independent coin-flips. Two of the three failed on the *same*
structural category, found on the second try and confirmed on the
third without needing to search further — that repetition is itself
evidence about where real Go/PostgreSQL open-source projects of any
public visibility tend to sit, not about this session's luck.

**The property a viable second target actually needs:** a shared-schema
application where crossing the tenant boundary is **never** a
legitimate, designed-for case — no public/global read path, no
system-admin role that spans tenants by design, no collaborator grant
that lets one tenant's user see another's rows on purpose. `coder`
itself has exactly this shape: an `organization_id` boundary with no
"public workspace visible to other organizations" concept anywhere in
its design (§10.1's Gate-0 findings). That is a strict B2B SaaS
data-isolation shape.

**Whether real-world Go/PostgreSQL open-source projects commonly have
that shape: on this evidence, no, not among the kind of project that
gets built and open-sourced as flagship software.** The open-source
projects large and visible enough to be worth choosing as a validation
target skew heavily toward *collaboration and access-control* software
— git forges, chat/comms platforms, forums, wikis, identity providers —
where enabling controlled cross-boundary visibility (a public resource,
an admin console, a shared team) is the product's own central value, not
an oversight. Strict multi-tenant SaaS data isolation — the shape this
tool needs — is disproportionately a property of *commercial, closed-
source* backend software, precisely because that isolation is often the
vendor's core security/compliance liability surface and not something
companies tend to open-source. `coder` is closer to an exception than a
representative example: it is an internal developer-platform tool where
"organization" is a hard boundary by the nature of what it's for
(a company's own private dev environments), not a social/collaborative
product where boundary-crossing is a feature.

**What this means for M3, stated rather than papered over with a fourth
guess:** `zitadel` (already named, Target 3, `M4`) is an identity
provider — plausibly closer to the needed shape, since customer/tenant
data separation is usually core to an IAM product's own value rather
than incidental, but it was deliberately chosen as the *hard* case
(versioned tables, split schemas) and is already gated under `M4`, not
available as M3's easier second target without contradicting its own
stated reason for being third. **Recommend revisiting M3's own exit
criterion** rather than continuing an undirected search: either (a)
accept that a second validated target may need to come from a narrower,
harder-to-find population (self-hosted internal platform tools closer
to `coder`'s own shape — cloud-dev-environment, internal PaaS, or
internal ticketing/CRM software, not developer-collaboration tools) and
budget real search time for it, explicitly, as its own effort rather
than an assumed quick win, or (b) treat `coder`'s own single-target
validation as sufficient evidence for v1's exit bar and move the
"second target" requirement to a v1.1+ milestone, reasoned openly rather
than quietly dropped. This is a decision for whoever owns the project's
scope, not one this session is positioned to make unilaterally — but
guessing a fourth repository name without addressing why the first three
attempts (two of them on the identical failure mode) went the way they
did would be searching, not reasoning.

### 7.13 M3's exit criterion revisited — decided: `coder`-only validation
for v1, second target moved to `v1.1+`

**Decision, recorded in the open rather than as a quiet descope.**
"Second target" is removed from M3's own exit criterion (§11); M3 is
CLOSED on `coder`-only validation. Three reasons, stated as the actual
basis for the decision, not reconstructed after the fact:

1. **The substantive claim v1.0.0 makes does not depend on a second
   target.** 37 real `LEAK` verdicts against `coder`'s real, unfamiliar
   132-relation schema (§7.3) is what demonstrates the differ, the
   oracle, and both tiers correctly separate safe queries from leaking
   ones on real traffic against a real, sizeable schema. A second target
   would strengthen the generality of that claim — it would not change
   whether the approach works, which the first target already
   establishes or fails to.

2. **§7.12's population finding makes an open-ended further search a
   poor use of remaining effort**, not merely a discouraging one: real
   Go/PostgreSQL open-source software visible enough to choose as a
   validation target skews toward collaboration/access-control products
   (git forges, chat, identity providers) where crossing the "tenant"
   boundary is a designed-in product feature, not strict B2B SaaS data
   isolation. Two of three candidates checked failed on exactly this
   structural category.

3. **What a second target would and would not add is now stated
   precisely, not left implicit** — recorded in `LIMITATIONS.md`'s new
   "Single-target validation" section: `coder` alone proves the
   mechanism works; it does not prove every real-world constraint/type
   shape has been exercised. Concrete precedent from this project's own
   history — `uuid` tenant columns (`TGD-BL-27`) and cross-column
   `CHECK` constraints (`TGD-BL-37`) were shapes `coder`'s own schema
   happened to contain and that were not anticipated before being
   encountered. A second, differently-shaped target remains the
   mechanism for finding the *next* such shape — deferred to `v1.1+`,
   not abandoned.

**`zitadel` checked against this same population question before being
left as `M4`'s unexamined third target — a direct answer, not deferred
to a future vetting session, though not a full five-question screening.**
Design §10 named `instance_id` and `resource_owner` together as
`zitadel`'s two candidate tenant columns; they are not interchangeable
under the access-control-vs-partition distinction §7.10–§7.12
established. Read directly, in `internal/query/user.go` and `org.go`:
every query unconditionally filters `instance_id =
authz.GetInstance(ctx).InstanceID()`, where the instance is resolved
from the request's own hostname at the API boundary — not a
caller-supplied or role-branching value. This is a genuinely hard
partition, structurally unlike `gitea`'s or `mattermost`'s app-computed
visibility. `resource_owner` (organization, within one instance) is
different: it flows through `PermissionClause`/`userPermissionCheckV2`,
a permission-computed join that can legitimately span multiple
organizations a caller holds grants in — the same RBAC-computed shape
that disqualified both prior candidates. `zitadel`'s own "System-API"
(instance provisioning, a separate gRPC service) is an out-of-band
operator control plane, not ordinary product-level cross-tenant access
— closer to this tool's own `A3` concern (does the connecting role
bypass RLS) than to `gitea`'s public-repo reads or `mattermost`'s
system-role escape hatch, both of which are everyday, user-facing
product behavior.

**So: no, `M4`'s `zitadel` work does not uniformly inherit §7.12's
population problem — but it isn't uniformly clear either, and the two
candidate columns must not be treated as equivalent going forward.**
`instance_id` is plausibly viable at the rigor `coder`'s
`organization_id` was held to; `resource_owner` is plausibly not, on
the same grounds that eliminated `gitea`/`mattermost`. This is a reading
of two query files, not the five-question checklist run in full
(Postgres-in-CI reality, driver, fixture density, injection point/TLS
all remain unchecked for `zitadel`) — stated as a preliminary finding to
inform whether and how `M4`'s `zitadel` work proceeds, not as a
completed vetting.

### 7.14 `zitadel` — checklist completed (`instance_id` only), session
handling checked directly, inference tested empirically: real, narrow
blocker found, not the one Gate 0 anticipated

Three pieces of work, per instruction, stopping before any pipeline run.
`resource_owner` is out of scope throughout, per §7.13's finding — only
`instance_id` was pursued.

**Checklist questions 2–5, completed** (question 1, `instance_id` real
vs. `resource_owner` app-computed, was already answered in §7.13):

- **Q2 — Postgres in CI, actually exercised?** Yes. `.devcontainer/docker-compose.yaml`'s `db-api-integration` service runs `postgres:18` (the only database image anywhere in that file — no MySQL/CockroachDB alternative). `apps/api/project.json`'s `test-integration` nx target depends on `test-integration-run-db` (`compose up ... db-api-integration`) then `test-integration-run-api`, which runs the API binary in `test-integration-api` mode and executes the real integration suite against it. CI's `lint_test_build.yml` runs `pnpm nx affected --targets lint test build`, which reaches this target on any affected change — not a decorative or separately-gated job.
- **Q3 — driver: `pgx` or `lib/pq`?** `pgx`, confirmed twice: `go.mod` lists `github.com/jackc/pgx/v5 v5.9.2` as a direct dependency; `github.com/lib/pq v1.12.3` is present only `// indirect` (pulled in transitively, not used by `zitadel`'s own code). **`TGD-FR-05`'s "pgx v5 (SHA of SQL)" claim confirmed exactly, at the source line**: `pgx`'s own `conn.go` (v5.9.2) — `digest := sha256.Sum256([]byte(sql)); psName = "stmt_" + hex.EncodeToString(digest[0:24])` — is the literal mechanism. **Flagged, not hidden: this project's own `[E]` tag on that claim has never been exercised against a real name in this format.** The one `"stmt_"`-prefixed name in this codebase's own tests (`internal/capture/session_test.go`'s `TestBindAfterCloseIsUnresolvable`, `const name = "stmt_1"`) is a hand-picked placeholder for an unrelated regression test (`TGD-BL-12`, Close-handling), not a SHA-format name, and was never intended to exercise this claim. Read `internal/capture/session.go` directly: statement resolution is a plain `map[string]string` keyed on the raw name string, with no length or format assumption anywhere — the mechanism is naming-scheme-agnostic by construction, so this is assessed as low-risk, not a blocker — but `zitadel` would be the **first** target that actually generates real `pgx`-SHA-named statements through the proxy, where this claim would receive its first genuine test.
- **Q4 — fixture/seed density on `instance_id`?** Strong yes. `internal/integration/instance.go`'s `NewInstance` creates a real, fully separate instance per call via the System API, documented in its own comment as "isolated and... safe for parallel testing" — the integration suite creates many such instances across its test packages, each seeded with real rows (an IAM owner, org owner, users) in the same shared tables. `coder`'s 2-of-22 sparse-table problem would not recur here; if anything the opposite risk (very high `instance_id` cardinality) is worth knowing about but is not itself disqualifying.
- **Q5 — injection point and TLS?** `cmd/defaults.yaml` documents `Database.postgres.DSN` directly: "a full PostgreSQL connection URL... Format: `postgresql://user:password@host:port/dbname?sslmode=disable`", overridable via the single env var `ZITADEL_DATABASE_POSTGRES_DSN` (or discrete `ZITADEL_DATABASE_POSTGRES_HOST`/`_PORT`) — a cleaner single-variable injection point than `coder`'s own. `apps/api/test-integration-api.yaml` sets `TLS: Enabled: false` for the integration-test configuration — no TLS obstacle.

**Checklist verdict for `instance_id`: GO on all five questions**, with the `pgx`-naming caveat above recorded rather than silently assumed clean.

**`TGD-BL-09` — session handling, checked directly, not inferred from
`go.mod`.** A full local checkout was searched (`grep -rn` across every
`.go` file) for `set_config`, `current_setting`, and any `SET`/`SET
LOCAL` statement. One match: `internal/v2/eventstore/postgres/push.go`'s
`setAppName`, which runs `SET LOCAL application_name TO '<name>'` purely
for `pg_stat_activity` observability/tracing — not tenant context. No
`CREATE POLICY`/`ENABLE ROW LEVEL SECURITY` appears anywhere in the
codebase either (like `coder`, `zitadel` has no RLS of its own to
conflict with policy synthesis). **`zitadel` does not set tenant context
via `SET`/`set_config` on the connection** — `instance_id`/
`resource_owner` are ordinary bound query parameters embedded directly
in each statement's own predicate (`internal/query/user.go`, `org.go`),
confirmed in §7.13. **`L5` does not become Tier 1-detectable for this
target; the `LIMITATIONS.md` entry does not narrow** — recorded there
directly, alongside the existing `coder` finding, both now confirmed by
the identical method rather than one target's finding being assumed to
generalize.

**`TGD-BL-07` — policy inference, tested empirically against real
PostgreSQL with `zitadel`'s exact table shape, not merely reasoned
about.** Gate 0 flagged two risks: versioned table naming
(`users6`…`users10` — the live schema now uses `users14`, confirmed from
a shallow clone of `zitadel/zitadel`, `internal/query/projection/user.go`:
`UserTable = "projections.users14"`) and split `eventstore`/
`projections`/`system` schemas (all confirmed real, literal Postgres
schemas via `CREATE SCHEMA` in the migration SQL). **Neither of these
turned out to be the actual blocker.** `schema.Infer`'s own
`relationQuery` (`internal/schema/schema.go`) already enumerates every
non-system schema (`WHERE n.nspname NOT IN ('pg_catalog',
'information_schema') AND n.nspname NOT LIKE 'pg_toast%'`), not merely
`public` — confirmed empirically: a real `projections.users14` table
(instance_id/id composite primary key, matching `zitadel`'s exact
column shape) and a `projections.orgs1` table were created in a live
Postgres 16 instance and correctly enumerated by `infer`. Table-name
versioning is irrelevant to `schema.Classify`, which operates purely on
column names, never table names — also confirmed empirically, both
before and after the fix below.

**The real blocker, found only by running `infer` for real: `instance_id`
is not in `schema.Classify`'s `candidateColumns` list**
(`"tenant_id", "org_id", "organization_id", "workspace_id", "account_id",
"owner"` — `internal/schema/schema.go`). Run as-is against the two
`zitadel`-shaped tables above: **`infer` reports 0 scoped, 2 unscoped**
— `"no tenant-column candidate found; believed global"` for both,
completely missing the real, hard `instance_id` partition confirmed in
§7.13. This is the worst possible failure mode for this tool
specifically: an `Unscoped` classification means no RLS synthesis, no
`A1`/`A4` proof, and Tier 2's `resolveReferences` treating every query
against these tables as contributing nothing to `anyScoped` — real
tenant-partitioned tables audited with zero scrutiny, silently — this
was found by testing inference against a real schema, not by reading
the code. **Fix landed, proven red before green, plus a mutation**
(§7.15, `TGD-BL-07`): `candidateColumns` extended with `"instance_id"`
(`resource_owner` deliberately excluded, consistent with §7.13 — it
would also introduce a genuine ambiguity on `users14`, which carries
both columns).

**Summary for the decision this section exists to inform:** the
checklist passes for `instance_id`; session handling confirmed `L5`
stays a blind spot exactly as it does for `coder`; policy inference's
one blocker is now fixed and proven, not merely diagnosed. `M4`'s
`zitadel` work proceeds on `instance_id` specifically — see §7.15 for
the fix's own red/green/mutation proof and the full pipeline run against
real `zitadel`.

### 7.15 `instance_id` fix landed and proven; pipeline run against real
`zitadel` found a genuine new defect at `verify` — `TGD-BL-46` filed,
run stopped before `audit`, per instruction

**The fix, proven, not just tested-and-reverted.** `internal/schema/schema.go`'s
`candidateColumns` now includes `"instance_id"`
(`resource_owner` deliberately excluded — §7.13). Red before green:
`TestClassify_ZitadelInstanceIDIsCandidate` (`internal/schema/schema_test.go`),
built against `zitadel`'s exact real `projections.users14` shape
(`id`, `instance_id`, `resource_owner`, `username`, `creation_date`),
confirmed failing (`class = unscoped`) before the fix, passing
(`class = scoped`, `tenant_column = instance_id`, no ambiguity —
`resource_owner` correctly excluded) after. `TestAllCandidateColumnNames`
extended the same way. **Mutation:** the fix was reverted (the
`candidateColumns` entry removed, doc comment left in place), both tests
confirmed failing identically to before, then restored — full
`internal/schema` suite green, `gofmt`/`go vet` clean, no other package
touched.

**Real pipeline stood up, not simulated.** A real, unmodified
`ghcr.io/zitadel/zitadel:latest` (`v4.17.2`) container, `start-from-init`,
pointed at a real `postgres:18` container through `tenantguard capture`
running as a container on the same Docker network — `tenantguard`'s own
proxy in front of `zitadel`'s real Postgres traffic, exactly `TGD-US-04`'s
model, `ZITADEL_DATABASE_POSTGRES_DSN` as the injection point confirmed
in §7.14. Migrations, projection prefilling, and the first-instance
bootstrap all ran through the proxy for real, plus a handful of real
HTTP requests (`/debug/healthz`, `/.well-known/openid-configuration`) —
**5,167 real captured events**, genuine `pgx`/`pgxpool` traffic.

**`TGD-FR-05`'s `pgx` claim settled against real traffic — and refined,
not merely confirmed.** Every one of 2,952+ real `Bind` events resolved
correctly (0 unresolved) — the capture layer handles real `pgx` traffic
cleanly. But the *exact* naming scheme observed was
`stmtcache_<48-hex-chars>` (e.g. `stmtcache_fb14e36d690338cfb195a0a3b01159caa0173620bf69e5eb`),
traced to `pgx` v5.9.2's `internal/stmtcache/stmtcache.go`:
`"stmtcache_" + hex.EncodeToString(sha256.Sum256([]byte(sql))[0:24])` —
`pgxpool`'s own shared statement cache, used because `zitadel` connects
through a pool (as any real production service does), not the bare
`*pgx.Conn` inline path (`conn.go`'s `"stmt_" + hex...`) this project's
design doc previously cited. Both are genuine "SHA of SQL" schemes,
differing only in prefix and code path — `TGD-FR-05`'s description is
accurate in substance; its own evidence is now real, not synthetic, and
more precise about which of `pgx`'s two internal naming paths actually
appears in production traffic.

**`infer` against the real schema: 144 relations — 121 scoped, 11
unscoped, 12 unclassifiable.** Reviewed for plausibility, not merely
counted. Every `Unscoped` result is a genuinely global/system table:
`projections.instances` (the instance registry itself — correctly has
no `instance_id` to scope it to), `cache.*`, `queue.river_*` (a Go job
queue library's own infrastructure tables), `system.encryption_keys`,
`projections.system_features4`. **A composite-tenancy shape `coder`
never exercised, caught correctly rather than mis-scoped:** five tables
(`auth.org_project_mapping`(`2`), `projections.org_domains2`,
`org_members4`, `org_metadata2`) carry *both* `instance_id` and `org_id`
— a genuine two-level instance-then-organization tenancy `zitadel`'s own
data model has, which `casdoor`/`coder` never presented. `Classify`
correctly reports these `Unclassifiable` ("ambiguous... composite
tenancy which this version does not support") rather than silently
picking one — the conservative design working exactly as intended on a
real, novel shape. `eventstore.events2` (the core event-sourcing table)
is ambiguous between `instance_id` and `owner` — inspected directly:
`owner` here is the eventstore's own name for the identical
app-computed-visibility concept `resource_owner` names at the
projection layer (confirmed via `\d eventstore.events2`) — the same
correct exclusion reasoning (§7.13) applies, and `Classify`'s honest
ambiguity again prevented a wrong auto-pick. Six further `Unclassifiable`
entries are views (`eventstore.instance_members`/`instance_orgs`/
`org_members`/`project_members`/`role_permissions`,
`projections.login_names3`), correctly excluded — RLS cannot attach to a
view, exactly as designed. `projections.users14`, `orgs1`, and every
real `users14_*` sibling table classified `Scoped`/`instance_id`
correctly, as predicted in §7.14.

**`verify`: found a genuine new defect, not a clean pass — the actual
pipeline run doing exactly what §7.14's checklist reading could not.**
Ran against the real database (`zitadel-api` stopped first — `verify`'s
`CREATE DATABASE ... TEMPLATE` needs an idle source, itself confirmed
directly, not assumed). Aborted: `exit 10`, `"A1: negative control did
not withhold rows; the oracle is blind"`. **Root-caused by bisection,
then reproduced directly against the real database via `internal/oracle`'s
own exported functions** (`EnableRLS`/`SeedCanaries`/`CreateRestrictedRole`/
`CheckA1`, called the same way `cmd/tenantguard/oracle_gate.go` calls
them, not a hand-approximation): `internal/oracle/probe.go`'s
`CreateRestrictedRole` grants `USAGE`/`SELECT`/etc. **only on schema
`public`** — hardcoded, no per-relation schema awareness. **Every one of
`zitadel`'s 121 scoped relations lives outside `public`**
(`adminapi`/`auth`/`eventstore`/`projections`/`system`/`logstore` — zero
in `public`, confirmed directly against the generated policy), so the
restricted role can reach **none** of them: `permission denied for
schema adminapi` on direct reproduction. `coder` and `casdoor` both kept
every scoped table in `public`, so this was never exercised before —
exactly the "second schema surfaces a constraint/type class `coder`
never exercised" question this run existed to answer, on the very first
real target that doesn't share that property.

**A second, connected defect found in the same reproduction, not a
separate session's discovery:** the CLI path (`oracle_gate.go`)
discards `CheckA1`/`CheckA4`'s actual returned error — `_, _, a1err :=
oracle.CheckA1(...)` keeps only the pass/fail boolean — so the real
cause (`permission denied for schema adminapi`) never reaches the
operator. `ProofState.PolicyProven()`'s bare `ErrA1` fallback (fired
when `A1Checked` never went true — §"nothing seeded" path) surfaces
instead, reading exactly like an RLS-correctness failure. An operator
hitting this on a real, non-`public`-schema target would investigate
the wrong thing entirely — a fixable, well-understood usability defect,
found only because this session traced the abort to its actual root
cause rather than accepting the generic message.

**`TGD-BL-46` filed, both parts, per instruction — run stopped here,
`audit` not attempted.** `CreateRestrictedRole`/`runOracleGate` need
per-relation schema-aware grants (every distinct schema among the
policy's `Scoped` relations, not a hardcoded `public`), and `CheckA1`/
`CheckA4`'s real error text needs to reach the operator-facing message
rather than being discarded in favor of a generic, potentially
misleading one. Neither fix is implemented here — this is a new defect
shape, filed and stopped, not patched and pushed through, per the
standing instruction. **`verify` cannot currently prove a single one of
`zitadel`'s 121 real scoped tables; `audit`, which depends on `verify`'s
proof, was not run** — running it against an oracle that cannot prove
anything would produce a report with no trustworthy `SAFE`/`LEAK`
distinction underneath it, exactly the failure mode this whole project
exists to prevent, now caught in its own pipeline rather than shipped.

**Environment note:** the real `zitadel`/Postgres/proxy stack (Docker
containers, network) was built for this run and torn down afterward —
not left running. The capture file (5,167 events), generated policy
(144 relations), and this section's own findings are the durable record;
reproducing them requires standing the stack up again, documented above
in enough detail to do so.

### 7.16 `TGD-BL-46` fixed, both parts, proven; the pipeline run completed —
`verify` proven, `audit` ran, the ceiling breached and was NOT ratcheted

**Fix 1 — schema-aware grants, proven against `zitadel`'s exact shape.**
`internal/oracle/probe.go`'s `CreateRestrictedRole` now takes
`relations []schema.Relation` and grants `USAGE`, `SELECT`/`INSERT`/
`UPDATE`/`DELETE`, and sequence privileges on every distinct schema
among them — `public` is still always included, so every existing
fixture and target (`coder`, `casdoor`) is unaffected. Proven
red-before-green: `TestCreateRestrictedRoleGrantsNonPublicSchema`
(`internal/oracle/oracle_integration_test.go`) builds a table shaped
exactly like the real `adminapi.styling2` that broke `verify` — same
schema name, same tenant column — confirmed failing
(`permission denied for schema adminapi`) before the fix, passing
after. **Mutation:** the schema-derivation reverted to `public` alone
(doc comment left in place), the same test failed identically, then
restored. All five call sites updated (`cmd/tenantguard/oracle_gate.go`
and four test fixtures across `internal/oracle`/`internal/differ`) —
full repository suite green throughout.

**Fix 2 — the real cause now reaches the operator.**
`cmd/tenantguard/oracle_gate.go` gained a new field on `tableProof`
(`A1Err`/`A4Err`, the real error `CheckA1`/`CheckA4` returned) and a new
pure function, `nameUnderlyingTableError`, extracted rather than inlined
for the identical reason `aggregateProof` already is (its own doc
comment: no way to make one table's outcome diverge from another's
through the CLI's own end-to-end surface without a database-level
defect this project's design does not otherwise construct — direct,
database-free testing is the only way to mutation-test it at all).
When `PolicyProven()`'s generic `ErrA1`/`ErrA4` fires, it now looks up
which table recorded a real underlying error and names both the table
and the real cause, still wrapped so `errors.Is(err, oracle.ErrA1)`
(the CLI's own exit-code dispatch) is unaffected. Proven
red-before-green: `TestNameUnderlyingTableError_SurfacesRealCause`
constructs the exact shape this investigation hit (a generic `ErrA1`
plus a table recording `"A1: count restricted: pq: permission denied
for schema adminapi"`) and asserts the real cause reaches the returned
error's own text — failed when the function was mutated to `return err`
unchanged, passed with the fix restored.
`TestNameUnderlyingTableError_NoRealCauseKnownIsUnchanged` guards the
other direction: no table with a recorded error leaves the generic
message untouched, not wrapped with nothing useful added.

**`infer` re-run against the real database, byte-identical to §7.15's
result** (144 relations, 121/11/12) — confirms nothing about inference
itself changed, only the oracle/CLI layer.

**`verify`, for the first time against real `zitadel`: succeeded.**
`proven: true`, all four checks (`a1`/`a2`/`a3`/`a4`) pass. **14 of 121
scoped relations row-level proven, all via real sampling (no fixtures
needed); 107 structural-only**, every one for the identical reason
("no rows to sample (fewer than 2 exist) and no fixture supplied").
**Honestly measured against the prediction, not silently left
unexplained:** the "per-call instance isolation should do better than
`coder`'s 13/22" prediction was never actually tested by this run —
only the single real instance from `start-from-init`'s own bootstrap
exists (`SELECT count(DISTINCT instance_id) FROM adminapi.styling2` →
`1`; `projections.instances` lists exactly one row). Most of the 107
structural-only tables are per-instance-singleton configuration
(one label policy, one password policy, one login policy per instance)
— genuinely sparse with one instance, and plausibly cleared by sampling
with as few as one additional real instance, which this run did not
attempt to create (it would need `zitadel`'s System-API JWT-profile
authentication, a real but separate setup cost not incurred here).
Reported as an honest gap in what was tested, not as a refutation or
confirmation of the prediction either way.

**`audit` over the preserved capture (5,167 events, unchanged from
§7.15) ran to completion — not aborted, unlike the pre-fix attempt.**
Raw counts: **`SAFE` 942 (238 vacuous — both legs matched nothing),
`LEAK` 3, `UNATTRIBUTABLE` 4,798** (`no_declared_table` 3,021,
`structural_only` 39, `row_level_unattributed` 480, `non_query` 1,258).
**All four denominators, from the tool's own report:**

| Denominator | Value | Unattributable | Rate |
|---|---|---|---|
| `all_captured_queries` | 5,743 | 4,798 | 83.55% |
| `real_app_sql_any_table` | 4,485 | 3,540 | 78.93% |
| `real_app_sql_touching_any_declared_table` | 1,226 | 519 | 42.33% |
| `row_level_touching_real_app_sql` | 1,187 | 480 | **40.44%** |

**None of these four numbers is a trustworthy measurement of `zitadel`'s
`LEAK`/`SAFE` distribution, and none should be read as one.** The
capture is `start-from-init`'s own bootstrap and migration sequence,
not steady-state application traffic — and, as the next paragraph
establishes directly rather than by inference, 93.5% of what makes
`row_level_touching_real_app_sql`'s 480 `UNATTRIBUTABLE` queries
`UNATTRIBUTABLE` is one specific, already-understood mechanism
(`TGD-BL-35`) reacting to that specific traffic shape, not a
measurement of how well this tool attributes `zitadel`'s real queries
in general. **40.44% is `TGD-BL-35`'s measured blast radius on
migration-heavy traffic — not zitadel's attribution rate.** Every
further mention of this number in this document should be read that
way.

**The baselined ceiling (`TGD-NFR-03`/`TGD-BL-06`, `cmd/tenantguard/ceiling.go`'s
current `122.0/378.0 ≈ 0.3228`) is breached: 40.44% > 32.28%. Exit 3.
The ceiling is left at `122.0/378.0`, unchanged — not ratcheted, not
otherwise touched, per explicit instruction.** §7.1's own standing
caveat on this ceiling — "a second target measuring a meaningfully
different rate does not by itself mean this baseline is wrong... it
would mean the two targets differ, which is expected, not an error to
reconcile toward one number" — is exactly what this breach now confirms
with a real second measurement rather than only asserting it in
advance: `TGD-NFR-03`'s ceiling is demonstrated here to be
**target-and-workload-specific, not a general property of this tool's
attribution logic**, which is what §7.1 warned it might turn out to be
before any second target existed to check. This breach is recorded as
that confirming evidence, not as grounds to move the number in either
direction.

**What is actually driving the breach, inspected directly rather than
left as a bare number — and corrected here after this session's own
follow-up work found the first pass had over-attributed it (§7.18):** of
the 480 `row_level_unattributed` queries, two shapes dominate but are
**not the same limitation**, a distinction the first inspection missed
by grouping on SQL text/table name rather than checking each group's own
`reason` field. **`INSERT INTO eventstore.unique_constraints` (277,
57.7%)** is genuinely `TGD-BL-35`: its `reason` names the exact cause,
`"unrestricted re-execution failed: exec: pq: duplicate key value
violates unique constraint"` — the probe is a `TEMPLATE` copy of the
live, already-migrated database, so replaying `start-from-init`'s own
bookkeeping INSERTs against a probe that already contains them collides
by construction. **`INSERT INTO projections.current_states` (174,
36.3%)** is a *different*, unrelated, pre-existing limitation —
verified directly, not assumed from the table name a second time: its
own `reason` is `"parameter position(s) $7 could not be decoded"`, an
undecoded-binary-parameter case (the same accepted, already-designed
fail-closed behavior `TestExtractTenant_BinaryParameterIsUnattributableNeverSafe`
already covers), never a duplicate-key collision at all. `TGD-BL-35`'s
real share of this capture's `UNATTRIBUTABLE` population is 277/480
(57.7%), not 449/480 — corrected, with the verification shown, not
silently edited. **Named as a capture-composition caveat, not an
excuse, for the part that IS `TGD-BL-35`:** `coder`'s baseline capture
was steady-state application traffic (`dbpurge` test-suite CRUD); this
`zitadel` capture is dominated by one-time migration/bootstrap SQL, a
fundamentally different traffic mix that `TGD-BL-35`'s limitation hits
far harder. Comparing this rate directly against a ceiling calibrated
on `coder`'s traffic shape is informative but not a strict
apples-to-apples measurement — stated plainly, not smoothed over,
alongside the breach itself. The remaining
18 `row_level_unattributed` entries are migration-script comment lines
and `CREATE OR REPLACE VIEW` DDL statements captured as "queries" —
correctly unattributable (no tenant-query semantics apply to DDL at
all), a different, non-`TGD-BL-35` shape but equally not a defect.

**The 3 `LEAK` verdicts, inspected directly, not left as a raw count:**
all three are the identical query — a multi-CTE domain-to-instance
resolution (`WITH domain AS (SELECT instance_id FROM eventstore.fields
WHERE object_id = $1 AND field_name = 'domain') ...`), bound to
`'localhost'`. This is the bootstrap query that determines *which*
instance a request belongs to from its hostname — the mechanism
`authz.GetInstance(ctx)` (§7.13/§7.14) resolves before any other query
runs. It touches `eventstore.fields` (declared `Scoped` on
`instance_id`) with no predicate comparing `instance_id` at all — by
design, since determining the instance is this query's own job, not
something it can be scoped by in advance. `ExtractTenant` resolves an
empty-string tenant for it (there being no real predicate to read a
value from), and the differential re-execution correctly finds the
unrestricted leg returns a row the restricted (`tenant=""`) leg does
not — a genuine, reproducible `L1`-shaped result by this tool's own
defined semantics. **Stated plainly, per the standing instruction never
to smooth this over: not a confirmed `zitadel` security finding.** This
specific query's architectural role (resolving the instance itself)
plausibly makes an instance-scoped predicate a category error here, not
a missing one — the same standing caveat `coder`'s own `LEAK` findings
carry (§7.3): the tool proves what the SQL does, not what the
application's surrounding logic does before or after issuing it.

**No new defect shape found while completing this run** — `TGD-BL-35`
recurring at larger scale on a migration-heavy capture is the same,
already-filed limitation, not a new one; the 3 `LEAK` findings are the
tool's own semantics firing correctly on a real, if architecturally
special-cased, query shape. Full suite re-verified after both fixes:
`go vet`, `gofmt -l` clean, `go test -p 1 -count=1 ./...` all green
against real Postgres. Environment (Docker containers, network) built
for this resumed run and torn down afterward, same as §7.15.

**`zitadel` is validated as a second target for exactly what this run
actually established — stated narrowly, not as a general endorsement:**

1. `pgx`'s real `pgxpool` statement-naming scheme
   (`stmtcache_<sha256-hex>`) proven against 2,952+ real `Bind` events,
   0 unresolved — `TGD-FR-05`'s `[E]` tag now rests on real traffic,
   with the actually-observed code path named, not merely read from
   `pgx`'s own source (§7.15).
2. A genuine composite instance-then-organization tenancy shape
   (`instance_id` + `org_id` on the same table) correctly caught
   `Unclassifiable` rather than mis-scoped — a constraint shape
   `coder` never presented (§7.15).
3. Non-`public` PostgreSQL schemas — a whole class of real-world layout
   both `coder` and `casdoor` happened to avoid, and the direct source
   of `TGD-BL-46` — found, fixed, and proven (this section).
4. The oracle proves what it can and abstains honestly on the rest: 14
   of 121 real scoped relations row-level proven via real sampling, 107
   correctly reported structural-only rather than guessed at.

**What this run did NOT establish, stated equally plainly:** a
trustworthy `LEAK`/`SAFE`/`UNATTRIBUTABLE` distribution for `zitadel`,
or any rate that should be compared to another target's as if measuring
the same thing. The capture is `start-from-init`'s bootstrap sequence,
and `TGD-BL-35` dominates it — 40.44% is a measurement of that
limitation's blast radius on migration-heavy traffic, not a property of
`zitadel`'s own code or of this tool's attribution quality against it.
No recapture was attempted to produce a cleaner number — the
measurement of `TGD-BL-35`'s real-world cost is the more useful result
of the two, not a defect in this run to be corrected before closing it.

### 7.17 `TGD-BL-35` reassessed: two independent measurements now exist;
recommend it move ahead of `M5` work

`TGD-BL-35` was filed as a deferred design question (design doc backlog,
"a design review of write re-execution against a probe seeded with real
historical data... None evaluated here"). It now has two independent,
real measurements of its cost, not zero:

- **`coder`:** "125 (later re-measured at a smaller count...) real
  captured `INSERT`s" — a bounded, minority slice of that target's
  `UNATTRIBUTABLE` population, `TGD-BL-38`'s own note (design doc)
  observes it moved the ceiling's numerator by "one more `TGD-BL-35`-shaped
  duplicate-key case" between baselines — small in absolute terms there.
- **`zitadel`:** 277 of 480 `row_level_touching_real_app_sql`
  `UNATTRIBUTABLE` queries (57.7%) — the dominant single cause of this
  target's entire unattributable population (a second, unrelated,
  pre-existing limitation — undecoded binary parameters — accounts for
  a further 174/480 (36.3%), corrected in §7.16 above from an earlier,
  over-attributed 449/480 figure that grouped both shapes together by
  table name instead of checking each group's own `reason` field), and
  a large enough share on its own that its measured rate still cannot
  be read as an attribution-quality signal for `zitadel` in general
  (§7.16 above).

**This is no longer a minor, bounded gap on the margins of one target's
measurement — it is, on real second-target evidence, capable of being
the largest single source of unattributable verdicts this tool
produces**, specifically on any target whose captured traffic includes
writes to rows the probe's own template already contains (migration
bootstraps, idempotent upserts, retried writes, anything with a
narrow/predictable key space) — a traffic shape `zitadel`'s real capture
happened to be dominated by, and one no target examined so far has been
free of entirely.

**Recommend: yes, move it ahead of `M5` work**, for a reason stated
precisely rather than asserted: `M5`'s own exit criterion is "every
`[U]` either baselined or withdrawn," and `TGD-NFR-03`'s ceiling — the
metric `TGD-BL-35` most directly corrupts — cannot be honestly
baselined against any target whose real traffic resembles `zitadel`'s
capture while `TGD-BL-35` stays unfixed; a v1.0.0 that ships without
addressing this is shipping a `LEAK`/`SAFE` distinction whose most
significant blind spot is not stated as a inherent property of tenant
isolation but as an artifact of how the tool re-executes captured
writes against its own probe. Deferring it further compounds the
`M5` bar it blocks, rather than sitting beside it.

**What a real fix requires, named rather than left to a future
design pass, drawing on `TGD-BL-35`'s own filed candidates (design
backlog) now weighed against what §7.16 actually observed:**

- **Re-execute against a point-in-time snapshot taken before the
  capture's own writes landed**, rather than a `TEMPLATE` copy of the
  live, already-written-to database. This is the structurally correct
  fix — it removes the collision at its root, a probe genuinely
  independent of the capture's own history — but costs the most: it
  needs either a `pg_dump`/restore or a logical snapshot taken at
  capture-start time, kept alongside the capture file as a new
  artifact this tool does not currently produce or manage, and a
  design decision about what happens when a query reads a row a
  *later*, still-in-the-same-capture write created (a real ordering
  dependency the current single-TEMPLATE-copy model sidesteps entirely
  by using the post-capture state for everything).
- **Treat "identical row already present with identical values" as
  `SAFE`-compatible, not an error.** Cheaper, but narrower and riskier:
  it only helps the sub-case where the collision is a true no-op
  replay (the exact row, unchanged) — `zitadel`'s own dominant shape
  here (`INSERT INTO eventstore.unique_constraints`, a real, literal
  duplicate of a row the migration itself already wrote) plausibly
  qualifies, but this needs care not to fold a genuine constraint
  violation that indicates something *else* went wrong into the same
  "harmless" bucket.
- **Accept the gap, but report it distinctly** rather than folding it
  into the generic `UNATTRIBUTABLE`/`row_level_unattributed` population
  the way it is today — a `Population` value naming the `23505`
  collision specifically (`PopulationRowLevelDuplicateKeyOnReplay` or
  similar) would at minimum stop this limitation from silently
  inflating the unattributable rate under a label that reads as
  "the tool couldn't figure out the tenant," when the real story is
  "the tool knows the tenant; replaying this exact write against this
  particular probe was never going to be evidence either way." This
  does not fix the coverage gap, but it stops misrepresenting its
  cause — the cheapest of the three, and the one most in keeping with
  this project's own discipline of naming a gap precisely rather than
  leaving it to look like a different, more general failure.

None of these is implemented here — this section states the
reassessment and the design options, per instruction. **Decided and
implemented in §7.18, immediately following**, once `TGD-BL-35` was
reassessed as blocking `M5` rather than sitting beside it.

### 7.18 `TGD-BL-35` fixed: surgical delete-and-retry inside the existing
rolled-back transaction ("Option 1-lite"), proven against both measured
shapes, both targets re-run

**Decision: a narrower, structurally-equivalent variant of §7.17's first
option ("point-in-time snapshot re-execution"), not the second or
third.** Argued against each in turn, per instruction:

- **Not option 3 (report the collision as its own population).**
  Renaming the number without changing what the tool can answer is a
  mislabeling fix. It would make `row_level_unattributed` honest about
  *why* it can't answer, but the tool would still be unable to say
  `SAFE`/`LEAK` for a write whose key the probe already contains — which
  is exactly the traffic zitadel's real capture is dominated by. Cheap,
  but it does not move the needle §7.17 says `M5` needs moved.
- **Not option 2 (treat identical-row-replay as `SAFE`-compatible).**
  This is the one direction the user's own constraint rules out: a
  captured write whose replay collides must never resolve to `SAFE` on
  the strength of "the row already matches" alone, because the probe's
  pre-existing row and the captured write's row are not guaranteed to
  be the *same* row from the same tenant — they merely collide on the
  same unique key. Treating that collision as evidence of safety risks
  calling a genuine cross-tenant key collision `SAFE` precisely because
  it collided, which is backwards.
- **Option 1, but not literally.** The design doc's literal reading —
  `capture` taking a real point-in-time snapshot (`pg_dump`/restore or a
  logical snapshot) at capture-start, kept as a new artifact alongside
  the capture file — was assessed and rejected as too large for what
  the actual defect requires: it would give `capture` direct DB
  access/a DSN it does not have today, a new artifact lifecycle to
  manage between `capture` and `audit`, and a design decision about
  intra-capture write ordering (a query reading a row a *later* write
  in the same capture created) that the current single-`TEMPLATE`-copy
  model does not need to answer today. None of that machinery is
  actually required to fix the defect: the defect is that the probe's
  `TEMPLATE` copy already contains the exact row a captured write's key
  collides with. **What's structurally needed is not a new snapshot
  mechanism — it's for the probe, at the moment of replay, to test the
  write as if that one colliding row were absent**, which can be done
  by identifying the exact colliding row via Postgres's own structured
  `23505` error fields and deleting it inside the *same* rolled-back
  transaction, then retrying the statement once, all before the
  transaction is ever committed. This reaches the same result as a
  snapshot taken before that row existed, for the one row that matters,
  without capture-time snapshot infrastructure — and stays inside the
  existing rolled-back-transaction model every other write in this
  package already uses, so `§3.3` ("no writes to the target's database,
  no exceptions, no flag") holds exactly as it always has: the delete is
  never committed, same as the write it's clearing room for.

**What "point-in-time snapshot re-execution" was actually asked to buy,
named precisely rather than left as a phrase**: a probe state in which
a captured write's colliding key does not yet exist. The fix delivers
exactly that state, scoped to the one row that collides, instead of to
the whole database — cheaper, and sufficient, because nothing else in
the probe's history is relevant to whether *this* write is safe.

**Implementation (`internal/differ/differ.go`):**

- `conflictKey{Schema, Table, Cols, Vals}` plus `asConflictKey(err)`
  extract the colliding row's identity from Postgres's own structured
  `*pq.Error` fields (`.Code == "23505"`, `.Schema`, `.Table`,
  `.Detail`) via `duplicateKeyDetailPattern`/`parseDuplicateKeyDetail` —
  never guessed from the failed `INSERT`'s own bound arguments, which
  would be wrong for e.g. a composite key with only one differing
  column, or a key derived from a default/trigger rather than a bound
  parameter.
- `execRolledBack` (probe leg) now retries under a `SAVEPOINT
  tgd_conflict_retry`: on a `23505`, it rolls back to the savepoint
  (required — Postgres aborts the whole transaction on any statement
  error unless a savepoint precedes it; confirmed directly against real
  Postgres before this was added, see Errors and Fixes below), deletes
  the identified conflicting row via `deleteConflictingRow`, and retries
  the original statement, up to `maxConflictRetries = 3` times. The
  whole thing still rolls back at the end — the delete is never durable,
  exactly like the write it made room for.
- `execRolledBackAsTenant` (restricted leg) takes the `conflictKey`
  already resolved on the probe leg and attempts the *same* delete
  first, as the restricted role, before the real statement. **This is
  the safety-critical branch**: if the restricted role's own RLS policy
  does not admit that row for deletion — meaning the pre-existing row
  belongs to a tenant other than the one under test — the delete
  affects 0 rows (or fails), and `diffWrite` reports `Unattributable`,
  never `Safe` and never `Leak`, because the real restricted-leg
  statement was never actually attempted. The error text for this case
  was deliberately worded to avoid the substring `"row-level security"`
  (caught in review before it reached a test, not by a failing one) —
  `isRowSecurityViolation` matches on that substring to detect a
  genuine RLS rejection, and this path must not be classified as one,
  since no real write was tested here at all.

**Tests, red-before-green plus a mutation
(`internal/differ/differ_test.go`):**

- `TestDiff_DuplicateKeyOnReplay_SingleColumnKey_Resolves` — coder's
  measured shape: a single-column primary key (`org_members.id`),
  replaying the exact pre-existing row. Asserts `Verdict == Safe` and
  that the original row is untouched afterward (row count still 1) —
  proving the delete-and-retry never leaves a durable side effect.
- `TestDiff_DuplicateKeyOnReplay_CompositeKey_Resolves` — zitadel's
  measured shape: a 3-column composite primary key
  (`unique_constraints(instance_id, unique_type, unique_field)`).
  Same assertions.
- `TestDiff_DuplicateKeyOnReplay_RLSBlocksResolution_NeverSafe` — the
  safety test the user's constraint required: the pre-existing row
  belongs to tenant `"acme"`; the captured write claims the identical
  key for tenant `"globex"`. Asserts `Verdict != Safe`, `Verdict !=
  Leak`, and `Verdict == Unattributable` — the one behavior that must
  hold regardless of which design option was chosen.

All three failed red (pre-fix: `Unattributable`, `"duplicate key value
violates unique constraint"`) before the fix, and passed green after.
A mutation (inverting the `deleted == 0` check in
`execRolledBackAsTenant` to admit the delete-failed case as success)
was caught by `TestDiff_DuplicateKeyOnReplay_RLSBlocksResolution_NeverSafe`,
which turned from pass to fail — confirming the safety assertion is
load-bearing, not incidentally true. Mutant reverted after confirmation.
Full suite re-verified clean after: `gofmt -l .`, `go vet ./...`,
`go test -p 1 -count=1 ./...`, all green against real Postgres.

**Both pipelines re-run end-to-end with the fixed binary — zitadel from
the preserved capture (not recaptured, per instruction), coder from a
freshly regenerated capture (`coder`'s real `dbpurge` test suite run
through the tenantguard proxy, 11,569 events) — both `exit=0`, no
ceiling breach. Four denominators, from each run's own `audit` report:**

**`zitadel`** (`row_level_unattributed` down from 480 to 204 — the
277 genuine `TGD-BL-35` collisions from §7.16/§7.17 resolved; the 174
undecoded-binary-parameter cases and the ~5 DDL/FK-dependency cases
characterized below are unaffected, as expected, since neither is
`TGD-BL-35`):

| Denominator | Value | Unattributable | Rate |
|---|---|---|---|
| `all_captured_queries` | 5,743 | 4,522 | 78.74% |
| `real_app_sql_any_table` | 4,485 | 3,264 | 72.78% |
| `real_app_sql_touching_any_declared_table` | 1,226 | 243 | 19.82% |
| `row_level_touching_real_app_sql` | 1,187 | 204 | **17.19%** |

**`coder`** (fresh capture, same `dbpurge` traffic shape as this
target's historical baseline; `infer`/`verify` both matched the
historical baseline exactly — 132/22/95/15 candidates, 13/22 row-level
proven — confirming the fresh capture is a like-for-like re-measurement,
not a different traffic mix):

| Denominator | Value | Unattributable | Rate |
|---|---|---|---|
| `all_captured_queries` | 5,069 | 4,108 | 81.04% |
| `real_app_sql_any_table` | 2,379 | 1,418 | 59.60% |
| `real_app_sql_touching_any_declared_table` | 806 | 62 | 7.69% |
| `row_level_touching_real_app_sql` | 762 | 18 | **2.36%** |

**The 18 residual `row_level_unattributed` cases in the fresh `coder`
run, inspected directly, not left as a bare count: all 18 are the
fix's own safety fallback firing correctly on real traffic** — a
restricted-role delete that the RLS policy did not admit, reported
`Unattributable`, never `Safe`. This is the fix working as designed,
not a residual gap in it: the fallback exists precisely for the case
where resolving the collision isn't safely possible, and real traffic
exercised that path.

**zitadel's remaining 276 `row_level_unattributed` entries, fully
characterized, not left partially inspected:** 204 are the
undecoded-binary-parameter case from §7.16/§7.17 (unrelated to
`TGD-BL-35`, unaffected by this fix, already covered by
`TestExtractTenant_BinaryParameterIsUnattributableNeverSafe`); a further
5 are consequences of already-known, already-documented design
boundaries rather than a new defect shape — 2 are DDL
(`CREATE OR REPLACE VIEW` "cannot drop columns from view"; an `ALTER
TABLE ... DROP CONSTRAINT` naming a constraint replay order left
already dropped), which Tier 1's row-diffing model was never built to
attribute against, and 3 are identical `INSERT INTO
projections.apps7_api_configs` statements failing a foreign-key
constraint on replay — a single captured statement replayed without
the other statements of its original transaction lacks the sibling
row its FK depends on. All 3 FK cases still correctly resolved to
`Unattributable`, never `Safe` — the safety constraint holds here too,
even outside `TGD-BL-35`'s own scope. No new backlog entry filed for
either shape.

**`TGD-BL-35` closed** (design doc backlog entry updated; `LIMITATIONS.md`
updated to record the fix and what it does/does not resolve).

**`TGD-NFR-03` re-baseline: recommended, not set, per instruction.**
Recommend re-baselining the ceiling to `18/762 ≈ 2.36%` (coder's own
fresh measurement above) — consistent with the ceiling's own existing
methodology of being calibrated against `coder` as the primary target,
now measured post-fix rather than carried forward from a pre-fix
baseline. This is a real reduction from the current `122.0/378.0 ≈
32.28%` (both the row count and the rate fall, since `coder`'s
denominator itself grew between baselines as more traffic was
captured) and satisfies the ratchet rule (`TGD-US-07` AC-2: may only be
lowered by a later measurement, never raised without a recorded
backlog entry) in the direction it permits without one. Flagging the
same caveat every prior `coder` baseline has carried, not a new one
introduced by this fix: this fresh capture reused the shared
`coder_target` database (`C-7`), so the 18-row numerator is measured
against real but not test-isolated traffic. zitadel's 17.19% is not
proposed as the ceiling — its capture is bootstrap/migration-heavy by
construction (§7.16), not the steady-state shape the ceiling has always
been calibrated against — but is recorded here as the second
independent post-fix measurement the fix was required to clear, per
instruction, and it does clear the current (unmoved) ceiling as well
as the recommended one. **Accepted and set in §7.19, immediately
following.**

### 7.19 `TGD-NFR-03` ratcheted to `18.0/762.0` (coder, 2026-09-04); the
gate proven to fire on the new number, not just on the old one

**Set** (`cmd/tenantguard/ceiling.go`'s `unattributableCeilingRate`):
`18.0/762.0 ≈ 0.023622` (`9/381` simplified), replacing `122.0/378.0 ≈
0.32275`. Denominator `row_level_touching_real_app_sql`, per
`unattributableCeilingDenominator` (unchanged). Capture: `coder`'s real
`dbpurge` test suite, run through the proxy against a freshly migrated,
shared `coder_target` (§7.18's fresh re-run). Date: 2026-09-04.

**This is the fourth baseline this project has recorded, and the first
of a fourth, distinct kind — stated explicitly because the prior three
did not all move for the same reason and conflating them would misread
this one:**

1. `TGD-BL-06` → `TGD-BL-38` (lowered, `130/611` → `131/664`): a
   **coverage change** — broadening which tables counted as row-level
   moved what the denominator measured, not a change in what the tool
   could attribute.
2. `TGD-BL-38` → `TGD-BL-42` (raised, `131/664` → `122/378`): a
   **measurement correction** — every prior number had been computed by
   a differ with a real extraction bug (`extractFromInsert`'s cast
   handling) silently inflating the numerator; fixing the bug revealed
   the ceiling had never been a trustworthy floor to begin with.
3. **`TGD-BL-42` → this ratchet (lowered, `122/378` → `18/762`): a
   capability improvement.** Unlike either prior move, nothing was
   wrong with how the previous number was computed and nothing changed
   about what counts as row-level. `TGD-BL-35`'s fix (§7.18) lets Tier 1
   correctly resolve a class of write re-execution it previously could
   only report `UNATTRIBUTABLE` for — the ceiling fell because the tool
   got better at answering the question, not because a bug in the
   measurement was found or the population being measured changed. This
   is the distinction worth recording: the first three moves were each
   either "we were counting something different" or "we were counting
   the same thing wrong"; this one is "we can now answer more of what
   we were already correctly counting."

**Same traffic shape as the prior baseline, not a differently-composed
sample** — `infer`/`verify` against this fresh capture matched the
`122/378` baseline's own historical policy exactly (132/22/95/15
candidates, 13/22 row-level proven), so the movement from `122/378` to
`18/762` is attributable to the fix, not to a smaller or differently-
shaped capture the way `TGD-BL-38`→`TGD-BL-42`'s `664`→`378` denominator
change partly was (§7.16's own comment on `ceiling.go` names that
caveat explicitly for its own move; this one does not need it).

**`zitadel`'s own post-fix measurement (17.19%, §7.18) is recorded but
deliberately does not set this ceiling, and breaches it** — clearing
the old `32.28%` ceiling while breaching the new `2.36%` one. **Stated
here in advance, not left to be discovered as an apparent regression
later:** this is expected. `zitadel`'s capture is `start-from-init`'s
bootstrap/migration sequence (§7.16), not the steady-state traffic this
ceiling has always been calibrated against (`coder`'s own `dbpurge` CRUD
suite, every baseline including this one). A future `zitadel` run
measuring above `2.36%` is not, by itself, evidence the fix regressed —
it is the same target-and-workload-specific effect §7.1 predicted in
advance of any second target existing to test it, now observed a second
time against a lower ceiling.

**The gate proven to fire on the new number, following the same
discipline as its first proof (`TGD-BL-06`, §7.x/`ceiling_test.go`'s own
header comment) — above fails, at-or-below passes, mutating the
constant moves the boundary, deleting the check goes red:**

- **Above fails / at-or-below passes**, on the real ratcheted constant,
  not just symbolically: `TestCheckUnattributableCeiling_AboveFails`
  (`0.90`), `_AtOrBelowPasses` (exactly `unattributableCeilingRate`,
  plus `0.01`), `_OtherDenominatorsIgnored` (`0.005`), and
  `_NoMatchingEntryPasses` all re-run and passed against the new
  `18.0/762.0` constant — two of these (`_AtOrBelowPasses`'s "below"
  case, `_OtherDenominatorsIgnored`) needed their fixed comparison rates
  lowered (`0.10`→`0.01`, `0.05`→`0.005`) because both sat *above* the
  new, much lower ceiling and would have failed the moment the ratchet
  landed — caught by actually running the suite after the edit, not
  assumed safe. End-to-end (`cmd/tenantguard/main_test.go`, real
  Postgres): `TestAuditCLI_UnattributableCeilingFailsAboveIt`,
  `_PassesAtOrBelowIt`, `TestAuditCLI_UnattributableRateByDenominator`,
  and `TestAuditCLI_VacuousSafeExcludedFromAttributedDenominator` all
  re-run and passed; `_PassesAtOrBelowIt` and
  `_UnattributableRateByDenominator` needed their fixture SAFE-event
  counts raised (5→50, 4→50) for the same reason — their old, healthy
  `1/6 ≈ 0.1667` rate cleared the `32.28%` ceiling comfortably but
  breaches `2.36%`, so proving "a healthy rate still passes" needed a
  fixture actually healthy enough for the new bar.
- **Mutating the constant moves the boundary** — proven directly, not
  inferred from the unit tests above: with `unattributableCeilingRate`
  temporarily changed to `0.30`, a rate of `0.10` (`10/100`) passed
  (`err == nil`); reverting to the real `18.0/762.0` and re-checking the
  identical `0.10` rate failed
  (`"rate 0.1000 (10/100) exceeds the baselined ceiling 0.0236"`) — the
  same input flips outcome purely as a function of the constant,
  confirming the check reads the constant rather than a hard-coded or
  stale comparison.
- **Deleting the check goes red** — proven end-to-end against real
  Postgres, not just noted as an expectation: with `main.go`'s call to
  `checkUnattributableCeiling` temporarily wrapped in `if false`,
  `TestAuditCLI_UnattributableCeilingFailsAboveIt` failed (`exit code =
  0, want 3`); restoring the call made it pass again (`0.90s`). Both
  mutations were reverted immediately after confirming the expected
  failure, never left in place.

Full suite re-verified clean after every edit and after the final
revert: `gofmt -l .`, `go vet ./...`, `go test -p 1 -count=1 ./...`, all
green against real Postgres (a disposable `tgd-pg-verify` container,
removed after use).

### 7.20 M5's remaining owed items resolved: `TGD-NFR-05`/`-06` measured;
`TGD-BL-26` and `TGD-BL-45` formally withdrawn, with reasons

Working through §11's M5 exit bar directly: "every `[U]` either
baselined or withdrawn." Two rows (`TGD-NFR-05`, `TGD-NFR-06`) were
genuinely measurable now, without a new target or a human subject, and
were measured. Two backlog items (`TGD-BL-26`'s remaining clause,
`TGD-BL-45`) were assessed against what they would actually require and
are formally withdrawn, each for a reason specific to it — not deferred
a third time without a decision recorded.

**`TGD-NFR-05` (proxy added latency per query) — measured, `[E]`.**
Method: a real `tenantguard capture` process (built binary, not a mock)
proxying a real PostgreSQL 16 container on loopback; a Go client using
`database/sql`/`lib/pq`, one connection, issuing `SELECT 1` serially
(no pipelining, so each query's full round-trip is charged) directly
against Postgres and again through the proxy, timed both ways. Four
runs (2,000-3,000 queries each): direct ≈99-112µs/query; proxied
≈144-163µs/query; added overhead ≈35-52µs/query, averaging ≈45µs. No
target is set — this is a first measurement, per the same "recorded as
a number, not asserted as a requirement" discipline every other `[U]`
in this project has followed. Read alongside `R-6`: ≈45µs is negligible
against real network/query latency at any scale a real target's own
test suite or production traffic would present, and `TGD-BL-11`'s own
full-suite runs against `coder` (thousands of real queries) never
surfaced proxy overhead as a bottleneck in practice — `R-6`'s severity
is downgraded from Medium to Low on this evidence.

**`TGD-NFR-06` (memory during a 100k-query capture) — measured for the
single-connection case, `[E]`; the multi-connection case stays `[U]`,
narrowed and named, not silently carried forward as a bare "unmeasured"
the way it was before.** Method: the same real capture process, `VmRSS`
sampled once per second (`/proc/<pid>/status`) while one sustained
connection drove 100,000 `SELECT 1` queries through it (111,005 total
captured queries counting the latency runs on the same process).
Result: `VmRSS` stayed flat at 12.2-12.6MB for the entire run, no
upward trend at any sampling point — the events file itself grew
(12.4MB for 111,005 queries, ≈112 bytes/event) but the proxy process's
own memory did not. **What this does and does not establish, stated
precisely:** it directly measures the concern `internal/capture`'s own
per-connection statement maps could pose under sustained load on one
connection — no growth found. It does NOT measure the original
worry this row's text named, "statement maps grow per connection" in
the sense of many connections opening and closing — whether a
`Session`'s state is actually freed when its connection closes, under
churn (e.g. a test suite that opens and drops thousands of short-lived
connections) is a different property, untested here, and stays `[U]`
under a narrower, honestly-scoped description rather than the original
one being marked resolved by a measurement that didn't test it.

**`TGD-BL-26`'s remaining clause (`TGD-US-10` AC-1: "the test or code
path a `LEAK` was reached from") — formally withdrawn from `v1.0.0`,
not deferred again.** Investigated directly before deciding, not
assumed: `internal/capture`'s `Proxy` (`internal/capture/proxy.go`) is
a pure PostgreSQL wire-protocol relay — it sees bytes on a TCP
connection and nothing else. A `capture.Event` carries a connection id
(`Conn`) and the parsed SQL/params; it has no channel to the calling
Go process's call stack, goroutine, or test name, because that
information is never transmitted over the wire in the first place —
it exists only in the client process's own memory. Satisfying this AC
as written requires one of two things, both assessed and rejected:
(1) **target-side instrumentation** — a driver wrapper or SDK the
target application must import to tag each query with caller info
before it reaches the wire — which contradicts this tool's own,
repeatedly-stated design principle throughout this SRS and `capture`'s
own doc comment: point `--listen`/`--upstream` at a target's existing
connection string and run its existing test suite, no target code
changes required. Building this would not be "closing a gap" in the
current tool; it would be a different tool, with a different
integration cost model, for a different v-next. (2) **timestamp/
connection-id correlation against the test runner's own output** — a
heuristic, not a proof, and one this project's own evidence already
shows would be unreliable on real targets: `coder`'s test suite reuses
a shared database and (per `C-7`) pools/reuses connections across
tests in ways that already distort other measurements in this
document; correlating a `LEAK` back to "the" test by timestamp alone
would misattribute it whenever two tests overlap on the same
connection, which `C-7` already establishes happens. **Withdrawn, not
built, for this reason**: the AC as written is incompatible with this
tool's black-box architecture without a redesign this session was not
asked to undertake, and the cheaper alternative is unreliable enough to
risk a wrong triage pointer — worse than the current, honest "not
implemented and not claimed" state `TGD-BL-26` already leaves it in.
Recorded as a possible `v2+` direction (an opt-in SDK/instrumented
capture mode, a genuinely different product surface), not as
in-scope-but-incomplete work for `v1.0.0`.

**`TGD-BL-45` (a full `TGD-NFR-02` measurement against a genuinely
unfamiliar target) — formally withdrawn from `v1.0.0`'s exit bar, the
tool-mechanical sub-measurement (§7.9) standing as the metric's
permanent `[E]` component.** The full metric names two unmeasured
phases: human policy-review time, and live-capture-to-first-finding
wall-clock time. Assessed separately, because they do not have the
same reason: **live-capture-to-first-finding** is mechanically
measurable by an agent session — it is just running the pipeline
against a target and timing it — but requires standing up a *third*,
genuinely-never-profiled real target (`coder` and `zitadel` are both
now the most-examined repositories in this project's own history,
disqualifying either), vetted to the same five-question rigor `casdoor`
and `gitea` were put through (§7.10), migrated, and run through its own
real test suite end to end — a multi-hour undertaking on the scale of
the `zitadel` integration work already completed this project (Docker
networking, schema discovery, policy-fixture authoring, capture-proxy
wiring), not started this session and not something to rush into
degraded just to close a row. **Human policy-review time is a
different, harder kind of gap**: it is, by definition, a measurement of
a real human operator's (`Priya`'s) unaided first encounter with
`infer`'s output. No session of this project — this one included — has
had a human operator present to time; an AI agent reading the same
policy file is not a substitute measurement of that quantity, and
reporting agent-review time as if it approximated human-review time
would be exactly the overclaiming this project's own conventions
forbid throughout — it is a different cognitive process at a different
pace, and citing it under `TGD-NFR-02`'s name would misrepresent what
was actually measured. **Withdrawn on this basis, following the same
precedent `TGD-BL-05` already set** (closed "measured as far as this
environment honestly allows," §7.9): the full metric cannot be
honestly closed inside a Claude Code session without either a
not-yet-selected third target (a real but boundable cost, own future
work) or a human subject (a structural limitation of what this kind of
session can produce, not a cost that more session time would resolve).
Filed as continuing `v1.1+` work, split into its two real parts rather
than left as one bundled, repeatedly-deferred item: a third-target
live-capture timing (mechanical, schedulable), and a genuine human-
operator review-time study (needs a person, not an agent).

**`TGD-NFR-02`'s row stands as already written** (partially baselined,
§7.9's tool-mechanical `[E]` plus the full metric's now more precisely
reasoned `[U]`) — this section explains *why* `TGD-BL-45` is withdrawn
rather than attempted, it does not change what was already measured.

### 7.21 Third-party disclosure decision for `v1.0.0`: discloses nothing,
explicitly, not silently

`coder`'s 37 `LEAK` verdicts (§7.3/design doc `TGD-BL-32`) and
`zitadel`'s 3 (§7.16) are the only `LEAK` findings this project has
produced against a real, third-party-owned target. **Decision: `v1.0.0`
discloses none of them.** Stated as a decision, not an omission, because
this milestone's own exit bar (§11) names "any third-party finding
disclosed and resolved," and a reader should not have to infer from
silence whether that bar was met or skipped.

**Why, reviewed against what each set of findings actually is, not
assumed from the count:**

- **`coder`'s 37**, inspected directly rather than merely counted
  (§7.3/design doc's own notes): every one traces to an already-
  understood shape — a bare-`id` filter with no tenant predicate
  (`GetChatFileByID`, `GetProvisionerJobByID`, `GetProvisionerDaemons`,
  `GetChatFileMetadataByChatID`) or an `INSERT...SELECT`/CTE-aggregate
  shape `extractFromInsert`'s literal-`VALUES` path never covers
  (`GetWorkspaceAgentStats`/`InsertWorkspaceAgentStats`). Several of
  these query names are plausibly gated by an authorization check
  elsewhere in `coder`'s own Go application code that this differential
  cannot see and does not model (§2's own stated scope boundary,
  restated at every prior mention of these findings) — a bare-`id`
  lookup behind a middleware that already validated the caller owns
  that id is a completely ordinary, non-leaking pattern this tool has
  no way to observe from SQL text alone.
- **`zitadel`'s 3** are the identical multi-CTE domain-to-instance
  resolution query, bound to `'localhost'` — the mechanism that
  determines *which* tenant a request belongs to before any other query
  can be scoped at all (§7.16). An instance-scoped predicate is
  plausibly a category error for this specific query, not a missing
  one; it was flagged `LEAK` by this tool's own defined semantics
  (no predicate on a `Scoped` column), not because a human reviewer
  concluded it leaks.

**The actual bar for responsible disclosure to an open-source
maintainer** — named in §2's own stakeholder table ("an open-source
maintainer" is an *involuntary subject*; "findings must be private-
disclosure-ready, accurate, and never fabricated") — is higher than
"this tool's own Tier-1 semantics fired." It requires the specific
code-path/application-layer review this project has repeatedly and
explicitly declined to claim it performs: confirming no compensating
control exists between the SQL this tool observed and the request that
produced it. That is exactly `TGD-BL-26`'s withdrawn provenance gap
(§7.20) — this tool cannot itself point at which code path issued a
`LEAK`, so it cannot itself clear that bar; only a human reading each
call site could. **None of these 40 findings have had that review
performed, by this project or by anyone else.** Filing any of them as a
security report without it would be reporting a Tier-1 SQL-shape match
as if it were a confirmed vulnerability — precisely the overclaiming
this project's own conventions have refused at every prior mention of
these same findings.

**Decision, recorded rather than left implicit:** `v1.0.0` discloses
nothing to either project. This is not a failure to meet §13's
success metric ("≥1 real isolation finding reported... disclosed
responsibly") — that metric is measured at GA + 60 days, not at
`v1.0.0`'s own exit, and it names a *bar to clear before reporting*
("disclosed responsibly"), not a count to hit regardless of confidence.
Reporting an unconfirmed finding now to hit the number sooner would
satisfy the metric's letter while violating exactly what "responsibly"
in its own name requires. **What would need to happen before any of
these 40 could honestly be disclosed:** a human reviewer reads the
actual call site(s) in `coder`'s or `zitadel`'s own Go source for the
specific query, confirms no authorization/tenant-scoping happens
upstream of the SQL this tool observed, and only then treats the
finding as a real, disclosable one — the manual version of the
provenance work `TGD-BL-26` would have automated, still possible by
hand, just not done in this session.

### 7.22 `TGD-BL-48`: `TestVerifyCLI_ProbeDatabaseIsAlwaysDropped` flaked
under `-race` from a test-isolation gap, not a product defect — fixed

**[E] The failure.** `go test ./... -race` and
`go test ./cmd/tenantguard ./internal/oracle -race -count=1` intermittently
failed `TestVerifyCLI_ProbeDatabaseIsAlwaysDropped`
(`cmd/tenantguard/main_test.go`) with `probe database count changed 0 -> 1;
a probe leaked on one of the two runs`. `go test ./cmd/tenantguard -race
-count=5` and any `-run`-filtered invocation always passed. Reproduced
directly, not inferred from the report: reverting the test's own query
predicate back to its original form and running
`go test ./cmd/tenantguard ./internal/oracle -race -count=1` failed on the
first attempt with the identical message and line; restoring the fix and
re-running the identical command passed, then passed again on three further
unfiltered runs of the same two-package combination.

**[E] Root cause, inspected directly.** The test's own `countProbes` helper
queried `pg_database` with `datname LIKE 'tgd_probe_%'` — a cluster-wide
predicate with no connection to which process created the row. The CLI
names its own probes `tgd_probe_<nanosecond-timestamp>`, all digits
(`cmd/tenantguard/oracle_gate.go`, `fmt.Sprintf("tgd_probe_%d",
time.Now().UnixNano())`). `internal/oracle`'s own test fixtures create
their own, differently-shaped `tgd_probe_<word>` databases (e.g.
`tgd_probe_dropcheck`, `tgd_probe_verify_ok` — `oracle_integration_test.go`,
`canary_type_test.go`) against the **same** PostgreSQL cluster, as a
**separate test binary** with its own lifecycle. `cmd/tenantguard`'s own
tests never call `t.Parallel()`, so no two CLI tests overlap this one —
the interleaving is between the two separately-compiled test binaries `go
test` runs concurrently when given both package paths, not within either
one. When an `internal/oracle` fixture's probe existed at the instant this
test sampled `pg_database` (before that fixture's own cleanup ran, or
because two fixtures were briefly both live), the wide `LIKE` pattern
counted it as if it belonged to the CLI under test, and the assertion — a
before/after equality check — failed on a count that had nothing to do
with the CLI's own behaviour. **After a full run completes, no
`tgd_probe_*` database survives in either scheme** — confirmed directly by
querying `pg_database`, not assumed. No probe leak exists, and `§3.3`
(no writes to the target's own database, only to disposable probes this
tool creates and drops) is not implicated: the disposable probes on both
sides of this test's confusion were being dropped correctly by their own
owning process the whole time.

**Fixed:** `countProbes`'s predicate narrowed to `datname ~
'^tgd_probe_[0-9]+$'` — a regex anchored to the CLI's own, all-digits
naming scheme, which no `internal/oracle` fixture name can ever match
(every one is a Go identifier-shaped word, never purely numeric). The
assertion itself is unchanged: it still fails if the CLI's own probe count
changes across the two `verify` runs it drives. A comment above the
function names the reason the pattern must stay this narrow, so a future
widening does not silently reintroduce the same nondeterministic failure.

**Not a new product defect, and not introduced by the `fix/stale-help-text`
PR.** The naming collision between the CLI's and `internal/oracle`'s test
probes has existed since both were written; the PR that first exposed this
failure changed only `--help` text and an unrelated Go doc comment, touching
neither `main_test.go` nor any probe-naming logic — it surfaced a latent
test-isolation gap by scheduling luck (which package's tests happened to
interleave with which), it did not create one. Filed and fixed in the same
session per the same discipline every other defect in this document
follows: found by running the suite, root-caused by direct inspection
before writing a fix, and proven red-before-green rather than asserted
fixed by inspection alone.

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
| `R-6` | Proxy overhead makes a real suite too slow to audit. | Low | Measured (`TGD-NFR-05` **[E]**, §7.20): ≈45µs/query added latency, single connection, loopback — negligible against real network/query latency at any meaningful scale. `TGD-BL-11`'s own full-suite runs (`coder`'s `dbpurge`, thousands of real queries) never showed proxy overhead as a bottleneck. |
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
| **M0 — SRS accepted — `CLOSED`** | This document reviewed. `TGD-BL-01` (name availability) **closed** (§7.8): the check was run against GitHub, `pkg.go.dev`, and npm and found a real naming collision (36 existing GitHub repos, an existing Go module with the identical `cmd/tenantguard` binary name, and a published `tenantguard-cli` npm package in the same problem space). **Decided: keeping the name** — the npm collision is a different ecosystem with no possible Go build collision and no legal issue found; the only real cost is search-result confusion, which does not justify cascading a rename through the `TGD-*` prefix across both spec documents. Closed on the decision, not merely on the check having run. |
| **M1 — Oracle proven — `CLOSED`** | `US-01`, `US-02`, `US-03`, `US-11` all closed. `TGD-BL-10`, `TGD-BL-15`, `TGD-BL-16`, `TGD-BL-17`, `TGD-BL-18`, `TGD-BL-19`, `TGD-BL-20` — done. `A1`–`A4` implemented and proven individually and together (`M1`–`M8` harness, all mutants caught by their mapped gate); `TGD-US-01` AC-2/AC-3/AC-4 closed; the CLI wires `A1`–`A4` and `ProofState` behind a real, user-invocable path, proven end-to-end against the built binary, closing `TGD-US-02` AC-5; `TGD-BL-19` fixed — a skipped canary insert now fails immediately and distinctly (exit 2) rather than surfacing as a misleading `A1` abort. **`TGD-US-11` AC-6 closed last**: a CI pipeline exists and was *executed*, not only written — `actionlint` and `act` (both real tools, freshly installed, not hand-rolled) linted and then ran the workflow against real Docker and a real Postgres service container. Both directions of the core requirement were demonstrated: disabling `A1`'s inequality check (the same mutation that blinds `M8`) turned the pipeline red, naming the exact broken tests; reverting turned it green again. The silent-skip failure mode this AC exists to prevent was independently reproduced by removing the Postgres service entirely — the pipeline failed outright rather than passing having tested nothing. Four real defects surfaced by *running* the workflow, not by reading it, were found and fixed in the same pass (a missing `pg_isready` binary, `actionlint`'s git-repository-dependent discovery, missing PyYAML, and PEP 668's package-install restriction) — none would have been caught by a syntax check alone. **What remains explicitly `[R]`, not folded into this closure:** whether GitHub's actual hosted runner behaves identically to `act`'s local image in every respect, and the branch-protection "required status check" setting that is the literal GitHub mechanism for merge-blocking — a repository administration setting requiring push access, unconfigured and unverifiable from here. At the time this closure was written, the repository had no git history and no remote, so GitHub itself had never run this workflow — that fact was stated, not hidden behind the local proof. |
| **M2 — Tier 1 usable — `CLOSED`** | ~~`US-04`~~ — done (AC-1, AC-3–AC-8 `[E]`; AC-2 stays `[R]`, deliberately retained rather than closed — no target exercising explicit statement preparation has been measured). ~~`US-05`~~ — done, AC-1–AC-3 all `[E]`. ~~`US-06`~~ — done, AC-1–AC-6 all `[E]` (AC-5 closed this session, §7.5). ~~`US-07`~~ — done, AC-1–AC-3 `[E]`/narrowed-and-accepted. ~~`US-09`~~ — AC-1–AC-3 `[E]`; **AC-4 formally descoped to `M4`**, not closed here — `zitadel` third-target work belongs alongside `TGD-BL-09`, not duplicated across two milestones (rationale in `TGD-US-09` AC-4's own entry). ~~`US-10`~~ — AC-2–AC-4 `[E]`; **AC-1's "test or code path" clause formally descoped to `M5`**, not closed here — a cross-cutting capture-layer feature, not required to demonstrate Tier 1 produces real findings (rationale in `TGD-US-10` AC-1's own entry). ~~First real finding on `coder`~~ — done: 37 real `LEAK` verdicts, the corrected differential run (§7.3). ~~`TGD-NFR-02`~~ — **partially baselined** (§7.9): the tool-mechanical setup phases (`infer`+`verify`) measured at under 1 second against a real 132-relation schema, `[E]`; the full metric (human review + live-capture time) stays `[U]`, with a measurement protocol recorded and a new backlog entry (`TGD-BL-45`) filed for the genuinely-unfamiliar-target run it requires — not silently set, per the same discipline `TGD-NFR-03`'s ratchet uses. **Closed on this basis:** every item M2's own exit criterion names is now either done, narrowly-accepted-and-closed, or explicitly and reasoned descoped to a later milestone — nothing here is closed by asserting more than what was actually measured or built. |
| **M3 — Tier 0, validated on `coder` — `CLOSED`** | `US-08`. ~~`TGD-BL-06` ceiling baselined~~ — **done** (§7.1, ratcheted §7.3). ~~`US-08`~~ — **done** (§7.4: `internal/triage`, `tenantguard triage`, measured against `coder`). ~~`TGD-BL-04`~~ — **resolved, not in the direction hoped**: `casdoor` disqualified, wrong database engine (§ above). ~~`go-gitea/gitea`~~/~~`mattermost/mattermost`~~ — both vetted and disqualified on the same structural category, application-computed access control rather than database-enforceable tenant partitioning (§7.10/§7.11). **"Second target" removed from this milestone's own exit criterion and moved to `v1.1+`, decided explicitly (§7.13), not descoped quietly:** the substantive claim v1.0.0 makes — the differ, oracle, and both tiers correctly separate safe queries from leaking ones on a real, unfamiliar 132-relation schema (37 real `LEAK` verdicts, §7.3) — does not change with a second target; a second target strengthens generality but does not establish whether the approach works, which `coder` alone already does. §7.12's population finding (real Go/PostgreSQL open-source software visible enough to choose skews toward collaboration/access-control products, not strict B2B SaaS isolation) makes further open-ended search a poor use of remaining v1 effort. `LIMITATIONS.md` records what single-target validation does and does not establish. **Closed on this basis.** |
| **M4 — Tier 2 — `CLOSED`** | ~~`US-12`. `L5` detection demonstrated.~~ — **done** (§7.5: `internal/guardrail`, `TestDB_BlocksWrongTenant`, `TestCorpus_Tier2ExactSetEquality`'s `L5` case). Not a `coder` integration — that needs code changes in their repo and was explicitly out of scope for this slice. ~~`TGD-BL-09`~~ — **done**, session handling checked directly on both targets: `coder` and `zitadel` both confirmed to set no `SET`/`set_config` tenant context anywhere (§7.14/§7.15); `L5` stays a confirmed Tier 1 blind spot on both, `LIMITATIONS.md` records both, neither narrowed. ~~`TGD-US-09` AC-4/`TGD-BL-07`~~ — **done**: policy inference against `zitadel`'s real schema fixed and proven (§7.15) — the versioned-table/split-schema risk originally feared was a non-issue; the real, narrow blocker (`instance_id` missing from the candidate list) was fixed, red-before-green plus a mutation, then re-verified against the live database (121/144 relations scoped correctly). ~~`TGD-BL-46`~~ — **done**: the pipeline's own `verify`/`audit` run found and fixed two connected defects (non-`public`-schema grants; a discarded real error message), both proven red-before-green plus a mutation (§7.16), then the pipeline resumed and completed. **`zitadel` validated as a second target, narrowly, for what this work actually established (§7.16's own closing summary):** real `pgx`/`pgxpool` statement naming proven against 2,952+ real Binds (`TGD-FR-05` now `[E]` on real traffic, not synthetic); a genuine composite `instance_id`/`org_id` tenancy shape `coder` never presented, correctly caught `Unclassifiable`; non-`public` schemas — a whole real-world layout class both prior targets happened to avoid — found, fixed, proven; the oracle proving 14/121 row-level and abstaining honestly on the rest. **What this work explicitly did NOT establish: a trustworthy `LEAK`/`SAFE` distribution for `zitadel`.** The captured traffic is `start-from-init`'s bootstrap sequence, and `TGD-BL-35` (an already-filed, pre-existing limitation, not a new defect) accounts for 277/480 (57.7%, corrected from an earlier over-attributed 449/480 — §7.16/§7.17) of this capture's `UNATTRIBUTABLE` population — the measured 40.44% rate is `TGD-BL-35`'s blast radius on migration-heavy traffic plus a second, unrelated, unaffected limitation (undecoded binary parameters, 174/480), not a property of `zitadel`'s own code or a general attribution-quality signal, and must never be read as the latter. The baselined ceiling (`TGD-NFR-03`, `122.0/378.0≈32.28%`) was breached by this measurement and left unchanged at the time, per instruction — recorded as real evidence that this ceiling is target-and-workload-specific, exactly the possibility §7.1 flagged in advance of any second target existing to test it. **`TGD-BL-35` reassessed with two independent real measurements (§7.17), then fixed, proven red-before-green plus a mutation against both measured shapes, and re-verified on both targets (§7.18)** — the fix cleared both re-runs well under the ceiling (zitadel 17.19%, coder 2.36%), and a re-baseline of `TGD-NFR-03` to coder's fresh `18/762≈2.36%` is recommended in §7.18 (not set, per instruction). **Closed on this basis**: every M4 exit-criterion item is done or has its own accepted, precisely-scoped closure — nothing here is closed by asserting more than what §7.14–§7.18 actually measured. |
| **M5 — v1.0.0 — `CLOSED`** | All `M` stories — **M0–M4 now closed** (M0/M1 prior sessions; M2/M3 this project's own recorded decisions; M4 this session, §7.16/§7.17). **`TGD-BL-35` fixed this session (§7.18)** — reassessed ahead of the rest of this milestone's own work (§7.17), then designed, implemented, proven red-before-green plus a mutation, and re-verified on both real targets: zitadel's re-run fell from 40.44% to 17.19%, coder's fresh re-run measured 2.36%. **`TGD-NFR-03` ratcheted to `18.0/762.0` (§7.19)** — the recommendation was acted on, not just recorded; the first ratchet driven by a real capability improvement, gate re-proven able to fire on the new number specifically (above fails, at-or-below passes, mutating the constant moves the boundary, deleting the check goes red). **`TGD-NFR-05`/`-06` measured (§7.20):** proxy overhead ≈45µs/query (`R-6` downgraded Medium→Low); memory flat across 111k queries on one connection, the multi-connection case narrowed and left `[U]` rather than falsely closed. **`TGD-BL-26`'s remaining clause and `TGD-BL-45` both formally withdrawn (§7.20)**, each for a reason specific to it (architectural incompatibility with the tool's black-box design; a genuinely-unfamiliar third target and a human subject neither available this session), not deferred a third time without a decision. **Third-party disclosure: decided to disclose nothing for `v1.0.0` (§7.21)** — none of `coder`'s 37 or `zitadel`'s 3 `LEAK` verdicts have had the code-path review that would make disclosing them to an involuntary open-source maintainer responsible rather than an overclaim; this is a recorded decision, not a skipped exit-criterion item, and does not conflict with §13's own GA+60-day disclosure metric. `LIMITATIONS.md` completeness checked (§7.20/§7.21's own work): `TGD-BL-35`'s entry updated for the fix, two new entries added for gaps found while closing it out (undecoded binary parameters; DDL/cross-statement-dependency replay), one duplicated paragraph and one stale cross-reference fixed. **Closed on this basis**: every item this milestone's own exit criterion names is now either done, measured, or explicitly and reasoned withdrawn — nothing here is closed by asserting more than what §7.16–§7.21 actually measured or decided. **`v1.1+`:** a second, cleaner (non-bootstrap) capture against `zitadel`, or a different second target; the two withdrawn `TGD-BL-45` sub-parts (a third-target live-capture timing; a human-operator review-time study); the multi-connection memory-churn measurement `TGD-NFR-06` still owes; `TGD-BL-26`'s code-path provenance as an opt-in instrumented-capture mode; manual code-path review of any of the 40 `LEAK` verdicts, should responsible disclosure ever become viable. |

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
