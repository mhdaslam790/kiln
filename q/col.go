package q

// Col is a typed column reference. `kiln gen` generates one per model field,
// so that both the column name and the value type are checked by the
// compiler:
//
//	users.FindAll(ctx, models.UserCols.Email.Eq("a@b.c"), models.UserCols.Age.Gte(18))
//	users.FindAll(ctx, models.UserCols.Emial.Eq("x"))  // compile error: no field Emial
//	users.FindAll(ctx, models.UserCols.Age.Eq("ten"))  // compile error: string is not int
//
// T is the field's Go type; for nullable pointer fields it is the element
// type (use IsNull / NotNull for NULL checks). A Col converts to its column
// name with Name or string(c).
type Col[T any] string

// Name returns the column name.
func (c Col[T]) Name() string { return string(c) }

// Of qualifies the column with a table name or alias, for joins:
// UserCols.ID.Of("u") is "u.id".
func (c Col[T]) Of(table string) Col[T] { return Col[T](table + "." + string(c)) }

func (c Col[T]) Eq(v T) Filter  { return Eq(string(c), v) }
func (c Col[T]) Ne(v T) Filter  { return Ne(string(c), v) }
func (c Col[T]) Lt(v T) Filter  { return Lt(string(c), v) }
func (c Col[T]) Lte(v T) Filter { return Lte(string(c), v) }
func (c Col[T]) Gt(v T) Filter  { return Gt(string(c), v) }
func (c Col[T]) Gte(v T) Filter { return Gte(string(c), v) }

// Between matches lo <= column <= hi.
func (c Col[T]) Between(lo, hi T) Filter { return Between(string(c), lo, hi) }

// In matches any of values. An empty list matches nothing.
func (c Col[T]) In(values ...T) Filter { return InOf(string(c), values) }

// NotIn matches none of values. An empty list matches everything.
func (c Col[T]) NotIn(values ...T) Filter {
	vs := make([]any, len(values))
	for i, v := range values {
		vs[i] = v
	}
	return NotIn(string(c), vs...)
}

// InQuery matches values returned by a sub-query.
func (c Col[T]) InQuery(sub Query) Filter {
	return Cond{Op: OpIn, Field: string(c), Values: []any{sub}}
}

func (c Col[T]) Like(pattern string) Filter    { return Like(string(c), pattern) }
func (c Col[T]) NotLike(pattern string) Filter { return NotLike(string(c), pattern) }
func (c Col[T]) IsNull() Filter                { return Null(string(c)) }
func (c Col[T]) NotNull() Filter               { return NotNull(string(c)) }

// EqCol compares with another column of the same type (e.g. in a join).
func (c Col[T]) EqCol(o Col[T]) Filter { return Eq(string(c), F(o)) }

// Asc and Desc sort by the column.
func (c Col[T]) Asc() Querier  { return Sort(string(c)) }
func (c Col[T]) Desc() Querier { return Sort("-" + string(c)) }

// Set assigns v in UpdateAny; SetNull assigns NULL.
func (c Col[T]) Set(v T) Mutation  { return Set(string(c), v) }
func (c Col[T]) SetNull() Mutation { return Set(string(c), nil) }

// Inc adds n (numeric columns).
func (c Col[T]) Inc(n T) Mutation { return Inc(string(c), n) }

// Named is anything with a column name, such as a Col.
type Named interface{ Name() string }

// Names returns the column names, for Select and Group:
// q.Select(q.Names(UserCols.ID, UserCols.Email)...).
func Names(cols ...Named) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name()
	}
	return out
}
