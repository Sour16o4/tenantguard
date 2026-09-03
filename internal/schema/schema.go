// Package schema reads a PostgreSQL schema and proposes which tables are
// tenant-scoped.
//
// Its output is a proposal for a human to review, never an assumption. A table
// the inference cannot classify is reported as such — it is never silently
// treated as unscoped, because "unscoped" is what lets a table's rows leave the
// tenant boundary unchecked.
package schema

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Class is the inference's verdict for one relation.
type Class string

const (
	// Scoped: exactly one tenant-column candidate was found.
	Scoped Class = "scoped"
	// Unscoped: no candidate column. The relation is believed global.
	Unscoped Class = "unscoped"
	// Unclassifiable: the inference cannot decide. This is never equivalent to
	// Unscoped and must be surfaced to the operator.
	Unclassifiable Class = "unclassifiable"
)

// Relation is one table, view or other relation with its classification.
type Relation struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// Kind is the relation kind as PostgreSQL reports it.
	Kind string `json:"kind"`
	// Class is the inference verdict.
	Class Class `json:"class"`
	// TenantColumn is set only when Class is Scoped.
	TenantColumn string `json:"tenant_column,omitempty"`
	// Candidates lists every tenant-column candidate found.
	Candidates []string `json:"candidates,omitempty"`
	// Reason explains a Class of Unscoped or Unclassifiable.
	Reason string `json:"reason,omitempty"`
	// Fixture supplies exactly two rows of column values, used to seed this
	// relation's canary rows ONLY when fewer than two real rows exist on the
	// probe to sample from (TGD-BL-29/§9.6) — an empty or freshly-migrated
	// table has nothing to copy. Each map is column name -> a literal SQL
	// value the operator is responsible for quoting/casting correctly (e.g.
	// "'x'", "42", "'{}'::jsonb"), for every NOT NULL column without a
	// database default other than the tenant column, which the tool supplies
	// automatically. Reviewed by the operator as part of the policy file,
	// the same TGD-US-09 AC-2 acceptance step the rest of the file is
	// already subject to.
	Fixture []map[string]string `json:"fixture,omitempty"`
}

func (r Relation) Qualified() string {
	return fmt.Sprintf("%s.%s", r.Schema, r.Name)
}

// Policy is the full inference result. It is the artifact TGD-US-09 AC-2
// requires a human to review before a differential run may use it — verify
// and audit refuse to run without a policy file (see cmd/tenantguard), which
// is the acceptance step: the file's existence and content is what an
// operator reviewed, not a checkbox this package tracks itself.
type Policy struct {
	Relations []Relation `json:"relations"`
}

// WritePolicy writes p as indented JSON, the format ReadPolicy reads back.
func WritePolicy(w io.Writer, p *Policy) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

// ReadPolicy reads a policy file written by WritePolicy (or by `tenantguard
// infer --out FILE`).
func ReadPolicy(r io.Reader) (*Policy, error) {
	var p Policy
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	return &p, nil
}

// Scoped returns the relations the inference is confident about.
func (p *Policy) Scoped() []Relation {
	var out []Relation
	for _, r := range p.Relations {
		if r.Class == Scoped {
			out = append(out, r)
		}
	}
	return out
}

// Unclassifiable returns the relations needing a human decision.
func (p *Policy) Unclassifiable() []Relation {
	var out []Relation
	for _, r := range p.Relations {
		if r.Class == Unclassifiable {
			out = append(out, r)
		}
	}
	return out
}

// Counts summarises the classification.
func (p *Policy) Counts() (scoped, unscoped, unclassifiable int) {
	for _, r := range p.Relations {
		switch r.Class {
		case Scoped:
			scoped++
		case Unscoped:
			unscoped++
		default:
			unclassifiable++
		}
	}
	return
}

// candidateColumns are the column names taken to indicate tenancy, in no
// priority order — finding more than one is ambiguity, not a ranking problem.
var candidateColumns = []string{
	"tenant_id", "org_id", "organization_id", "workspace_id", "account_id", "owner",
}

// IsCandidate reports whether a column name indicates tenancy.
func IsCandidate(col string) bool {
	for _, c := range candidateColumns {
		if col == c {
			return true
		}
	}
	return false
}

// Candidates returns the tenant-column candidates present in cols.
func Candidates(cols []string) []string {
	var out []string
	for _, c := range cols {
		if IsCandidate(c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// Classify decides a relation's class from its kind and columns.
//
// The rules are deliberately conservative. Anything the inference cannot reason
// about becomes Unclassifiable rather than defaulting to either extreme.
func Classify(kind string, cols []string) (Class, string, []string, string) {
	cands := Candidates(cols)

	switch kind {
	case "VIEW":
		return Unclassifiable, "", cands,
			"row-level security cannot be attached to a view; its tenancy follows the underlying tables"
	case "MATERIALIZED VIEW":
		return Unclassifiable, "", cands,
			"row-level security cannot be attached to a materialized view"
	case "FOREIGN TABLE":
		return Unclassifiable, "", cands,
			"foreign table; rows live outside this database and cannot be constrained here"
	case "PARTITIONED TABLE":
		if len(cands) == 1 {
			return Scoped, cands[0], cands,
				"partitioned table; the policy attaches to the parent and is inherited"
		}
	}

	switch len(cands) {
	case 1:
		return Scoped, cands[0], cands, ""
	case 0:
		return Unscoped, "", nil,
			"no tenant-column candidate found; believed global"
	default:
		return Unclassifiable, "", cands,
			fmt.Sprintf("ambiguous: %d tenant-column candidates (%s); a human must choose, "+
				"or the table may use composite tenancy which this version does not support",
				len(cands), strings.Join(cands, ", "))
	}
}

const relationQuery = `
SELECT n.nspname,
       c.relname,
       CASE c.relkind
         WHEN 'r' THEN 'BASE TABLE'
         WHEN 'p' THEN 'PARTITIONED TABLE'
         WHEN 'v' THEN 'VIEW'
         WHEN 'm' THEN 'MATERIALIZED VIEW'
         WHEN 'f' THEN 'FOREIGN TABLE'
         ELSE 'OTHER'
       END AS kind,
       COALESCE(
         (SELECT array_agg(a.attname ORDER BY a.attnum)
            FROM pg_attribute a
           WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped),
         '{}'
       ) AS cols
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r','p','v','m','f')
   AND n.nspname NOT IN ('pg_catalog','information_schema')
   AND n.nspname NOT LIKE 'pg_toast%'
   AND NOT c.relispartition
 ORDER BY n.nspname, c.relname`

// Infer reads the schema and classifies every relation.
//
// It queries pg_class rather than information_schema.tables because the latter
// omits materialized views entirely — reading it alone would skip them silently,
// which is exactly the failure this package is written to avoid.
func Infer(ctx context.Context, db *sql.DB) (*Policy, error) {
	rows, err := db.QueryContext(ctx, relationQuery)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	defer rows.Close()

	var p Policy
	for rows.Next() {
		var r Relation
		var cols pqStringArray
		if err := rows.Scan(&r.Schema, &r.Name, &r.Kind, &cols); err != nil {
			return nil, fmt.Errorf("scan relation: %w", err)
		}
		r.Class, r.TenantColumn, r.Candidates, r.Reason = Classify(r.Kind, cols)
		p.Relations = append(p.Relations, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relations: %w", err)
	}
	return &p, nil
}

// pqStringArray decodes a PostgreSQL text array without a driver dependency.
type pqStringArray []string

func (a *pqStringArray) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	case nil:
		*a = nil
		return nil
	default:
		return fmt.Errorf("schema: cannot scan %T as string array", src)
	}
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return fmt.Errorf("schema: malformed array literal %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		*a = nil
		return nil
	}
	var out []string
	var cur strings.Builder
	inQuotes, escaped := false, false
	for _, r := range inner {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuotes = !inQuotes
		case r == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	*a = out
	return nil
}
