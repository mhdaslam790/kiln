package migrate

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/schema"
)

// declared records which column attributes the model states explicitly via
// the `schema` tag; only those are compared against the database, so
// hand-written migrations may refine anything the model leaves unspecified.
type declared struct {
	limit, precision, scale, def, sqlType bool
}

// ModelTable is the desired shape of a model's table.
type ModelTable struct {
	schema.TableState
	declared map[string]declared
	skip     map[string]bool // columns excluded from diffs (schema:"-")
}

// TableOf derives the desired table for a model struct (value or pointer).
//
// Types and nullability come from the Go fields (pointers and sql.Null*
// are nullable). Extra attributes come from the `schema` tag:
//
//	Email string `db:"email" schema:"unique,limit:120"`
//	Body  string `schema:"type:text"`
//	Price float64 `schema:"type:decimal,precision:10,scale:2,default:0"`
//	Slug  string `schema:"index"`                 // or index:name to group columns
//	Code  string `schema:"uniqueindex:uniq_code"` // composite unique index
//	OrgID int64  `schema:"references:orgs,ondelete:CASCADE"`
//	Legacy string `schema:"-"`                    // ignored by diffs
func TableOf(model any) (*ModelTable, error) {
	m, err := meta.Of(reflect.TypeOf(model))
	if err != nil {
		return nil, err
	}
	mt := &ModelTable{
		TableState: schema.TableState{Name: m.Table},
		declared:   map[string]declared{},
		skip:       map[string]bool{},
	}
	groups := map[string]*schema.CreateIndex{}
	var groupOrder []string
	addFK := func(col, ref, onDelete string) {
		refTable, refCol, _ := strings.Cut(ref, ".")
		if refCol == "" {
			refCol = "id"
		}
		mt.ForeignKeys = append(mt.ForeignKeys, schema.ForeignKey{
			Name: "fk_" + m.Table + "_" + col, Columns: []string{col},
			RefTable: refTable, RefColumns: []string{refCol}, OnDelete: onDelete,
		})
	}

	for _, f := range m.Fields {
		tag := parseSchemaTag(f.Schema)
		if _, ok := tag["-"]; ok {
			mt.skip[f.Column] = true
			continue
		}
		col, dec, err := columnOf(m, f, tag)
		if err != nil {
			return nil, err
		}
		mt.Columns = append(mt.Columns, col)
		mt.declared[col.Name] = dec
		if col.Primary {
			mt.PrimaryKey = append(mt.PrimaryKey, col.Name)
		}
		if v, ok := tag["index"]; ok {
			addGroup(groups, &groupOrder, m.Table, v, col.Name, false)
		}
		if v, ok := tag["uniqueindex"]; ok {
			addGroup(groups, &groupOrder, m.Table, v, col.Name, true)
		}
		if ref := tag["references"]; ref != "" {
			addFK(col.Name, ref, tag["ondelete"])
		}
	}
	// belongs_to associations imply a foreign key.
	if assocs, err := m.Assocs(); err == nil {
		for _, a := range assocs {
			if a.Kind != meta.BelongsTo || mt.hasFK(a.OwnerCol) || mt.skip[a.OwnerCol] {
				continue
			}
			tm, err := meta.Of(a.Elem)
			if err != nil {
				return nil, err
			}
			addFK(a.OwnerCol, tm.Table+"."+a.TargetCol, "")
		}
	}
	for _, name := range groupOrder {
		mt.Indexes = append(mt.Indexes, *groups[name])
	}
	// Foreign key columns get an index, like t.References does.
	for _, fk := range mt.ForeignKeys {
		if !mt.HasIndexOn(fk.Columns, false) {
			mt.Indexes = append(mt.Indexes, schema.CreateIndex{
				Table: m.Table, Name: schema.IndexName(m.Table, fk.Columns, false), Columns: fk.Columns,
			})
		}
	}
	if len(mt.PrimaryKey) == 1 {
		mt.PrimaryKey = nil // inline primary key
	} else {
		for i := range mt.Columns {
			mt.Columns[i].Primary = false
		}
	}
	return mt, nil
}

func (mt *ModelTable) hasFK(col string) bool {
	for _, fk := range mt.ForeignKeys {
		if len(fk.Columns) == 1 && fk.Columns[0] == col {
			return true
		}
	}
	return false
}

func addGroup(groups map[string]*schema.CreateIndex, order *[]string, table, name, col string, unique bool) {
	if name == "" {
		name = schema.IndexName(table, []string{col}, unique)
	}
	g := groups[name]
	if g == nil {
		g = &schema.CreateIndex{Table: table, Name: name, Unique: unique}
		groups[name] = g
		*order = append(*order, name)
	}
	g.Columns = append(g.Columns, col)
}

func parseSchemaTag(tag string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, ":")
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

var (
	timeType = reflect.TypeOf(time.Time{})
	nullable = map[reflect.Type]schema.ColumnType{
		reflect.TypeOf(sql.NullString{}):  schema.String,
		reflect.TypeOf(sql.NullInt64{}):   schema.BigInt,
		reflect.TypeOf(sql.NullInt32{}):   schema.Int,
		reflect.TypeOf(sql.NullInt16{}):   schema.SmallInt,
		reflect.TypeOf(sql.NullByte{}):    schema.SmallInt,
		reflect.TypeOf(sql.NullFloat64{}): schema.Float,
		reflect.TypeOf(sql.NullBool{}):    schema.Bool,
		reflect.TypeOf(sql.NullTime{}):    schema.DateTime,
	}
	typeNames = map[string]schema.ColumnType{
		"bool": schema.Bool, "smallint": schema.SmallInt, "int": schema.Int, "integer": schema.Int,
		"bigint": schema.BigInt, "float": schema.Float, "decimal": schema.Decimal, "string": schema.String,
		"varchar": schema.String, "text": schema.Text, "date": schema.Date, "datetime": schema.DateTime,
		"timestamp": schema.DateTime, "time": schema.Time, "bytes": schema.Bytes, "json": schema.JSON,
		"uuid": schema.UUID,
	}
)

func columnOf(m *meta.Model, f *meta.Field, tag map[string]string) (schema.Column, declared, error) {
	c := schema.Column{Name: f.Column}
	var dec declared
	t := f.Type
	null := false
	if t.Kind() == reflect.Pointer {
		null, t = true, t.Elem()
	}
	switch {
	case f.JSON:
		c.Type, null = schema.JSON, true
	case nullable[t] != "":
		c.Type, null = nullable[t], true
	case t == timeType:
		c.Type = schema.DateTime
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		c.Type, null = schema.Bytes, true
	case t.Kind() == reflect.Array && t.Len() == 16 && t.Elem().Kind() == reflect.Uint8:
		c.Type = schema.UUID
	default:
		switch t.Kind() {
		case reflect.Bool:
			c.Type = schema.Bool
		case reflect.Int8, reflect.Int16, reflect.Uint8:
			c.Type = schema.SmallInt
		case reflect.Int, reflect.Int32, reflect.Uint16, reflect.Uint32:
			c.Type = schema.Int
		case reflect.Int64, reflect.Uint, reflect.Uint64:
			c.Type = schema.BigInt
		case reflect.Float32, reflect.Float64:
			c.Type = schema.Float
		case reflect.String:
			c.Type = schema.String
		case reflect.Slice, reflect.Map: // a nil slice or map is stored as NULL
			c.Type, null = schema.Text, true
		default:
			c.Type = schema.Text
		}
	}
	if v, ok := tag["type"]; ok {
		ct, ok := typeNames[strings.ToLower(v)]
		if !ok {
			return c, dec, fmt.Errorf("%s.%s: unknown schema type %q", m.Type.Name(), f.Name, v)
		}
		c.Type = ct
	}
	if v, ok := tag["sqltype"]; ok {
		c.SQLType, dec.sqlType = v, true
	}
	if _, ok := tag["null"]; ok {
		null = true
	}
	if _, ok := tag["notnull"]; ok {
		null = false
	}
	if _, ok := tag["unique"]; ok {
		c.Unique = true
	}
	num := func(key string, dst *int, flag *bool) error {
		if v, ok := tag[key]; ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s.%s: schema %s must be a number", m.Type.Name(), f.Name, key)
			}
			*dst, *flag = n, true
		}
		return nil
	}
	if err := num("limit", &c.Limit, &dec.limit); err != nil {
		return c, dec, err
	}
	if err := num("precision", &c.Precision, &dec.precision); err != nil {
		return c, dec, err
	}
	if err := num("scale", &c.Scale, &dec.scale); err != nil {
		return c, dec, err
	}
	if v, ok := tag["default"]; ok {
		c.Default, c.HasDef, dec.def = parseDefault(c.Type, v), true, true
	}
	for _, pk := range m.PKs {
		if pk == f {
			c.Primary, null = true, false
			if pk.Auto && len(m.PKs) == 1 {
				c.Type = schema.ID
			}
		}
	}
	c.Required = !null
	return c, dec, nil
}

func parseDefault(t schema.ColumnType, v string) any {
	switch {
	case strings.EqualFold(v, "now"), strings.EqualFold(v, "current_timestamp"):
		return schema.Raw("CURRENT_TIMESTAMP")
	case strings.EqualFold(v, "null"):
		return nil
	}
	switch t {
	case schema.Bool:
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	case schema.SmallInt, schema.Int, schema.BigInt:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	case schema.Float, schema.Decimal:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return strings.Trim(v, `'"`)
}

// sortTables orders tables so referenced tables come first.
func sortTables(tables []*ModelTable) []*ModelTable {
	byName := map[string]*ModelTable{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	var out []*ModelTable
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(t *ModelTable)
	visit = func(t *ModelTable) {
		if state[t.Name] != 0 {
			return
		}
		state[t.Name] = 1
		for _, fk := range t.ForeignKeys {
			if dep := byName[fk.RefTable]; dep != nil && dep != t {
				visit(dep)
			}
		}
		state[t.Name] = 2
		out = append(out, t)
	}
	for _, t := range tables {
		visit(t)
	}
	return out
}
