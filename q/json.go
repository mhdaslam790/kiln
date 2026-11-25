package q

// JSONPath is a value inside a JSON column, for filters:
//
//	q.JSON("prefs", "theme").Eq("dark")
//	q.JSON("prefs", "limits", "max").Gt(10)
//	q.JSON("doc", "items", "0", "sku").Eq("A-1") // a key of digits is an array index
//
// It works on PostgreSQL, MySQL, SQLite and SQL Server, on json, jsonb and
// text columns. The value's Go type picks the comparison: strings compare
// as text, numbers numerically, bools as JSON true/false. Keys are passed
// as query parameters, never spliced into the SQL.
type JSONPath struct {
	Field string
	Path  []string
}

// JSON names the value at path (object keys, or digits for array
// indexes) in the JSON column field.
func JSON(field string, path ...string) JSONPath {
	return JSONPath{Field: field, Path: append([]string(nil), path...)}
}

// JSONCond compares a value inside a JSON column (see JSON).
type JSONCond struct {
	Op     Op // OpEq OpNe OpLt OpLte OpGt OpGte OpIn OpNull OpNotNull
	Field  string
	Path   []string
	Value  any
	Values []any // OpIn
}

// JSONContainsCond matches rows whose JSON column contains Value (see
// JSONContains).
type JSONContainsCond struct {
	Field string
	Value any
}

func (JSONCond) isFilter()                {}
func (JSONContainsCond) isFilter()        {}
func (f JSONCond) Build(q *Query)         { q.WhereCond = And(q.WhereCond, f) }
func (f JSONContainsCond) Build(q *Query) { q.WhereCond = And(q.WhereCond, f) }
func (j JSONPath) cond(op Op, v any) Filter {
	return JSONCond{Op: op, Field: j.Field, Path: j.Path, Value: v}
}

func (j JSONPath) Eq(v any) Filter  { return j.cond(OpEq, v) }
func (j JSONPath) Ne(v any) Filter  { return j.cond(OpNe, v) }
func (j JSONPath) Lt(v any) Filter  { return j.cond(OpLt, v) }
func (j JSONPath) Lte(v any) Filter { return j.cond(OpLte, v) }
func (j JSONPath) Gt(v any) Filter  { return j.cond(OpGt, v) }
func (j JSONPath) Gte(v any) Filter { return j.cond(OpGte, v) }

// In matches values equal to one of vs (all strings, or all numbers).
func (j JSONPath) In(vs ...any) Filter {
	return JSONCond{Op: OpIn, Field: j.Field, Path: j.Path, Values: vs}
}

// IsNull matches rows where the path is missing or holds JSON null.
func (j JSONPath) IsNull() Filter { return j.cond(OpNull, nil) }

// NotNull matches rows where the path holds a value other than null.
func (j JSONPath) NotNull() Filter { return j.cond(OpNotNull, nil) }

// JSONContains matches rows whose JSON column contains v (encoded as JSON):
// all its object keys with the same values, or all its array elements.
//
//	q.JSONContains("prefs", map[string]any{"beta": true})
//	q.JSONContains("tags", []string{"go"})
//
// PostgreSQL (@>) and MySQL (JSON_CONTAINS) only; other databases return an
// error rather than a slow emulation.
func JSONContains(field string, v any) Filter { return JSONContainsCond{Field: field, Value: v} }
