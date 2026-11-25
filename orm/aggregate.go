package orm

import (
	"context"
	"fmt"

	"github.com/mhdaslam790/kiln/q"
)

// Number is the type of a column Sum and Avg add up.
type Number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// Sum adds up col over the matching records. The result has the column's
// type, from a typed column (`kiln gen`) or q.Col[T]("name"):
//
//	total, err := orm.Sum(ctx, orders, c.Amount, c.Status.Eq("paid")) // int64
//
// No matching rows (or only NULLs) give 0. Like every repository query it
// applies scopes, soft delete and plugins (a tenant only sums its own rows).
func Sum[T any, V Number](ctx context.Context, r *Repo[T], col q.Col[V], qs ...q.Querier) (V, error) {
	var out []struct {
		V *V `db:"v"`
	}
	if err := r.aggregate(ctx, &out, "SUM(%s)", string(col), "", qs); err != nil || len(out) == 0 || out[0].V == nil {
		return 0, err
	}
	return *out[0].V, nil
}

// Avg returns the average of col over the matching records, as a float64
// on every database (integer columns aren't rounded). No matching rows give
// 0; use Count to tell that apart from an average of 0.
func Avg[T any, V Number](ctx context.Context, r *Repo[T], col q.Col[V], qs ...q.Querier) (float64, error) {
	var out []struct {
		V *float64 `db:"v"`
	}
	if err := r.aggregate(ctx, &out, "AVG(%s * 1.0)", string(col), "", qs); err != nil || len(out) == 0 || out[0].V == nil {
		return 0, err
	}
	return *out[0].V, nil
}

// Min returns the smallest value of col over the matching records (any
// ordered type: numbers, strings, times). It returns ErrNotFound when no
// matching row has a value.
func Min[T any, V any](ctx context.Context, r *Repo[T], col q.Col[V], qs ...q.Querier) (V, error) {
	return extreme(ctx, r, "MIN(%s)", col, qs)
}

// Max returns the largest value of col; see Min.
func Max[T any, V any](ctx context.Context, r *Repo[T], col q.Col[V], qs ...q.Querier) (V, error) {
	return extreme(ctx, r, "MAX(%s)", col, qs)
}

func extreme[T, V any](ctx context.Context, r *Repo[T], fn string, col q.Col[V], qs []q.Querier) (V, error) {
	var zero V
	var out []struct {
		V *V `db:"v"`
	}
	if err := r.aggregate(ctx, &out, fn, string(col), "", qs); err != nil {
		return zero, err
	}
	if len(out) == 0 || out[0].V == nil {
		return zero, ErrNotFound
	}
	return *out[0].V, nil
}

// CountBy counts the matching records per value of col (GROUP BY):
//
//	byStatus, err := orm.CountBy(ctx, orders, c.Status) // map[Status]int64
//
// Rows where col is NULL are not counted; count them with
// r.Count(ctx, col.IsNull()).
func CountBy[T any, K comparable](ctx context.Context, r *Repo[T], col q.Col[K], qs ...q.Querier) (map[K]int64, error) {
	var out []struct {
		K *K    `db:"k"`
		N int64 `db:"n"`
	}
	if err := r.aggregate(ctx, &out, "COUNT(*)", "", string(col), qs); err != nil {
		return nil, err
	}
	m := make(map[K]int64, len(out))
	for _, row := range out {
		if row.K != nil {
			m[*row.K] = row.N
		}
	}
	return m, nil
}

// SumBy adds up col per value of by (GROUP BY):
//
//	revenue, err := orm.SumBy(ctx, orders, c.Country, c.Amount) // map[string]int64
//
// Rows where by is NULL are left out, like CountBy.
func SumBy[T any, K comparable, V Number](ctx context.Context, r *Repo[T], by q.Col[K], col q.Col[V], qs ...q.Querier) (map[K]V, error) {
	var out []struct {
		K *K `db:"k"`
		V *V `db:"v"`
	}
	if err := r.aggregate(ctx, &out, "SUM(%s)", string(col), string(by), qs); err != nil {
		return nil, err
	}
	m := make(map[K]V, len(out))
	for _, row := range out {
		if row.K != nil && row.V != nil {
			m[*row.K] = *row.V
		}
	}
	return m, nil
}

// aggregate selects fn(col) AS v (fn may have no %s, like COUNT(*)) over
// the repository's query, grouped by `by` (selected as k) when given. Both
// names must be fields of the model, so nothing from them reaches the SQL
// unquoted. Sorting, limit and offset don't apply to an aggregate.
func (r *Repo[T]) aggregate(ctx context.Context, dst any, fn, col, by string, qs []q.Querier) error {
	qq := r.query(qs)
	qq.Sorts, qq.LimitN, qq.OffsetN = nil, 0, 0
	name := func(field string) (string, error) {
		f := r.m.Field(field)
		if f == nil {
			return "", fmt.Errorf("orm: %s has no field %q", r.m.Type.Name(), field)
		}
		return r.db.dialect.QuoteName(r.m.Table + "." + f.Column), nil
	}
	expr := fn
	if col != "" {
		c, err := name(col)
		if err != nil {
			return err
		}
		expr = fmt.Sprintf(fn, c)
	}
	qq.Fields = []string{expr + " AS v"}
	if fn == "COUNT(*)" {
		qq.Fields = []string{"COUNT(*) AS n"}
	}
	if by != "" {
		k, err := name(by)
		if err != nil {
			return err
		}
		qq.Fields = append([]string{k + " AS k"}, qq.Fields...)
		qq.GroupBy = []string{k}
	}
	return r.db.Select(ctx, dst, qq)
}
