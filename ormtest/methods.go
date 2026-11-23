package ormtest

import (
	"context"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// ---- expectations -------------------------------------------------------------

func (r *Repo[T]) ExpectFind(qs ...q.Querier) *Expectation[T] {
	return r.expect("Find", nonNil(qs), nil)
}
func (r *Repo[T]) ExpectFindByID(id any, qs ...q.Querier) *Expectation[T] {
	return r.expect("FindByID", nonNil(qs), []any{id})
}
func (r *Repo[T]) ExpectFindByPK(keys ...any) *Expectation[T] {
	return r.expect("FindByPK", nil, keys)
}
func (r *Repo[T]) ExpectFindAll(qs ...q.Querier) *Expectation[T] {
	return r.expect("FindAll", nonNil(qs), nil)
}
func (r *Repo[T]) ExpectCount(qs ...q.Querier) *Expectation[T] {
	return r.expect("Count", nonNil(qs), nil)
}
func (r *Repo[T]) ExpectExists(qs ...q.Querier) *Expectation[T] {
	return r.expect("Exists", nonNil(qs), nil)
}
func (r *Repo[T]) ExpectPaginate(page, perPage int, qs ...q.Querier) *Expectation[T] {
	return r.expect("Paginate", nonNil(qs), []any{page, perPage})
}
func (r *Repo[T]) ExpectCursor(o orm.CursorOpts, qs ...q.Querier) *Expectation[T] {
	return r.expect("Cursor", nonNil(qs), []any{o})
}

// ExpectEach expects an Each call; Return([]T) supplies the streamed items.
func (r *Repo[T]) ExpectEach(qs ...q.Querier) *Expectation[T] {
	return r.expect("Each", nonNil(qs), nil)
}

// ExpectInsert expects an Insert of any record (narrow it with Match).
func (r *Repo[T]) ExpectInsert() *Expectation[T]    { return r.expect("Insert", nil, nil) }
func (r *Repo[T]) ExpectInsertAll() *Expectation[T] { return r.expect("InsertAll", nil, nil) }

// ExpectUpdate expects an Update; with fields, they must match exactly.
func (r *Repo[T]) ExpectUpdate(fields ...string) *Expectation[T] {
	var args []any
	if len(fields) > 0 {
		args = []any{fields}
	}
	return r.expect("Update", nil, args)
}
func (r *Repo[T]) ExpectUpdateChanged() *Expectation[T] { return r.expect("UpdateChanged", nil, nil) }
func (r *Repo[T]) ExpectSave() *Expectation[T]          { return r.expect("Save", nil, nil) }
func (r *Repo[T]) ExpectDelete() *Expectation[T]        { return r.expect("Delete", nil, nil) }
func (r *Repo[T]) ExpectHardDelete() *Expectation[T]    { return r.expect("HardDelete", nil, nil) }
func (r *Repo[T]) ExpectRestore() *Expectation[T]       { return r.expect("Restore", nil, nil) }

// ExpectLink / ExpectUnlink match the association name (targets are not compared).
func (r *Repo[T]) ExpectLink(assoc string) *Expectation[T] {
	return r.expect("Link", nil, []any{assoc})
}
func (r *Repo[T]) ExpectUnlink(assoc string) *Expectation[T] {
	return r.expect("Unlink", nil, []any{assoc})
}

// ExpectUpdateAny matches the filter and mutations; Return(n) sets the affected rows.
func (r *Repo[T]) ExpectUpdateAny(where q.Filter, muts ...q.Mutation) *Expectation[T] {
	return r.expect("UpdateAny", []q.Querier{where}, []any{muts})
}
func (r *Repo[T]) ExpectDeleteAny(where q.Filter) *Expectation[T] {
	return r.expect("DeleteAny", []q.Querier{where}, nil)
}
func (r *Repo[T]) ExpectPreload(paths ...string) *Expectation[T] {
	return r.expect("Preload", nil, []any{paths})
}
func (r *Repo[T]) ExpectPreloadAll(paths ...string) *Expectation[T] {
	return r.expect("PreloadAll", nil, []any{paths})
}

// nonNil turns "no queriers" into an explicit empty list, so ExpectFind()
// matches only a call without queriers. Use ormtest.Any for any query.
func nonNil(qs []q.Querier) []q.Querier {
	if qs == nil {
		return []q.Querier{}
	}
	return qs
}

// ---- orm.Repository[T] --------------------------------------------------------------

func (r *Repo[T]) Find(ctx context.Context, qs ...q.Querier) (T, error) {
	r.t.Helper()
	e, err := r.call("Find", qs, true, nil, nil)
	return result[T, T](r.t, e), err
}

func (r *Repo[T]) FindByID(ctx context.Context, id any, qs ...q.Querier) (T, error) {
	r.t.Helper()
	e, err := r.call("FindByID", qs, true, []any{id}, nil)
	return result[T, T](r.t, e), err
}

func (r *Repo[T]) FindByPK(ctx context.Context, keys ...any) (T, error) {
	r.t.Helper()
	e, err := r.call("FindByPK", nil, false, keys, nil)
	return result[T, T](r.t, e), err
}

func (r *Repo[T]) FindAll(ctx context.Context, qs ...q.Querier) ([]T, error) {
	r.t.Helper()
	e, err := r.call("FindAll", qs, true, nil, nil)
	out := result[T, []T](r.t, e)
	if out == nil && err == nil {
		out = []T{}
	}
	return out, err
}

func (r *Repo[T]) Count(ctx context.Context, qs ...q.Querier) (int64, error) {
	r.t.Helper()
	e, err := r.call("Count", qs, true, nil, nil)
	return result[T, int64](r.t, e), err
}

func (r *Repo[T]) Exists(ctx context.Context, qs ...q.Querier) (bool, error) {
	r.t.Helper()
	e, err := r.call("Exists", qs, true, nil, nil)
	return result[T, bool](r.t, e), err
}

func (r *Repo[T]) Paginate(ctx context.Context, page, perPage int, qs ...q.Querier) (orm.Page[T], error) {
	r.t.Helper()
	e, err := r.call("Paginate", qs, true, []any{page, perPage}, nil)
	return result[T, orm.Page[T]](r.t, e), err
}

func (r *Repo[T]) Cursor(ctx context.Context, o orm.CursorOpts, qs ...q.Querier) (orm.CursorPage[T], error) {
	r.t.Helper()
	e, err := r.call("Cursor", qs, true, []any{o}, nil)
	return result[T, orm.CursorPage[T]](r.t, e), err
}

func (r *Repo[T]) Each(ctx context.Context, size int, fn func(T) error, qs ...q.Querier) error {
	r.t.Helper()
	e, err := r.call("Each", qs, true, nil, nil)
	if err != nil {
		return err
	}
	for _, it := range result[T, []T](r.t, e) {
		if err := fn(it); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo[T]) write(method string, rec *T, args []any) error {
	r.t.Helper()
	_, err := r.call(method, nil, false, args, rec)
	return err
}

func (r *Repo[T]) Insert(ctx context.Context, rec *T, opts ...orm.InsertOption) error {
	r.t.Helper()
	return r.write("Insert", rec, nil)
}

func (r *Repo[T]) InsertAll(ctx context.Context, recs []T, opts ...orm.InsertOption) error {
	r.t.Helper()
	e, err := r.call("InsertAll", nil, false, nil, nil)
	if e != nil && e.runAll != nil {
		e.runAll(recs)
	}
	return err
}

func (r *Repo[T]) Update(ctx context.Context, rec *T, fields ...string) error {
	r.t.Helper()
	var args []any
	if len(fields) > 0 {
		args = []any{fields}
	}
	_, err := r.call("Update", nil, false, args, rec)
	return err
}

func (r *Repo[T]) UpdateChanged(ctx context.Context, rec *T, cs *orm.Changeset) error {
	r.t.Helper()
	return r.write("UpdateChanged", rec, nil)
}
func (r *Repo[T]) Save(ctx context.Context, rec *T) error {
	r.t.Helper()
	return r.write("Save", rec, nil)
}
func (r *Repo[T]) Delete(ctx context.Context, rec *T) error {
	r.t.Helper()
	return r.write("Delete", rec, nil)
}
func (r *Repo[T]) HardDelete(ctx context.Context, rec *T) error {
	r.t.Helper()
	return r.write("HardDelete", rec, nil)
}
func (r *Repo[T]) Link(ctx context.Context, rec *T, assoc string, targets ...any) error {
	r.t.Helper()
	return r.write("Link", rec, []any{assoc})
}
func (r *Repo[T]) Unlink(ctx context.Context, rec *T, assoc string, targets ...any) error {
	r.t.Helper()
	return r.write("Unlink", rec, []any{assoc})
}
func (r *Repo[T]) Restore(ctx context.Context, rec *T) error {
	r.t.Helper()
	return r.write("Restore", rec, nil)
}

func (r *Repo[T]) UpdateAny(ctx context.Context, where q.Filter, muts ...q.Mutation) (int64, error) {
	r.t.Helper()
	e, err := r.call("UpdateAny", []q.Querier{where}, true, []any{muts}, nil)
	return result[T, int64](r.t, e), err
}

func (r *Repo[T]) DeleteAny(ctx context.Context, where q.Filter) (int64, error) {
	r.t.Helper()
	e, err := r.call("DeleteAny", []q.Querier{where}, true, nil, nil)
	return result[T, int64](r.t, e), err
}

func (r *Repo[T]) Preload(ctx context.Context, rec *T, paths ...string) error {
	r.t.Helper()
	return r.write("Preload", rec, []any{paths})
}

func (r *Repo[T]) PreloadAll(ctx context.Context, recs []T, paths ...string) error {
	r.t.Helper()
	e, err := r.call("PreloadAll", nil, false, []any{paths}, nil)
	if e != nil && e.runAll != nil {
		e.runAll(recs)
	}
	return err
}

// Tx is a fake orm.Transactor that simply runs the function.
type Tx struct {
	// Err, if set, is returned instead of running the function.
	Err error
}

func (t Tx) Transaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if t.Err != nil {
		return t.Err
	}
	return fn(ctx)
}

var _ orm.Transactor = Tx{}
