package dialect

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/mhdaslam790/kiln/q"
)

type jsonMode int

const (
	jsonText jsonMode = iota
	jsonNum
	jsonBool
)

func modeOf(v any) (jsonMode, error) {
	switch reflect.ValueOf(v).Kind() {
	case reflect.String:
		return jsonText, nil
	case reflect.Bool:
		return jsonBool, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return jsonNum, nil
	}
	return 0, fmt.Errorf("dialect: a JSON value can be compared with a string, number or bool, not %T", v)
}

// jsonPathArg is the path as the database expects it: a text[] literal for
// PostgreSQL (#>>), a $."key"[0] path elsewhere.
func (b *buf) jsonPathArg(path []string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	var sb strings.Builder
	if b.d.kind == KindPostgres {
		sb.WriteByte('{')
		for i, k := range path {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`"` + esc.Replace(k) + `"`)
		}
		sb.WriteByte('}')
		return sb.String()
	}
	sb.WriteByte('$')
	for _, k := range path {
		if k != "" && strings.Trim(k, "0123456789") == "" {
			sb.WriteString("[" + k + "]")
		} else {
			sb.WriteString(`."` + esc.Replace(k) + `"`)
		}
	}
	return sb.String()
}

// jsonExtract writes the expression for the value at path, in mode.
func (b *buf) jsonExtract(c q.JSONCond, m jsonMode) {
	col, path := b.d.ident(c.Field), b.jsonPathArg(c.Path)
	switch b.d.kind {
	case KindPostgres:
		b.w("(", col, "::jsonb #>> ")
		b.arg(path)
		b.w("::text[])")
		if m == jsonNum {
			b.w("::numeric")
		}
	case KindMySQL:
		if m == jsonText {
			b.w("JSON_UNQUOTE(JSON_EXTRACT(", col, ", ")
			b.arg(path)
			b.w("))")
		} else {
			b.w("JSON_EXTRACT(", col, ", ")
			b.arg(path)
			b.w(")")
		}
	case KindMSSQL:
		if m == jsonNum {
			b.w("TRY_CAST(JSON_VALUE(", col, ", ")
			b.arg(path)
			b.w(") AS FLOAT)")
		} else {
			b.w("JSON_VALUE(", col, ", ")
			b.arg(path)
			b.w(")")
		}
	default: // SQLite
		b.w("json_extract(", col, ", ")
		b.arg(path)
		b.w(")")
	}
}

// jsonValue writes a comparison value in mode.
func (b *buf) jsonValue(v any, m jsonMode) {
	if m != jsonBool {
		b.arg(v)
		return
	}
	t := reflect.ValueOf(v).Bool()
	switch b.d.kind {
	case KindSQLite:
		if t {
			b.arg(1)
		} else {
			b.arg(0)
		}
	case KindMySQL:
		b.w("JSON_EXTRACT(") // a JSON true/false (MariaDB has no CAST AS JSON)
		b.arg(fmt.Sprint(t))
		b.w(", '$')")
	default: // text 'true'/'false'
		b.arg(fmt.Sprint(t))
	}
}

func (b *buf) jsonCond(c q.JSONCond) error {
	if c.Value == nil && (c.Op == q.OpEq || c.Op == q.OpNe) {
		c.Op = map[q.Op]q.Op{q.OpEq: q.OpNull, q.OpNe: q.OpNotNull}[c.Op]
	}
	switch c.Op {
	case q.OpNull, q.OpNotNull:
		is := " IS NULL"
		if c.Op == q.OpNotNull {
			is = " IS NOT NULL"
		}
		if b.d.kind == KindMySQL { // JSON null is a value, not SQL NULL
			b.w("NULLIF(JSON_TYPE(JSON_EXTRACT(", b.d.ident(c.Field), ", ")
			b.arg(b.jsonPathArg(c.Path))
			b.w(")), 'NULL')", is)
			return nil
		}
		b.jsonExtract(c, jsonText)
		b.w(is)
		return nil
	case q.OpIn:
		if len(c.Values) == 0 {
			b.w("1=0")
			return nil
		}
		m, err := modeOf(c.Values[0])
		if err != nil {
			return err
		}
		for _, v := range c.Values[1:] {
			if mv, err := modeOf(v); err != nil || mv != m {
				return fmt.Errorf("dialect: JSON In values must all be strings, all numbers or all bools")
			}
		}
		b.jsonExtract(c, m)
		b.w(" IN (")
		for i, v := range c.Values {
			if i > 0 {
				b.w(", ")
			}
			b.jsonValue(v, m)
		}
		b.w(")")
		return nil
	}
	op, ok := opSQL[c.Op]
	if !ok || c.Op == q.OpLike || c.Op == q.OpNotLike {
		return fmt.Errorf("dialect: unsupported JSON operator %d", c.Op)
	}
	m, err := modeOf(c.Value)
	if err != nil {
		return err
	}
	if m == jsonBool && c.Op != q.OpEq && c.Op != q.OpNe {
		return fmt.Errorf("dialect: JSON bools can only be compared with Eq or Ne")
	}
	b.jsonExtract(c, m)
	b.w(op)
	b.jsonValue(c.Value, m)
	return nil
}

func (b *buf) jsonContains(c q.JSONContainsCond) error {
	js, err := json.Marshal(c.Value)
	if err != nil {
		return fmt.Errorf("dialect: JSONContains value: %w", err)
	}
	col := b.d.ident(c.Field)
	switch b.d.kind {
	case KindPostgres:
		b.w(col, "::jsonb @> ")
		b.arg(string(js))
		b.w("::jsonb")
	case KindMySQL:
		b.w("JSON_CONTAINS(", col, ", ")
		b.arg(string(js))
		b.w(")")
	default:
		return fmt.Errorf("dialect: JSONContains is supported on PostgreSQL and MySQL, not %s", b.d.kind)
	}
	return nil
}
