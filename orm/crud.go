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
// primary keys. Postgres and SQLite use multi-row INSERT ... RETURNING;
// MySQL and SQL Server insert row by row inside a transaction so generated
// IDs are always correct.
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
	k := db.dialect.Kind()
	if (k != dialect.KindPostgres && k != dialect.KindSQLite) || o.cascade {
		return false
	}
	if o.conflict != nil && m.PK != nil && m.PK.Auto {
		return false // skipped rows would break RETURNING order
	}
	if m.PK != nil && m.PK.Auto {
		zero := items[0].FieldByIndex(m.PK.Index).IsZero()
		for _, v := range items[1:] {
			if v.FieldByIndex(m.PK.Index).IsZero() != zero {
				return false
			}
		}
	}
	return true
}

func (db *DB) insertBulk(ctx context.Context, m *meta.Model, items []reflect.Value, o insertOpts) error {
	for _, v := range items {
		if err := callHook(ctx, v, hookBeforeInsert); err != nil {
			return err
		}
		db.touchCreate(m, v)
	}
	cols, _, retPK, err := insertColumns(m, items[0], false)
	if err != nil {
		return err
	}
	maxParams := 65535
	if db.dialect.Kind() == dialect.KindSQLite {
		maxParams = 32766
	}
	chunk := max(1, maxParams/max(1, len(cols)))
	defer db.touched(ctx, m.Table)
	for start := 0; start < len(items); start += chunk {
		part := items[start:min(start+chunk, len(items))]
		ins := dialect.Insert{Table: m.Table, Columns: cols, OnConflict: conflictFor(o, m, cols)}
		for _, v := range part {
			_, vals, _, err := insertColumns(m, v, false)
			if err != nil {
				return err
			}
			ins.Rows = append(ins.Rows, vals)
		}
		if retPK {
			ins.Returning = m.PK.Column
		}
		st, err := db.dialect.Insert(ins)
		if err != nil {
			return err
		}
		if !retPK {
			if _, err := db.execStmt(ctx, st); err != nil {
				return err
			}
			continue
		}
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
			if isPK(m, f) || f == m.CreatedAt || f.ReadOnly {
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
			if f.ReadOnly || seen[f] {
				continue
			}
			seen[f] = true
			targets = append(targets, f)
		}
		if m.UpdatedAt != nil && !seen[m.UpdatedAt] {
			targets = append(targets, m.UpdatedAt)
		}
	}
	muts := make([]q.Mutation, 0, len(targets))
	for _, f := range targets {
		val, err := toDB(f, v.FieldByIndex(f.Index))
		if err != nil {
			return err
		}
		muts = append(muts, q.Set(f.Column, val))
	}
	st, err := db.dialect.Update(m.Table, muts, where)
	if err != nil {
		return err
	}
	defer db.touched(ctx, m.Table)
	if _, err := db.execStmt(ctx, st); err != nil {
		return err
	}
	return callHook(ctx, v, hookAfterUpdate)
}

// UpdateAny applies mutations to every row matched by qq and returns the
// number of affected rows.
func (db *DB) UpdateAny(ctx context.Context, qq q.Query, muts ...q.Mutation) (int64, error) {
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
		var st dialect.Stmt
		if m.DeletedAt != nil && !hard {
			now := db.now()
			st, err = db.dialect.Update(m.Table, []q.Mutation{q.Set(m.DeletedAt.Column, now)}, where)
			if err == nil {
				setTime(v.FieldByIndex(m.DeletedAt.Index), now)
			}
		} else {
			st, err = db.dialect.Delete(m.Table, where)
		}
		if err != nil {
			return err
		}
		defer db.touched(ctx, m.Table)
		if _, err := db.execStmt(ctx, st); err != nil {
			return err
		}
		return callHook(ctx, v, hookAfterDelete)
	})
}

// DeleteAny permanently deletes every row matched by qq.
func (db *DB) DeleteAny(ctx context.Context, qq q.Query) (int64, error) {
	if qq.WhereCond == nil {
		return 0, errors.New("orm: DeleteAny without a filter; use q.Expr(\"1=1\") to delete everything")
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
