// Package audit records who changed what and when: one row per insert,
// update and delete, written in the same transaction as the change.
//
//	db := orm.Open(sqlDB, dialect.Postgres(),
//		audit.Plugin(audit.Actor(func(ctx context.Context) string { return userFrom(ctx) })))
//
// Create the table first: `kiln audit init` writes the migration into your
// project. Each row holds the table, the record's primary key, the action,
// the actor, the time and the changes as JSON:
//
//	insert  {"email": "ann@x.io", "age": 30}
//	update  {"email": ["ann@x.io", "ann@y.io"]}   (only columns that changed)
//	delete  {"email": "ann@y.io", "age": 30}      (the record as deleted)
//
// An update that changes nothing writes no row; updated_at is left out of
// update diffs (the entry's own time records when). Bulk queries (UpdateAny,
// DeleteAny), raw SQL and Restore are not recorded: they change rows kiln
// never sees as records.
package audit

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// DefaultTable is the table entries go to (see Table).
const DefaultTable = "audit_log"

// Option configures the plugin.
type Option func(*plugin)

// Actor sets the function that names who made a change, usually the user
// id from the request context. Without it the actor is empty.
func Actor(fn func(ctx context.Context) string) Option { return func(p *plugin) { p.actor = fn } }

// Table writes entries to name instead of DefaultTable.
func Table(name string) Option { return func(p *plugin) { p.table = name } }

// Skip never records changes to these tables.
func Skip(tables ...string) Option {
	return func(p *plugin) {
		for _, t := range tables {
			p.skip[t] = true
		}
	}
}

// Redact stores "[redacted]" instead of the value for these columns (in
// any table), e.g. password hashes or tokens. Changes are still recorded.
func Redact(columns ...string) Option {
	return func(p *plugin) {
		for _, c := range columns {
			p.redact[c] = true
		}
	}
}

// Plugin returns the option that records every insert, update and delete.
func Plugin(opts ...Option) orm.Option {
	p := &plugin{table: DefaultTable, skip: map[string]bool{}, redact: map[string]bool{}}
	for _, o := range opts {
		o(p)
	}
	p.skip[p.table] = true
	p.insert = "INSERT INTO " + p.table + " (table_name, record_id, action, actor, changes, created_at) VALUES (?, ?, ?, ?, ?, ?)"
	return func(db *orm.DB) {
		p.db = db
		orm.Use(p)(db)
	}
}

type plugin struct {
	db     *orm.DB
	table  string
	insert string
	actor  func(context.Context) string
	skip   map[string]bool // read-only after Plugin
	redact map[string]bool // read-only after Plugin
}

type oldKey struct{}

func (p *plugin) Name() string { return "audit" }

// BeforeWrite loads the row as it is before an update, for the diff.
func (p *plugin) BeforeWrite(ctx context.Context, w *orm.WriteInfo) error {
	if w.Action != orm.ActionUpdate || p.skip[w.Table] {
		return nil
	}
	where, ok := pkFilter(w)
	if !ok {
		return nil
	}
	old := reflect.New(reflect.TypeOf(w.Record).Elem())
	if err := p.db.Select(ctx, old.Interface(), q.From(w.Table).Where(where)); err != nil {
		return fmt.Errorf("audit: loading %s before update: %w", w.Table, err)
	}
	w.SetValue(oldKey{}, old.Elem())
	return nil
}

func (p *plugin) AfterWrite(ctx context.Context, w *orm.WriteInfo) error {
	if p.skip[w.Table] {
		return nil
	}
	changes := map[string]any{}
	switch w.Action {
	case orm.ActionInsert, orm.ActionDelete:
		for _, f := range w.Model.Fields {
			v, _ := w.Get(f.Column)
			changes[f.Column] = p.value(f.Column, v)
		}
	case orm.ActionUpdate:
		old, _ := w.Value(oldKey{}).(reflect.Value)
		for _, f := range w.Model.Fields {
			// Not written by an update (key, created_at, read-only), or
			// noise: the entry's own time already says when.
			if f.Primary || f.ReadOnly || f.Column == "created_at" || f.Column == "updated_at" ||
				(w.Fields != nil && !contains(w.Fields, f.Column)) {
				continue
			}
			now, _ := w.Get(f.Column)
			if !old.IsValid() {
				changes[f.Column] = p.value(f.Column, now)
				continue
			}
			before := old.FieldByName(f.Name).Interface()
			if !same(plain(before), plain(now)) {
				changes[f.Column] = []any{p.value(f.Column, before), p.value(f.Column, now)}
			}
		}
		if len(changes) == 0 {
			return nil // nothing changed
		}
	default:
		return nil
	}
	js, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("audit: encoding changes: %w", err)
	}
	var actor string
	if p.actor != nil {
		actor = p.actor(ctx)
	}
	_, err = p.db.Exec(ctx, p.insert, w.Table, recordID(w), w.Action.String(), actor, string(js), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("audit: writing entry: %w", err)
	}
	return nil
}

func (p *plugin) value(column string, v any) any {
	if p.redact[column] {
		return "[redacted]"
	}
	return plain(v)
}

// plain turns driver.Valuer types (sql.NullString, decimal types, ...) into
// the value the database stores, so JSON shows 5 rather than {"Int64":5,...}.
func plain(v any) any {
	if vr, ok := v.(driver.Valuer); ok {
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return nil
		}
		if x, err := vr.Value(); err == nil {
			return x
		}
	}
	return v
}

// same compares old and new values, looking through pointers and treating
// times as equal when they are the same instant (the database may return a
// different location, and Go keeps a monotonic reading).
func same(a, b any) bool {
	a, b = deref(a), deref(b)
	if ta, ok := a.(time.Time); ok {
		tb, ok := b.(time.Time)
		return ok && ta.Equal(tb)
	}
	return reflect.DeepEqual(a, b)
}

func deref(v any) any {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	return rv.Interface()
}

func pkFilter(w *orm.WriteInfo) (q.Filter, bool) {
	var conds []q.Filter
	for _, f := range w.Model.Fields {
		if f.Primary {
			v, _ := w.Get(f.Column)
			conds = append(conds, q.Eq(f.Column, v))
		}
	}
	if len(conds) == 0 {
		return nil, false
	}
	return q.And(conds...), true
}

func recordID(w *orm.WriteInfo) string {
	var parts []string
	for _, f := range w.Model.Fields {
		if f.Primary {
			v, _ := w.Get(f.Column)
			parts = append(parts, fmt.Sprint(plain(v)))
		}
	}
	return strings.Join(parts, ",")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
