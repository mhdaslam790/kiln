package orm

import (
	"context"
	"fmt"
	"reflect"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/q"
)

// m2mLinks reads the join rows for the owner keys. It returns, per target
// key, the owner keys linked to it, plus the distinct target keys.
func (db *DB) m2mLinks(ctx context.Context, a *meta.Assoc, ownerKeys []any) (map[any][]any, []any, error) {
	qq := q.From(a.JoinTable).
		Select(a.JoinOwnerCol, a.JoinTargetCol).
		Where(q.In(a.JoinOwnerCol, ownerKeys...))
	st, err := db.dialect.Select(qq)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := db.stmtCtx(ctx)
	defer cancel()
	rows, err := db.queryRead(ctx, st, &qq)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	links := map[any][]any{}
	var targets []any
	for rows.Next() {
		var o, t any
		if err := rows.Scan(&o, &t); err != nil {
			return nil, nil, err
		}
		ok1, okO := normKey(reflect.ValueOf(o))
		tk, okT := normKey(reflect.ValueOf(t))
		if !okO || !okT {
			continue
		}
		if _, seen := links[tk]; !seen {
			targets = append(targets, tk)
		}
		links[tk] = append(links[tk], ok1)
	}
	return links, targets, rows.Err()
}

// m2mAssoc resolves owner (*Struct) and its many-to-many association.
func m2mAssoc(owner any, name string) (reflect.Value, *meta.Model, *meta.Assoc, *meta.Model, error) {
	v, m, err := structPtr(owner)
	if err != nil {
		return v, nil, nil, nil, err
	}
	a, err := m.Assoc(name)
	if err != nil {
		return v, nil, nil, nil, err
	}
	if a.Kind != meta.ManyToMany {
		return v, nil, nil, nil, fmt.Errorf("orm: %s.%s is %s, not many_to_many", m.Type.Name(), a.Name, a.Kind)
	}
	tm, err := meta.Of(a.Elem)
	return v, m, a, tm, err
}

// keyOf returns the non-zero key value of field col on struct v.
func keyOf(m *meta.Model, v reflect.Value, col, what string) (any, error) {
	fv := v.FieldByIndex(m.Field(col).Index)
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil, fmt.Errorf("orm: %s has no %s yet (insert it first)", m.Type.Name(), what)
		}
		fv = fv.Elem()
	}
	if fv.IsZero() {
		return nil, fmt.Errorf("orm: %s has no %s yet (insert it first)", m.Type.Name(), what)
	}
	return fv.Interface(), nil
}

// targetKeys extracts the key of each target (struct, *struct, or a bare key value).
func targetKeys(tm *meta.Model, col string, targets []any) ([]any, error) {
	out := make([]any, 0, len(targets))
	for _, t := range targets {
		rv := reflect.ValueOf(t)
		if rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Type() == tm.Type {
			rv = rv.Elem()
		}
		if rv.Kind() == reflect.Struct && rv.Type() == tm.Type {
			k, err := keyOf(tm, rv, col, "primary key")
			if err != nil {
				return nil, err
			}
			out = append(out, k)
			continue
		}
		out = append(out, t) // a key value
	}
	return out, nil
}

// Link adds rows to a many-to-many join table, linking owner (*Struct) to
// targets (structs, pointers to structs, or key values). Existing links
// are left as they are, so Link is idempotent.
//
//	db.Link(ctx, &user, "Roles", &admin, &editor)
func (db *DB) Link(ctx context.Context, owner any, assoc string, targets ...any) error {
	v, m, a, tm, err := m2mAssoc(owner, assoc)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	ok, err := keyOf(m, v, a.OwnerCol, a.OwnerCol)
	if err != nil {
		return err
	}
	keys, err := targetKeys(tm, a.TargetCol, targets)
	if err != nil {
		return err
	}
	return db.link(ctx, a, ok, keys)
}

func (db *DB) link(ctx context.Context, a *meta.Assoc, ownerKey any, keys []any) error {
	// Skip pairs that already exist (portable alternative to ON CONFLICT).
	st, err := db.dialect.Select(q.From(a.JoinTable).Select(a.JoinTargetCol).
		Where(q.Eq(a.JoinOwnerCol, ownerKey), q.In(a.JoinTargetCol, keys...)))
	if err != nil {
		return err
	}
	qctx, cancel := db.stmtCtx(ctx)
	defer cancel()
	rows, err := db.queryStmt(qctx, st)
	if err != nil {
		return err
	}
	have := map[any]bool{}
	for rows.Next() {
		var t any
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		if k, ok := normKey(reflect.ValueOf(t)); ok {
			have[k] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var insert [][]any
	for _, k := range keys {
		nk, ok := normKey(reflect.ValueOf(k))
		if !ok || have[nk] {
			continue
		}
		have[nk] = true
		insert = append(insert, []any{ownerKey, k})
	}
	if len(insert) == 0 {
		return nil
	}
	ins, err := db.dialect.Insert(dialect.Insert{Table: a.JoinTable, Columns: []string{a.JoinOwnerCol, a.JoinTargetCol}, Rows: insert})
	if err != nil {
		return err
	}
	defer db.touched(ctx, a.JoinTable)
	_, err = db.execStmt(ctx, ins)
	return err
}

// Unlink removes many-to-many links between owner and targets, or all of
// the owner's links for the association when no targets are given. The
// target records themselves are not deleted.
func (db *DB) Unlink(ctx context.Context, owner any, assoc string, targets ...any) error {
	v, m, a, tm, err := m2mAssoc(owner, assoc)
	if err != nil {
		return err
	}
	ok, err := keyOf(m, v, a.OwnerCol, a.OwnerCol)
	if err != nil {
		return err
	}
	where := q.Eq(a.JoinOwnerCol, ok)
	if len(targets) > 0 {
		keys, err := targetKeys(tm, a.TargetCol, targets)
		if err != nil {
			return err
		}
		where = q.And(where, q.In(a.JoinTargetCol, keys...))
	}
	st, err := db.dialect.Delete(a.JoinTable, where)
	if err != nil {
		return err
	}
	defer db.touched(ctx, a.JoinTable)
	_, err = db.execStmt(ctx, st)
	return err
}
