package q

import "strings"

// Querier modifies a Query. Filters, Sort, Limit, etc. are all Queriers,
// so they can be passed loosely to repository methods:
//
//	repo.FindAll(ctx, q.Eq("active", true), q.Sort("-id"), q.Limit(5))
type Querier interface {
	Build(*Query)
}

// QuerierFunc adapts a function to a Querier (useful for reusable scopes).
type QuerierFunc func(*Query)

func (f QuerierFunc) Build(q *Query) { f(q) }

// JoinKind is the SQL join type.
type JoinKind string

const (
	InnerJoin JoinKind = "INNER JOIN"
	LeftJoin  JoinKind = "LEFT JOIN"
	RightJoin JoinKind = "RIGHT JOIN"
	FullJoin  JoinKind = "FULL JOIN"
)

// JoinClause joins another table using a raw ON expression.
type JoinClause struct {
	Kind  JoinKind
	Table string
	On    string
	Args  []any
}

// SortField orders by a single field.
type SortField struct {
	Field string
	Desc  bool
}

// Query is the full description of a SELECT (and the scope of UPDATE/DELETE).
// The zero value selects * from an unspecified table.
type Query struct {
	Table      string
	Fields     []string
	IsDistinct bool
	Joins      []JoinClause
	WhereCond  Filter
	GroupBy    []string
	HavingCond Filter
	Sorts      []SortField
	LimitN     int
	OffsetN    int
	Lock       bool
	// Unscoped disables automatic scopes such as soft-delete filtering.
	IsUnscoped bool
	// RawSQL, when set, replaces the generated statement entirely.
	RawSQL  string
	RawArgs []any
}

// From starts a query on table.
func From(table string) Query { return Query{Table: table} }

// Build applies the query's non-zero parts onto another query, so a Query
// is itself a Querier.
func (qq Query) Build(dst *Query) {
	if qq.Table != "" {
		dst.Table = qq.Table
	}
	if len(qq.Fields) > 0 {
		dst.Fields = qq.Fields
	}
	dst.IsDistinct = dst.IsDistinct || qq.IsDistinct
	dst.Joins = append(dst.Joins, qq.Joins...)
	dst.WhereCond = And(dst.WhereCond, qq.WhereCond)
	dst.GroupBy = append(dst.GroupBy, qq.GroupBy...)
	dst.HavingCond = And(dst.HavingCond, qq.HavingCond)
	dst.Sorts = append(dst.Sorts, qq.Sorts...)
	if qq.LimitN > 0 {
		dst.LimitN = qq.LimitN
	}
	if qq.OffsetN > 0 {
		dst.OffsetN = qq.OffsetN
	}
	dst.Lock = dst.Lock || qq.Lock
	dst.IsUnscoped = dst.IsUnscoped || qq.IsUnscoped
	if qq.RawSQL != "" {
		dst.RawSQL, dst.RawArgs = qq.RawSQL, qq.RawArgs
	}
}

// Apply builds a Query from a list of queriers.
func Apply(base Query, qs ...Querier) Query {
	for _, x := range qs {
		if x != nil {
			x.Build(&base)
		}
	}
	return base
}

// Clone returns a deep-enough copy so that further chaining never aliases
// the receiver's slices.
func (qq Query) clone() Query {
	c := qq
	c.Fields = append([]string(nil), qq.Fields...)
	c.Joins = append([]JoinClause(nil), qq.Joins...)
	c.GroupBy = append([]string(nil), qq.GroupBy...)
	c.Sorts = append([]SortField(nil), qq.Sorts...)
	return c
}

func (qq Query) Select(fields ...string) Query { c := qq.clone(); c.Fields = fields; return c }
func (qq Query) Distinct() Query               { c := qq.clone(); c.IsDistinct = true; return c }
func (qq Query) Where(f ...Filter) Query {
	c := qq.clone()
	c.WhereCond = And(append([]Filter{c.WhereCond}, f...)...)
	return c
}
func (qq Query) OrWhere(f ...Filter) Query {
	c := qq.clone()
	c.WhereCond = Or(c.WhereCond, And(f...))
	return c
}
func (qq Query) Group(fields ...string) Query {
	c := qq.clone()
	c.GroupBy = append(c.GroupBy, fields...)
	return c
}
func (qq Query) Having(f ...Filter) Query {
	c := qq.clone()
	c.HavingCond = And(append([]Filter{c.HavingCond}, f...)...)
	return c
}
func (qq Query) Limit(n int) Query  { c := qq.clone(); c.LimitN = n; return c }
func (qq Query) Offset(n int) Query { c := qq.clone(); c.OffsetN = n; return c }
func (qq Query) ForUpdate() Query   { c := qq.clone(); c.Lock = true; return c }
func (qq Query) Unscoped() Query    { c := qq.clone(); c.IsUnscoped = true; return c }

// Sort adds ORDER BY fields; prefix a field with '-' for descending.
func (qq Query) Sort(fields ...string) Query {
	c := qq.clone()
	c.Sorts = append(c.Sorts, parseSorts(fields)...)
	return c
}

// Join adds an INNER JOIN with a raw ON clause.
func (qq Query) Join(table, on string, args ...any) Query {
	return qq.JoinWith(InnerJoin, table, on, args...)
}

// LeftJoin adds a LEFT JOIN with a raw ON clause.
func (qq Query) LeftJoin(table, on string, args ...any) Query {
	return qq.JoinWith(LeftJoin, table, on, args...)
}

// JoinWith adds a join of the given kind.
func (qq Query) JoinWith(kind JoinKind, table, on string, args ...any) Query {
	c := qq.clone()
	c.Joins = append(c.Joins, JoinClause{Kind: kind, Table: table, On: on, Args: args})
	return c
}

func parseSorts(fields []string) []SortField {
	out := make([]SortField, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, "-") {
			out = append(out, SortField{Field: f[1:], Desc: true})
		} else {
			out = append(out, SortField{Field: strings.TrimPrefix(f, "+")})
		}
	}
	return out
}

// ---- standalone queriers -------------------------------------------------

// Where ANDs the filters into the query.
func Where(f ...Filter) Querier { return And(f...) }

// Select restricts the selected fields.
func Select(fields ...string) Querier { return QuerierFunc(func(x *Query) { x.Fields = fields }) }

// Sort orders the result; prefix with '-' for descending.
func Sort(fields ...string) Querier {
	s := parseSorts(fields)
	return QuerierFunc(func(x *Query) { x.Sorts = append(x.Sorts, s...) })
}

// Limit caps the number of rows.
func Limit(n int) Querier { return QuerierFunc(func(x *Query) { x.LimitN = n }) }

// Offset skips n rows.
func Offset(n int) Querier { return QuerierFunc(func(x *Query) { x.OffsetN = n }) }

// Page is Limit+Offset for 1-based page numbers.
func Page(page, size int) Querier {
	if page < 1 {
		page = 1
	}
	return QuerierFunc(func(x *Query) { x.LimitN = size; x.OffsetN = (page - 1) * size })
}

// Group adds GROUP BY fields.
func Group(fields ...string) Querier {
	return QuerierFunc(func(x *Query) { x.GroupBy = append(x.GroupBy, fields...) })
}

// Having adds HAVING filters.
func Having(f ...Filter) Querier {
	return QuerierFunc(func(x *Query) { x.HavingCond = And(append([]Filter{x.HavingCond}, f...)...) })
}

// Join adds an INNER JOIN.
func Join(table, on string, args ...any) Querier {
	return QuerierFunc(func(x *Query) {
		x.Joins = append(x.Joins, JoinClause{Kind: InnerJoin, Table: table, On: on, Args: args})
	})
}

// LeftJoinOn adds a LEFT JOIN.
func LeftJoinOn(table, on string, args ...any) Querier {
	return QuerierFunc(func(x *Query) {
		x.Joins = append(x.Joins, JoinClause{Kind: LeftJoin, Table: table, On: on, Args: args})
	})
}

// Distinct selects distinct rows.
func Distinct() Querier { return QuerierFunc(func(x *Query) { x.IsDistinct = true }) }

// ForUpdate locks selected rows (no-op on SQLite).
func ForUpdate() Querier { return QuerierFunc(func(x *Query) { x.Lock = true }) }

// Unscoped disables automatic scopes such as soft-delete filtering.
func Unscoped() Querier { return QuerierFunc(func(x *Query) { x.IsUnscoped = true }) }

// SQL replaces the generated statement with raw SQL ('?' placeholders).
func SQL(sql string, args ...any) Query { return Query{RawSQL: sql, RawArgs: args} }
