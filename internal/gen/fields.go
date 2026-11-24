package gen

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mhdaslam790/kiln/internal/meta"
)

// FieldSpec is a parsed "name:type[:modifier...]" argument.
//
// Types: string text int bigint float decimal bool datetime date time bytes
// json uuid references
// Modifiers: null required unique index limit=N default=V precision=P scale=S
type FieldSpec struct {
	Column    string
	Kind      string // normalized type keyword
	Null      bool
	Required  bool // explicit "required"
	Unique    bool
	Index     bool
	Limit     int
	Precision int
	Scale     int
	Default   *string
	RefTable  string // for references
	// userDefault is true when Default came from the command line (and is
	// therefore declared on the model), not from PrepareForAdd.
	userDefault bool
}

var kindAliases = map[string]string{
	"string": "string", "varchar": "string", "str": "string",
	"text": "text",
	"int":  "int", "integer": "int", "int32": "int",
	"bigint": "bigint", "int64": "bigint", "long": "bigint",
	"float": "float", "float64": "float", "double": "float",
	"decimal": "decimal", "numeric": "decimal", "money": "decimal",
	"bool": "bool", "boolean": "bool",
	"datetime": "datetime", "timestamp": "datetime", "timestamptz": "datetime",
	"date": "date", "time": "time",
	"bytes": "bytes", "blob": "bytes", "binary": "bytes",
	"json": "json", "jsonb": "json",
	"uuid":       "uuid",
	"references": "references", "ref": "references", "belongs_to": "references",
}

// ParseField parses one CLI field argument.
func ParseField(arg string) (FieldSpec, error) {
	parts := strings.Split(arg, ":")
	if len(parts) < 2 || parts[0] == "" {
		return FieldSpec{}, fmt.Errorf("invalid field %q, expected name:type[:modifier...]", arg)
	}
	kind, ok := kindAliases[strings.ToLower(parts[1])]
	if !ok {
		return FieldSpec{}, fmt.Errorf("field %q: unknown type %q", parts[0], parts[1])
	}
	f := FieldSpec{Column: meta.Snake(parts[0]), Kind: kind}
	if kind == "references" {
		base := strings.TrimSuffix(f.Column, "_id")
		f.Column = base + "_id"
		f.RefTable = meta.Plural(base)
		f.Index = true
	}
	for _, mod := range parts[2:] {
		key, val, hasVal := strings.Cut(mod, "=")
		switch strings.ToLower(key) {
		case "null", "nullable", "optional":
			f.Null = true
		case "required", "notnull":
			f.Required = true
		case "unique":
			f.Unique = true
		case "index":
			f.Index = true
		case "default":
			if !hasVal {
				return f, fmt.Errorf("field %q: default needs a value (default=x)", parts[0])
			}
			v := val
			f.Default, f.userDefault = &v, true
		case "limit", "size", "precision", "scale":
			n, err := strconv.Atoi(val)
			if err != nil || !hasVal {
				return f, fmt.Errorf("field %q: %s needs a number", parts[0], key)
			}
			switch strings.ToLower(key) {
			case "limit", "size":
				f.Limit = n
			case "precision":
				f.Precision = n
			default:
				f.Scale = n
			}
		case "to", "table":
			f.RefTable = val
		default:
			return f, fmt.Errorf("field %q: unknown modifier %q", parts[0], mod)
		}
	}
	if f.Kind == "json" || f.Kind == "bytes" {
		f.Null = true // nil maps / slices are stored as NULL
	}
	return f, nil
}

// zeroDefault returns a literal default matching the Go zero value, used so
// NOT NULL columns can be added to tables that already contain rows.
func (f FieldSpec) zeroDefault() (string, bool) {
	switch f.Kind {
	case "string", "text":
		return `""`, true
	case "int", "bigint", "float", "decimal", "references":
		return "0", true
	case "bool":
		return "false", true
	}
	return "", false
}

// PrepareForAdd adjusts a spec for adding to an existing table: NOT NULL
// columns get a zero default, and types without a natural zero become nullable.
func (f *FieldSpec) PrepareForAdd() {
	if f.Null || f.Default != nil {
		return
	}
	if f.Kind == "references" {
		// A zero default would violate the foreign key on existing rows.
		f.Null = !f.Required
		return
	}
	if lit, ok := f.zeroDefault(); ok {
		f.Default = &lit
		return
	}
	if !f.Required {
		f.Null = true
	}
}

// GoName is the struct field name.
func (f FieldSpec) GoName() string { return fieldName(f.Column) }

// fieldName is the Go field name for a column. A column whose name would
// clash with a method kiln models use (table_name -> TableName) gets a
// "Col" suffix; its db tag keeps the real column name.
func fieldName(column string) string {
	n := meta.Camel(column)
	if n == "TableName" {
		n += "Col"
	}
	return n
}

// GoType is the struct field type.
func (f FieldSpec) GoType() string {
	var t string
	switch f.Kind {
	case "string", "text", "uuid":
		t = "string"
	case "int":
		t = "int"
	case "bigint", "references":
		t = "int64"
	case "float", "decimal":
		t = "float64"
	case "bool":
		t = "bool"
	case "datetime", "date", "time":
		t = "time.Time"
	case "bytes":
		return "[]byte"
	case "json":
		return "map[string]any"
	}
	if f.Null {
		return "*" + t
	}
	return t
}

// Tag is the struct tag, including a schema tag describing what the Go
// type alone cannot (so `kiln diff` agrees with the generated migration).
func (f FieldSpec) Tag() string {
	db := f.Column
	if f.Kind == "json" {
		db += ",json"
	}
	tag := `db:"` + db + `"`
	if st := f.SchemaTag(); st != "" {
		tag += ` schema:"` + st + `"`
	}
	return "`" + tag + "`"
}

// SchemaTag returns the schema tag value for the field.
func (f FieldSpec) SchemaTag() string {
	var o []string
	switch f.Kind {
	case "text", "decimal", "date", "time", "uuid":
		o = append(o, "type:"+f.Kind)
	case "references":
		o = append(o, "references:"+f.RefTable)
	}
	if f.Limit > 0 {
		o = append(o, fmt.Sprintf("limit:%d", f.Limit))
	}
	if f.Precision > 0 {
		o = append(o, fmt.Sprintf("precision:%d", f.Precision))
	}
	if f.Scale > 0 {
		o = append(o, fmt.Sprintf("scale:%d", f.Scale))
	}
	if f.Unique {
		o = append(o, "unique")
	}
	if f.Index && !f.Unique && f.Kind != "references" {
		o = append(o, "index")
	}
	if f.userDefault && f.Default != nil && !strings.ContainsAny(*f.Default, ",`\"") {
		o = append(o, "default:"+*f.Default)
	}
	return strings.Join(o, ",")
}

// DSL renders the table-builder call, e.g. t.String("name", schema.Required()).
func (f FieldSpec) DSL(recv string) []string {
	if f.Kind == "references" {
		opt := ""
		if !f.Null {
			opt = ", schema.Required()"
		}
		return []string{fmt.Sprintf("%s.References(%q, %q%s)", recv, f.Column, f.RefTable, opt)}
	}
	method := map[string]string{
		"string": "String", "text": "Text", "int": "Int", "bigint": "BigInt", "float": "Float",
		"decimal": "Decimal", "bool": "Bool", "datetime": "DateTime", "date": "Date", "time": "Time",
		"bytes": "Bytes", "json": "JSON", "uuid": "UUID",
	}[f.Kind]
	out := []string{fmt.Sprintf("%s.%s(%q%s)", recv, method, f.Column, f.optsSuffix())}
	if f.Index && !f.Unique {
		out = append(out, fmt.Sprintf("%s.Index([]string{%q})", recv, f.Column))
	}
	return out
}

func (f FieldSpec) optsSuffix() string {
	var o []string
	if !f.Null {
		o = append(o, "schema.Required()")
	}
	if f.Unique {
		o = append(o, "schema.Unique()")
	}
	if f.Limit > 0 {
		o = append(o, fmt.Sprintf("schema.Limit(%d)", f.Limit))
	}
	if f.Precision > 0 {
		o = append(o, fmt.Sprintf("schema.Precision(%d)", f.Precision))
	}
	if f.Scale > 0 {
		o = append(o, fmt.Sprintf("schema.Scale(%d)", f.Scale))
	}
	if f.Default != nil {
		o = append(o, "schema.Default("+f.defaultLiteral()+")")
	}
	if len(o) == 0 {
		return ""
	}
	return ", " + strings.Join(o, ", ")
}

func (f FieldSpec) defaultLiteral() string {
	v := *f.Default
	switch f.Kind {
	case "int", "bigint", "float", "decimal", "references":
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return v
		}
	case "bool":
		if b, err := strconv.ParseBool(v); err == nil {
			return strconv.FormatBool(b)
		}
	}
	if strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) && len(v) >= 2 {
		return v
	}
	if strings.EqualFold(v, "now") || strings.EqualFold(v, "current_timestamp") {
		return `schema.Raw("CURRENT_TIMESTAMP")`
	}
	return strconv.Quote(v)
}

// SpecFromGo reconstructs a FieldSpec from a Go field type, used to build
// the Down side of drop-column.
func SpecFromGo(column, goType string, isJSON bool) FieldSpec {
	f := FieldSpec{Column: column}
	t := goType
	if strings.HasPrefix(t, "*") {
		f.Null = true
		t = t[1:]
	}
	switch {
	case isJSON:
		f.Kind, f.Null = "json", true
	case t == "string":
		f.Kind = "string"
	case t == "int64" || t == "uint64":
		f.Kind = "bigint"
	case strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint"):
		f.Kind = "int"
	case strings.HasPrefix(t, "float"):
		f.Kind = "float"
	case t == "bool":
		f.Kind = "bool"
	case t == "time.Time":
		f.Kind = "datetime"
	case t == "[]byte":
		f.Kind, f.Null = "bytes", true
	default:
		f.Kind, f.Null = "text", true
	}
	return f
}
