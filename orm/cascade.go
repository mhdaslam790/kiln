package orm

import (
	"context"
	"fmt"
	"reflect"

	"github.com/mhdaslam790/kiln/internal/meta"
)

// cascadeParents inserts unsaved belongs_to targets and copies their keys
// into the record.
func (db *DB) cascadeParents(ctx context.Context, m *meta.Model, v reflect.Value) error {
	assocs, err := m.Assocs()
	if err != nil {
		return err
	}
	for _, a := range assocs {
		if a.Kind != meta.BelongsTo {
			continue
		}
		target, ok := assocValue(v.FieldByIndex(a.Index))
		if !ok {
			continue
		}
		tm, err := meta.Of(a.Elem)
		if err != nil {
			return err
		}
		if needsInsert(tm, target) {
			if err := db.insertOne(ctx, tm, target, insertOpts{cascade: true}); err != nil {
				return fmt.Errorf("cascade %s.%s: %w", m.Type.Name(), a.Name, err)
			}
		}
		if err := copyValue(v.FieldByIndex(m.Field(a.OwnerCol).Index), target.FieldByIndex(tm.Field(a.TargetCol).Index)); err != nil {
			return err
		}
	}
	return nil
}

// cascadeChildren links has_one / has_many records to the freshly inserted
// owner and inserts the unsaved ones.
func (db *DB) cascadeChildren(ctx context.Context, m *meta.Model, v reflect.Value) error {
	assocs, err := m.Assocs()
	if err != nil {
		return err
	}
	for _, a := range assocs {
		if a.Kind == meta.BelongsTo {
			continue
		}
		tm, err := meta.Of(a.Elem)
		if err != nil {
			return err
		}
		fv := v.FieldByIndex(a.Index)
		if a.Kind == meta.ManyToMany {
			// Insert new targets, then link all of them.
			var keys []any
			for i := 0; i < fv.Len(); i++ {
				c, ok := assocValue(fv.Index(i))
				if !ok {
					continue
				}
				if needsInsert(tm, c) {
					if err := db.insertOne(ctx, tm, c, insertOpts{cascade: true}); err != nil {
						return fmt.Errorf("cascade %s.%s: %w", m.Type.Name(), a.Name, err)
					}
				}
				k, err := keyOf(tm, c, a.TargetCol, a.TargetCol)
				if err != nil {
					return err
				}
				keys = append(keys, k)
			}
			if len(keys) > 0 {
				ok, err := keyOf(m, v, a.OwnerCol, a.OwnerCol)
				if err != nil {
					return err
				}
				if err := db.link(ctx, a, ok, keys); err != nil {
					return fmt.Errorf("cascade %s.%s: %w", m.Type.Name(), a.Name, err)
				}
			}
			continue
		}
		var children []reflect.Value
		if a.Slice {
			for i := 0; i < fv.Len(); i++ {
				if c, ok := assocValue(fv.Index(i)); ok {
					children = append(children, c)
				}
			}
		} else if c, ok := assocValue(fv); ok {
			children = append(children, c)
		}
		key := v.FieldByIndex(m.Field(a.OwnerCol).Index)
		for _, c := range children {
			if err := copyValue(c.FieldByIndex(tm.Field(a.TargetCol).Index), key); err != nil {
				return err
			}
			if !needsInsert(tm, c) {
				continue
			}
			if err := db.insertOne(ctx, tm, c, insertOpts{cascade: true}); err != nil {
				return fmt.Errorf("cascade %s.%s: %w", m.Type.Name(), a.Name, err)
			}
		}
	}
	return nil
}

// assocValue returns the addressable struct behind an association field
// (struct or pointer), or false when it is nil / zero.
func assocValue(fv reflect.Value) (reflect.Value, bool) {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return reflect.Value{}, false
		}
		return fv.Elem(), true
	}
	if fv.IsZero() {
		return reflect.Value{}, false
	}
	return fv, true
}

func needsInsert(m *meta.Model, v reflect.Value) bool {
	return m.PK == nil || v.FieldByIndex(m.PK.Index).IsZero()
}

// copyValue assigns src to dst converting between compatible key types
// (int64 <-> int, T <-> *T).
func copyValue(dst, src reflect.Value) error {
	for src.Kind() == reflect.Pointer {
		if src.IsNil() {
			dst.SetZero()
			return nil
		}
		src = src.Elem()
	}
	target := dst
	if dst.Kind() == reflect.Pointer {
		p := reflect.New(dst.Type().Elem())
		dst.Set(p)
		target = p.Elem()
	}
	if !src.Type().ConvertibleTo(target.Type()) {
		return fmt.Errorf("orm: cannot assign %s to %s", src.Type(), target.Type())
	}
	target.Set(src.Convert(target.Type()))
	return nil
}
