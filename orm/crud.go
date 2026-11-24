package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/q"
)

// ---- insert options ---------------------------------------------------------

// InsertOption configures Insert / InsertAll.
type InsertOption func(*insertOpts)

type insertOpts struct {
	conflict  *q.OnConflict
	updateAll bool // OnConflictUpdate with no explicit columns
	cascade   bool
}

// Cascade also inserts associated records: belongs_to parents first (their
// keys are copied into the record), then has_one / has_many children (the
// record's key is copied into them). Records whose primary key is already
// set are linked but not re-inserted. Runs in a transaction.
func Cascade() InsertOption { return func(o *insertOpts) { o.cascade = true } }

// OnConflictDoNothing skips rows that violate a unique constraint on cols.
func OnConflictDoNothing(cols ...string) InsertOption {
	return func(o *insertOpts) { o.conflict = &q.OnConflict{Columns: cols, DoNothing: true} }
}

// OnConflictUpdate upserts: on conflict on cols, overwrite update columns
// (or every inserted non-key column when update is empty).
func OnConflictUpdate(cols []string, update ...string) InsertOption {
	return func(o *insertOpts) {
		o.conflict = &q.OnConflict{Columns: cols, Update: update}
		o.updateAll = len(update) == 0
	}
}

func applyInsertOpts(opts []InsertOption) insertOpts {
	var o insertOpts
	for _, f := range opts {
		f(&o)
	}
	return o
}

// ---- helpers ---------------------------------------------------------------

func structPtr(dst any) (reflect.Value, *meta.Model, error) {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return reflect.Value{}, nil, fmt.Errorf("%w, got %T", meta.ErrNotStruct, dst)
	}
	m, err := meta.Of(rv.Elem().Type())
	if err != nil {
		return reflect.Value{}, nil, err
	}
	return rv.Elem(), m, nil
}

// structSlice returns addressable struct values from []T, []*T, *[]T or *[]*T.
func structSlice(recs any) ([]reflect.Value, *meta.Model, error) {
	rv := reflect.ValueOf(recs)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Slice {
		return nil, nil, fmt.Errorf("orm: expected a slice of structs, got %T", recs)
	}
	et := rv.Type().Elem()
	isPtr := et.Kind() == reflect.Pointer
	if isPtr {
		et = et.Elem()
	}
	m, err := meta.Of(et)
	if err != nil {
		return nil, nil, err
	}
	out := make([]reflect.Value, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		e := rv.Index(i)
		if isPtr {
			if e.IsNil() {
				return nil, nil, fmt.Errorf("orm: nil element at index %d", i)
			}
			e = e.Elem()
		}
		out = append(out, e)
	}
	return out, m, nil
}

func toDB(f *meta.Field, v reflect.Value) (any, error) {
	if f.JSON {
		switch v.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			if v.IsNil() {
				return nil, nil
			}
		}
		b, err := json.Marshal(v.Interface())
		if err != nil {
			return nil, fmt.Errorf("orm: marshal %s: %w", f.Name, err)
		}
		return string(b), nil
	}
	return v.Interface(), nil
}

func setTime(v reflect.Value, t time.Time) {
	if v.CanAddr() { // assign without boxing through reflect.ValueOf
		switch p := v.Addr().Interface().(type) {
		case *time.Time:
			*p = t
			return
		case **time.Time:
			*p = &t
			return
		case *sql.NullTime:
			*p = sql.NullTime{Time: t, Valid: true}
			return
		}
	}
	switch v.Type() {
	case reflect.TypeOf(time.Time{}):
		v.Set(reflect.ValueOf(t))
	case reflect.TypeOf(&time.Time{}):
		v.Set(reflect.ValueOf(&t))
	case reflect.TypeOf(sql.NullTime{}):
		v.Set(reflect.ValueOf(sql.NullTime{Time: t, Valid: true}))
	}
}

func setInt(v reflect.Value, id int64) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(id)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(id))
	}
}

// scoped builds a query on the model's table with automatic scopes applied.
func scoped(m *meta.Model, qs []q.Querier) q.Query {
	qq := q.Apply(q.From(m.Table), qs...)
	if m.DeletedAt != nil && !qq.IsUnscoped && qq.RawSQL == "" {
		qq.WhereCond = q.And(q.Null(qq.Table+"."+m.DeletedAt.Column), qq.WhereCond)
	}
	return qq
}

func pkFilter(m *meta.Model, v reflect.Value) (q.Filter, error) {
	if len(m.PKs) == 0 {
		return nil, fmt.Errorf("orm: %s has no primary key", m.Type.Name())
	}
	if m.PK != nil && v.FieldByIndex(m.PK.Index).IsZero() {
		return nil, fmt.Errorf("orm: %s has a zero primary key", m.Type.Name())
	}
	fs := make([]q.Filter, len(m.PKs))
	for i, f := range m.PKs {
		fs[i] = q.Eq(f.Column, v.FieldByIndex(f.Index).Interface())
	}
	return q.And(fs...), nil
}

func isPK(m *meta.Model, f *meta.Field) bool {
	for _, p := range m.PKs {
		if p == f {
			return true
		}
	}
	return false
}

// hasAfterHooks reports whether a write on v should be wrapped in a
// transaction so that a failing After hook undoes the write.
func hasAfterHooks(t reflect.Type) bool {
	p := reflect.PointerTo(t)
	for _, i := range []reflect.Type{
		reflect.TypeOf((*AfterInserter)(nil)).Elem(),
		reflect.TypeOf((*AfterUpdater)(nil)).Elem(),
		reflect.TypeOf((*AfterDeleter)(nil)).Elem(),
	} {
		if p.Implements(i) {
			return true
		}
	}
	return false
}

func (db *DB) atomic(ctx context.Context, m *meta.Model, fn func(ctx context.Context) error) error {
	if !db.InTransaction(ctx) && hasAfterHooks(m.Type) {
		return db.Transaction(ctx, fn)
	}
	return fn(ctx)
}

// ---- read ------------------------------------------------------------------

func (db *DB) selectModels(ctx context.Context, m *meta.Model, qq q.Query) (reflect.Value, error) {
	qq, _, err := db.beforeQuery(ctx, ActionSelect, m, qq, nil)
	if err != nil {
		return reflect.Value{}, err
	}
	st, err := db.dialect.Select(qq)
	if err != nil {
		return reflect.Value{}, err
	}
	rows, err := db.queryRead(ctx, st, &qq)
	if err != nil {
		return reflect.Value{}, err
	}
	return scanAll(ctx, rows, m)
}

// Find loads the first matching row into dst (*Struct). Soft-deleted rows
// are skipped unless q.Unscoped() is given.
func (db *DB) Find(ctx context.Context, dst any, qs ...q.Querier) error {
	v, m, err := structPtr(dst)
	if err != nil {
		return err
	}
	qq := scoped(m, qs)
	qq.LimitN = 1
	items, err := db.selectModels(ctx, m, qq)
	if err != nil {
		return err
	}
	if items.Len() == 0 {
		return ErrNotFound
	}
	v.Set(items.Index(0))
	return nil
}

// FindAll loads every matching row into dst (*[]Struct or *[]*Struct).
func (db *DB) FindAll(ctx context.Context, dst any, qs ...q.Querier) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("orm: FindAll destination must be a pointer to a slice, got %T", dst)
	}
	et := rv.Elem().Type().Elem()
	if et.Kind() == reflect.Pointer {
		et = et.Elem()
	}
	m, err := meta.Of(et)
	if err != nil {
		return err
	}
	items, err := db.selectModels(ctx, m, scoped(m, qs))
	if err != nil {
		return err
	}
	return assignSlice(rv.Elem(), items)
}

// Count counts rows in table matching the queriers.
func (db *DB) Count(ctx context.Context, table string, qs ...q.Querier) (int64, error) {
	return db.count(ctx, q.Apply(q.From(table), qs...))
}

func (db *DB) count(ctx context.Context, qq q.Query) (int64, error) {
	qq, _, err := db.beforeQuery(ctx, ActionCount, nil, qq, nil)
	if err != nil {
		return 0, err
	}
	st, err := db.dialect.Count(qq)
	if err != nil {
		return 0, err
	}
	rows, err := db.queryRead(ctx, st, &qq)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := scanInto(ctx, rows, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// Select runs an arbitrary query and scans into dst: a struct, slice of
// structs, scalar or slice of scalars. Ideal for joins and aggregates.
func (db *DB) Select(ctx context.Context, dst any, qq q.Query) error {
	qq, _, err := db.beforeQuery(ctx, ActionSelect, nil, qq, nil)
	if err != nil {
		return err
	}
	st, err := db.dialect.Select(qq)
	if err != nil {
		return err
	}
	rows, err := db.queryRead(ctx, st, &qq)
	if err != nil {
		return err
	}
	return scanInto(ctx, rows, dst)
}

// Query runs raw SQL ('?' placeholders) and scans into dst like Select.
func (db *DB) Query(ctx context.Context, dst any, query string, args ...any) error {
	return db.Select(ctx, dst, q.SQL(query, args...))
}

// ---- insert ----------------------------------------------------------------

func (db *DB) touchCreate(m *meta.Model, v reflect.Value) {
	now := db.now()
	for _, f := range []*meta.Field{m.CreatedAt, m.UpdatedAt} {
		if f != nil {
			if fv := v.FieldByIndex(f.Index); fv.IsZero() {
				setTime(fv, now)
			}
		}
	}
	if m.Version != nil {
		if fv := v.FieldByIndex(m.Version.Index); fv.IsZero() {
			bumpVersion(fv) // new records start at version 1
		}
	}
}

// lockFilter adds the optimistic-lock condition (version = the value the
// record was read with) to where, and returns the SET that bumps it.
func lockFilter(m *meta.Model, v reflect.Value, where q.Filter) (q.Filter, q.Mutation) {
	cur := v.FieldByIndex(m.Version.Index).Interface()
	return q.And(where, q.Eq(m.Version.Column, cur)), q.Inc(m.Version.Column, 1)
}

// checkLocked turns "no row matched" into ErrStale for a lock model and
// otherwise advances the record's version to match the row.
func checkLocked(m *meta.Model, v reflect.Value, res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStale
	}
	bumpVersion(v.FieldByIndex(m.Version.Index))
	return nil
}

func bumpVersion(fv reflect.Value) {
	if fv.CanInt() {
		fv.SetInt(fv.Int() + 1)
	} else {
		fv.SetUint(fv.Uint() + 1)
	}
}

func insertColumns(m *meta.Model, v reflect.Value, omitEmpty bool) (cols []string, vals []any, returnPK bool, err error) {
	cols = make([]string, 0, len(m.Fields))
	vals = make([]any, 0, len(m.Fields))
	for _, f := range m.Fields {
		if f.ReadOnly {
			continue
		}
		fv := v.FieldByIndex(f.Index)
		if f.Primary && f.Auto && fv.IsZero() {
			returnPK = true
			continue
		}
		if omitEmpty && f.OmitEmpty && fv.IsZero() {
			continue
		}
		val, err := toDB(f, fv)
		if err != nil {
			return nil, nil, false, err
		}
		cols = append(cols, f.Column)
		vals = append(vals, val)
	}
	return cols, vals, returnPK, nil
}

// insertValues appends v's values for the columns insertColumns(m, v,
// false) returns, without building the column list.
func insertValues(m *meta.Model, v reflect.Value, dst []any) ([]any, error) {
	for _, f := range m.Fields {
		if f.ReadOnly {
			continue
		}
		fv := v.FieldByIndex(f.Index)
		if f.Primary && f.Auto && fv.IsZero() {
			continue
		}
		val, err := toDB(f, fv)
		if err != nil {
			return nil, err
		}
		dst = append(dst, val)
	}
	return dst, nil
}

func conflictFor(o insertOpts, m *meta.Model, cols []string) *q.OnConflict {
	if o.conflict == nil {
		return nil
	}
	c := *o.conflict
	if o.updateAll {
		for _, col := range cols {
			if f := m.Field(col); f != nil && isPK(m, f) {
				continue
			}
			if m.CreatedAt != nil && col == m.CreatedAt.Column {
				continue
			}
			c.Update = append(c.Update, col)
		}
	}
	return &c
}

// Insert inserts a record (*Struct), filling in its auto-increment primary
// key and created_at / updated_at timestamps.
func (db *DB) Insert(ctx context.Context, rec any, opts ...InsertOption) error {
	v, m, err := structPtr(rec)
	if err != nil {
		return err
	}
	o := applyInsertOpts(opts)
	fn := func(ctx context.Context) error { return db.insertOne(ctx, m, v, o) }
	if o.cascade && !db.InTransaction(ctx) {
		return db.Transaction(ctx, fn)
	}
	return db.atomic(ctx, m, fn)
}

func (db *DB) insertOne(ctx context.Context, m *meta.Model, v reflect.Value, o insertOpts) error {
	if err := callHook(ctx, v, hookBeforeInsert); err != nil {
		return err
	}
	if o.cascade {
		if err := db.cascadeParents(ctx, m, v); err != nil {
			return err
		}
	}
	db.touchCreate(m, v)
	if err := db.beforeWrite(ctx, ActionInsert, m, v, nil, true); err != nil {
		return err
	}
	cols, vals, retPK, err := insertColumns(m, v, true)
	if err != nil {
		return err
	}
	ins := dialect.Insert{Table: m.Table, Columns: cols, Rows: [][]any{vals}, OnConflict: conflictFor(o, m, cols)}
	if retPK {
		ins.Returning = m.PK.Column
	}
	st, err := db.dialect.Insert(ins)
	if err != nil {
		return err
	}
	defer db.touched(ctx, m.Table)
	if retPK && db.dialect.IDMode() == dialect.IDReturning {
		rows, err := db.queryStmt(ctx, st)
		if err != nil {
			return err
		}
		pk := v.FieldByIndex(m.PK.Index)
		var ids []any
		if err := scanInto(ctx, rows, &ids); err != nil {
			return db.wrap(err)
		}
		if len(ids) > 0 { // zero rows: skipped by ON CONFLICT DO NOTHING
			if err := assignPK(pk, ids[0]); err != nil {
				return err
			}
		}
	} else {
		res, err := db.execStmt(ctx, st)
		if err != nil {
			return err
		}
		if retPK {
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if id > 0 {
				setInt(v.FieldByIndex(m.PK.Index), id)
			}
		}
	}
	if o.cascade {
		if err := db.cascadeChildren(ctx, m, v); err != nil {
			return err
		}
	}
	return callHook(ctx, v, hookAfterInsert)
}

// assignPK converts a scanned driver value into the primary key field.
func assignPK(pk reflect.Value, raw any) error {
	switch x := raw.(type) {
	case int64:
		setInt(pk, x)
		return nil
	case []byte:
		raw = string(x)
	}
	rv := reflect.ValueOf(raw)
	if rv.Type().ConvertibleTo(pk.Type()) {
		pk.Set(rv.Convert(pk.Type()))
		return nil
	}
	if s, ok := pk.Addr().Interface().(sql.Scanner); ok {
		return s.Scan(raw)
	}
	return fmt.Errorf("orm: cannot assign %T to primary key of type %s", raw, pk.Type())
}

// InsertAll inserts many records ([]T, []*T, *[]T or *[]*T), filling in
// primary keys. Rows are sent in batches inside one transaction: multi-row
// INSERT ... RETURNING on Postgres and SQLite, multi-row INSERT on MySQL and
// MERGE ... OUTPUT on SQL Server. Every record gets the id the database
// stored for it.
func (db *DB) InsertAll(ctx context.Context, recs any, opts ...InsertOption) error {
	items, m, err := structSlice(recs)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	o := applyInsertOpts(opts)
	return db.Transaction(ctx, func(ctx context.Context) error {
		if !db.canBulk(m, items, o) {
			for _, v := range items {
				if err := db.insertOne(ctx, m, v, o); err != nil {
					return err
				}
			}
			return nil
		}
		return db.insertBulk(ctx, m, items, o)
	})
}

func (db *DB) canBulk(m *meta.Model, items []reflect.Value, o insertOpts) bool {
	if o.cascade || len(items) < 2 {
		return false
	}
	if o.conflict != nil && m.PK != nil && m.PK.Auto {
		return false // skipped rows would break the mapping of new ids to rows
	}
	if m.PK != nil && m.PK.Auto {
		zero := items[0].FieldByIndex(m.PK.Index).IsZero()
		for _, v := range items[1:] {
			if v.FieldByIndex(m.PK.Index).IsZero() != zero {
				return false
			}
		}
		// Explicit values for an IDENTITY column need IDENTITY_INSERT on
		// SQL Server; the row-by-row path handles that.
		if !zero && db.dialect.Kind() == dialect.KindMSSQL {
			return false
		}
	}
	return true
}

// bulkLimits returns the most parameters and rows one statement may carry.
func (db *DB) bulkLimits() (params, rows int) {
	switch db.dialect.Kind() {
	case dialect.KindSQLite:
		// SQLite runs in-process, so there is no round trip to save; the
		// gain comes from fewer statement executions. Large statements cost
		// more to prepare than they save: ~256 parameters per statement
		// measured fastest (1,000 rows: 5.4 ms vs 17 ms row by row and
		// 37 ms as one statement).
		return 256, 1 << 30
	case dialect.KindMSSQL:
		return 2099, 1000
	}
	return 65535, 1 << 30
}

func (db *DB) insertBulk(ctx context.Context, m *meta.Model, items []reflect.Value, o insertOpts) error {
	w := db.newWriteInfo(ActionInsert, m) // nil without write plugins
	for _, v := range items {
		if err := callHook(ctx, v, hookBeforeInsert); err != nil {
			return err
		}
		db.touchCreate(m, v)
		if w != nil {
			if err := db.callWritePlugins(ctx, w, v); err != nil {
				return err
			}
		}
	}
	cols, _, retPK, err := insertColumns(m, items[0], false)
	if err != nil {
		return err
	}
	maxParams, maxRows := db.bulkLimits()
	chunk := min(maxRows, max(1, maxParams/max(1, len(cols))))
	kind := db.dialect.Kind()
	idStep := int64(0) // MySQL: auto_increment_increment, read once
	defer db.touched(ctx, m.Table)
	// One value buffer and one row index, reused for every chunk: the
	// statement copies the values into its own args.
	n := min(chunk, len(items))
	flat := make([]any, 0, n*len(cols))
	rowsBuf := make([][]any, 0, n)
	conflict := conflictFor(o, m, cols)
	for start := 0; start < len(items); start += chunk {
		part := items[start:min(start+chunk, len(items))]
		flat, rowsBuf = flat[:0], rowsBuf[:0]
		for _, v := range part {
			from := len(flat)
			if flat, err = insertValues(m, v, flat); err != nil {
				return err
			}
			if len(flat)-from != len(cols) {
				return fmt.Errorf("orm: InsertAll: records of %s have different columns", m.Type.Name())
			}
			rowsBuf = append(rowsBuf, flat[from:len(flat):len(flat)])
		}
		ins := dialect.Insert{Table: m.Table, Columns: cols, Rows: rowsBuf, OnConflict: conflict}
		if retPK {
			ins.Returning = m.PK.Column
		}
		st, err := db.dialect.Insert(ins)
		if err != nil {
			return err
		}
		switch {
		case !retPK:
			if _, err := db.execStmt(ctx, st); err != nil {
				return err
			}
		case kind == dialect.KindMySQL:
			if idStep == 0 {
				if idStep, err = db.mysqlIDStep(ctx); err != nil {
					return err
				}
			}
			if err := db.mysqlBulkIDs(ctx, st, m, part, idStep); err != nil {
				return err
			}
		case kind == dialect.KindMSSQL && len(part) > 1:
			if err := db.mssqlBulkIDs(ctx, st, m, part); err != nil {
				return err
			}
		default:
			rows, err := db.queryStmt(ctx, st)
			if err != nil {
				return err
			}
			var ids []any
			if err := scanInto(ctx, rows, &ids); err != nil {
				return db.wrap(err)
			}
			if len(ids) != len(part) {
				return fmt.Errorf("orm: inserted %d rows but got %d ids", len(part), len(ids))
			}
			for i, v := range part {
				if err := assignPK(v.FieldByIndex(m.PK.Index), ids[i]); err != nil {
					return err
				}
			}
		}
	}
	for _, v := range items {
		if err := callHook(ctx, v, hookAfterInsert); err != nil {
			return err
		}
	}
	return nil
}

// ---- update ----------------------------------------------------------------

// Update writes a record by primary key. With no fields every column is
// written; otherwise only the named fields (Go or column names). updated_at
// is always refreshed.
func (db *DB) Update(ctx context.Context, rec any, fields ...string) error {
	v, m, err := structPtr(rec)
	if err != nil {
		return err
	}
	return db.atomic(ctx, m, func(ctx context.Context) error { return db.updateOne(ctx, m, v, fields) })
}

func (db *DB) updateOne(ctx context.Context, m *meta.Model, v reflect.Value, fields []string) error {
	where, err := pkFilter(m, v)
	if err != nil {
		return err
	}
	if err := callHook(ctx, v, hookBeforeUpdate); err != nil {
		return err
	}
	if m.UpdatedAt != nil {
		setTime(v.FieldByIndex(m.UpdatedAt.Index), db.now())
	}
	var targets []*meta.Field
	if len(fields) == 0 {
		for _, f := range m.Fields {
			if isPK(m, f) || f == m.CreatedAt || f.ReadOnly || f == m.Version {
				continue
			}
			targets = append(targets, f)
		}
	} else {
		seen := map[*meta.Field]bool{}
		for _, name := range fields {
			f := m.Field(name)
			if f == nil {
				return fmt.Errorf("orm: %s has no field %q", m.Type.Name(), name)
			}
			if f.ReadOnly || seen[f] || f == m.Version {
				continue // the version is managed by the lock
			}
			seen[f] = true
			targets = append(targets, f)
		}
		if m.UpdatedAt != nil && !seen[m.UpdatedAt] {
			targets = append(targets, m.UpdatedAt)
		}
	}
	if err := db.beforeWrite(ctx, ActionUpdate, m, v, targets, len(fields) == 0); err != nil {
		return err
	}
	muts := make([]q.Mutation, 0, len(targets))
	for _, f := range targets {
		val, err := toDB(f, v.FieldByIndex(f.Index))
		if err != nil {
			return err
		}
		muts = append(muts, q.Set(f.Column, val))
	}
	if m.Version != nil {
		var inc q.Mutation
		where, inc = lockFilter(m, v, where)
		muts = append(muts, inc)
	}
	qq := q.Query{Table: m.Table, WhereCond: where}
	if qq, muts, err = db.beforeQuery(ctx, ActionUpdate, m, qq, muts); err != nil {
		return err
	}
	st, err := db.dialect.Update(m.Table, muts, qq.WhereCond)
	if err != nil {
		return err
	}
	defer db.touched(ctx, m.Table)
	res, err := db.execStmt(ctx, st)
	if err != nil {
		return err
	}
	if m.Version != nil {
		if err := checkLocked(m, v, res); err != nil {
			return err
		}
	}
	return callHook(ctx, v, hookAfterUpdate)
}

// UpdateAny applies mutations to every row matched by qq and returns the
// number of affected rows.
func (db *DB) UpdateAny(ctx context.Context, qq q.Query, muts ...q.Mutation) (int64, error) {
	return db.updateAny(ctx, ActionUpdate, qq, muts)
}

// updateAny is UpdateAny reporting action to plugins (soft deletes are
// updates in SQL but deletes to plugins).
func (db *DB) updateAny(ctx context.Context, action Action, qq q.Query, muts []q.Mutation) (int64, error) {
	qq, muts, err := db.beforeQuery(ctx, action, nil, qq, muts)
	if err != nil {
		return 0, err
	}
	st, err := db.dialect.Update(qq.Table, muts, qq.WhereCond)
	if err != nil {
		return 0, err
	}
	defer db.touched(ctx, qq.Table)
	res, err := db.execStmt(ctx, st)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- delete ----------------------------------------------------------------

// Delete deletes a record by primary key. Models with a DeletedAt field are
// soft-deleted (deleted_at is set) instead.
func (db *DB) Delete(ctx context.Context, rec any) error {
	return db.deleteRec(ctx, rec, false)
}

// HardDelete permanently deletes a record, even for soft-delete models.
func (db *DB) HardDelete(ctx context.Context, rec any) error {
	return db.deleteRec(ctx, rec, true)
}

func (db *DB) deleteRec(ctx context.Context, rec any, hard bool) error {
	v, m, err := structPtr(rec)
	if err != nil {
		return err
	}
	return db.atomic(ctx, m, func(ctx context.Context) error {
		where, err := pkFilter(m, v)
		if err != nil {
			return err
		}
		if err := callHook(ctx, v, hookBeforeDelete); err != nil {
			return err
		}
		var (
			st   dialect.Stmt
			now  = db.now()
			soft = m.DeletedAt != nil && !hard
			set  []q.Mutation
		)
		if soft {
			set = []q.Mutation{q.Set(m.DeletedAt.Column, now)}
		}
		if m.Version != nil {
			var inc q.Mutation
			where, inc = lockFilter(m, v, where)
			if soft {
				set = append(set, inc)
			}
		}
		qq := q.Query{Table: m.Table, WhereCond: where}
		if qq, set, err = db.beforeQuery(ctx, ActionDelete, m, qq, set); err != nil {
			return err
		}
		if soft {
			st, err = db.dialect.Update(m.Table, set, qq.WhereCond)
		} else {
			st, err = db.dialect.Delete(m.Table, qq.WhereCond)
		}
		if err != nil {
			return err
		}
		defer db.touched(ctx, m.Table)
		res, err := db.execStmt(ctx, st)
		if err != nil {
			return err
		}
		if m.Version != nil {
			if err := checkLocked(m, v, res); err != nil {
				return err
			}
		}
		if soft {
			setTime(v.FieldByIndex(m.DeletedAt.Index), now)
		}
		return callHook(ctx, v, hookAfterDelete)
	})
}

// DeleteAny permanently deletes every row matched by qq.
func (db *DB) DeleteAny(ctx context.Context, qq q.Query) (int64, error) {
	if qq.WhereCond == nil {
		return 0, errors.New("orm: DeleteAny without a filter; use q.Expr(\"1=1\") to delete everything")
	}
	qq, _, err := db.beforeQuery(ctx, ActionDelete, nil, qq, nil)
	if err != nil {
		return 0, err
	}
	st, err := db.dialect.Delete(qq.Table, qq.WhereCond)
	if err != nil {
		return 0, err
	}
	defer db.touched(ctx, qq.Table)
	res, err := db.execStmt(ctx, st)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// mysqlIDStep reads auto_increment_increment (1 unless the server is set
// up for multi-primary replication, e.g. Galera or group replication).
func (db *DB) mysqlIDStep(ctx context.Context) (int64, error) {
	var step int64
	if err := db.conn(ctx).QueryRowContext(ctx, "SELECT @@auto_increment_increment").Scan(&step); err != nil {
		return 0, db.wrap(err)
	}
	return max(step, 1), nil
}

// mysqlBulkIDs runs a multi-row INSERT and assigns the new ids. MySQL
// reports only the first id; the ids of the rows of a single multi-row
// INSERT are consecutive (step auto_increment_increment) in row order.
func (db *DB) mysqlBulkIDs(ctx context.Context, st dialect.Stmt, m *meta.Model, part []reflect.Value, step int64) error {
	res, err := db.execStmt(ctx, st)
	if err != nil {
		return err
	}
	first, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n != int64(len(part)) {
		return fmt.Errorf("orm: inserted %d rows but MySQL reported %d", len(part), n)
	}
	for i, v := range part {
		setInt(v.FieldByIndex(m.PK.Index), first+int64(i)*step)
	}
	return nil
}

// mssqlBulkIDs runs the MERGE built for a multi-row insert on SQL Server and
// assigns ids by the position each output row carries.
func (db *DB) mssqlBulkIDs(ctx context.Context, st dialect.Stmt, m *meta.Model, part []reflect.Value) error {
	rows, err := db.queryStmt(ctx, st)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var pos int64
		var id any
		if err := rows.Scan(&pos, &id); err != nil {
			return db.wrap(err)
		}
		if pos < 0 || pos >= int64(len(part)) {
			return fmt.Errorf("orm: SQL Server returned row position %d of %d", pos, len(part))
		}
		if err := assignPK(part[pos].FieldByIndex(m.PK.Index), id); err != nil {
			return err
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return db.wrap(err)
	}
	if seen != len(part) {
		return fmt.Errorf("orm: inserted %d rows but got %d ids", len(part), seen)
	}
	return nil
}
