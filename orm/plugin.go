package orm

import (
	"context"
	"fmt"
	"reflect"

	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/q"
)

// Plugin extends a DB. Users enable one with a single option when opening
// the DB:
//
//	db := orm.Open(sqlDB, dialect.Postgres(), tenant.Plugin("tenant_id"))
//
// A plugin implements Name plus any of the hook interfaces QueryPlugin,
// WritePlugin and ObservePlugin. Hooks run in the order plugins were
// given; an error from a Before hook aborts the operation and is returned
// to the caller unchanged.
type Plugin interface {
	Name() string
}

// QueryPlugin can change which rows a select, count, update or delete on a
// model table touches, for example by adding a filter. It is called before
// kiln builds the SQL. It is not called for raw SQL (DB.Query, DB.Exec,
// q.SQL) or for many-to-many join tables.
type QueryPlugin interface {
	Plugin
	BeforeQuery(ctx context.Context, info *QueryInfo) error
}

// WritePlugin can check or set values on each record before it is
// inserted or updated (after the model's own Before hooks).
type WritePlugin interface {
	Plugin
	BeforeWrite(ctx context.Context, w *WriteInfo) error
}

// ObservePlugin is told about every executed statement and every cache
// hit, for logging, tracing and metrics. It must be safe for concurrent
// use.
type ObservePlugin interface {
	Plugin
	AfterQuery(ctx context.Context, e Event)
}

// Use enables a plugin. Plugin packages usually wrap it, so users write
// tenant.Plugin("tenant_id") rather than orm.Use(...). It panics if p
// implements none of the hook interfaces.
func Use(p Plugin) Option {
	return func(db *DB) {
		hooked := false
		if qp, ok := p.(QueryPlugin); ok {
			db.queryPlugins = append(db.queryPlugins, qp)
			hooked = true
		}
		if wp, ok := p.(WritePlugin); ok {
			db.writePlugins = append(db.writePlugins, wp)
			hooked = true
		}
		if op, ok := p.(ObservePlugin); ok {
			db.hooks = append(db.hooks, op.AfterQuery)
			hooked = true
		}
		if !hooked {
			panic(fmt.Sprintf("orm: plugin %q implements none of BeforeQuery, BeforeWrite, AfterQuery", p.Name()))
		}
	}
}

// Action is the kind of operation a plugin hook is called for.
type Action uint8

const (
	ActionSelect Action = iota + 1
	ActionCount
	ActionInsert
	ActionUpdate
	ActionDelete // also soft deletes
)

func (a Action) String() string {
	switch a {
	case ActionSelect:
		return "select"
	case ActionCount:
		return "count"
	case ActionInsert:
		return "insert"
	case ActionUpdate:
		return "update"
	case ActionDelete:
		return "delete"
	}
	return fmt.Sprintf("Action(%d)", uint8(a))
}

// QueryInfo is passed to QueryPlugin.BeforeQuery.
type QueryInfo struct {
	Action Action
	Table  string
	// Model describes the table's model; nil when no model with this table
	// has been used yet (e.g. DB.Select on a table without a struct).
	Model *ModelInfo
	// Query is the query about to run. Add filters with Where; plugins may
	// change other parts too.
	Query *q.Query
	// Set holds the columns an update writes (ActionUpdate, and soft
	// deletes). Plugins may inspect it, or append to it to write more.
	Set []q.Mutation
}

// Where adds a filter to the query (AND-ed with the existing ones).
func (i *QueryInfo) Where(f q.Filter) { i.Query.WhereCond = q.And(i.Query.WhereCond, f) }

// WriteInfo is passed to WritePlugin.BeforeWrite, once per record.
type WriteInfo struct {
	Action Action // ActionInsert or ActionUpdate
	Table  string
	Model  *ModelInfo
	// Record points to the struct being written.
	Record any
	// Fields lists the columns an update writes; nil means every column.
	// Values set on other columns are kept on the record but not written.
	Fields []string

	m *meta.Model
	v reflect.Value
}

// Get returns the record's value for column.
func (w *WriteInfo) Get(column string) (any, bool) {
	f := w.m.Field(column)
	if f == nil {
		return nil, false
	}
	return w.v.FieldByIndex(f.Index).Interface(), true
}

// Set assigns value to the record's column. The value must be assignable
// to the field, or a number or string convertible to it (e.g. an int to an
// int64 field). For a pointer field, a value of the element type is
// accepted.
func (w *WriteInfo) Set(column string, value any) error {
	f := w.m.Field(column)
	if f == nil {
		return fmt.Errorf("orm: %s has no column %q", w.m.Type.Name(), column)
	}
	return assignValue(w.v.FieldByIndex(f.Index), value, column)
}

func assignValue(dst reflect.Value, value any, column string) error {
	if value == nil {
		dst.SetZero()
		return nil
	}
	src := reflect.ValueOf(value)
	switch {
	case src.Type().AssignableTo(dst.Type()):
		dst.Set(src)
		return nil
	case sameFamily(src.Kind(), dst.Kind()) && src.Type().ConvertibleTo(dst.Type()):
		dst.Set(src.Convert(dst.Type()))
		return nil
	case dst.Kind() == reflect.Pointer:
		elem := reflect.New(dst.Type().Elem())
		if err := assignValue(elem.Elem(), value, column); err != nil {
			return err
		}
		dst.Set(elem)
		return nil
	}
	return fmt.Errorf("orm: cannot set column %q of type %s to %T", column, dst.Type(), value)
}

// sameFamily allows numeric<->numeric and string<->string conversions only
// (reflect would also turn an int into a one-rune string).
func sameFamily(a, b reflect.Kind) bool {
	num := func(k reflect.Kind) bool { return k >= reflect.Int && k <= reflect.Float64 }
	return (num(a) && num(b)) || (a == reflect.String && b == reflect.String)
}

// beforeQuery runs the query plugins on qq and returns the query and
// mutations they produced. m may be nil; it is then looked up by table.
// qq is passed by value so that, without plugins, it stays on the stack.
func (db *DB) beforeQuery(ctx context.Context, a Action, m *meta.Model, qq q.Query, set []q.Mutation) (q.Query, []q.Mutation, error) {
	if len(db.queryPlugins) == 0 || qq.RawSQL != "" {
		return qq, set, nil
	}
	return db.runQueryPlugins(ctx, a, m, qq, set)
}

// runQueryPlugins is the slow path of beforeQuery (qq escapes here).
func (db *DB) runQueryPlugins(ctx context.Context, a Action, m *meta.Model, qq q.Query, set []q.Mutation) (q.Query, []q.Mutation, error) {
	table := tableName(qq.Table)
	if m == nil {
		m = meta.ByTable(table)
	}
	info := &QueryInfo{Action: a, Table: table, Model: modelInfo(m), Query: &qq, Set: set}
	for _, p := range db.queryPlugins {
		if err := p.BeforeQuery(ctx, info); err != nil {
			return qq, nil, err
		}
	}
	return *info.Query, info.Set, nil
}

// beforeWrite runs the write plugins on one record.
func (db *DB) beforeWrite(ctx context.Context, a Action, m *meta.Model, v reflect.Value, fields []string) error {
	if len(db.writePlugins) == 0 {
		return nil
	}
	w := &WriteInfo{Action: a, Table: m.Table, Model: modelInfo(m), Record: v.Addr().Interface(), Fields: fields, m: m, v: v}
	for _, p := range db.writePlugins {
		if err := p.BeforeWrite(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

// hookFunc adapts WithHook functions to ObservePlugin.
type hookFunc func(ctx context.Context, e Event)

func (hookFunc) Name() string                              { return "hook" }
func (h hookFunc) AfterQuery(ctx context.Context, e Event) { h(ctx, e) }
