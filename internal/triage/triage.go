// Package triage implements Tier 0 (TGD-FR-09/TGD-US-08): a syntactic pass
// over a Go repository's own source, requiring no database connection and no
// target-specific setup — the only tier that keeps the "point at any repo,
// no setup" promise (design §3, §9; SRS TGD-US-08).
//
// It is not a SQL parser and is not the differential engine (internal/differ)
// demoted — it is a distinct, deliberately simpler mechanism, built
// independently so the two tiers' output can never be confused for one
// another. It finds a table's name and whether a tenant-shaped column name
// appears anywhere in a statement's own text; it does not understand what
// the statement means. It cannot prove a leak and never claims to: every
// Suspicion this package reports is unverified by construction, and Run
// never returns an error condition that should cause its caller to exit
// non-zero for reasons of what it found (TGD-US-08 AC-1).
//
// **Table-list decision, recorded because the alternative was real and
// rejected deliberately, not by default.** Tier 1's own `infer` reads a
// live database's information_schema to classify tables — more accurate,
// because it sees the schema PostgreSQL itself agrees is true. This package
// does NOT read `infer`'s policy file, and does not shell out to a database
// at all, for two reasons that both trace back to TGD-US-08's own story
// ("a zero-setup pass over any Go repository ... to rank what to
// investigate before committing to a Tier 1 run"): (1) requiring a policy
// file means requiring a prior Tier 1 database run to have already
// happened, which is circular for a tier whose entire purpose is to run
// BEFORE that investment is made — most repositories pointed at for the
// first time will have no such file; (2) design §9's own precondition
// table states Tier 0's precondition as "None. Any Go repo." — a database
// dependency, even an indirect one through a stale policy file, is not
// "none." The cost of this choice is real and is not hidden: a table is
// only "known scoped" here if this package finds textual evidence of a
// tenant-shaped column somewhere in the corpus's own SQL text, which is
// less accurate than reading a live schema and will miss a scoped table no
// scanned statement happens to mention its tenant column on (undercounting,
// the safe direction for a tier whose own hard constraint is "must not
// imply proof").
package triage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tenantColumnNames is Tier 0's column-naming heuristic — the same
// unaccented shape design §9 names for Tier 1's information_schema-driven
// inference (tenant_id, org_id, workspace_id, account_id, owner), widened
// with the variants a real corpus (coder's own: organization_id) and common
// convention (customer_id, owner_id) actually use. This list is a known,
// named limitation, not a claim of completeness: a repo whose tenant column
// is spelled some other way will simply produce no suspicions for it,
// which is the safe failure direction for a tier that must not overclaim.
var tenantColumnNames = []string{
	"tenant_id", "org_id", "organization_id", "workspace_id",
	"account_id", "customer_id", "owner_id", "owner",
}

// skipDirs are directory basenames never descended into: version control,
// dependency trees, and generated/vendored asset directories that hold no
// application SQL and would only cost time against the < 5 minute budget
// (TGD-NFR-01).
var skipDirs = map[string]bool{
	".git": true, "vendor": true, "node_modules": true,
	"testdata": true, ".terraform": true,
}

// generatedHeader matches Go's own convention for a generated file (the
// same marker staticcheck and gofmt-adjacent tooling already treat as
// authoritative: https://golang.org/s/generatedcode). sqlc emits every
// query's SQL text a second time into a generated *.sql.go file as a raw
// string constant — scanning both that file and the *.sql source it was
// generated from would double-count every statement in a sqlc-shaped repo
// (coder among them). Skipping generated Go files, and preferring the *.sql
// source when present, avoids the double-count without a sqlc-specific
// special case.
var generatedHeader = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.?$`)

// sqlVerb matches a leading SQL statement keyword, used both to recognise a
// Go string literal as SQL at all and to classify a statement's Verb.
var sqlVerb = regexp.MustCompile(`(?i)^(SELECT|INSERT|UPDATE|DELETE|WITH)\b`)

// writeKeyword flags a statement as write-shaped for ranking purposes
// (higher-severity than a read, since an unscoped write can create or
// mutate cross-tenant data rather than merely returning it) — deliberately
// approximate for a CTE-wrapped write, the same imprecision this project's
// own differ named and fixed for its own routing (TGD-BL-41) but which
// Tier 0, being naive by design, does not attempt to resolve exactly:
// ranking, not verdict correctness, is what depends on it.
var writeKeyword = regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE\s+|DELETE\s+FROM)\b`)

// tableRef matches a table name following FROM/JOIN/INTO/UPDATE — this
// package's own, independent version of the same idea internal/differ's
// unexported tableRef implements, not a shared import: the two tiers are
// kept architecturally separate on purpose (package doc), and Tier 1's
// tableRef is unexported besides.
var tableRef = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE)\s+"?(?:\w+\.)?"?(\w+)"?`)

// sqlcName matches sqlc's own "-- name: X :verb" convention, used to split
// a *.sql query file into individual statements and to label each with its
// query name for a more readable report.
var sqlcName = regexp.MustCompile(`(?m)^--\s*name:\s*(\S+)\s*:\S+`)

// Statement is one candidate SQL statement found in the repository, before
// any judgement about whether it is suspicious.
type Statement struct {
	File  string
	Line  int
	Name  string // sqlc "-- name: X" if present, else ""
	Verb  string // "READ" or "WRITE" — see writeKeyword's doc comment
	Table string
	SQL   string
}

// Suspicion is one flagged Statement: a known-scoped table referenced with
// no textual trace of its tenant column anywhere in the statement.
// Everything about this type is deliberately labelled unverified — see
// Report.Label and the package doc's own opening paragraph.
type Suspicion struct {
	File                  string  `json:"file"`
	Line                  int     `json:"line"`
	QueryName             string  `json:"query_name,omitempty"`
	Table                 string  `json:"table"`
	StatementKind         string  `json:"statement_kind"`
	Reason                string  `json:"reason"`
	Rank                  int     `json:"rank"`
	TableScopedConfidence float64 `json:"table_scoped_confidence"`
	SQL                   string  `json:"sql"`
}

// Report is Tier 0's complete output. Unverified is always true and Label
// is a fixed constant — both exist so a reader (or a script grepping
// terminal scrollback) can tell this apart from a Tier 1 report on sight,
// per TGD-US-08 AC-2, without having to parse the rest of the structure.
type Report struct {
	Unverified        bool        `json:"unverified"`
	Label             string      `json:"label"`
	RepoRoot          string      `json:"repo_root"`
	FilesScanned      int         `json:"files_scanned"`
	StatementsFound   int         `json:"statements_found"`
	KnownScopedTables []string    `json:"known_scoped_tables"`
	Suspicions        []Suspicion `json:"suspicions"`
	DurationSeconds   float64     `json:"duration_seconds"`
}

// unverifiedLabel is printed into every Report and is the wording TGD-US-08
// AC-2 requires be distinct from the differential (Tier 1) report's own
// vocabulary — deliberately not sharing a single word with SAFE/LEAK/
// UNATTRIBUTABLE or "proven", so the two cannot be mistaken for each other
// in a terminal scrollback.
const unverifiedLabel = "UNVERIFIED — Tier 0 syntactic triage. No database was consulted. " +
	"Nothing below is confirmed; every entry is a suspicion for a human to " +
	"investigate before committing to a Tier 1 run."

// Run walks repoRoot, finds candidate SQL statements in *.sql query files
// and non-generated *.go source, and returns a ranked, always-successful
// Report — see the package doc for what "always" excludes (nothing: Run
// itself can still return an error for a repoRoot that does not exist or
// is not readable at all, which is a usage problem for the CLI layer to
// turn into its own, ordinary exit code — TGD-US-08 AC-1's "exits zero
// regardless of findings" is a promise about the ANALYSIS, not about
// whether main() got told a real path).
func Run(repoRoot string) (Report, error) {
	start := time.Now()

	info, err := os.Stat(repoRoot)
	if err != nil {
		return Report{}, err
	}
	if !info.IsDir() {
		return Report{}, &fs.PathError{Op: "triage", Path: repoRoot, Err: os.ErrInvalid}
	}

	var statements []Statement
	filesScanned := 0

	walkErr := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable file or directory is skipped, not fatal —
			// Tier 0 must degrade gracefully, never abort (package doc).
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".sql"):
			if strings.Contains(strings.ToLower(path), string(filepath.Separator)+"migrations"+string(filepath.Separator)) {
				return nil // schema, not application queries — see package doc.
			}
			filesScanned++
			statements = append(statements, statementsFromSQLFile(path)...)
		case strings.HasSuffix(path, ".go"):
			filesScanned++
			statements = append(statements, statementsFromGoFile(path)...)
		}
		return nil
	})
	if walkErr != nil {
		return Report{}, walkErr
	}

	knownScoped, evidenceRatio := classifyTables(statements)

	var suspicions []Suspicion
	for _, s := range statements {
		col, ok := knownScoped[s.Table]
		if !ok {
			continue
		}
		if mentionsColumn(s.SQL, col) {
			continue
		}
		suspicions = append(suspicions, Suspicion{
			File: s.File, Line: s.Line, QueryName: s.Name,
			Table: s.Table, StatementKind: s.Verb,
			Reason: "no reference to this table's known tenant column (" + col +
				") found anywhere in the statement's text",
			TableScopedConfidence: evidenceRatio[s.Table],
			SQL:                   s.SQL,
		})
	}

	rankSuspicions(suspicions)

	tables := make([]string, 0, len(knownScoped))
	for t := range knownScoped {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	return Report{
		Unverified:        true,
		Label:             unverifiedLabel,
		RepoRoot:          repoRoot,
		FilesScanned:      filesScanned,
		StatementsFound:   len(statements),
		KnownScopedTables: tables,
		Suspicions:        suspicions,
		DurationSeconds:   time.Since(start).Seconds(),
	}, nil
}

// classifyTables is the column-naming, no-database table-list mechanism
// (package doc): pass one over every statement, recording which tenant
// column name (the first one found, from tenantColumnNames in the order
// declared) is ever mentioned anywhere in ANY statement against a table,
// and how often (evidenceRatio: statements-with-evidence / statements
// touching that table) — the confidence figure Rank uses. A table with
// zero evidence anywhere in the corpus is never "known scoped" and
// contributes no suspicions: undercounting, the safe direction (package
// doc).
func classifyTables(statements []Statement) (knownScoped map[string]string, evidenceRatio map[string]float64) {
	total := map[string]int{}
	withEvidence := map[string]int{}
	col := map[string]string{}

	for _, s := range statements {
		if s.Table == "" {
			continue
		}
		total[s.Table]++
		for _, c := range tenantColumnNames {
			if mentionsColumn(s.SQL, c) {
				withEvidence[s.Table]++
				if col[s.Table] == "" {
					col[s.Table] = c
				}
				break
			}
		}
	}

	knownScoped = map[string]string{}
	evidenceRatio = map[string]float64{}
	for table, c := range col {
		knownScoped[table] = c
		evidenceRatio[table] = float64(withEvidence[table]) / float64(total[table])
	}
	return knownScoped, evidenceRatio
}

// rankSuspicions orders in place: write-shaped statements before
// read-shaped (a write with no tenant-column trace can create or mutate
// cross-tenant data, not merely return it), then by the table's own
// scoped-confidence descending (a table scoped in nearly every other
// statement is a stronger signal than one rarely scoped at all), then by
// file/line for a deterministic, reproducible order — then assigns the
// 1-based Rank field to match.
func rankSuspicions(s []Suspicion) {
	sort.SliceStable(s, func(i, j int) bool {
		wi, wj := s[i].StatementKind == "WRITE", s[j].StatementKind == "WRITE"
		if wi != wj {
			return wi
		}
		if s[i].TableScopedConfidence != s[j].TableScopedConfidence {
			return s[i].TableScopedConfidence > s[j].TableScopedConfidence
		}
		if s[i].File != s[j].File {
			return s[i].File < s[j].File
		}
		return s[i].Line < s[j].Line
	})
	for i := range s {
		s[i].Rank = i + 1
	}
}

// mentionsColumn is TGD-US-08 AC-3's naive predicate-presence check, word-
// bounded and case-insensitive: does column appear anywhere in sqlText at
// all, as a whole identifier. Deliberately not "is it a real, effective
// predicate" — a tautology (L6: tenant_id = tenant_id) and an OR-defeated
// comparison (L10: tenant_id = $1 OR status = $2) both mention the column
// textually, so both pass this check and are correctly NOT flagged, which
// is the acceptance criterion, not an oversight: it is the empirical
// argument for why Tier 1 exists at all (TGD-US-08's own description).
func mentionsColumn(sqlText, column string) bool {
	re := columnMentionRegex(column)
	return re.MatchString(sqlText)
}

var columnMentionCache = map[string]*regexp.Regexp{}

func columnMentionRegex(column string) *regexp.Regexp {
	if re, ok := columnMentionCache[column]; ok {
		return re
	}
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(column) + `\b`)
	columnMentionCache[column] = re
	return re
}

// statementsFromSQLFile splits a *.sql file into one Statement per sqlc
// "-- name:" block when present, else treats the whole file as one
// statement (a plain, hand-written query file with no sqlc annotations).
func statementsFromSQLFile(path string) []Statement {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := string(data)

	locs := sqlcName.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		verb, table := classifyOne(text)
		if verb == "" {
			return nil
		}
		return []Statement{{File: path, Line: 1, Verb: verb, Table: table, SQL: strings.TrimSpace(text)}}
	}

	var out []Statement
	for i, loc := range locs {
		start := loc[0]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		chunk := text[start:end]
		name := text[loc[2]:loc[3]]
		verb, table := classifyOne(chunk)
		if verb == "" {
			continue
		}
		line := 1 + strings.Count(text[:start], "\n")
		out = append(out, Statement{
			File: path, Line: line, Name: name, Verb: verb, Table: table,
			SQL: strings.TrimSpace(chunk),
		})
	}
	return out
}

// statementsFromGoFile parses a Go source file and returns one Statement
// per SQL-shaped string literal — plain double-quoted or raw backtick,
// found via go/parser rather than a text regex so a string that merely
// looks SQL-shaped inside a comment or another string is never picked up.
// A file with the generated-code header, or one that fails to parse, is
// skipped (never fatal to the run — package doc).
func statementsFromGoFile(path string) []Statement {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if generatedHeader.Match(data) {
		return nil
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return nil
	}

	var out []Statement
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		unquoted := unquoteGoString(lit.Value)
		trimmed := strings.TrimSpace(unquoted)
		verb, table := classifyOne(trimmed)
		if verb == "" {
			return true
		}
		name := ""
		if m := sqlcName.FindStringSubmatch(trimmed); m != nil {
			name = m[1]
		}
		pos := fset.Position(lit.Pos())
		out = append(out, Statement{
			File: path, Line: pos.Line, Name: name, Verb: verb, Table: table,
			SQL: trimmed,
		})
		return true
	})
	return out
}

// classifyOne determines a candidate statement's Verb ("READ"/"WRITE") and
// primary Table, or ("", "") if text does not look like a SQL statement at
// all (sqlVerb never matched after stripping any leading sqlc name
// comment) — the caller's signal to discard it rather than record a
// meaningless Statement.
func classifyOne(text string) (verb, table string) {
	body := text
	if m := sqlcName.FindStringIndex(body); m != nil && m[0] == 0 {
		nl := strings.IndexByte(body, '\n')
		if nl >= 0 {
			body = body[nl+1:]
		}
	}
	body = strings.TrimSpace(body)
	m := sqlVerb.FindStringSubmatch(body)
	if m == nil || !looksLikeSQL(strings.ToUpper(m[1]), body) {
		return "", ""
	}
	// No valid table reference at all (matchTable rejects it, e.g. English
	// prose that happened to contain FROM/SET/INTO as an ordinary word) is
	// disqualifying, not merely a Statement with an empty Table: real SQL
	// of every shape this package recognises always names a target table,
	// so its absence is itself evidence this was never SQL — found
	// necessary in the same session as sqlClauseWords itself (its doc
	// comment): requiresKeyword's gate alone still let prose become a
	// zero-table "Statement" that inflated FilesScanned/StatementsFound
	// without ever producing a false Suspicion, which is silent noise, not
	// safety, and TestRun_PlainEnglishCLIHelpTextNotMisclassifiedAsSQL
	// checks the count directly rather than only the (absence of a)
	// Suspicion.
	t, ok := matchTable(body)
	if !ok {
		return "", ""
	}
	if writeKeyword.MatchString(body) {
		verb = "WRITE"
	} else {
		verb = "READ"
	}
	return verb, t
}

// requiresKeyword names, per leading verb, the structural keyword real SQL
// of that shape always contains. This alone is NOT sufficient — see
// sqlClauseWords's doc comment for the second, load-bearing gate found
// necessary in the same session this package was written.
var requiresKeyword = map[string]*regexp.Regexp{
	"SELECT": regexp.MustCompile(`(?i)\bFROM\b`),
	"DELETE": regexp.MustCompile(`(?i)\bFROM\b`),
	"UPDATE": regexp.MustCompile(`(?i)\bSET\b`),
	"INSERT": regexp.MustCompile(`(?i)\bINTO\b`),
	// WITH-led statements vary (SELECT or a writable CTE) — any one of the
	// other verbs' own structural keywords is sufficient evidence.
	"WITH": regexp.MustCompile(`(?i)\b(FROM|SET|INTO|VALUES)\b`),
}

func looksLikeSQL(verb, body string) bool {
	re, ok := requiresKeyword[verb]
	if !ok {
		return false
	}
	return re.MatchString(body)
}

// sqlClauseWords is the second gate, found load-bearing by a real failure:
// requiresKeyword alone still misclassified plain English CLI help text as
// SQL on real coder source — "Delete all encrypted data from the
// database." contains "from" as an ordinary preposition, and "Select an
// option from the menu below" does too, both satisfying requiresKeyword's
// bare \bFROM\b check without being SQL at all. The fix validates what
// immediately follows the matched table token (see matchTable): real SQL
// always has a clause keyword, punctuation, or end-of-statement there
// (`FROM widgets WHERE ...`, `FROM widgets;`, `FROM widgets, other`) where
// English prose has another ordinary word ("from the menu below" — "the"
// is neither a clause keyword nor followed immediately by one within two
// words). This is the same alias/clause-boundary disambiguation problem
// internal/differ's own tableRef solves for its OWN, unrelated purpose
// (recognising a trailing alias) — solved independently here, not shared,
// per the package doc's architectural-separation rule.
var sqlClauseWords = map[string]bool{
	"where": true, "set": true, "values": true, "on": true, "using": true,
	"order": true, "group": true, "limit": true, "offset": true,
	"returning": true, "join": true, "inner": true, "left": true,
	"right": true, "full": true, "outer": true, "cross": true,
	"natural": true, "as": true, "for": true, "select": true, "union": true,
}

// matchTable finds the primary table name in body (the FROM/JOIN/INTO/
// UPDATE tableRef match) and validates it is really followed by SQL, not
// prose — see sqlClauseWords's doc comment for why this check exists at
// all. Looks at the next two whitespace-delimited words after the matched
// name (covering `table clause-word` and `table alias clause-word`); a
// hit against sqlClauseWords, or immediate punctuation/end-of-string,
// accepts. Anything else — an ordinary English word in both positions —
// is rejected.
func matchTable(body string) (table string, ok bool) {
	loc := tableRef.FindStringSubmatchIndex(body)
	if loc == nil {
		return "", false
	}
	name := strings.ToLower(body[loc[2]:loc[3]])
	rest := strings.TrimLeftFunc(body[loc[1]:], func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' })
	if rest == "" || strings.ContainsRune(";,(", rune(rest[0])) {
		return name, true
	}
	for range 2 {
		w := leadingWord(rest)
		if w == "" {
			break
		}
		if sqlClauseWords[strings.ToLower(w)] {
			return name, true
		}
		rest = strings.TrimLeftFunc(rest[len(w):], func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' })
		if rest != "" && strings.ContainsRune(";,(", rune(rest[0])) {
			return name, true
		}
	}
	return "", false
}

var wordRe = regexp.MustCompile(`^\w+`)

func leadingWord(s string) string {
	return wordRe.FindString(s)
}

func unquoteGoString(raw string) string {
	s, err := strconv.Unquote(raw)
	if err != nil {
		return raw
	}
	return s
}
