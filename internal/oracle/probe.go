package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Sour16o4/tenantguard/internal/schema"
)

// column describes one column of a table being seeded.
type column struct {
	Name       string
	Type       string
	NotNull    bool
	HasDefault bool
	IsIdentity bool
}

const columnQuery = `
SELECT a.attname,
       format_type(a.atttypid, a.atttypmod),
       a.attnotnull,
       (a.atthasdef OR a.attidentity <> ''),
       (a.attidentity <> '')
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1 AND c.relname = $2
   AND a.attnum > 0 AND NOT a.attisdropped
 ORDER BY a.attnum`

func readColumns(ctx context.Context, db *sql.DB, r schema.Relation) ([]column, error) {
	rows, err := db.QueryContext(ctx, columnQuery, r.Schema, r.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.HasDefault, &c.IsIdentity); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// sampleValue returns a literal suitable for a NOT NULL column of the given
// type, or ok=false when the type is not supported.
//
// Returning false is the honest outcome: the table is then skipped and
// reported. Inventing a value for a type we do not understand risks a constraint
// violation that looks like a tool failure, or worse, silent success with
// meaningless data.
// baseType normalizes a format_type() result to a bare type keyword for
// dispatch: lower-cased, with any array suffix and parenthesized modifier
// (varchar(50)) stripped. arr reports whether pgType named an array type —
// checked before the modifier strip, since a modifier can appear before the
// array suffix (character varying(50)[]).
func baseType(pgType string) (base string, arr bool) {
	s := strings.ToLower(strings.TrimSpace(pgType))
	if strings.HasSuffix(s, "[]") {
		arr = true
		s = strings.TrimSuffix(s, "[]")
	}
	if idx := strings.Index(s, "("); idx > 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s), arr
}

// sampleValue's output varies with i for every case below, on purpose
// (TGD-BL-28): SeedCanaries calls this once per canary row with a different
// i each time, and any NOT NULL column without a database default can carry
// a uniqueness constraint that a value shared between both rows would
// violate — exactly what happened seeding coder's audit_logs.id (uuid,
// app-generated, no default). The two exceptions are intrinsic, not a coding
// gap: boolean's two-row canary pair already gets both possible values (i
// parity), and a single-label enum's entire domain has size 1 — no encoding
// can make two rows distinct against a value space that small.
func sampleValue(pgType string, i int) (string, bool) {
	base, isArray := baseType(pgType)
	if isArray {
		if base == "" {
			return "", false
		}
		// A per-row-distinct single-element array when the element type is
		// itself known, so an array column carrying a uniqueness constraint
		// still gets two different rows — otherwise fall back to the empty
		// array every element type accepts, honestly not distinguishable
		// per row for a type this dispatch cannot generate a sample of.
		if elem, ok := sampleValue(base, i); ok {
			return fmt.Sprintf("ARRAY[%s]::%s[]", elem, base), true
		}
		return fmt.Sprintf("'{}'::%s[]", base), true
	}
	switch base {
	case "smallint", "integer", "bigint":
		return fmt.Sprintf("%d", 1000+i), true
	case "real", "double precision", "numeric", "decimal":
		return fmt.Sprintf("%d.0", 1000+i), true
	case "boolean":
		if i%2 == 0 {
			return "false", true
		}
		return "true", true
	case "text", "character varying", "character", "name", "citext":
		return fmt.Sprintf("'tgd-%d'", i), true
	case "uuid":
		return fmt.Sprintf("'00000000-0000-4000-8000-%012d'::uuid", i), true
	case "timestamp with time zone", "timestamp without time zone":
		return fmt.Sprintf("(now() + (%d * interval '1 microsecond'))", i), true
	case "date":
		return fmt.Sprintf("(current_date + %d)", i), true
	case "time without time zone", "time with time zone":
		return fmt.Sprintf("('00:00:00'::%s + (%d * interval '1 second'))", base, i), true
	case "json", "jsonb":
		return fmt.Sprintf("'{\"tgd\":%d}'::%s", i, base), true
	case "bytea":
		return fmt.Sprintf("'\\x%02x'::bytea", i%256), true
	case "inet", "cidr":
		return fmt.Sprintf("'127.0.0.%d'::%s", 1+(i%254), base), true
	case "interval":
		return fmt.Sprintf("'%d seconds'::interval", 1+i), true
	}
	return "", false
}

// enumSampleValue returns a valid label for a PostgreSQL enum type by
// consulting the catalog, since (unlike sampleValue's other cases) an enum's
// legal values are defined by the schema, not derivable from its type name
// alone — every enum type in a database has a different name. ok=false when
// pgType does not name an enum type at all: the same honest-skip contract as
// sampleValue, just requiring a catalog round trip to decide.
//
// i selects which of the type's labels to use (i modulo the label count),
// the same per-row-distinctness contract sampleValue's other cases follow —
// SeedCanaries calls this with a different i for each canary row so a
// uniqueness constraint on the column does not collide, unless the enum has
// only one label at all, an intrinsic limit no encoding can work around.
func enumSampleValue(ctx context.Context, db *sql.DB, pgType string, i int) (string, bool, error) {
	base, isArray := baseType(pgType)
	if isArray {
		return "", false, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.enumlabel
		  FROM pg_type t
		  JOIN pg_enum e ON e.enumtypid = t.oid
		 WHERE t.typname = $1
		 ORDER BY e.enumsortorder`, base)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return "", false, err
		}
		labels = append(labels, l)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(labels) == 0 {
		return "", false, nil
	}
	label := labels[((i%len(labels))+len(labels))%len(labels)]
	return "'" + strings.ReplaceAll(label, "'", "''") + "'::" + quoteIdent(pgType), true, nil
}

// columnSampleValue resolves a NOT NULL, no-default column's canary value for
// row index i: sampleValue first (pure), then the catalog-backed enum
// fallback. Callers must pass a DIFFERENT i for the two canary rows on the
// same column — see sampleValue's and enumSampleValue's doc comments for why.
func columnSampleValue(ctx context.Context, db *sql.DB, c column, i int) (string, bool, error) {
	if v, ok := sampleValue(c.Type, i); ok {
		return v, true, nil
	}
	v, isEnum, err := enumSampleValue(ctx, db, c.Type, i)
	if err != nil {
		return "", false, err
	}
	return v, isEnum, nil
}

// Fixed, distinct UUID-syntax stand-ins for CanaryA/CanaryB, used only when
// the tenant column's type is uuid. They must never collapse to the same
// value: A1 seeds exactly two canary rows and depends on telling them apart.
const (
	canaryAUUID = "00000000-0000-4000-8000-0000000000a1"
	canaryBUUID = "00000000-0000-4000-8000-0000000000b2"
)

// canaryText returns the plain text form a canary tenant identity takes for
// a column of the given type. This exact string is embedded (after
// type-appropriate SQL literal formatting by canaryLiteral) in the row
// SeedCanaries inserts into the tenant column, AND is the value CheckA1 sets
// as the session variable — the two must agree bit-for-bit, since
// EnableRLS's synthesised policy compares tenant_column::text to
// current_setting() as plain text equality (oracle.go). ok=false when the
// type cannot represent a distinguishable canary identity at all — mirrors
// sampleValue's own honest-skip contract, applied to the tenant column
// specifically.
func canaryText(pgType, canary string) (string, bool) {
	base, isArray := baseType(pgType)
	if isArray {
		return "", false
	}
	switch base {
	case "uuid":
		switch canary {
		case CanaryA:
			return canaryAUUID, true
		case CanaryB:
			return canaryBUUID, true
		}
		return "", false
	case "text", "character varying", "character", "name", "citext":
		return canary, true
	}
	return "", false
}

// canaryLiteral returns the SQL literal SeedCanaries inserts for a canary
// tenant identity into a column of the given type — canaryText's value,
// quoted and cast where the type requires it, following sampleValue's own
// type-dispatch pattern (a switch on the normalized PostgreSQL type name,
// honest ok=false for anything not recognised).
func canaryLiteral(pgType, canary string) (string, bool) {
	text, ok := canaryText(pgType, canary)
	if !ok {
		return "", false
	}
	base, _ := baseType(pgType)
	if base == "uuid" {
		return "'" + text + "'::uuid", true
	}
	return "'" + text + "'", true
}

// uniqueIndexColumnSets returns the column-name set of every unique or
// primary-key index on relation r, one slice per index. Used to find which
// columns genuinely need a fresh, non-colliding value when seeding from a
// sample (see columnsNeedingSyntheticValues) — CheckA4's rowKeyColumns reads
// only the primary key for a different purpose (row identity), so this is
// deliberately broader: TGD-BL-28's collision was on a primary key, but any
// unique index carries the identical risk.
func uniqueIndexColumnSets(ctx context.Context, db *sql.DB, r schema.Relation) ([][]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT i.indexrelid, a.attname
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		 WHERE n.nspname = $1 AND c.relname = $2 AND (i.indisunique OR i.indisprimary)
		 ORDER BY i.indexrelid, array_position(i.indkey, a.attnum)`, r.Schema, r.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var order []int64
	sets := map[int64][]string{}
	for rows.Next() {
		var idx int64
		var col string
		if err := rows.Scan(&idx, &col); err != nil {
			return nil, err
		}
		if _, seen := sets[idx]; !seen {
			order = append(order, idx)
		}
		sets[idx] = append(sets[idx], col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([][]string, 0, len(order))
	for _, idx := range order {
		out = append(out, sets[idx])
	}
	return out, nil
}

// columnsNeedingSyntheticValues returns the set of columns that must get a
// fresh, per-row-distinct synthetic value rather than a value borrowed from a
// sampled row: members of a unique or primary-key index that does NOT
// already include the tenant column.
//
// A unique index that DOES include the tenant column is automatically
// satisfied by canaryLiteral alone — CanaryA and CanaryB always differ, so
// any tuple containing one of them already differs from the other row's
// tuple, whatever the rest of the tuple holds. audit_logs' plain `id uuid`
// primary key does not include the tenant column, so id needs a synthetic
// value; organization_members' composite `(organization_id, user_id)`
// primary key does, so user_id can safely be borrowed from the sample —
// verified directly against both real shapes in canary_type_test.go.
func columnsNeedingSyntheticValues(ctx context.Context, db *sql.DB, r schema.Relation) (map[string]bool, error) {
	sets, err := uniqueIndexColumnSets(ctx, db, r)
	if err != nil {
		return nil, err
	}
	need := map[string]bool{}
	for _, set := range sets {
		includesTenant := false
		for _, c := range set {
			if c == r.TenantColumn {
				includesTenant = true
				break
			}
		}
		if includesTenant {
			continue
		}
		for _, c := range set {
			need[c] = true
		}
	}
	return need, nil
}

// sampleRowKeys returns the ctid of up to n existing rows in relation r, in
// no particular order — just something with n rows in it, if that many
// exist.
func sampleRowKeys(ctx context.Context, db *sql.DB, r schema.Relation, n int) ([]string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT ctid FROM %s LIMIT %d", qualified(r), n))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ctid string
		if err := rows.Scan(&ctid); err != nil {
			return nil, err
		}
		out = append(out, ctid)
	}
	return out, rows.Err()
}

// rowPlan is one canary row's worth of column plan: for every column
// SeedCanaries must supply a value for (everything except identity/default
// columns, which the database supplies), the SQL expression to use.
type rowPlan struct {
	insertCols []string
	exprs      []string
}

// planRow builds one canary row's column plan. source selects where each
// column's value comes from:
//   - the tenant column always gets canaryLiteral for the given canary identity.
//   - a column in needSynthetic always gets columnSampleValue(ctx, db, c, idx)
//     — idx must differ between the two canary rows on the same column (TGD-BL-28).
//   - identity/default columns are omitted (the database supplies them).
//   - every other column is sourced via borrowExpr(c), which the caller
//     controls: a bare column reference when copying from a sampled row
//     (SELECT ... FROM ... WHERE ctid = ...), or a fixture-supplied literal
//     (INSERT ... VALUES) when there was no row to sample.
func planRow(ctx context.Context, db *sql.DB, r schema.Relation, cols []column, needSynthetic map[string]bool,
	canary string, colIdx func(colPos int) int, borrowExpr func(c column) (string, bool, error)) (rowPlan, string, error) {
	var plan rowPlan
	for i, c := range cols {
		switch {
		case c.Name == r.TenantColumn:
			lit, ok := canaryLiteral(c.Type, canary)
			if !ok {
				return rowPlan{}, fmt.Sprintf("tenant column %q has unsupported type %q for canary seeding", c.Name, c.Type), nil
			}
			plan.insertCols = append(plan.insertCols, quoteIdent(c.Name))
			plan.exprs = append(plan.exprs, lit)
		case c.IsIdentity || c.HasDefault:
			continue
		case needSynthetic[c.Name]:
			v, ok, err := columnSampleValue(ctx, db, c, colIdx(i))
			if err != nil {
				return rowPlan{}, "", fmt.Errorf("column %q: enum lookup failed: %w", c.Name, err)
			}
			if !ok {
				return rowPlan{}, fmt.Sprintf("column %q has unsupported type %q", c.Name, c.Type), nil
			}
			plan.insertCols = append(plan.insertCols, quoteIdent(c.Name))
			plan.exprs = append(plan.exprs, v)
		default:
			expr, ok, err := borrowExpr(c)
			if err != nil {
				return rowPlan{}, "", err
			}
			if !ok {
				continue // nullable, no fixture value: leave it NULL, matching the pre-sampling default.
			}
			plan.insertCols = append(plan.insertCols, quoteIdent(c.Name))
			plan.exprs = append(plan.exprs, expr)
		}
	}
	return plan, "", nil
}

// seedFromSample builds and runs the two-row INSERT for a relation with at
// least two existing rows to copy: every column not the tenant column, an
// identity/default column, or a needSynthetic column is borrowed verbatim
// from the sampled row (a plain column reference in the SELECT list) — real
// data that, by having already been valid in this exact table, satisfies
// every constraint on it by construction: type, cross-column CHECK, and any
// FK (whose referenced row is already present, since the probe is a full
// data-and-schema TEMPLATE copy of the target, not a partial one — see
// §9.6's FK-ordering analysis).
func seedFromSample(ctx context.Context, db *sql.DB, conn *sql.Conn, r schema.Relation, cols []column,
	needSynthetic map[string]bool, ctidA, ctidB string) (string, error) {
	borrow := func(c column) (string, bool, error) { return quoteIdent(c.Name), true, nil }

	planA, unsupportedA, err := planRow(ctx, db, r, cols, needSynthetic, CanaryA, func(i int) int { return 2 * i }, borrow)
	if err != nil {
		return "", err
	}
	if unsupportedA != "" {
		return unsupportedA, nil
	}
	planB, unsupportedB, err := planRow(ctx, db, r, cols, needSynthetic, CanaryB, func(i int) int { return 2*i + 1 }, borrow)
	if err != nil {
		return "", err
	}
	if unsupportedB != "" {
		return unsupportedB, nil
	}

	// Both rows must insert the same column LIST for UNION ALL to line up
	// positionally — true as long as no column's inclusion depends on which
	// canary it is, which holds for every branch in planRow.
	stmt := fmt.Sprintf(
		"INSERT INTO %s (%s) SELECT %s FROM %s WHERE ctid = %s UNION ALL SELECT %s FROM %s WHERE ctid = %s",
		qualified(r), strings.Join(planA.insertCols, ", "),
		strings.Join(planA.exprs, ", "), qualified(r), ctidLiteral(ctidA),
		strings.Join(planB.exprs, ", "), qualified(r), ctidLiteral(ctidB))
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return "", fmt.Errorf("insert: %w", err)
	}
	return "", nil
}

// ctidLiteral formats a ctid value (already read back from this same table
// moments earlier, via sampleRowKeys) as a SQL literal. ctid's text form
// (e.g. "(0,1)") is entirely numeric and parentheses — not user input, never
// containing a quote character, so embedding it directly is as safe as this
// file's other self-generated literals.
func ctidLiteral(ctid string) string {
	return "'" + ctid + "'::tid"
}

// seedFromFixture builds and runs the two-row INSERT for a relation with
// fewer than two existing rows to sample — the fallback for a freshly
// migrated or never-exercised table (coder's own post-migration state,
// §9.1). fixture must supply exactly two rows; each is consulted for every
// column not otherwise determined (tenant, identity/default, needSynthetic),
// and a NOT NULL column present in neither the fixture nor covered by those
// is a named, reported error — never a silent NULL and never the old
// unconditional synthetic generator reappearing for a class of column this
// mechanism has no way to reason about on its own.
func seedFromFixture(ctx context.Context, db *sql.DB, conn *sql.Conn, r schema.Relation, cols []column,
	needSynthetic map[string]bool, fixture []map[string]string) (string, error) {
	if len(fixture) == 0 {
		return "no rows to sample (fewer than 2 exist) and no fixture supplied", nil
	}
	if len(fixture) != 2 {
		return fmt.Sprintf("fixture must supply exactly 2 rows, got %d", len(fixture)), nil
	}

	buildRow := func(canary string, idxBase func(int) int, fx map[string]string) (rowPlan, string, error) {
		borrow := func(c column) (string, bool, error) {
			v, ok := fx[c.Name]
			if !ok {
				if !c.NotNull {
					return "", false, nil // leave NULL, as sampling would for an absent nullable value.
				}
				return "", false, fmt.Errorf("fixture missing required column %q", c.Name)
			}
			return v, true, nil
		}
		return planRow(ctx, db, r, cols, needSynthetic, canary, idxBase, borrow)
	}

	planA, unsupportedA, err := buildRow(CanaryA, func(i int) int { return 2 * i }, fixture[0])
	if err != nil {
		return "", err
	}
	if unsupportedA != "" {
		return unsupportedA, nil
	}
	planB, unsupportedB, err := buildRow(CanaryB, func(i int) int { return 2*i + 1 }, fixture[1])
	if err != nil {
		return "", err
	}
	if unsupportedB != "" {
		return unsupportedB, nil
	}

	// Unlike the sample path, the two rows' column lists are not guaranteed
	// identical (a nullable column absent from one fixture row but present
	// in the other), so each gets its own INSERT rather than a UNION ALL.
	for _, p := range []rowPlan{planA, planB} {
		stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			qualified(r), strings.Join(p.insertCols, ", "), strings.Join(p.exprs, ", "))
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return "", fmt.Errorf("insert: %w", err)
		}
	}
	return "", nil
}

// SeedCanaries inserts one row per canary tenant into every scoped relation
// it can, and reports the ones it could not.
//
// TGD-BL-29/§9.6: the primary mechanism is sampling, not synthesis. For each
// relation, two existing rows are copied (seedFromSample) with only the
// tenant column rewritten — every other constraint (type, uniqueness via
// columnsNeedingSyntheticValues, cross-column CHECK, FK) is satisfied by
// construction, because the row was already valid in this exact table.
// Synthetic per-row generation (sampleValue/columnSampleValue, TGD-BL-27/28)
// survives narrowly, not as the primary path: it supplies a fresh value only
// for columns a unique index requires to differ and that borrowing verbatim
// from the sample would collide on. A relation with fewer than two existing
// rows falls back to an operator-supplied fixture (seedFromFixture); with
// neither, it is skipped with a named reason, never silently and never via
// the old unconditional generator.
//
// Reads for sampling operate entirely on db (the probe) — never a second
// connection to the target — which is what makes "read-only against the
// target" provable by this function's own signature, not merely observed:
// there is no argument through which it could reach the target at all. The
// probe's rows are themselves a read of the target (CreateProbeDatabase's
// TEMPLATE copy), already covered by that function's own contract.
//
// Foreign keys are disabled for the session rather than relied upon to
// resolve automatically. For a sampled row this is defence in depth, not a
// load-bearing requirement — the probe is a full data-and-schema TEMPLATE
// copy of the target, so any parent row a sampled row's FK points to is
// already present in the probe by construction (§9.6). It remains load-
// bearing for the fixture and needSynthetic paths, whose values are not
// guaranteed to reference anything real.
// SeedSource records how a relation's canary rows were populated —
// TGD-BL-36's distinction, load-bearing for what an A1/A4 pass on that
// table actually proves. SeedSourceSampled means the rows are the target's
// own real historical data with only the tenant column rewritten
// (seedFromSample): a row-level pass says something about the target's
// actual schema and data. SeedSourceFixture means the rows are hand-authored
// by whoever wrote the policy file (seedFromFixture): a row-level pass on a
// fixture-seeded table proves the oracle mechanism works against a
// constraint-satisfying shape, not that it has been exercised against
// anything the target itself produced — a different, weaker strength of
// evidence that must not be reported identically to a sampled pass.
type SeedSource string

const (
	SeedSourceSampled SeedSource = "sampled"
	SeedSourceFixture SeedSource = "fixture"
)

// SeededTable records one relation SeedCanaries successfully seeded, and
// which mechanism (SeedSource) populated it.
type SeededTable struct {
	Table  string
	Source SeedSource
}

func SeedCanaries(ctx context.Context, db *sql.DB, relations []schema.Relation) ([]SeededTable, []SkippedTable, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SET session_replication_role = replica"); err != nil {
		return nil, nil, fmt.Errorf("disable triggers on probe: %w", err)
	}

	var seeded []SeededTable
	var skipped []SkippedTable

	for _, r := range relations {
		cols, err := readColumns(ctx, db, r)
		if err != nil {
			skipped = append(skipped, SkippedTable{r.Qualified(), "read columns: " + err.Error()})
			continue
		}

		needSynthetic, err := columnsNeedingSyntheticValues(ctx, db, r)
		if err != nil {
			skipped = append(skipped, SkippedTable{r.Qualified(), "read unique indexes: " + err.Error()})
			continue
		}

		ctids, err := sampleRowKeys(ctx, db, r, 2)
		if err != nil {
			skipped = append(skipped, SkippedTable{r.Qualified(), "sample rows: " + err.Error()})
			continue
		}

		var reason string
		var source SeedSource
		if len(ctids) == 2 {
			source = SeedSourceSampled
			reason, err = seedFromSample(ctx, db, conn, r, cols, needSynthetic, ctids[0], ctids[1])
		} else {
			source = SeedSourceFixture
			reason, err = seedFromFixture(ctx, db, conn, r, cols, needSynthetic, r.Fixture)
		}
		if err != nil {
			skipped = append(skipped, SkippedTable{r.Qualified(), err.Error()})
			continue
		}
		if reason != "" {
			skipped = append(skipped, SkippedTable{r.Qualified(), reason})
			continue
		}
		seeded = append(seeded, SeededTable{Table: r.Qualified(), Source: source})
	}
	return seeded, skipped, nil
}

// CreateProbeDatabase creates a probe database from a template.
//
// The application's database is never written to; template copying is a read of
// it. PostgreSQL requires no other session to be connected to the template, so
// the caller must not hold a connection to it.
func CreateProbeDatabase(ctx context.Context, admin *sql.DB, template, probe string) error {
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(probe))); err != nil {
		return fmt.Errorf("drop stale probe: %w", err)
	}
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", quoteIdent(probe), quoteIdent(template))); err != nil {
		return fmt.Errorf("create probe from template %s: %w", template, err)
	}
	return nil
}

// DropProbeDatabase removes the probe. Callers must invoke this on every exit
// path, including failure.
func DropProbeDatabase(ctx context.Context, admin *sql.DB, probe string) error {
	_, err := admin.ExecContext(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(probe)))
	return err
}

// CreateRestrictedRole creates a login role that is subject to RLS: not a
// superuser, without BYPASSRLS, and not owning the tables it reads.
//
// Grants cover SELECT, INSERT, UPDATE and DELETE — not SELECT alone. A1–A4
// only ever read, but TGD-US-06 AC-6 explicitly anticipates the differ dry-
// running captured INSERT/UPDATE/DELETE statements (inside a transaction that
// is always rolled back — see internal/differ), which requires the privilege
// to attempt the statement at all, independent of whatever RLS's USING/WITH
// CHECK clauses separately decide once the attempt is made. Found by running
// the differ's own insert-fixture test against a SELECT-only role and
// watching it fail on a privilege error rather than an RLS decision.
func CreateRestrictedRole(ctx context.Context, db *sql.DB, role, password string) error {
	stmts := []string{
		fmt.Sprintf("DROP ROLE IF EXISTS %s", quoteIdent(role)),
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS",
			quoteIdent(role), strings.ReplaceAll(password, "'", "''")),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", quoteIdent(role)),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", quoteIdent(role)),
		// A serial/bigserial column's DEFAULT calls nextval() on its backing
		// sequence, which needs its own grant separate from the table's — an
		// INSERT that never mentions the sequence by name still fails with
		// "permission denied for sequence" without this. Found the same way
		// as the statement above: by running the differ's insert fixture and
		// watching the specific error.
		fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", quoteIdent(role)),
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("create restricted role: %w", err)
		}
	}
	return nil
}
