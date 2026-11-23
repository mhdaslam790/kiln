package orm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/q"
)

// Changeset remembers a record's column values so that only modified
// columns are written later.
//
//	cs := orm.Track(&user)
//	user.Name = "New"
//	err := users.UpdateChanged(ctx, &user, cs) // UPDATE users SET name = ?, updated_at = ? ...
type Changeset struct {
	m    *meta.Model
	rec  reflect.Value // pointer to the struct
	snap map[*meta.Field]any
}

// Track snapshots rec (a pointer to a model struct).
func Track(rec any) *Changeset {
	v, m, err := structPtr(rec)
	if err != nil {
		panic(err)
	}
	c := &Changeset{m: m, rec: v.Addr()}
	c.Reset()
	return c
}

// Reset takes a fresh snapshot (done automatically after UpdateChanged).
func (c *Changeset) Reset() {
	c.snap = make(map[*meta.Field]any, len(c.m.Fields))
	v := c.rec.Elem()
	for _, f := range c.tracked() {
		c.snap[f] = snapValue(f, v.FieldByIndex(f.Index))
	}
}

func (c *Changeset) tracked() []*meta.Field {
	out := make([]*meta.Field, 0, len(c.m.Fields))
	for _, f := range c.m.Fields {
		if isPK(c.m, f) || f.ReadOnly || f == c.m.UpdatedAt || f == c.m.CreatedAt {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (c *Changeset) changedFields() []*meta.Field {
	var out []*meta.Field
	v := c.rec.Elem()
	for _, f := range c.tracked() {
		if !snapEqual(c.snap[f], snapValue(f, v.FieldByIndex(f.Index))) {
			out = append(out, f)
		}
	}
	return out
}

// Changed returns the Go names of modified fields.
func (c *Changeset) Changed() []string {
	var out []string
	for _, f := range c.changedFields() {
		out = append(out, f.Name)
	}
	return out
}

// HasChanges reports whether any tracked field was modified.
func (c *Changeset) HasChanges() bool { return len(c.changedFields()) > 0 }

// Changes maps modified column names to their new values.
func (c *Changeset) Changes() map[string]any {
	out := map[string]any{}
	v := c.rec.Elem()
	for _, f := range c.changedFields() {
		out[f.Column] = v.FieldByIndex(f.Index).Interface()
	}
	return out
}

// UpdateChanged writes only the columns changed since cs was taken (plus
// updated_at). It is a no-op when nothing changed.
func (db *DB) UpdateChanged(ctx context.Context, cs *Changeset) error {
	if cs == nil {
		return errors.New("orm: nil changeset")
	}
	fields := cs.Changed()
	if len(fields) == 0 {
		return nil
	}
	if err := db.Update(ctx, cs.rec.Interface(), fields...); err != nil {
		return err
	}
	cs.Reset()
	return nil
}

// snapValue copies a field value deeply enough that later in-place
// mutations (through pointers, maps, slices) are detected.
func snapValue(f *meta.Field, v reflect.Value) any {
	if f.JSON {
		b, _ := json.Marshal(v.Interface())
		return string(b)
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return string(v.Bytes())
		}
		b, _ := json.Marshal(v.Interface())
		return "json:" + string(b)
	case reflect.Map:
		b, _ := json.Marshal(v.Interface())
		return "json:" + string(b)
	}
	return v.Interface()
}

func snapEqual(a, b any) bool {
	if ta, ok := a.(time.Time); ok {
		tb, ok := b.(time.Time)
		return ok && ta.Equal(tb)
	}
	return reflect.DeepEqual(a, b)
}

// ---- cursor pagination ---------------------------------------------------------

// CursorOpts configures cursor (keyset) pagination.
type CursorOpts struct {
	// After is a page's Next cursor: return the page that follows it.
	After string
	// Before is a page's Prev cursor: return the page that precedes it.
	// Set at most one of After and Before; neither means the first page.
	Before string
	// Limit is the page size (default 20).
	Limit int
	// Sort is one column, "-" prefixed for descending. The primary key is
	// always used as a tie-breaker. Default: the primary key ascending.
	Sort string
}

// CursorPage is one page of cursor pagination. Next / Prev are opaque
// cursors to pass back as CursorOpts.After / CursorOpts.Before.
type CursorPage[T any] struct {
	Items   []T    `json:"items"`
	Next    string `json:"next,omitempty"`
	Prev    string `json:"prev,omitempty"`
	HasNext bool   `json:"has_next"`
	HasPrev bool   `json:"has_prev"`
}

// Cursor returns a page using cursor (keyset) pagination: each page carries
// pointers to its neighbours instead of an offset, so deep pages stay fast
// and rows inserted meanwhile never shift or duplicate results.
//
//	p1, _ := users.Cursor(ctx, orm.CursorOpts{Limit: 20, Sort: "-created_at"})
//	p2, _ := users.Cursor(ctx, orm.CursorOpts{Limit: 20, Sort: "-created_at", After: p1.Next})
//	back, _ := users.Cursor(ctx, orm.CursorOpts{Limit: 20, Sort: "-created_at", Before: p2.Prev}) // == p1
func (r *Repo[T]) Cursor(ctx context.Context, o CursorOpts, qs ...q.Querier) (CursorPage[T], error) {
	var page CursorPage[T]
	pk := r.m.PK
	if pk == nil {
		return page, pkMissing(r.m)
	}
	if o.After != "" && o.Before != "" {
		return page, errors.New("orm: set only one of CursorOpts.After and CursorOpts.Before")
	}
	if o.Limit < 1 {
		o.Limit = 20
	}
	desc := len(o.Sort) > 0 && o.Sort[0] == '-'
	name := o.Sort
	if desc || (len(name) > 0 && name[0] == '+') {
		name = name[1:]
	}
	sortF := pk
	if name != "" {
		if sortF = r.m.Field(name); sortF == nil {
			return page, fmt.Errorf("orm: %s has no field %q", r.m.Type.Name(), name)
		}
	}
	col, pkCol := r.m.Table+"."+sortF.Column, r.m.Table+"."+pk.Column

	// Walking backwards = querying in the opposite direction, then reversing.
	backward := o.Before != ""
	scanDesc := desc != backward
	cursor := o.After
	if backward {
		cursor = o.Before
	}

	all := append([]q.Querier(nil), qs...)
	if cursor != "" {
		vals, err := decodeCursor(cursor, sortF, pk)
		if err != nil {
			return page, err
		}
		cmp := q.Gt
		if scanDesc {
			cmp = q.Lt
		}
		if sortF == pk {
			all = append(all, cmp(pkCol, vals[0]))
		} else {
			all = append(all, q.Or(cmp(col, vals[0]), q.And(q.Eq(col, vals[0]), cmp(pkCol, vals[1]))))
		}
	}
	dir := ""
	if scanDesc {
		dir = "-"
	}
	if sortF == pk {
		all = append(all, q.Sort(dir+pkCol))
	} else {
		all = append(all, q.Sort(dir+col, dir+pkCol))
	}
	all = append(all, q.Limit(o.Limit+1))

	items, err := r.FindAll(ctx, all...)
	if err != nil {
		return page, err
	}
	more := len(items) > o.Limit
	if more {
		items = items[:o.Limit]
	}
	if backward {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
		page.HasPrev, page.HasNext = more, true
	} else {
		page.HasNext, page.HasPrev = more, cursor != ""
	}
	page.Items = items
	if len(items) == 0 {
		page.HasNext, page.HasPrev = false, false
		return page, nil
	}
	key := func(it T) string {
		v := reflect.ValueOf(it)
		vals := []any{v.FieldByIndex(sortF.Index).Interface()}
		if sortF != pk {
			vals = append(vals, v.FieldByIndex(pk.Index).Interface())
		}
		b, _ := json.Marshal(vals)
		return cursorEnc.EncodeToString(b)
	}
	if page.HasNext {
		page.Next = key(items[len(items)-1])
	}
	if page.HasPrev {
		page.Prev = key(items[0])
	}
	return page, nil
}

var cursorEnc = base64.RawURLEncoding

// ErrBadCursor is returned for cursors that cannot be decoded.
var ErrBadCursor = errors.New("orm: invalid cursor")

func decodeCursor(s string, fields ...*meta.Field) ([]any, error) {
	b, err := cursorEnc.DecodeString(s)
	if err != nil {
		return nil, ErrBadCursor
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, ErrBadCursor
	}
	n := 1
	if fields[0] != fields[1] {
		n = 2
	}
	if len(raws) != n {
		return nil, ErrBadCursor
	}
	out := make([]any, n)
	for i := range out {
		t := fields[i].Type
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		p := reflect.New(t)
		if err := json.Unmarshal(raws[i], p.Interface()); err != nil {
			return nil, ErrBadCursor
		}
		out[i] = p.Elem().Interface()
	}
	return out, nil
}
