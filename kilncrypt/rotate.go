package kilncrypt

import (
	"context"
	"reflect"

	"github.com/mhdaslam790/kiln/orm"
)

// Rotate re-encrypts every T record whose String fields are sealed with an
// old key or stored in the v0.1 format (not bound to a column), page by
// page, and returns how many records it updated. The db
// must have kilncrypt.Plugin with the new key as current and the old ones
// still in Keys; once Rotate has run for every model, the old keys can be
// dropped.
//
// Each record is a normal Update of only its encrypted fields: hooks run,
// updated_at changes and optimistic locks apply. It is safe to stop and run
// again. Blind indexes are not recomputed (the index key is separate and
// values don't change).
func Rotate[T any](ctx context.Context, db *orm.DB, keys *Keys) (int, error) {
	repo := orm.For[T](db)
	var fields []int // indexes of String fields in T
	t := reflect.TypeOf((*T)(nil)).Elem()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Type == stringType {
			fields = append(fields, i)
		}
	}
	if len(fields) == 0 {
		return 0, nil
	}
	updated := 0
	opts := orm.CursorOpts{Limit: 500}
	for {
		page, err := repo.Cursor(ctx, opts)
		if err != nil {
			return updated, err
		}
		for i := range page.Items {
			rec := &page.Items[i]
			rv := reflect.ValueOf(rec).Elem()
			var stale []string
			for _, f := range fields {
				s := rv.Field(f).Interface().(String)
				if s.sealed != "" && !s.current(keys, s.bound) {
					stale = append(stale, t.Field(f).Name)
				}
			}
			if len(stale) == 0 {
				continue
			}
			if err := repo.Update(ctx, rec, stale...); err != nil {
				return updated, err
			}
			updated++
		}
		if !page.HasNext {
			return updated, nil
		}
		opts.After = page.Next
	}
}
