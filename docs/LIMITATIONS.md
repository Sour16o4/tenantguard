# Known limitations

`L5` and `L7` below are confirmed blind spots of the current differ
(`internal/differ`), verified by running the fixture corpus
(`internal/differ/corpus_test.go`, design doc §7) against real PostgreSQL —
not predicted from reading the code. Each comes back `SAFE` when a leak may
actually be present, for a structural reason stated below, not a bug.

A third entry, below them, is a different kind of limitation — not a case
the differ gets wrong, but a caveat on what an `A1`/`A4` pass is evidence
*of*, depending on how the table it ran against was seeded.

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
