# Known limitations

`L5` and `L7` below are confirmed blind spots of the current differ
(`internal/differ`), verified by running the fixture corpus
(`internal/differ/corpus_test.go`, design doc §7) against real PostgreSQL —
not predicted from reading the code. Each comes back `SAFE` when a leak may
actually be present, for a structural reason stated below, not a bug.

The entries below them are different kinds of limitation, not cases the
differ gets wrong: a caveat on what an `A1`/`A4` pass is evidence *of*
depending on how the table it ran against was seeded; undecoded binary
parameters and DDL/cross-statement-dependency replay failures, two
coverage gaps found while characterizing real-target traffic that
correctly fail toward `UNATTRIBUTABLE` rather than guessing; what
single-target validation does and does not establish; and `TGD-BL-35`
(**fixed**, kept here as a record of what the gap was and what the fix
does and does not resolve).

## L5 — correct predicate, wrong tenant value (Tier 1 only)

**Fixture:** `SELECT * FROM invoices WHERE tenant_id = $1`, bound to a tenant
value that is syntactically well-formed but is not the caller's actual
tenant (e.g. an application bug binds the wrong constant).

**Why it's invisible:** at Tier 1, the tool's only source for "which tenant
should this query have been scoped to?" is the value already present in the
query's own predicate — there is no independent ground truth to compare it
against. The query is self-consistent (unrestricted and restricted-as-that-
value produce identical results by construction), so it reports `SAFE`
regardless of whether that value is actually correct for the caller.

**What fixes it:** Tier 2 (context propagation — not yet built; see design
doc §11, `TGD-US-12`), which recovers the intended tenant independently of
the query text (e.g. from the request's authenticated session), and can
therefore detect a mismatch between claimed and intended tenant. The 24-
fixture corpus asserts `L5` as `SAFE` at Tier 1 and `LEAK` at Tier 2 —
two distinct expected sets, per design doc §7.4.

**Confirmed for `coder`:** `coder` emits no `SET`/`set_config` tenant
context anywhere in `coderd/database/` (read-only source inspection), so
`L5` is a confirmed Tier 1 blind spot for this target specifically, not
just a theoretical one.

**Also confirmed for `zitadel`, checked directly rather than assumed:**
a full local checkout was searched for `set_config`, `current_setting`,
and any `SET`/`SET LOCAL` statement (`TGD-BL-09`, SRS §7.14). The only
`SET LOCAL` found (`internal/v2/eventstore/postgres/push.go`) sets
`application_name` for `pg_stat_activity` observability, not tenant
context. Every real tenant filter (`instance_id`, `resource_owner`) is
passed as an ordinary bound query parameter embedded in each statement's
own `WHERE`/`JOIN` clause (`internal/query/user.go`, `org.go`), the same
shape as `coder`'s. `L5` is a confirmed Tier 1 blind spot for `zitadel`
too, for the identical reason — this does **not** narrow the entry.

## L7 — a view without `security_invoker` (every tier)

**Fixture:** `SELECT * FROM some_view` (or any predicate at all) against a
view created without `WITH (security_invoker = true)`.

**Why it's invisible:** PostgreSQL evaluates RLS for such a view using the
**view owner's** privileges, not the querying role's. This tool's
"unrestricted" baseline is, by necessity, a superuser connection (it must
see every row to have any ground truth at all). In essentially any real
target, the view's owner is the same privileged/migration role — so when RLS
is bypassed through the view, it is bypassed identically on *both* legs of
the diff: the restricted role sees exactly what the unrestricted baseline
sees, the comparison finds no divergence, and the verdict comes back `SAFE`
even when the view is genuinely leaking every tenant's rows to every caller.

Verified directly against PostgreSQL 18: a restricted, RLS-bound role
querying the base table correctly saw only its own tenant's row; the same
role querying a `security_invoker`-less view over the identical table saw
every tenant's rows.

**Why Tier 2 does not help:** unlike `L5`, this is not a tenant-attribution
problem — the tool correctly determines the tenant and correctly binds the
restricted execution to it. The flaw is in the comparison's own choice of
baseline (superuser bypass vs. RLS-restricted), which a view can collapse
onto itself regardless of which tier attributed the tenant. No configuration
of view ownership produces a genuinely detectable leak through this
construction: an owner without `BYPASSRLS`/superuser just makes RLS apply
correctly instead (the policy expression reads the *calling session's* GUC
regardless of whose privileges gate the `SELECT`), so there is no owner
choice under which this shape is both leaky and catchable.

**What would fix it:** not designed. Would require either re-executing the
"unrestricted" baseline through a mechanism that does not itself bypass RLS
(undermining what "unrestricted, sees everything" means for every other
fixture), or a separate, targeted check for views lacking
`security_invoker` — a different mechanism from row-diffing entirely,
out of scope for this differ. See `TGD-BL-23`.

## Fixture-seeded proof is not the same evidence as sampled proof

Not a blind spot in the differ's verdict logic — a caveat on what a
row-level `A1`/`A4` pass on a specific table actually demonstrates,
depending on `seed_source` (`TGD-BL-36`, present in every `verify`/`audit`
report's `tables_row_level` entries).

**What `sampled` proves:** `A1`/`A4` ran against the target's own real rows
— copied out of the live database via `CreateProbeDatabase`'s `TEMPLATE`
copy, with only the tenant column rewritten (§9.6). Every other column's
value, and every relationship between columns, is something the target
itself produced. A pass says the synthesised RLS correctly withholds and
correctly admits real application data on this table.

**What `fixture` proves, and does not:** the canary rows come from
`schema.Relation.Fixture` — literal SQL an operator typed into the policy
file (`TGD-BL-29`/`-37`), reviewed only as text, never checked against what
the application actually writes. `A1`/`A4` passing on a fixture-seeded table
proves the synthesised RLS policy correctly withholds and admits rows
**shaped the way the fixture author encoded the schema's constraints** — not
rows the target has ever produced or ever will. If a real row shape the
target actually writes violates an assumption the fixture encodes (a column
the fixture left `NULL` that production always populates with a value that
interacts with a `CHECK` the fixture never exercised; an enum label the
fixture picked that behaves differently from one it didn't; a composite key
combination the fixture's two rows happen not to collide on but a real
pair might), `A1`/`A4` never sees that shape and cannot rule it out. This is
the same *class* of gap `L5` names for tenant values — a Tier 1 constraint
of what the tool has actually observed — extended to the shape of the rows
themselves on any fixture-seeded table.

**Confirmed for `coder`:** as of `TGD-BL-37`, 9 of `coder`'s 22 declared
scoped tables (`chat_organization_model_overrides`,
`chat_user_model_overrides`, `groups`, `jfrog_xray_scans`,
`mcp_server_configs`, `workspace_agent_port_share`, `workspace_agent_stats`,
`workspace_app_stats`, `workspace_app_statuses`) are row-level-proven only
on fixture data, on a freshly-migrated database that has never held real
rows in them. Their `A1`/`A4` passes are real proof the oracle mechanism
works against those tables' schemas as encoded in the fixtures — not proof
against anything `coder` itself has ever written there.

**What would close it:** re-seeding from real rows once they exist — the
same target re-audited after real usage (a `coder` deployment with actual
chat/MCP/port-sharing/scan activity) would upgrade these tables from
`fixture` to `sampled` automatically, with no policy change, the next time
`verify`/`audit` runs, since sampling is always tried first (§9.6) and only
falls back to fixture when fewer than two real rows exist.

## Undecoded binary parameters — a bound value in a type this tool's
decoder does not cover (every tier)

**Not a case the differ gets wrong** — `TGD-NFR-17`'s own row predicted
this residual before it was ever observed on real traffic ("the decoder
covers a fixed type set; `zitadel` is untested and may bind types with
no decoder... which stay undecodable by design"); this entry records
where it actually showed up.

**The mechanism.** A `Bind` message can carry a parameter in PostgreSQL's
binary wire format instead of text. `internal/capture`'s decoder covers
a fixed set of OIDs; a parameter bound in a type outside that set (e.g.
`numeric`, `jsonb`, array types) is captured as raw bytes with no
recoverable value. When that undecodable parameter is the one a
predicate compares the tenant column against, `ExtractTenant` correctly
refuses to guess — it reports the query `UNATTRIBUTABLE`
(`"parameter position(s) $N could not be decoded"`), never `SAFE` or
`LEAK` from an unrecoverable value. This is the same accepted,
already-designed fail-closed behavior `TestExtractTenant_BinaryParameterIsUnattributableNeverSafe`
covers — verdict-safety is preserved; the cost is coverage, not
correctness.

**Confirmed on real traffic, not merely a predicted gap any more.**
`zitadel`'s real capture: 174 of 480 `row_level_unattributed` queries
pre-`TGD-BL-35`-fix (204 of 276 post-fix, once `TGD-BL-35`'s own,
larger share was resolved — SRS §7.16/§7.18) are `INSERT INTO
projections.current_states`, each failing with the identical
`"parameter position(s) $7 could not be decoded"` reason — distinct
from, and initially miscounted together with, `TGD-BL-35`'s duplicate-
key shape until each group's own `reason` field was checked
individually rather than assumed from the table name (SRS §7.16/§7.17,
a correction this project made of its own earlier work). This is now
the single largest component of `zitadel`'s residual unattributable
rate.

**What would close it:** extending `internal/capture`'s binary decoder
to cover whichever OID `current_states`' `$7` parameter actually is
(not yet identified in this project's own work) and any other types a
real target binds — the same kind of incremental, evidence-driven
extension `TGD-BL-14` already did once for the original binary-capture
gap. Not attempted this session; filed as future work rather than
guessed at without first identifying the actual type.

## DDL and cross-statement-dependency replay failures — outside Tier 1's
single-statement model by design (Tier 1, write re-execution only)

**Not `TGD-BL-35`, and not evaluated as a defect** — found while fully
characterizing `zitadel`'s residual `row_level_unattributed` population
after the `TGD-BL-35` fix landed (SRS §7.18), inspected directly rather
than left as an unexplained remainder.

**Two shapes, both outside what Tier 1's replay model ever claimed to
attribute.** (1) DDL statements captured as "queries" (`CREATE OR
REPLACE VIEW ... cannot drop columns from view`; an `ALTER TABLE ...
DROP CONSTRAINT` naming a constraint replay order already dropped) —
schema modification has no tenant-row semantics for a row-diffing
oracle to compare legs on at all. (2) A captured write replayed as a
single, isolated statement can depend on a *sibling* statement from the
same original transaction that Tier 1's per-statement replay never
re-executes alongside it — three identical `INSERT INTO
projections.apps7_api_configs` cases fail a foreign-key constraint on
replay for exactly this reason. **Both shapes still resolve to
`Unattributable`, never `Safe` — the same safety property `TGD-BL-35`'s
fix was required to hold applies here too, unprompted, because neither
shape reaches a point where a real restricted-leg write is ever
actually attempted.** No new backlog entry filed: closing either would
require capabilities (a DDL-aware differ mode; multi-statement
transactional replay) this project has never scoped Tier 1 to have, not
a bug in what Tier 1 already claims.

## Single-target validation — what it does and doesn't establish

v1.0.0 ships validated against exactly one real target, `coder/coder` —
a deliberate, recorded decision (design doc §10, SRS §7.12, M3's own exit
criterion), not an oversight or a quiet descope. Stated plainly what that
does and doesn't cover.

**What it establishes.** The differ, the oracle, and both Tier 0 and
Tier 1 are proven — not merely built — against one real, unfamiliar
132-relation production schema: 37 real `LEAK` verdicts on real `coder`
traffic (design §7.3), the mutation harness's all-eight-gates-fire proof
run against `coder`'s own migrated schema, and the fixture corpus's
24/24 (Tier 1) and 20/23 (Tier 2) exact-set results. This is the
substantive claim this tool makes: given a real schema and real traffic,
it correctly separates safe queries from leaking ones, at a scale and
level of schema complexity a hand-built fixture corpus cannot exercise
on its own.

**What it doesn't establish.** A single target's schema, however large,
is one sample of the space of real-world constraint and type shapes a
PostgreSQL application can use — not a claim that every such shape has
been exercised. Concretely, within this project's own history: `uuid`
tenant columns and their canary-casting requirements (`TGD-BL-27`),
cross-column `CHECK` constraints (`TGD-BL-37`'s fixture-authoring work),
and composite-unique-index interactions were all real shapes `coder`'s
own schema happened to contain and that surfaced defects or required new
handling when first encountered — not shapes anticipated in advance. A
second, differently-shaped target is exactly the mechanism that finds
the *next* such shape (a different tenant-column type, a different
constraint pattern, a different `INSERT`/`CTE` idiom) before a real
user's target does. Choosing not to seek one for v1 is a decision about
where to spend limited validation effort next, not a claim that no more
such shapes exist.

**Why a second target is deferred rather than pursued now (SRS §7.12):**
three real-world candidates were checked before this decision was made,
not zero — `casdoor` (wrong database engine), `go-gitea/gitea` and
`mattermost/mattermost` (both disqualified on the same structural
category: application-computed access control, not database-enforceable
tenant partitioning — see this document's own scope and SRS §3.2's
"Access-control applications" exclusion). That pattern, not merely one
failed attempt, is why an open-ended further search was judged a poor
use of remaining v1 effort rather than a promising path cut short.

## `TGD-BL-35` — a captured write colliding with the probe's own history
(Tier 1, `INSERT`/write re-execution only) — **FIXED, SRS §7.18**

**Fixed, not an open limitation any more.** Kept here as a record of
what the gap was, what the fix resolves, and — precisely, so this entry
doesn't overclaim — what it does not.

**The mechanism (unchanged description; this is what the fix
addresses) — a structural limitation of *how* Tier 1 re-executes a
captured write to check it, on a specific, identifiable class of query,
not a case the differ gets wrong.** The probe database Tier 1 re-executes captured
queries against is a PostgreSQL `TEMPLATE` copy of the target's own
live database (§9.6), taken at audit time — after the target has
already run the exact traffic being replayed. A captured `INSERT`
carrying a literal, already-used primary/unique key therefore replays
against a probe that already contains that literal row: the
`unrestricted` re-execution leg fails outright with a real Postgres
`23505` (duplicate key) error, not because the tenant scoping is wrong,
but because the write already happened once, for real, before the
probe was ever created.

**Why it's invisible to the fixture corpus.** Every fixture seeds a
synthetic, single- or dual-tenant table with no prior collision risk to
replay into — this mechanism only exists on tables carrying real,
already-written history, a property no hand-built fixture has by
construction.

**Verdict, not a crash:** the query is reported `UNATTRIBUTABLE`
(`PopulationRowLevelUnattributed`) — correctly refusing to guess
`SAFE`/`LEAK` from a re-execution that never actually completed — but
this means a real slice of real write traffic is unattributable for a
reason that has nothing to do with tenant-attribution logic at all.

**Confirmed on two independent real targets before the fix, not
asserted from one.** `coder`: a bounded, minority slice — around 125
real captured `INSERT`s at first measurement, later smaller once other
fixes shrank the `row_level_unattributed` population
(`TGD-BL-33`/`-34`/design doc's own history). `zitadel`: **277 of 480
(57.7%) of this target's entire `row_level_touching_real_app_sql`
`UNATTRIBUTABLE` population** — `INSERT INTO
eventstore.unique_constraints`, `zitadel`'s own internal migration
bookkeeping, captured during `start-from-init` and replayed against a
probe templated from the same, already-migrated database (SRS §7.16).
(Corrected here from an initial, over-attributed 449/480 figure that
grouped this table together with `projections.current_states`, a
separate, unrelated, unaffected undecoded-binary-parameter limitation —
the two were distinguished by checking each group's own `reason` field,
not by table name; see SRS §7.16/§7.17.)

**The fix (SRS §7.18): surgical delete-and-retry inside the existing
rolled-back transaction, not a capture-time snapshot.** The probe leg
identifies the exact colliding row via Postgres's own structured
`23505` error fields, deletes it under a `SAVEPOINT` inside the same
transaction the write is already being tested in, and retries the
write once — never a durable write, so `TGD-BL-03`/§3.3 hold exactly as
before. The restricted leg attempts the identical delete first; if RLS
does not admit it (the pre-existing row belongs to a different tenant),
the result is `Unattributable`, never `Safe` or `Leak` — the one
direction this fix was required never to fail toward. Proven
red-before-green plus a caught mutation against both measured shapes
(coder's single-column key, zitadel's composite key), and by re-running
both targets end-to-end: zitadel's rate fell from 40.44% to 17.19%
(`row_level_unattributed` 480→204); coder's fresh capture measured
2.36% (18/762), all 18 residual cases confirmed as this fix's own
safety fallback firing correctly on real traffic, not a residual gap.

**What the fix does not resolve, stated precisely so this entry doesn't
overclaim:** `zitadel`'s remaining 204 `row_level_unattributed` entries
(the undecoded-binary-parameter limitation, unrelated to this fix) and
a further ~5 DDL/foreign-key-dependency cases (outside Tier 1's
single-statement replay model entirely) are untouched by this fix, as
expected — neither is `TGD-BL-35`. `TGD-NFR-03`'s ceiling was left
unmoved by the pre-fix breach per instruction, then ratcheted to
coder's fresh `18/762≈2.36%` once the fix was accepted (SRS §7.19) —
the first ratchet driven by a real capability improvement rather than
a coverage change or a measurement correction. `zitadel`'s own post-fix
17.19% clears the OLD ceiling but breaches this new one — expected,
not a regression: its capture is bootstrap-heavy by construction, not
the steady-state traffic this ceiling has always been calibrated
against.
