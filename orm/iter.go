package orm

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"

	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/q"
)

// iterPage is the page size Iter reads with.
const iterPage = 500

// Iter returns the matching records one at a time, for a range loop:
//
//	for u, err := range users.Iter(ctx, q.Eq("active", true)) {
//		if err != nil {
//			return err
//		}
//		...
//	}
//
// It reads pages of 500 by keyset, like Each: memory stays flat however
// many rows match, and no connection is held while the loop body runs, so
// the body may query and write (also with SQLite's single connection).
// Breaking out of the loop stops reading. An error is yielded once, with
// the zero T, and ends the loop.
//
// The walk follows a q.Sort on one field (then the primary key; NULLs come
// last ascending, first descending), or the primary key without one.
// q.Limit caps the total. More sort fields or q.Offset are an error: use
// Cursor or Paginate for those.
func (r *Repo[T]) Iter(ctx context.Context, qs ...q.Querier) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		err := r.walk(ctx, iterPage, qs, func(it T) (bool, error) { return yield(it, nil), nil })
		if err != nil {
			var zero T
			yield(zero, err)
		}
	}
}

// walk reads the records matching qs in keyset pages of size and calls fn
// for each until it returns false or an error.
func (r *Repo[T]) walk(ctx context.Context, size int, qs []q.Querier, fn func(T) (bool, error)) error {
	pk := r.m.PK
	if pk == nil {
		return pkMissing(r.m)
	}
	base := r.query(qs)
	if base.OffsetN > 0 {
		return errors.New("orm: Each and Iter don't take q.Offset (they page by keyset); use Paginate")
	}
	if len(base.Sorts) > 1 {
		return errors.New("orm: Each and Iter sort by one field (then the primary key); use Cursor or FindAll for more")
	}
	sortF, desc := pk, false
	if len(base.Sorts) == 1 {
		s := base.Sorts[0]
		name := strings.TrimPrefix(s.Field, r.m.Table+".")
		if sortF = r.m.Field(name); sortF == nil {
			return fmt.Errorf("orm: %s has no field %q to sort by", r.m.Type.Name(), s.Field)
		}
		desc = s.Desc
	}
	col, pkCol := r.m.Table+"."+sortF.Column, r.m.Table+"."+pk.Column
	nullable := sortF != pk && canBeNull(sortF.Type)
	if sortF == pk {
		base.Sorts = []q.SortField{{Field: pkCol, Desc: desc}}
	} else {
		base.Sorts = []q.SortField{{Field: col, Desc: desc, NullsLarge: nullable}, {Field: pkCol, Desc: desc}}
	}
	limit := base.LimitN
	where := base.WhereCond
	cmp := q.Gt
	if desc {
		cmp = q.Lt
	}

	var lastV, lastID any
	seen := 0
	for {
		page := base
		page.LimitN = size
		if limit > 0 && limit-seen < size {
			page.LimitN = limit - seen
		}
		if seen > 0 {
			var after q.Filter
			switch {
			case sortF == pk:
				after = cmp(pkCol, lastID)
			case !nullable:
				after = q.Or(cmp(col, lastV), q.And(q.Eq(col, lastV), cmp(pkCol, lastID)))
			default:
				after = afterNullable(col, pkCol, lastV, lastID, desc)
			}
			if where != nil {
				after = q.And(where, after)
			}
			page.WhereCond = after
		}
		items, err := r.db.selectModels(ctx, r.m, page)
		if err != nil {
			return err
		}
		list := items.Interface().([]T)
		for _, it := range list {
			more, err := fn(it)
			if err != nil || !more {
				return err
			}
		}
		seen += len(list)
		if len(list) < page.LimitN || (limit > 0 && seen >= limit) {
			return nil
		}
		last := reflect.ValueOf(list[len(list)-1])
		lastID = keyValue(last, pk)
		lastV = keyValue(last, sortF)
	}
}

// keyValue is f's value in v as a query argument: nil for NULL, the
// pointed-to value for pointers, the driver value for sql.Null* types.
func keyValue(v reflect.Value, f *meta.Field) any {
	x := v.FieldByIndex(f.Index)
	if x.Kind() == reflect.Pointer {
		if x.IsNil() {
			return nil
		}
		x = x.Elem()
	}
	if vr, ok := x.Interface().(driver.Valuer); ok {
		val, err := vr.Value()
		if err != nil {
			return nil
		}
		return val
	}
	return x.Interface()
}
