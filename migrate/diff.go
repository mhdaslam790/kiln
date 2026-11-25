package migrate

import (
	"fmt"
	"strings"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/schema"
)

// DiffResult is the set of operations that bring a schema in line with
// the models, plus their inverse.
type DiffResult struct {
	Up    []schema.Op
	Down  []schema.Op
	Notes []string
}

// Empty reports whether the schema already matches the models.
func (r DiffResult) Empty() bool { return len(r.Up) == 0 }

// DiffMigrations replays migrations in memory (no database needed) and
// diffs the result against the models.
func DiffMigrations(migrations []Migration, models ...any) (DiffResult, error) {
	fns := make([]func(*schema.Schema), 0, len(migrations))
	for _, m := range migrations {
		fns = append(fns, m.Up)
	}
	st, err := schema.Replay(fns...)
	if err != nil {
		return DiffResult{}, fmt.Errorf("replaying migrations: %w", err)
	}
	return Diff(st, models...)
}

// Diff compares a schema state with model structs.
//
// The models are authoritative for which tables (of those that have a
// model) and columns exist, for column types and nullability, and for
// whatever their `schema` tags declare. Tables without a model, and
// indexes / foreign keys the models do not mention, are left alone.
func Diff(current *schema.State, models ...any) (DiffResult, error) {
	return DiffFor(nil, current, models...)
}

// DiffFor is Diff for a specific database: column types that the dialect
// stores identically (e.g. JSON and TEXT on SQLite) are not reported as
// changes. Use it when current comes from Inspect.
func DiffFor(d dialect.Dialect, current *schema.State, models ...any) (DiffResult, error) {
	var res DiffResult
	if current.Opaque > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d raw SQL / Go step(s) in the migrations were not modelled; review the result", current.Opaque))
	}
	var tables []*ModelTable
	seen := map[string]bool{}
	for _, m := range models {
		mt, err := TableOf(m)
		if err != nil {
			return res, err
		}
		if seen[mt.Name] {
			continue
		}
		seen[mt.Name] = true
		tables = append(tables, mt)
	}
	tables = sortTables(tables)

	var downs [][]schema.Op // per step, applied in reverse
	push := func(up []schema.Op, down ...schema.Op) {
		res.Up = append(res.Up, up...)
		downs = append(downs, down)
	}

	for _, want := range tables {
		cur := current.Tables[want.Name]
		if cur == nil {
			up := []schema.Op{schema.CreateTable{
				Name: want.Name, Columns: want.Columns, PrimaryKey: want.PrimaryKey, ForeignKeys: want.ForeignKeys,
			}}
			for _, ix := range want.Indexes {
				up = append(up, ix)
			}
			push(up, schema.DropTable{Name: want.Name})
			continue
		}
		diffTable(&res, d, cur, want, push)
	}

	for _, name := range current.TableNames() {
		if !seen[name] && name != "schema_migrations" && name != sqliteLockTable {
			res.Notes = append(res.Notes, fmt.Sprintf("table %s has no model; it was left untouched", name))
		}
	}
	for i := len(downs) - 1; i >= 0; i-- {
		res.Down = append(res.Down, downs[i]...)
	}
	return res, nil
}

func diffTable(res *DiffResult, d dialect.Dialect, cur *schema.TableState, want *ModelTable, push func([]schema.Op, ...schema.Op)) {
	t := want.Name
	var added, dropped []schema.Column
	fkFor := map[string]schema.ForeignKey{}
	for _, fk := range want.ForeignKeys {
		if len(fk.Columns) == 1 {
			fkFor[fk.Columns[0]] = fk
		}
	}

	for _, wc := range want.Columns {
		cc := cur.Column(wc.Name)
		if cc == nil {
			add := wc
			add.Primary = false
			if add.Required && !add.HasDef {
				if lit, ok := zeroDefault(add.Type); ok {
					add.Default, add.HasDef = lit, true
					res.Notes = append(res.Notes, fmt.Sprintf("%s.%s is NOT NULL: existing rows get default %s", t, wc.Name, literal(lit)))
				} else {
					add.Required = false
					res.Notes = append(res.Notes, fmt.Sprintf("%s.%s added as NULL (no safe default for existing rows); tighten it once backfilled", t, wc.Name))
				}
			}
			if fk, ok := fkFor[wc.Name]; ok && cur.ForeignKey(fk.Name) == nil {
				fk := fk
				add.Ref = &fk
			}
			push([]schema.Op{schema.AddColumn{Table: t, Column: add}}, schema.DropColumn{Table: t, Name: wc.Name})
			added = append(added, wc)
			continue
		}
		if changed, ok := columnChange(d, *cc, wc, want.declared[wc.Name]); ok {
			if cc.Primary || cc.Type == schema.ID {
				res.Notes = append(res.Notes, fmt.Sprintf("%s.%s: primary key differs from the model; change it by hand", t, wc.Name))
				continue
			}
			old := *cc
			old.Primary = false
			push([]schema.Op{schema.ChangeColumn{Table: t, Column: changed}}, schema.ChangeColumn{Table: t, Column: old})
		}
	}
	for _, cc := range cur.Columns {
		if want.Column(cc.Name) != nil || want.skip[cc.Name] {
			continue
		}
		restore := cc
		restore.Primary = false
		if restore.Required && !restore.HasDef {
			if lit, ok := zeroDefault(restore.Type); ok {
				restore.Default, restore.HasDef = lit, true
			} else {
				restore.Required = false
			}
		}
		push([]schema.Op{schema.DropColumn{Table: t, Name: cc.Name}}, schema.AddColumn{Table: t, Column: restore})
		dropped = append(dropped, cc)
	}
	for _, d := range dropped {
		for _, a := range added {
			if baseType(d.Type) == baseType(a.Type) {
				res.Notes = append(res.Notes, fmt.Sprintf("%s: %s dropped and %s added; if this is a rename, replace both with s.RenameColumn(%q, %q, %q) to keep the data", t, d.Name, a.Name, t, d.Name, a.Name))
			}
		}
	}

	for _, fk := range want.ForeignKeys {
		if cur.ForeignKey(fk.Name) != nil || hasFKOn(cur, fk.Columns) {
			continue
		}
		if len(fk.Columns) == 1 && cur.Column(fk.Columns[0]) == nil {
			continue // created inline with its new column
		}
		push([]schema.Op{schema.AddForeignKey{Table: t, Key: fk}}, schema.DropForeignKey{Table: t, Name: fk.Name})
	}
	for _, ix := range want.Indexes {
		if cur.HasIndexOn(ix.Columns, ix.Unique) {
			continue
		}
		push([]schema.Op{ix}, schema.DropIndex{Table: t, Name: ix.Name})
	}
	for _, wc := range want.Columns {
		if !wc.Unique || cur.Column(wc.Name) == nil || cur.HasIndexOn([]string{wc.Name}, true) {
			continue
		}
		ix := schema.CreateIndex{Table: t, Name: schema.IndexName(t, []string{wc.Name}, true), Columns: []string{wc.Name}, Unique: true}
		push([]schema.Op{ix}, schema.DropIndex{Table: t, Name: ix.Name})
	}
}

func hasFKOn(t *schema.TableState, cols []string) bool {
	for _, fk := range t.ForeignKeys {
		if strings.Join(fk.Columns, ",") == strings.Join(cols, ",") {
			return true
		}
	}
	return false
}

func baseType(t schema.ColumnType) schema.ColumnType {
	if t == schema.ID {
		return schema.BigInt
	}
	return t
}

// columnChange returns the column to change to when cur differs from want.
// Attributes the model does not declare are kept from cur.
func columnChange(d dialect.Dialect, cur, want schema.Column, dec declared) (schema.Column, bool) {
	out := cur
	out.Primary = false
	out.Unique = false // uniqueness is handled with indexes
	changed := false
	// With the same declared SQL type, the database type is that one and
	// the abstract type (string or text for an enum) doesn't matter.
	sqlSame := want.SQLType != "" && sameSQLType(cur.SQLType, want.SQLType)
	if !sqlSame && baseType(cur.Type) != baseType(want.Type) && !samePhysical(d, cur, want) {
		out.Type, changed = want.Type, true
		if dec.limit || want.Type != schema.String {
			out.Limit = want.Limit
		}
	}
	if (cur.Required || cur.Primary) != (want.Required || want.Primary) {
		out.Required, changed = want.Required, true
	}
	if dec.limit && effLimit(cur) != effLimit(want) {
		out.Limit, changed = want.Limit, true
	}
	if dec.precision && cur.Precision != want.Precision {
		out.Precision, changed = want.Precision, true
	}
	if dec.scale && cur.Scale != want.Scale {
		out.Scale, changed = want.Scale, true
	}
	if dec.sqlType && !sameSQLType(cur.SQLType, want.SQLType) {
		out.SQLType, changed = want.SQLType, true
	}
	if dec.def && (cur.HasDef != want.HasDef || fmt.Sprint(cur.Default) != fmt.Sprint(want.Default)) {
		out.Default, out.HasDef, changed = want.Default, want.HasDef, true
	}
	return out, changed
}

// samePhysical reports whether the dialect stores both columns with the
// same SQL type.
func samePhysical(d dialect.Dialect, a, b schema.Column) bool {
	if d == nil {
		return false
	}
	a.Type, b.Type = baseType(a.Type), baseType(b.Type)
	if a.Type == schema.String && a.Limit == 0 {
		a.Limit = 255
	}
	if b.Type == schema.String && b.Limit == 0 {
		b.Limit = 255
	}
	ta, err1 := d.ColumnType(a)
	tb, err2 := d.ColumnType(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ta, tb)
}

func effLimit(c schema.Column) int {
	if c.Type == schema.String && c.Limit == 0 {
		return 255
	}
	return c.Limit
}

func zeroDefault(t schema.ColumnType) (any, bool) {
	switch t {
	case schema.String, schema.Text:
		return "", true
	case schema.SmallInt, schema.Int, schema.BigInt, schema.Float, schema.Decimal:
		return 0, true
	case schema.Bool:
		return false, true
	}
	return nil, false
}

// sameSQLType compares two SQL types, ignoring case and identifier quotes
// ("mood" and mood, TEXT[] and text[]).
func sameSQLType(a, b string) bool {
	if strings.HasSuffix(a, "[]") != strings.HasSuffix(b, "[]") {
		return false
	}
	return canonSQLType(a) == canonSQLType(b)
}

// sqlTypeAlias maps PostgreSQL's alternative spellings to one name.
var sqlTypeAlias = map[string]string{
	"int": "integer", "int4": "integer", "int8": "bigint", "int2": "smallint",
	"float8": "double precision", "float4": "real", "bool": "boolean",
	"varchar": "character varying", "bpchar": "character", "char": "character",
}

func canonSQLType(s string) string {
	s = strings.ToLower(strings.NewReplacer(`"`, "", "`", "", "[", "", "]", "").Replace(s))
	s = strings.Join(strings.Fields(s), " ")
	base, args, _ := strings.Cut(s, "(")
	base = strings.TrimSpace(base)
	if a, ok := sqlTypeAlias[base]; ok {
		base = a
	}
	if args != "" {
		return base + "(" + args
	}
	return base
}
