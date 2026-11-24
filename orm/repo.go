package orm

import (
	"context"
	"fmt"
	"reflect"

	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/q"
)

// Repo is a typed repository for model T.
//
//	users := orm.For[User](db)
//	u, err := users.FindByID(ctx, 1)
//	active, err := users.FindAll(ctx, q.Eq("active", true), q.Sort("-id"))
type Repo[T any] struct {
	db     *DB
	m      *meta.Model
	scopes []q.Querier
}

// For returns a repository for T. It panics if T is not a valid model,
// so call it at startup.
func For[T any](db *DB) *Repo[T] {
	var zero T
	return &Repo[T]{db: db, m: meta.MustOf(reflect.TypeOf(zero))}
}

// Table returns the model's table name.
func (r *Repo[T]) Table() string { return r.m.Table }

// DB returns the underlying DB.
func (r *Repo[T]) DB() *DB { return r.db }

// Scope returns a copy of the repository that applies qs to every read and
// bulk write (FindAll, Count, UpdateAny, DeleteAny, ...). Great for
// multi-tenancy: orm.For[Order](db).Scope(q.Eq("tenant_id", tid)).
func (r *Repo[T]) Scope(qs ...q.Querier) *Repo[T] {
	c := *r
	c.scopes = append(append([]q.Querier(nil), r.scopes...), qs...)
	return &c
}

func (r *Repo[T]) query(qs []q.Querier) q.Query {
	return scoped(r.m, append(append([]q.Querier(nil), r.scopes...), qs...))
}

// Query returns the scoped base query for T, for use with DB.Select or as
// a sub-query.
func (r *Repo[T]) Query(qs ...q.Querier) q.Query { return r.query(qs) }

// Find returns the first matching record or ErrNotFound.
func (r *Repo[T]) Find(ctx context.Context, qs ...q.Querier) (T, error) {
	var out T
	qq := r.query(qs)
	qq.LimitN = 1
	items, err := r.db.selectModels(ctx, r.m, qq)
	if err != nil {
		return out, err
	}
	if items.Len() == 0 {
		return out, ErrNotFound
	}
	return items.Index(0).Interface().(T), nil
}

// FindByID finds by a single-column primary key.
func (r *Repo[T]) FindByID(ctx context.Context, id any, qs ...q.Querier) (T, error) {
	if r.m.PK == nil {
		var zero T
		return zero, pkMissing(r.m)
	}
	return r.Find(ctx, append([]q.Querier{q.Eq(r.m.Table+"."+r.m.PK.Column, id)}, qs...)...)
}

// FindByPK finds by a (possibly composite) primary key, values in
// declaration order: tags.FindByPK(ctx, postID, tagID).
func (r *Repo[T]) FindByPK(ctx context.Context, keys ...any) (T, error) {
	var zero T
	if len(keys) != len(r.m.PKs) || len(keys) == 0 {
		return zero, fmt.Errorf("orm: %s has %d primary key column(s), got %d value(s)", r.m.Type.Name(), len(r.m.PKs), len(keys))
	}
	fs := make([]q.Querier, len(keys))
	for i, f := range r.m.PKs {
		fs[i] = q.Eq(r.m.Table+"."+f.Column, keys[i])
	}
	return r.Find(ctx, fs...)
}

// FindAll returns all matching records (never nil on success).
func (r *Repo[T]) FindAll(ctx context.Context, qs ...q.Querier) ([]T, error) {
	items, err := r.db.selectModels(ctx, r.m, r.query(qs))
	if err != nil {
		return nil, err
	}
	return items.Interface().([]T), nil
}

// Count returns the number of matching records.
func (r *Repo[T]) Count(ctx context.Context, qs ...q.Querier) (int64, error) {
	return r.db.count(ctx, r.query(qs))
}

// Exists reports whether any record matches.
func (r *Repo[T]) Exists(ctx context.Context, qs ...q.Querier) (bool, error) {
	qq := r.query(qs)
	qq.Fields, qq.LimitN = []string{"1"}, 1
	var ones []int
	if err := r.db.Select(ctx, &ones, qq); err != nil {
		return false, err
	}
	return len(ones) > 0, nil
}

// Page is one page of results plus the total count.
type Page[T any] struct {
	Items      []T   `json:"items"`
	Total      int64 `json:"total"`
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	TotalPages int   `json:"total_pages"`
}

// Paginate returns page (1-based) of size perPage plus the total count.
func (r *Repo[T]) Paginate(ctx context.Context, page, perPage int, qs ...q.Querier) (Page[T], error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	total, err := r.Count(ctx, qs...)
	if err != nil {
		return Page[T]{}, err
	}
	items, err := r.FindAll(ctx, append(qs, q.Page(page, perPage))...)
	if err != nil {
		return Page[T]{}, err
	}
	return Page[T]{
		Items: items, Total: total, Page: page, PerPage: perPage,
		TotalPages: int((total + int64(perPage) - 1) / int64(perPage)),
	}, nil
}

// Each streams matching records in batches of size (ordered by primary
// key), so arbitrarily large tables can be processed with bounded memory.
// Returning an error from fn stops the iteration.
func (r *Repo[T]) Each(ctx context.Context, size int, fn func(T) error, qs ...q.Querier) error {
	if r.m.PK == nil {
		return pkMissing(r.m)
	}
	if size < 1 {
		size = 500
	}
	pk := r.m.Table + "." + r.m.PK.Column
	var last any
	for {
		page := append([]q.Querier(nil), qs...)
		if last != nil {
			page = append(page, q.Gt(pk, last))
		}
		page = append(page, q.Sort(pk), q.Limit(size))
		items, err := r.FindAll(ctx, page...)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := fn(it); err != nil {
				return err
			}
		}
		if len(items) < size {
			return nil
		}
		last = reflect.ValueOf(items[len(items)-1]).FieldByIndex(r.m.PK.Index).Interface()
	}
}

// Insert inserts rec and fills its primary key and timestamps.
func (r *Repo[T]) Insert(ctx context.Context, rec *T, opts ...InsertOption) error {
	return r.db.Insert(ctx, rec, opts...)
}

// InsertAll inserts recs in bulk, filling primary keys in place.
func (r *Repo[T]) InsertAll(ctx context.Context, recs []T, opts ...InsertOption) error {
	return r.db.InsertAll(ctx, recs, opts...)
}

// Update writes rec by primary key (all fields, or only the named ones).
func (r *Repo[T]) Update(ctx context.Context, rec *T, fields ...string) error {
	return r.db.Update(ctx, rec, fields...)
}

// Save inserts rec when it is new and updates it otherwise. A record is new
// when its auto-increment key is zero; for other keys, existence is checked.
func (r *Repo[T]) Save(ctx context.Context, rec *T) error {
	v := reflect.ValueOf(rec).Elem()
	if r.m.PK != nil && r.m.PK.Auto {
		if v.FieldByIndex(r.m.PK.Index).IsZero() {
			return r.Insert(ctx, rec)
		}
		return r.Update(ctx, rec)
	}
	where, err := pkFilter(r.m, v)
	if err != nil {
		return err
	}
	exists, err := r.db.count(ctx, q.Apply(q.From(r.m.Table), where, q.Unscoped()))
	if err != nil {
		return err
	}
	if exists == 0 {
		return r.Insert(ctx, rec)
	}
	return r.Update(ctx, rec)
}

// UpdateChanged writes only the fields changed since cs := orm.Track(rec).
func (r *Repo[T]) UpdateChanged(ctx context.Context, rec *T, cs *Changeset) error {
	if cs == nil || cs.rec.Interface() != any(rec) {
		return fmt.Errorf("orm: changeset does not track this record")
	}
	return r.db.UpdateChanged(ctx, cs)
}

// Delete deletes rec (soft delete when the model has DeletedAt).
func (r *Repo[T]) Delete(ctx context.Context, rec *T) error { return r.db.Delete(ctx, rec) }

// Link adds many-to-many links from rec to targets (see DB.Link).
func (r *Repo[T]) Link(ctx context.Context, rec *T, assoc string, targets ...any) error {
	return r.db.Link(ctx, rec, assoc, targets...)
}

// Unlink removes many-to-many links from rec (see DB.Unlink).
func (r *Repo[T]) Unlink(ctx context.Context, rec *T, assoc string, targets ...any) error {
	return r.db.Unlink(ctx, rec, assoc, targets...)
}

// HardDelete permanently deletes rec.
func (r *Repo[T]) HardDelete(ctx context.Context, rec *T) error { return r.db.HardDelete(ctx, rec) }

// Restore clears deleted_at on a soft-deleted record.
func (r *Repo[T]) Restore(ctx context.Context, rec *T) error {
	if r.m.DeletedAt == nil {
		return nil
	}
	v := reflect.ValueOf(rec).Elem()
	where, err := pkFilter(r.m, v)
	if err != nil {
		return err
	}
	qq := q.Query{Table: r.m.Table, WhereCond: where}
	qq, set, err := r.db.beforeQuery(ctx, ActionUpdate, r.m, qq, []q.Mutation{q.Set(r.m.DeletedAt.Column, nil)})
	if err != nil {
		return err
	}
	st, err := r.db.dialect.Update(r.m.Table, set, qq.WhereCond)
	if err != nil {
		return err
	}
	defer r.db.touched(ctx, r.m.Table)
	if _, err := r.db.execStmt(ctx, st); err != nil {
		return err
	}
	v.FieldByIndex(r.m.DeletedAt.Index).SetZero()
	return nil
}

// UpdateAny applies mutations to all matching (non-deleted) records and
// refreshes updated_at when the model has one.
func (r *Repo[T]) UpdateAny(ctx context.Context, where q.Filter, muts ...q.Mutation) (int64, error) {
	if r.m.UpdatedAt != nil {
		has := false
		for _, m := range muts {
			if m.Field == r.m.UpdatedAt.Column {
				has = true
			}
		}
		if !has {
			muts = append(muts, q.Set(r.m.UpdatedAt.Column, r.db.now()))
		}
	}
	return r.db.UpdateAny(ctx, r.query([]q.Querier{where}), muts...)
}

// DeleteAny deletes all matching records; soft-delete models are marked
// deleted instead. Returns the number of affected rows.
func (r *Repo[T]) DeleteAny(ctx context.Context, where q.Filter) (int64, error) {
	qq := r.query([]q.Querier{where})
	if r.m.DeletedAt != nil && !qq.IsUnscoped {
		return r.db.updateAny(ctx, ActionDelete, qq, []q.Mutation{q.Set(r.m.DeletedAt.Column, r.db.now())})
	}
	return r.db.DeleteAny(ctx, qq)
}

// Preload loads associations on one record. Paths may be nested ("Posts.Comments").
func (r *Repo[T]) Preload(ctx context.Context, rec *T, paths ...string) error {
	for _, p := range paths {
		if err := r.db.Preload(ctx, rec, p); err != nil {
			return err
		}
	}
	return nil
}

// PreloadAll loads associations on many records in one query per level.
func (r *Repo[T]) PreloadAll(ctx context.Context, recs []T, paths ...string) error {
	for _, p := range paths {
		if err := r.db.Preload(ctx, recs, p); err != nil {
			return err
		}
	}
	return nil
}

func pkMissing(m *meta.Model) error {
	if len(m.PKs) > 1 {
		return &modelError{m.Type.Name() + " has a composite primary key; use FindByPK"}
	}
	return &modelError{m.Type.Name() + " has no primary key"}
}

// Repository is the interface implemented by *Repo[T]. Depend on it in
// your services so they can be tested with package ormtest.
type Repository[T any] interface {
	Find(ctx context.Context, qs ...q.Querier) (T, error)
	FindByID(ctx context.Context, id any, qs ...q.Querier) (T, error)
	FindByPK(ctx context.Context, keys ...any) (T, error)
	FindAll(ctx context.Context, qs ...q.Querier) ([]T, error)
	Count(ctx context.Context, qs ...q.Querier) (int64, error)
	Exists(ctx context.Context, qs ...q.Querier) (bool, error)
	Paginate(ctx context.Context, page, perPage int, qs ...q.Querier) (Page[T], error)
	Cursor(ctx context.Context, o CursorOpts, qs ...q.Querier) (CursorPage[T], error)
	Each(ctx context.Context, size int, fn func(T) error, qs ...q.Querier) error
	Insert(ctx context.Context, rec *T, opts ...InsertOption) error
	InsertAll(ctx context.Context, recs []T, opts ...InsertOption) error
	Update(ctx context.Context, rec *T, fields ...string) error
	UpdateChanged(ctx context.Context, rec *T, cs *Changeset) error
	Save(ctx context.Context, rec *T) error
	Delete(ctx context.Context, rec *T) error
	HardDelete(ctx context.Context, rec *T) error
	Link(ctx context.Context, rec *T, assoc string, targets ...any) error
	Unlink(ctx context.Context, rec *T, assoc string, targets ...any) error
	Restore(ctx context.Context, rec *T) error
	UpdateAny(ctx context.Context, where q.Filter, muts ...q.Mutation) (int64, error)
	DeleteAny(ctx context.Context, where q.Filter) (int64, error)
	Preload(ctx context.Context, rec *T, paths ...string) error
	PreloadAll(ctx context.Context, recs []T, paths ...string) error
}

// Transactor runs functions in a transaction; *DB implements it.
type Transactor interface {
	Transaction(ctx context.Context, fn func(ctx context.Context) error) error
}

var (
	_ Repository[struct{ ID int64 }] = (*Repo[struct{ ID int64 }])(nil)
	_ Transactor                     = (*DB)(nil)
)

type modelError struct{ msg string }

func (e *modelError) Error() string { return "orm: " + e.msg }
