package orm

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"

	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/q"
)

// Preload loads an association for one record (*Struct) or many records
// ([]T, []*T, *[]T, *[]*T) using a single IN query per level.
//
// Paths may be nested with dots: "Posts.Comments". Extra queriers (filters,
// sorting) apply to the last level only. Soft-deleted children are skipped.
func (db *DB) Preload(ctx context.Context, recs any, path string, qs ...q.Querier) error {
	var owners []reflect.Value
	var m *meta.Model
	rv := reflect.ValueOf(recs)
	if rv.Kind() == reflect.Pointer && rv.Elem().Kind() == reflect.Struct {
		v, mm, err := structPtr(recs)
		if err != nil {
			return err
		}
		owners, m = []reflect.Value{v}, mm
	} else {
		vs, mm, err := structSlice(recs)
		if err != nil {
			return err
		}
		owners, m = vs, mm
	}
	if len(owners) == 0 {
		return nil
	}
	return db.preload(ctx, owners, m, strings.Split(path, "."), qs)
}

func (db *DB) preload(ctx context.Context, owners []reflect.Value, m *meta.Model, path []string, qs []q.Querier) error {
	a, err := m.Assoc(path[0])
	if err != nil {
		return err
	}
	tm, err := meta.Of(a.Elem)
	if err != nil {
		return err
	}
	ownerField := m.Field(a.OwnerCol)
	targetField := tm.Field(a.TargetCol)

	// Collect distinct keys.
	ownerKeys := make([]any, len(owners))
	var keys []any
	seen := map[any]bool{}
	for i, o := range owners {
		k, ok := normKey(o.FieldByIndex(ownerField.Index))
		if !ok {
			continue
		}
		ownerKeys[i] = k
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	byKey := map[any][]reflect.Value{}
	if len(keys) > 0 {
		var levelQs []q.Querier
		if len(path) == 1 {
			levelQs = qs
		}
		// For many-to-many, the join table maps target keys to owner keys.
		var links map[any][]any
		targetKeys := keys
		if a.Kind == meta.ManyToMany {
			if links, targetKeys, err = db.m2mLinks(ctx, a, keys); err != nil {
				return err
			}
		}
		children := reflect.MakeSlice(reflect.SliceOf(tm.Type), 0, 0)
		if len(targetKeys) > 0 {
			qq := scoped(tm, append([]q.Querier{q.In(a.TargetCol, targetKeys...)}, levelQs...))
			if children, err = db.selectModels(ctx, tm, qq); err != nil {
				return err
			}
		}
		if len(path) > 1 && children.Len() > 0 {
			cs := make([]reflect.Value, children.Len())
			for i := range cs {
				cs[i] = children.Index(i)
			}
			if err := db.preload(ctx, cs, tm, path[1:], qs); err != nil {
				return err
			}
		}
		for i := 0; i < children.Len(); i++ {
			c := children.Index(i)
			k, ok := normKey(c.FieldByIndex(targetField.Index))
			if !ok {
				continue
			}
			if links == nil {
				byKey[k] = append(byKey[k], c)
				continue
			}
			for _, ok := range links[k] {
				byKey[ok] = append(byKey[ok], c)
			}
		}
	}

	for i, o := range owners {
		field := o.FieldByIndex(a.Index)
		var matches []reflect.Value
		if ownerKeys[i] != nil {
			matches = byKey[ownerKeys[i]]
		}
		if a.Slice {
			s := reflect.MakeSlice(field.Type(), len(matches), len(matches))
			for j, c := range matches {
				if a.Ptr {
					p := reflect.New(a.Elem)
					p.Elem().Set(c)
					s.Index(j).Set(p)
				} else {
					s.Index(j).Set(c)
				}
			}
			field.Set(s)
			continue
		}
		if len(matches) == 0 {
			field.SetZero()
			continue
		}
		if a.Ptr {
			p := reflect.New(a.Elem)
			p.Elem().Set(matches[0])
			field.Set(p)
		} else {
			field.Set(matches[0])
		}
	}
	return nil
}

// normKey turns a key column value into a comparable map key so that, for
// example, an int owner key matches an int64 target key.
func normKey(v reflect.Value) (any, bool) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	if vr, ok := v.Interface().(driver.Valuer); ok {
		x, err := vr.Value()
		if err != nil || x == nil {
			return nil, false
		}
		v = reflect.ValueOf(x)
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(v.Uint()), true
	case reflect.String:
		return v.String(), true
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return string(v.Bytes()), true
		}
	}
	if v.Comparable() {
		return v.Interface(), true
	}
	return fmt.Sprint(v.Interface()), true
}
