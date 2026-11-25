package orm

import (
	"context"
	"database/sql"
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

// RoutePlugin steers reads between the primary and read replicas
// (WithReplicas), e.g. to read your own writes despite replica lag.
//
// Wrote runs after every statement that may have changed data on the
// primary and succeeded (inserts, updates, deletes, raw Exec; also inside
// transactions). ReadPrimary runs for reads that would go to a replica;
// true sends the read to the primary. Reads inside a transaction or with
// UsePrimary go to the primary without asking. Both must be safe for
// concurrent use and fast.
type RoutePlugin interface {
	Plugin
	Wrote(ctx context.Context)
	ReadPrimary(ctx context.Context) bool
}

// GuardPlugin decides whether a round trip to the database may happen,
// e.g. a circuit breaker that fails fast while the database is down. It
// guards every statement (model queries, raw SQL, writes; also those inside
// transactions) and every transaction start, but not cache hits.
//
// Allow runs first; an error skips the round trip and is returned (wrapped)
// to the caller. For every Allow that returned nil, Done reports the outcome
// once: nil, or the error from the database. For queries, Done runs when
// the query returns its rows, so errors while reading rows are not reported.
// Both must be safe for concurrent use and fast: they run on every query.
type GuardPlugin interface {
	Plugin
	Allow(ctx context.Context) error
	Done(ctx context.Context, err error)
}

// ObservePlugin is told about every executed statement and every cache
// hit, for logging, tracing and metrics. It must be safe for concurrent
// use.
type ObservePlugin interface {
	Plugin
	AfterQuery(ctx context.Context, e Event)
}

// AfterWritePlugin runs after kiln wrote one record: Insert (and every
// InsertAll record), Update, Delete and HardDelete, with w.Action set to
// ActionInsert, ActionUpdate or ActionDelete. It is not called for bulk
// queries (UpdateAny, DeleteAny), for an insert skipped by ON CONFLICT DO
// NOTHING, or when the write failed.
//
// While such a plugin is registered, kiln runs each single-record write in
// a transaction (if it is not already in one) and calls AfterWrite inside
// it, so writing something alongside the change (an audit entry) is atomic
// with it: an error rolls both back. w is the WriteInfo BeforeWrite saw for
// the same write; use w.SetValue / w.Value to pass data between the two
// (except for InsertAll, which runs every BeforeWrite before the batch and
// starts each record's AfterWrite with no values).
type AfterWritePlugin interface {
	Plugin
	AfterWrite(ctx context.Context, w *WriteInfo) error
}

// CheckPlugin is a WritePlugin that only checks records, such as
// validation. Its BeforeWrite runs after every other plugin's, whatever the
// order the plugins were given in, so it sees the values they fill in (a
// tenant id, timestamps).
type CheckPlugin interface {
	WritePlugin
	ChecksOnly()
}

// ReadPlugin runs after kiln loaded records of a model: Find, FindAll,
// FindByID, Each, Cursor, Paginate, preloads and cached results. It is not
// called for raw SQL scanned into structs (DB.Query) or when nothing was
// found. An error fails the read.
type ReadPlugin interface {
	Plugin
	AfterRead(ctx context.Context, r *ReadInfo) error
}

// ReadInfo gives a ReadPlugin access to the loaded records.
type ReadInfo struct {
	Table string
	Model *ModelInfo
	m     *meta.Model
	items reflect.Value // []T, addressable elements
}

// Len returns the number of records.
func (r *ReadInfo) Len() int { return r.items.Len() }

// Field returns a pointer to record i's field for column (e.g. *string),
// or nil if the model has no such column.
func (r *ReadInfo) Field(i int, column string) any {
	f := r.m.Field(column)
	if f == nil {
		return nil
	}
	return r.items.Index(i).FieldByIndex(f.Index).Addr().Interface()
}

func (db *DB) afterRead(ctx context.Context, m *meta.Model, items reflect.Value) error {
	r := &ReadInfo{Table: m.Table, Model: modelInfo(m), m: m, items: items}
	for _, p := range db.readPlugins {
		if err := p.AfterRead(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// TxPlugin runs when a top-level transaction starts (Transaction or
// Begin; not savepoints), before your function sees the transaction. Use it
// for per-transaction session settings, e.g. SET LOCAL / set_config for
// PostgreSQL row-level security. ctx is the context Transaction was called
// with. An error rolls the transaction back and is returned to the caller.
type TxPlugin interface {
	Plugin
	AfterBegin(ctx context.Context, tx *sql.Tx) error
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
			if _, check := p.(CheckPlugin); check {
				db.writePlugins = append(db.writePlugins, wp)
				db.checkPlugins++
			} else { // before the check plugins
				i := len(db.writePlugins) - db.checkPlugins
				db.writePlugins = append(db.writePlugins[:i], append([]WritePlugin{wp}, db.writePlugins[i:]...)...)
			}
			hooked = true
		}
		if rp, ok := p.(RoutePlugin); ok {
			db.routes = append(db.routes, rp)
			hooked = true
		}
		if gp, ok := p.(GuardPlugin); ok {
			db.guards = append(db.guards, gp)
			hooked = true
		}
		if op, ok := p.(ObservePlugin); ok {
			db.hooks = append(db.hooks, op.AfterQuery)
			hooked = true
		}
		if ap, ok := p.(AfterWritePlugin); ok {
			db.afterWritePlugins = append(db.afterWritePlugins, ap)
			hooked = true
		}
		if rp, ok := p.(ReadPlugin); ok {
			db.readPlugins = append(db.readPlugins, rp)
			hooked = true
		}
		if tp, ok := p.(TxPlugin); ok {
			db.txPlugins = append(db.txPlugins, tp)
			hooked = true
		}
		if !hooked {
			panic(fmt.Sprintf("orm: plugin %q implements none of BeforeQuery, BeforeWrite, AfterWrite, AfterRead, AfterQuery, AfterBegin, Allow/Done, Wrote/ReadPrimary", p.Name()))
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
	// ActionRaw marks raw SQL (db.Query, db.Exec, q.SQL) in Event. Plugins'
	// BeforeQuery never sees it: raw SQL is not rewritten.
	ActionRaw
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
	case ActionRaw:
		return "raw"
	}
	return fmt.Sprintf("Action(%d)", uint8(a))
}

// QueryInfo is passed to QueryPlugin.BeforeQuery. It is valid only during
// the call; plugins must not keep it.
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

	query q.Query // backing store for Query: one allocation per call
}

// Where adds a filter to the query (AND-ed with the existing ones).
func (i *QueryInfo) Where(f q.Filter) { i.Query.WhereCond = q.And(i.Query.WhereCond, f) }

// WriteInfo is passed to WritePlugin.BeforeWrite, once per record. It is
// valid only during the call: InsertAll reuses it for the next record, so
// plugins must not keep it.
type WriteInfo struct {
	Action Action // ActionInsert or ActionUpdate
	Table  string
	Model  *ModelInfo
	// Record points to the struct being written (or deleted).
	Record any
	// Fields lists the columns an update writes; nil means every column.
	// Values set on other columns are kept on the record but not written.
	Fields []string

	m      *meta.Model
	v      reflect.Value
	values map[any]any // SetValue; nil until used
}

// SetValue stores val under key for the rest of this write, so a plugin's
// AfterWrite can read what its BeforeWrite saw (e.g. old values). Use an
// unexported key type, as with context.WithValue.
func (w *WriteInfo) SetValue(key, val any) {
	if w.values == nil {
		w.values = make(map[any]any, 1)
	}
	w.values[key] = val
}

// Value returns the value stored by SetValue, or nil.
func (w *WriteInfo) Value(key any) any { return w.values[key] }

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

// runQueryPlugins is the slow path of beforeQuery.
func (db *DB) runQueryPlugins(ctx context.Context, a Action, m *meta.Model, qq q.Query, set []q.Mutation) (q.Query, []q.Mutation, error) {
	table := qq.Table
	if m == nil || table != m.Table {
		table = tableName(table)
		if m == nil {
			m = meta.ByTable(table)
		}
	}
	info := &QueryInfo{Action: a, Table: table, Model: modelInfo(m), Set: set, query: qq}
	info.Query = &info.query
	for _, p := range db.queryPlugins {
		if err := p.BeforeQuery(ctx, info); err != nil {
			return qq, nil, err
		}
	}
	return *info.Query, info.Set, nil
}

// beforeWrite runs the write plugins on one record. targets are the
// fields an update writes, unless all is true.
// It returns the WriteInfo for afterWrite, nil without write plugins.
func (db *DB) beforeWrite(ctx context.Context, a Action, m *meta.Model, v reflect.Value, targets []*meta.Field, all bool) (*WriteInfo, error) {
	if len(db.writePlugins) == 0 && len(db.afterWritePlugins) == 0 {
		return nil, nil
	}
	return db.runWritePlugins(ctx, a, m, v, targets, all)
}

// runWritePlugins is the slow path of beforeWrite.
func (db *DB) runWritePlugins(ctx context.Context, a Action, m *meta.Model, v reflect.Value, targets []*meta.Field, all bool) (*WriteInfo, error) {
	var fields []string
	if !all {
		fields = make([]string, len(targets))
		for i, f := range targets {
			fields[i] = f.Column
		}
	}
	w := db.newWriteInfo(a, m)
	w.Fields = fields
	return w, db.callWritePlugins(ctx, w, v)
}

// newWriteInfo returns a WriteInfo for m, or nil without write plugins.
// InsertAll creates one per call and reuses it for every record.
func (db *DB) newWriteInfo(a Action, m *meta.Model) *WriteInfo {
	if len(db.writePlugins) == 0 && len(db.afterWritePlugins) == 0 {
		return nil
	}
	return &WriteInfo{Action: a, Table: m.Table, Model: modelInfo(m), m: m}
}

// callWritePlugins runs the write plugins on record v using w.
func (db *DB) callWritePlugins(ctx context.Context, w *WriteInfo, v reflect.Value) error {
	w.v, w.Record = v, v.Addr().Interface()
	if w.values != nil {
		clear(w.values) // InsertAll reuses w: each record starts empty
	}
	for _, p := range db.writePlugins {
		if err := p.BeforeWrite(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

// afterWrite runs the after-write plugins for record v. w is the WriteInfo
// from beforeWrite (nil for deletes, which have no BeforeWrite).
// Small enough to inline: without after-write plugins it is one length check.
func (db *DB) afterWrite(ctx context.Context, w *WriteInfo, a Action, m *meta.Model, v reflect.Value) error {
	if len(db.afterWritePlugins) == 0 {
		return nil
	}
	return db.runAfterWrite(ctx, w, a, m, v)
}

// runAfterWrite is the slow path of afterWrite.
func (db *DB) runAfterWrite(ctx context.Context, w *WriteInfo, a Action, m *meta.Model, v reflect.Value) error {
	if w == nil {
		w = &WriteInfo{Table: m.Table, Model: modelInfo(m), m: m}
	}
	w.Action, w.v, w.Record = a, v, v.Addr().Interface()
	for _, p := range db.afterWritePlugins {
		if err := p.AfterWrite(ctx, w); err != nil {
			return fmt.Errorf("orm: plugin %s: %w", p.Name(), err)
		}
	}
	return nil
}

// hookFunc adapts WithHook functions to ObservePlugin.
type hookFunc func(ctx context.Context, e Event)

func (hookFunc) Name() string                              { return "hook" }
func (h hookFunc) AfterQuery(ctx context.Context, e Event) { h(ctx, e) }
