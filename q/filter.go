// Package q is a composable, immutable query builder.
//
//	q.From("users").
//		Where(q.Eq("active", true), q.Gt("age", 18)).
//		Sort("-created_at").
//		Limit(10)
//
// Filters and query parts are plain values; they carry no SQL and are
// compiled by a dialect (see package dialect).
package q

// Op is a comparison operator used by Cond.
type Op int

const (
	OpEq Op = iota
	OpNe
	OpLt
	OpLte
	OpGt
	OpGte
	OpIn
	OpNotIn
	OpLike
	OpNotLike
	OpNull
	OpNotNull
)

// Filter is a boolean expression usable in WHERE / HAVING clauses.
// Every Filter is also a Querier that ANDs itself into the query's WHERE.
type Filter interface {
	Querier
	isFilter()
}

// Cond compares a single field against a value.
type Cond struct {
	Op     Op
	Field  string
	Value  any   // for scalar ops
	Values []any // for In / NotIn
}

// AndFilter is the conjunction of its inner filters.
type AndFilter struct{ Inner []Filter }

// OrFilter is the disjunction of its inner filters.
type OrFilter struct{ Inner []Filter }

// NotFilter negates its inner filter.
type NotFilter struct{ Inner Filter }

// ExprFilter is a raw SQL fragment. Use '?' for arguments; the dialect
// rewrites them to its own placeholder style.
type ExprFilter struct {
	SQL  string
	Args []any
}

func (Cond) isFilter()       {}
func (AndFilter) isFilter()  {}
func (OrFilter) isFilter()   {}
func (NotFilter) isFilter()  {}
func (ExprFilter) isFilter() {}

func (f Cond) Build(q *Query)       { q.WhereCond = And(q.WhereCond, f) }
func (f AndFilter) Build(q *Query)  { q.WhereCond = And(q.WhereCond, f) }
func (f OrFilter) Build(q *Query)   { q.WhereCond = And(q.WhereCond, f) }
func (f NotFilter) Build(q *Query)  { q.WhereCond = And(q.WhereCond, f) }
func (f ExprFilter) Build(q *Query) { q.WhereCond = And(q.WhereCond, f) }

func Eq(field string, v any) Filter        { return Cond{Op: OpEq, Field: field, Value: v} }
func Ne(field string, v any) Filter        { return Cond{Op: OpNe, Field: field, Value: v} }
func Lt(field string, v any) Filter        { return Cond{Op: OpLt, Field: field, Value: v} }
func Lte(field string, v any) Filter       { return Cond{Op: OpLte, Field: field, Value: v} }
func Gt(field string, v any) Filter        { return Cond{Op: OpGt, Field: field, Value: v} }
func Gte(field string, v any) Filter       { return Cond{Op: OpGte, Field: field, Value: v} }
func Like(field, pattern string) Filter    { return Cond{Op: OpLike, Field: field, Value: pattern} }
func NotLike(field, pattern string) Filter { return Cond{Op: OpNotLike, Field: field, Value: pattern} }
func Null(field string) Filter             { return Cond{Op: OpNull, Field: field} }
func NotNull(field string) Filter          { return Cond{Op: OpNotNull, Field: field} }

// In matches rows whose field is one of values. An empty list matches nothing.
func In(field string, values ...any) Filter { return Cond{Op: OpIn, Field: field, Values: values} }

// NotIn matches rows whose field is none of values. An empty list matches everything.
func NotIn(field string, values ...any) Filter {
	return Cond{Op: OpNotIn, Field: field, Values: values}
}

// InOf is In for a typed slice: q.InOf("id", []int{1, 2, 3}).
func InOf[T any](field string, values []T) Filter {
	vs := make([]any, len(values))
	for i, v := range values {
		vs[i] = v
	}
	return In(field, vs...)
}

// Between matches lo <= field <= hi.
func Between(field string, lo, hi any) Filter { return And(Gte(field, lo), Lte(field, hi)) }

// Expr is a raw SQL filter fragment: q.Expr("lower(email) = ?", email).
func Expr(sql string, args ...any) Filter { return ExprFilter{SQL: sql, Args: args} }

// And combines filters with AND. Nil filters are skipped and nested ANDs flattened.
func And(filters ...Filter) Filter {
	// Fast path: at most one plain filter (e.g. a condition and a nil) is
	// returned as is, without allocating.
	var only Filter
	n := 0
	for _, f := range filters {
		switch f.(type) {
		case nil:
		case AndFilter:
			n = 2 // flatten below
		default:
			only = f
			n++
		}
	}
	switch n {
	case 0:
		return nil
	case 1:
		return only
	}
	var out []Filter
	for _, f := range filters {
		switch v := f.(type) {
		case nil:
		case AndFilter:
			out = append(out, v.Inner...)
		default:
			out = append(out, f)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return AndFilter{Inner: out}
}

// Or combines filters with OR. Nil filters are skipped.
func Or(filters ...Filter) Filter {
	var out []Filter
	for _, f := range filters {
		if f != nil {
			out = append(out, f)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return OrFilter{Inner: out}
}

// Not negates a filter.
func Not(f Filter) Filter { return NotFilter{Inner: f} }
