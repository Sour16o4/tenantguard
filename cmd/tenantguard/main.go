// Command tenantguard captures the SQL a Go application issues and proves
// whether a synthesised isolation policy can be trusted.
//
// Four subcommands. `capture` records queries only — it is not a verdict.
// `infer` reads a target's schema and proposes which tables are tenant-scoped
// (TGD-US-09) — a proposal for a human to review, never an assumption.
// `verify` runs the four oracle self-checks (A1–A4) against every scoped
// table in a reviewed policy file and reports whether the policy is proven,
// with no further claim. `audit` runs the same gate; if the policy is proven
// it additionally re-executes captured events differentially (TGD-US-06).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/Sour16o4/tenantguard/internal/capture"
	"github.com/Sour16o4/tenantguard/internal/differ"
	"github.com/Sour16o4/tenantguard/internal/schema"
	"github.com/Sour16o4/tenantguard/internal/triage"
)

const usage = `tenantguard — record SQL and prove whether an isolation policy can be trusted

usage:
  tenantguard capture --listen ADDR --upstream ADDR [--out FILE]
  tenantguard triage  [--output json|text] PATH
  tenantguard infer   --dsn URL --out FILE
  tenantguard verify  --dsn URL --policy FILE [--shared-database] [--output json|text]
  tenantguard audit   --dsn URL --policy FILE [--events FILE] [--shared-database] [--output json|text]

capture: point the target's PostgreSQL connection URL at --listen and run its
  test suite. The variable is target-specific: coder uses
  CODER_PG_CONNECTION_URL, not DATABASE_URL. Events are written as
  newline-delimited JSON. The proxy declines TLS (SRS C-2), so the target's
  DSN must not require TLS. Capture only — it does not judge isolation.

triage: Tier 0 (TGD-FR-09/TGD-US-08). A syntactic pass over PATH, a Go
  repository — no database, no target-specific setup, the only tier that
  keeps the "point at any repo" promise. Finds candidate SQL statements in
  *.sql query files and non-generated *.go source, infers which tables are
  probably tenant-scoped from column naming alone (never from a database or
  from a prior 'infer' policy file — see internal/triage's package doc for
  why), and flags a statement against a known-scoped table that never
  mentions that table's tenant column anywhere in its own text. Every
  finding is a ranked suspicion, not a verdict: the words "leak", "proven"
  and "violation" never appear in this command's output (TGD-US-08 AC-2),
  and it exits 0 regardless of what it finds (AC-1) — a bad PATH or a bad
  --output value is a usage error (exit 2) and is the only way this command
  exits non-zero. It is a naive presence check: a tautology (tenant_id =
  tenant_id) or an OR-defeated predicate (tenant_id = $1 OR status =
  'public') both mention the column and are deliberately NOT flagged
  (AC-3) — the documented reason Tier 1 exists.

infer: reads --dsn's schema and writes a policy file to --out, classifying
  every relation Scoped (exactly one tenant-column candidate), Unscoped (no
  candidate; believed global) or Unclassifiable (a view, an ambiguous table,
  or anything else the tool cannot reason about — never treated as Unscoped).
  This is a proposal only: TGD-US-09 AC-2's human-acceptance step is that an
  operator reads this file before pointing verify/audit at it with --policy;
  the tool itself enforces nothing beyond requiring the file to exist.

verify: reads --policy, attempts to seed two canary tenants into every table
  it classifies Scoped inside a disposable probe database built from --dsn's
  own database as a template, synthesises row-level security on each, and
  runs all four oracle self-checks per table. A2 (policy coverage) and A3
  (role privilege) are catalog-only and always run for every scoped table.
  A1 (withholding) and A4 (correctness) need seeded rows: a table with
  nothing to seed is reported "tables_structural_only" — A2/A3 proved, A1/A4
  never attempted, named with why — rather than aborting the whole run; a
  table that DID seed but failed A1 or A4 still aborts the whole run, since
  that is a genuine oracle defect, not a data gap. The run is proven once
  A2/A3 pass for every scoped table and A1/A4 pass for every table that
  seeded ("tables_row_level"). On success, a report is written to stdout
  (format per --output) and the process exits 0. On failure, stdout is left
  completely empty — no finding is ever printed from a policy that was not
  proven — a one-line reason goes to stderr, and the process exits with the
  code for whichever check failed: 10 (A1, the oracle can't see), 11 (A2, no
  policy covers some table), 12 (A3, the role can bypass RLS), or 13 (A4,
  some seeded table's policy is wrong or over-restrictive) — or 2 if not one
  scoped table could be seeded at all. --dsn is never written to; every
  write happens against the probe this command creates and drops.

audit: runs the identical gate over every scoped table in --policy. If not
  proven, it behaves exactly like verify: empty stdout, the mapped exit code,
  no exception. If proven and --events is given, it re-executes every
  captured query in that file (produced by a prior 'tenantguard capture --out
  FILE' run) under the probe and its restricted role — never against --dsn's
  own database — and adds a per-query SAFE/LEAK/UNATTRIBUTABLE verdict, its
  full bound parameter list, and (for LEAK) the withheld row count. A query
  attributed to a "tables_structural_only" table (see verify above) is never
  re-executed at all — it comes back UNATTRIBUTABLE, the same as a query
  against a table the policy never declared, since that table was never
  proven at the row level in the first place. If proven
  but --events is omitted, it prints the same report as verify and a stderr
  note that no events were supplied, so no per-query verdicts exist. Every
  report carries "shared_database" (--shared-database; an operator
  declaration, not automatic detection — TGD-FR-18), "tier" (always 1 in this
  build) and "proof_source" (always "probe": every check runs against the
  disposable probe database, never --dsn's own data — TGD-US-10 AC-3). When
  --events produced at least one verdict, the report also carries "counts"
  (SAFE/LEAK/UNATTRIBUTABLE, summing to the number of verdicts — TGD-US-05
  AC-3 / TGD-US-10 AC-3), "unattributable_rate", and
  "unattributable_rate_by_denominator" (the same rate against four
  candidate denominators — all_captured_queries, real_app_sql_any_table,
  real_app_sql_touching_any_declared_table, row_level_touching_real_app_sql).
  TGD-NFR-03's ceiling IS baselined (currently 18.0/762.0, ≈2.36%) and DOES
  fail the run: if row_level_touching_real_app_sql's own rate exceeds it,
  the full report still reaches stdout — a breach is a completed, proven
  run, not an aborted self-check — but the process exits 3, with a stderr
  line naming the denominator, the measured rate, and the ceiling.

  Known gap (TGD-BL-26): a LEAK verdict names the SQL, parameters and table,
  but not the test or application code path that issued it — capture has no
  provenance beyond the connection id, so this build does not claim it.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "capture":
		os.Exit(runCapture(os.Args[2:]))
	case "triage":
		os.Exit(runTriage(os.Args[2:]))
	case "infer":
		os.Exit(runInfer(os.Args[2:]))
	case "verify":
		os.Exit(runGateCommand("verify", os.Args[2:], false))
	case "audit":
		os.Exit(runGateCommand("audit", os.Args[2:], true))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func runCapture(args []string) int {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:6432", "address to accept client connections on")
	upstream := fs.String("upstream", "127.0.0.1:5432", "address of the real PostgreSQL server")
	out := fs.String("out", "", "write JSONL events here (default: stdout)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sink := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tenantguard: create %s: %v\n", *out, err)
			return 1
		}
		defer f.Close()
		sink = f
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard: listen on %s: %v\n", *listen, err)
		return 1
	}
	defer ln.Close()

	fmt.Fprintf(os.Stderr, "tenantguard: capturing %s -> %s (capture only; no isolation verdict)\n",
		*listen, *upstream)

	p := &capture.Proxy{Upstream: *upstream, Sink: capture.NewRecorder(sink)}
	if err := p.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard: %v\n", err)
		return 1
	}
	return 0
}

// runTriage implements Tier 0 (TGD-FR-09/TGD-US-08): a syntactic pass with
// no database precondition — see internal/triage's package doc for the
// table-list decision (column-naming heuristic, not infer's policy file)
// and every other design choice.
//
// The exit-code split documented here is deliberate, not an oversight
// against "never exits non-zero, ever": that guarantee is AC-1's, and AC-1
// is about the ANALYSIS ("exits zero regardless of findings") — it does not
// extend to telling the CLI what to do when it was not told what to
// analyse at all. A missing/malformed --output value, or triage.Run itself
// reporting the given path does not exist or is not a directory, both
// return 2 — usage errors, exactly like every other subcommand's --dsn/
// --policy validation (runGateCommand, runInfer) — never triage.Run's own
// findings. Once triage.Run actually starts walking real files, nothing
// past that point can produce a non-zero exit: an unreadable file or
// directory is skipped internally (triage.Run's own doc comment), never
// surfaced as an error here.
func runTriage(args []string) int {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	output := fs.String("output", "json", "report format: json or text")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "tenantguard triage: exactly one argument required — the repository root to scan")
		return 2
	}
	if *output != "json" && *output != "text" {
		fmt.Fprintf(os.Stderr, "tenantguard triage: --output must be json or text, got %q\n", *output)
		return 2
	}

	rep, err := triage.Run(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard triage: %v\n", err)
		return 2
	}

	if *output == "text" {
		writeTriageText(os.Stdout, rep)
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			// A JSON-encoding failure is this process's own bug, not a
			// finding about the scanned repo — still exit 0 per AC-1's
			// letter (nothing about the ANALYSIS failed), but say so loudly
			// on stderr since a caller piping stdout would otherwise see an
			// empty or truncated report and no explanation.
			fmt.Fprintf(os.Stderr, "tenantguard triage: encode report: %v\n", err)
		}
	}
	return 0
}

// writeTriageText renders rep in the same --output text spirit
// runGateCommand's own text writer uses, but with vocabulary AC-2 requires
// stay visibly distinct from a Tier 1 report: "suspicion", not "verdict";
// "unverified", never "proven".
func writeTriageText(w io.Writer, rep triage.Report) {
	fmt.Fprintln(w, rep.Label)
	fmt.Fprintf(w, "repo_root: %s\n", rep.RepoRoot)
	fmt.Fprintf(w, "files_scanned: %d\n", rep.FilesScanned)
	fmt.Fprintf(w, "statements_found: %d\n", rep.StatementsFound)
	fmt.Fprintf(w, "known_scoped_tables: %s\n", strings.Join(rep.KnownScopedTables, ", "))
	fmt.Fprintf(w, "duration_seconds: %.3f\n", rep.DurationSeconds)
	fmt.Fprintf(w, "suspicions: %d\n", len(rep.Suspicions))
	for _, s := range rep.Suspicions {
		name := s.QueryName
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(w, "  #%d [%s] %s:%d %s (%s) — %s — table_scoped_confidence=%.2f\n",
			s.Rank, s.StatementKind, s.File, s.Line, name, s.Table, s.Reason, s.TableScopedConfidence)
	}
}

// runInfer implements TGD-US-09 AC-1/AC-3 at the product level: it is the
// only way a policy file — the input verify/audit now require — comes into
// existence, and every ambiguous relation is written out with its candidates
// listed rather than one being picked silently.
func runInfer(args []string) int {
	fs := flag.NewFlagSet("infer", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "admin connection URL to the target database (read-only: SELECTs pg_catalog only)")
	out := fs.String("out", "", "write the policy file here")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "tenantguard infer: --dsn and --out are both required")
		return 2
	}

	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard infer: open %s: %v\n", *dsn, err)
		return 1
	}
	defer db.Close()

	policy, err := schema.Infer(context.Background(), db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard infer: %v\n", err)
		return 1
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard infer: create %s: %v\n", *out, err)
		return 1
	}
	defer f.Close()
	if err := schema.WritePolicy(f, policy); err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard infer: write policy: %v\n", err)
		return 1
	}

	scopedN, unscopedN, unclassifiableN := policy.Counts()
	fmt.Fprintf(os.Stderr,
		"tenantguard infer: %d relation(s) — %d scoped, %d unscoped, %d unclassifiable. "+
			"Review %s before using it with verify/audit --policy.\n",
		len(policy.Relations), scopedN, unscopedN, unclassifiableN, *out)
	return 0
}

// verifyReport is the report both verify and audit produce on success. It is
// written to stdout ONLY when every check passed on every scoped table; on
// any abort, stdout is left completely empty, which is what TGD-US-02 AC-5
// requires.
//
// Queries is populated only by audit, only when --events was given, and only
// once Proven is true — never partially. Counts and UnattributableRate are
// set only alongside Queries — a report with no queries has neither, so a
// reader can tell "no differential run happened" from a genuine, reportable
// zero.
type verifyReport struct {
	Proven bool `json:"proven"`
	// Tier is always 1 in this build — Tier 2 (context propagation,
	// TGD-US-12) is not implemented. Present so a future build's reports are
	// distinguishable from this one's without guessing (TGD-US-10 AC-3).
	Tier int `json:"tier"`
	// ProofSource is always "probe": every oracle check and every
	// differential re-execution runs against the disposable probe database
	// this command builds from --dsn as a template, never against --dsn's
	// own data (TGD-US-10 AC-3's "A1 was probe-verified or target-verified").
	ProofSource string   `json:"proof_source"`
	Tables      []string `json:"tables"`
	// TablesRowLevel and TablesStructuralOnly are TGD-BL-32/§9.6's tiered
	// proof-depth breakdown of Tables: TablesRowLevel seeded and had A1
	// (withholding) and A4 (correctness) proved against real canary rows.
	// TablesStructuralOnly could not seed at all — A2 (policy coverage) and
	// A3 (role privilege) were still proved for it, but A1/A4 never had rows
	// to operate on, named with why. A query attributed to a
	// structural-only table is never handed to the differ (runOracleGate's
	// onProven receives only TablesRowLevel), so it can never be reported
	// SAFE or LEAK — only UNATTRIBUTABLE, the same as any table the policy
	// never declared at all.
	//
	// Each TablesRowLevel entry also names its SeedSource (TGD-BL-36):
	// "sampled" rows are the target's own real historical data with only the
	// tenant column rewritten; "fixture" rows are hand-authored in the
	// policy file. A row-level pass on a fixture-seeded table is real proof
	// the oracle mechanism works against that constraint shape, but it is
	// NOT proof the oracle has been exercised against anything the target
	// itself produced — a reader must be able to tell the two apart rather
	// than reading every TablesRowLevel entry as equally strong evidence.
	TablesRowLevel       []rowLevelTable       `json:"tables_row_level"`
	TablesStructuralOnly []structuralOnlyTable `json:"tables_structural_only"`
	Checks               struct {
		A1 bool `json:"a1"`
		A2 bool `json:"a2"`
		A3 bool `json:"a3"`
		A4 bool `json:"a4"`
	} `json:"checks"`
	// SharedDatabase is TGD-FR-18's header. It is an operator declaration
	// (--shared-database), never automatic detection: a single --dsn
	// invocation gives the CLI no way to observe whether the target's own
	// test harness reuses one database across its tests. It is never
	// omitted, even when false — a reader must be able to tell "declared not
	// shared" from "the tool never asked."
	SharedDatabase bool           `json:"shared_database"`
	Queries        []queryVerdict `json:"queries,omitempty"`
	// Counts is TGD-US-05 AC-3 / TGD-US-10 AC-3: SAFE/LEAK/UNATTRIBUTABLE
	// counts that must sum to len(Queries) — asserted by
	// TestAuditCLI_CountsSumToQueryTotal, not just implied by the list.
	Counts *verdictCounts `json:"counts,omitempty"`
	// UnattributableRate is TGD-US-07's reported rate — set only when audit
	// actually ran the differ over at least one query: the same
	// all_captured_queries figure UnattributableRateByDenominator's own
	// first entry carries, kept as its own top-level field for a reader who
	// wants the simple number without the four-way breakdown. TGD-NFR-03's
	// ceiling is baselined (currently 18.0/762.0, TGD-BL-35/TGD-BL-47) and
	// enforced against row_level_touching_real_app_sql specifically, not
	// against this field — see checkUnattributableCeiling. A nil pointer
	// (omitted key) distinguishes "no queries were audited" from a genuine,
	// reportable 0%.
	UnattributableRate *float64 `json:"unattributable_rate,omitempty"`
	// UnattributableRateByDenominator is TGD-BL-33/TGD-BL-06's baselining
	// prerequisite: the same rate computed against four candidate
	// denominators, labelled, so the next run is comparable to this one
	// without re-running an ad hoc script. "all_captured_queries" is the
	// same number as UnattributableRate above, included here too so every
	// candidate lives in one place. Set only alongside Counts (i.e. only
	// when --events produced at least one verdict) — see
	// unattributableRateBreakdown's own doc comment for what each
	// denominator means and why TGD-BL-33 recommends baselining
	// TGD-NFR-03 against "row_level_touching_real_app_sql" specifically,
	// not any of the other three.
	UnattributableRateByDenominator []unattributableRateEntry `json:"unattributable_rate_by_denominator,omitempty"`
}

// rowLevelTable is one entry in verifyReport.TablesRowLevel: a scoped table
// A1/A4 both proved, alongside the strength of evidence behind that proof —
// SeedSource is "sampled" (the target's own data) or "fixture" (hand-
// authored in the policy file) (TGD-BL-36).
type rowLevelTable struct {
	Table      string `json:"table"`
	SeedSource string `json:"seed_source"`
}

// structuralOnlyTable is one entry in verifyReport.TablesStructuralOnly: a
// scoped table that could not be seeded, so A1/A4 (row-level) were never
// attempted for it, alongside why.
type structuralOnlyTable struct {
	Table  string `json:"table"`
	Reason string `json:"reason"`
}

type verdictCounts struct {
	Safe           int `json:"safe"`
	Leak           int `json:"leak"`
	Unattributable int `json:"unattributable"`
	// Population is TGD-BL-33/TGD-BL-06's baselining prerequisite: the
	// Unattributable count broken down by differ.Population, computed by
	// the tool itself (per-query, in the onProven closure) — must sum to
	// Unattributable.
	Population populationCounts `json:"population"`
	// VacuousSafe (TGD-BL-39) is how many of Safe had both comparison legs
	// match/affect zero rows — a subset of Safe, not a separate class of
	// verdict, so it does not appear in the SAFE/LEAK/UNATTRIBUTABLE
	// sum-to-total invariant. Excluded from the "attributed" population
	// unattributableRateBreakdown's row-level denominators rest on.
	VacuousSafe int `json:"vacuous_safe"`
}

// populationCounts mirrors differ.Population's four values by name, so the
// JSON field names are the population values themselves rather than
// reader-invented labels.
type populationCounts struct {
	NoDeclaredTable      int `json:"no_declared_table"`
	StructuralOnly       int `json:"structural_only"`
	RowLevelUnattributed int `json:"row_level_unattributed"`
	NonQuery             int `json:"non_query"`
}

// unattributableRateEntry is one labelled denominator/rate pair in
// verifyReport.UnattributableRateByDenominator (TGD-BL-33's four candidate
// denominators). Denominator is the population size; Unattributable is how
// much of it came back UNATTRIBUTABLE; Rate is Unattributable/Denominator,
// omitted (left at its zero value alongside Denominator==0) rather than
// computed as 0/0 when a run has no queries in that population at all.
type unattributableRateEntry struct {
	Label          string  `json:"label"`
	Denominator    int     `json:"denominator"`
	Unattributable int     `json:"unattributable"`
	Rate           float64 `json:"rate"`
}

// queryVerdict is one captured query's differential result.
type queryVerdict struct {
	SQL     string `json:"sql,omitempty"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Tenant  string `json:"tenant,omitempty"`
	// Params is TGD-US-10 AC-1's full bound parameter list, one entry per
	// captured parameter position, in order — not just the single value
	// ExtractTenant happened to use. "NULL" and "<undecoded>" mark the two
	// cases with no real text value to show.
	Params       []string `json:"params,omitempty"`
	WithheldRows int      `json:"withheld_rows,omitempty"`
	// Population is set only when Verdict is UNATTRIBUTABLE: one of
	// differ.PopulationNoDeclaredTable/StructuralOnly/RowLevelUnattributed/
	// NonQuery (TGD-BL-33/TGD-BL-06's baselining prerequisite).
	Population string `json:"population,omitempty"`
	// Vacuous is set only when Verdict is SAFE (TGD-BL-39): true when both
	// legs of the comparison matched/affected zero rows — ∅=∅ held, but the
	// comparison proved nothing about whether RLS would withhold anything
	// had the query matched something. See differ.Result.Vacuous.
	Vacuous bool `json:"vacuous,omitempty"`
}

func runGateCommand(name string, args []string, isAudit bool) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dsn := fs.String("dsn", "", "admin connection URL to the target database (never written to)")
	policyPath := fs.String("policy", "",
		"policy file produced by 'tenantguard infer --out FILE' and reviewed by a human")
	sharedDatabase := fs.Bool("shared-database", false,
		"declare that the target's own test harness shares one database across its tests "+
			"(TGD-FR-18) — the tool cannot detect this on its own; recorded in the report header verbatim")
	output := fs.String("output", "json", "report format: json or text")
	var events *string
	if isAudit {
		events = fs.String("events", "",
			"JSONL file of captured events (from tenantguard capture --out FILE) to audit differentially")
	}
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" || *policyPath == "" {
		fmt.Fprintf(os.Stderr, "tenantguard %s: --dsn and --policy are both required\n", name)
		return 2
	}
	if *output != "json" && *output != "text" {
		fmt.Fprintf(os.Stderr, "tenantguard %s: --output must be json or text, got %q\n", name, *output)
		return 2
	}

	policy, err := loadPolicy(*policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard %s: --policy: %v\n", name, err)
		return 2
	}

	// evs is populated by loadEvents below, before the gate ever runs, so a
	// malformed --events file is reported without spending time on a probe
	// database it turns out cannot be used.
	var evs []capture.Event
	if isAudit && *events != "" {
		var err error
		evs, err = loadEvents(*events)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tenantguard %s: --events: %v\n", name, err)
			return 2
		}
	}

	var queries []queryVerdict
	var onProven func(probeDB, restrictedDB *sql.DB, rowLevel, structuralOnly []schema.Relation) error
	if isAudit && *events != "" {
		onProven = func(probeDB, restrictedDB *sql.DB, rowLevel, structuralOnly []schema.Relation) error {
			for _, ev := range evs {
				if ev.Kind != capture.KindBind && ev.Kind != capture.KindQuery {
					// Not a statement execution (parse/param_description/
					// row_description/close carry protocol bookkeeping, not
					// something to re-execute).
					continue
				}
				r := differ.Diff(context.Background(), probeDB, restrictedDB, rowLevel, ev)
				qv := queryVerdict{
					SQL: ev.SQL, Verdict: string(r.Verdict), Reason: r.Reason,
					Tenant: r.Tenant, WithheldRows: r.WithheldRows,
					Params: paramStrings(ev.Params), Vacuous: r.Vacuous,
				}
				// TGD-BL-33/TGD-BL-06's baselining prerequisite: every
				// UNATTRIBUTABLE verdict carries a machine-computed
				// population, from the same rowLevel/structuralOnly split
				// the gate already produced — never from Reason's free text.
				if r.Verdict == differ.Unattributable {
					qv.Population = string(differ.ClassifyUnattributable(ev.SQL, rowLevel, structuralOnly))
				}
				queries = append(queries, qv)
			}
			return nil
		}
	}

	result, gateErr := runOracleGate(context.Background(), *dsn, policy.Relations, onProven)
	if gateErr != nil {
		// Nothing is written to stdout on this path, in any format — this is
		// the assertion TGD-US-02 AC-5 requires and the CLI test checks for.
		fmt.Fprintf(os.Stderr, "tenantguard %s: policy not proven: %v\n", name, gateErr)
		return exitCodeFor(gateErr)
	}

	// Report-layer assertion, independent of runOracleGate's own gating
	// (which already refuses to call onProven unless proven): per-query
	// verdicts may never be attached to a report whose policy was not proven.
	// This is deliberately redundant with the gate — ProofState gates the
	// report itself, not only the pipeline that produced it. Unreachable
	// through this file's own current wiring (gateErr would already be
	// non-nil, and the function would have returned above, before queries
	// could exist) — kept anyway as an honestly-labelled fallback rather than
	// a panic, the same pattern paramArgs uses in internal/differ/differ.go.
	// A mutation that deletes this block is a known, documented blind spot,
	// not a caught regression: no test can reach it without runOracleGate's
	// own gate already having a bug, which is a different mutation.
	if len(queries) > 0 && result.proof.PolicyProven() != nil {
		fmt.Fprintln(os.Stderr, "tenantguard: internal invariant violated: "+
			"per-query results exist without a proven policy; refusing to report")
		return 1
	}

	var report verifyReport
	report.Proven = true
	report.Tier = 1
	report.ProofSource = "probe"
	for _, r := range result.relations {
		report.Tables = append(report.Tables, r.Qualified())
	}
	for _, tp := range result.tableProof {
		if tp.Seeded {
			report.TablesRowLevel = append(report.TablesRowLevel,
				rowLevelTable{Table: tp.Table, SeedSource: tp.SeedSource})
		} else {
			report.TablesStructuralOnly = append(report.TablesStructuralOnly,
				structuralOnlyTable{Table: tp.Table, Reason: tp.SkipReason})
		}
	}
	report.Checks.A1 = result.proof.A1Passed
	report.Checks.A2 = result.proof.A2Passed
	report.Checks.A3 = result.proof.A3Passed
	report.Checks.A4 = result.proof.A4Passed
	report.SharedDatabase = *sharedDatabase
	report.Queries = queries
	// *events != "" is redundant here in practice — queries can only be
	// non-empty when onProven was set above, which itself required
	// isAudit && *events != "" — kept for readability at the point counts
	// and the rate are computed, not because the two conditions can disagree.
	if isAudit && *events != "" && len(queries) > 0 {
		counts := &verdictCounts{}
		for _, q := range queries {
			switch differ.Verdict(q.Verdict) {
			case differ.Safe:
				counts.Safe++
				if q.Vacuous {
					counts.VacuousSafe++
				}
			case differ.Leak:
				counts.Leak++
			case differ.Unattributable:
				counts.Unattributable++
				switch differ.Population(q.Population) {
				case differ.PopulationNoDeclaredTable:
					counts.Population.NoDeclaredTable++
				case differ.PopulationStructuralOnly:
					counts.Population.StructuralOnly++
				case differ.PopulationRowLevelUnattributed:
					counts.Population.RowLevelUnattributed++
				case differ.PopulationNonQuery:
					counts.Population.NonQuery++
				}
			}
		}
		report.Counts = counts
		rate := float64(counts.Unattributable) / float64(len(queries))
		report.UnattributableRate = &rate
		report.UnattributableRateByDenominator = unattributableRateBreakdown(counts, len(queries))
	}

	if err := writeReport(os.Stdout, report, *output); err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard %s: encode report: %v\n", name, err)
		return 1
	}

	// TGD-NFR-03/TGD-FR-08/TGD-BL-06: the report above is written FIRST and
	// unconditionally — a ceiling breach is a completed, proven run with
	// real per-query verdicts, not an oracle self-check failure (§8.2's
	// codes 10-13, which suppress the report entirely because there is
	// nothing genuine to report). TGD-US-07 AC-1's "the summary states the
	// rate" is satisfied by the report already on stdout; this adds the
	// distinct stderr message and the reserved exit code.
	if err := checkUnattributableCeiling(report.UnattributableRateByDenominator); err != nil {
		fmt.Fprintf(os.Stderr, "tenantguard %s: %v\n", name, err)
		return ExitUnattributableCeilingExceeded
	}

	if isAudit && *events == "" {
		fmt.Fprintln(os.Stderr,
			"tenantguard audit: policy proven. No --events file was given; "+
				"per-query auditing (TGD-US-06) requires captured events from "+
				"`tenantguard capture --out FILE`. No per-query verdicts were produced.")
	}
	return 0
}

// unattributableRateBreakdown computes TGD-BL-33's four candidate
// denominators for TGD-NFR-03's ceiling, from counts alone — every input is
// already-aggregated data the caller just produced, no re-scan of queries.
//
// TGD-BL-33's own session found, against a real coder capture: "all
// captured queries" (denominator 1) reported 90.6%, but 58% of that
// denominator was cursor-protocol/session-bookkeeping traffic that is not a
// data query at all (PopulationNonQuery), and most of the rest was real
// application SQL against tables the policy never declared tenant-scoped
// (PopulationNoDeclaredTable) — neither has anything to do with whether
// attribution logic works. "row_level_touching_real_app_sql" (denominator
// 4) isolates exactly the population where attribution was possible in
// principle (a table the oracle proved it can see) and still failed —
// 27.8% in that same session, the number TGD-BL-33 recommends TGD-BL-06
// actually baseline TGD-NFR-03 against, once this breakdown lets an
// operator confirm that recommendation still holds on their own run rather
// than trusting one session's numbers by name.
//
// A denominator of 0 (a run whose traffic happens to contain nothing in
// that population) omits the entry rather than reporting a fabricated 0/0.
//
// TGD-BL-39: "attributed" excludes VacuousSafe — a SAFE where both
// comparison legs matched/affected zero rows proved nothing about whether
// RLS would withhold anything, so it must not inflate the denominators
// TGD-NFR-03's ceiling rests on the same way a real, non-empty pass does.
// This affects only the two "touching a declared/row-level table"
// denominators below; the first two are plain query counts against
// UNATTRIBUTABLE, unrelated to the Safe/Leak split, so Vacuous has no
// effect on them.
func unattributableRateBreakdown(counts *verdictCounts, totalQueries int) []unattributableRateEntry {
	p := counts.Population
	attributed := (counts.Safe - counts.VacuousSafe) + counts.Leak

	candidates := []struct {
		label          string
		denominator    int
		unattributable int
	}{
		{"all_captured_queries", totalQueries, counts.Unattributable},
		{"real_app_sql_any_table", totalQueries - p.NonQuery, counts.Unattributable - p.NonQuery},
		{"real_app_sql_touching_any_declared_table",
			attributed + p.StructuralOnly + p.RowLevelUnattributed,
			p.StructuralOnly + p.RowLevelUnattributed},
		{"row_level_touching_real_app_sql",
			attributed + p.RowLevelUnattributed,
			p.RowLevelUnattributed},
	}

	var out []unattributableRateEntry
	for _, c := range candidates {
		if c.denominator == 0 {
			continue
		}
		out = append(out, unattributableRateEntry{
			Label: c.label, Denominator: c.denominator, Unattributable: c.unattributable,
			Rate: float64(c.unattributable) / float64(c.denominator),
		})
	}
	return out
}

// writeReport renders report to w in the requested format. "json" is a
// single JSON document, matching every prior build's behaviour and every
// existing consumer. "text" is TGD-US-10 AC-2's second, human-oriented
// format, added specifically so a test can select between the two — the
// underlying content is identical either way.
func writeReport(w *os.File, report verifyReport, format string) error {
	if format == "text" {
		fmt.Fprintf(w, "proven: %v\n", report.Proven)
		fmt.Fprintf(w, "tier: %d\n", report.Tier)
		fmt.Fprintf(w, "proof_source: %s\n", report.ProofSource)
		fmt.Fprintf(w, "tables: %s\n", strings.Join(report.Tables, ", "))
		for _, rl := range report.TablesRowLevel {
			fmt.Fprintf(w, "tables_row_level: %s (seed_source=%s)\n", rl.Table, rl.SeedSource)
		}
		for _, so := range report.TablesStructuralOnly {
			fmt.Fprintf(w, "tables_structural_only: %s (%s)\n", so.Table, so.Reason)
		}
		fmt.Fprintf(w, "checks: a1=%v a2=%v a3=%v a4=%v\n",
			report.Checks.A1, report.Checks.A2, report.Checks.A3, report.Checks.A4)
		fmt.Fprintf(w, "shared_database: %v\n", report.SharedDatabase)
		if report.Counts != nil {
			fmt.Fprintf(w, "counts: safe=%d leak=%d unattributable=%d vacuous_safe=%d\n",
				report.Counts.Safe, report.Counts.Leak, report.Counts.Unattributable, report.Counts.VacuousSafe)
			fmt.Fprintf(w, "counts.population: no_declared_table=%d structural_only=%d row_level_unattributed=%d non_query=%d\n",
				report.Counts.Population.NoDeclaredTable, report.Counts.Population.StructuralOnly,
				report.Counts.Population.RowLevelUnattributed, report.Counts.Population.NonQuery)
		}
		if report.UnattributableRate != nil {
			fmt.Fprintf(w, "unattributable_rate: %.4f\n", *report.UnattributableRate)
		}
		for _, e := range report.UnattributableRateByDenominator {
			fmt.Fprintf(w, "unattributable_rate_by_denominator: %s denominator=%d unattributable=%d rate=%.4f\n",
				e.Label, e.Denominator, e.Unattributable, e.Rate)
		}
		for _, q := range report.Queries {
			fmt.Fprintf(w, "query: verdict=%s tenant=%q withheld=%d sql=%q reason=%q params=%v population=%q vacuous=%v\n",
				q.Verdict, q.Tenant, q.WithheldRows, q.SQL, q.Reason, q.Params, q.Population, q.Vacuous)
		}
		return nil
	}
	enc := json.NewEncoder(w)
	return enc.Encode(report)
}

// paramStrings renders every captured parameter as a comparable string for
// the report — TGD-US-10 AC-1's full bound parameter list, not just the
// single value ExtractTenant used.
func paramStrings(params []capture.Param) []string {
	if len(params) == 0 {
		return nil
	}
	out := make([]string, len(params))
	for i, p := range params {
		switch {
		case p.IsNull:
			out[i] = "NULL"
		case p.Known:
			out[i] = p.Text
		default:
			out[i] = "<undecoded>"
		}
	}
	return out
}

// loadPolicy reads a policy file produced by `tenantguard infer --out FILE`.
// Its existence is the human-acceptance step TGD-US-09 AC-2 requires: verify
// and audit refuse to run without one, and never infer a policy on the fly.
func loadPolicy(path string) (*schema.Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return schema.ReadPolicy(f)
}

// loadEvents reads a JSONL capture file produced by `tenantguard capture
// --out FILE`. It is read once, in full, before the oracle gate ever runs.
func loadEvents(path string) ([]capture.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return capture.DecodeEvents(f)
}
