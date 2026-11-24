// Package tenant keeps each customer's rows separate in a shared database
// (multi-tenancy by a tenant column).
//
// Turn it on with one line, and put the tenant in the request's context,
// usually in the middleware that authenticates the request:
//
//	db := orm.Open(sqlDB, dialect.Postgres(), tenant.Plugin("tenant_id"))
//
//	ctx = tenant.With(ctx, "acme")
//	users.FindAll(ctx)                  // ... WHERE users.tenant_id = 'acme'
//	users.Insert(ctx, &u)               // sets u.TenantID = "acme"
//	users.FindAll(context.Background()) // tenant.ErrNoTenant
//
// It applies to every model with the tenant column; other tables are not
// touched. It fails safe: without a tenant in the context, queries on
// those models return ErrNoTenant instead of running unfiltered. Use
// Bypass for admin tools and background jobs that work across tenants.
//
// Not filtered: raw SQL (db.Query, db.Exec, q.SQL), joined tables (only
// the queried table is filtered) and many-to-many join tables. The added
// condition is part of the SQL, so it shows up in logs and query plans.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

var (
	// ErrNoTenant is returned for a query or write on a tenant table when
	// the context has no tenant and is not a Bypass context.
	ErrNoTenant = errors.New("tenant: no tenant in context")
	// ErrCrossTenant is returned when a write would put a record in, or
	// move it to, a tenant other than the context's.
	ErrCrossTenant = errors.New("tenant: record belongs to another tenant")
)

type ctxKey struct{}

// bypass is stored under ctxKey by Bypass.
type bypass struct{}

// With returns a context for the given tenant id. The id may be any value
// the tenant column accepts (string, integer, UUID type, ...).
func With(ctx context.Context, id any) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the tenant id carried by ctx.
func From(ctx context.Context) (id any, ok bool) {
	v := ctx.Value(ctxKey{})
	if v == nil {
		return nil, false
	}
	if _, isBypass := v.(bypass); isBypass {
		return nil, false
	}
	return v, true
}

// Bypass returns a context in which tenant filtering is off, for admin
// tools and background jobs that work across tenants. Inserts made with it
// must set the tenant column themselves.
func Bypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, bypass{})
}

// Plugin returns the option that enables tenant filtering on every model
// with the given column.
func Plugin(column string) orm.Option {
	return orm.Use(&plugin{column: column})
}

type plugin struct {
	column    string
	qualified sync.Map // table -> "table.column"; bounded by the number of tables
}

func (p *plugin) Name() string { return "tenant" }

// lookup returns the context's tenant: (id, false, nil), (nil, true, nil)
// for a Bypass context, or ErrNoTenant.
func lookup(ctx context.Context) (id any, bypassed bool, err error) {
	switch v := ctx.Value(ctxKey{}).(type) {
	case nil:
		return nil, false, ErrNoTenant
	case bypass:
		return nil, true, nil
	default:
		return v, false, nil
	}
}

func (p *plugin) applies(m *orm.ModelInfo) bool {
	if m == nil {
		return false
	}
	_, ok := m.Column(p.column)
	return ok
}

func (p *plugin) qualify(table string) string {
	if v, ok := p.qualified.Load(table); ok {
		return v.(string)
	}
	v, _ := p.qualified.LoadOrStore(table, table+"."+p.column)
	return v.(string)
}

func (p *plugin) BeforeQuery(ctx context.Context, info *orm.QueryInfo) error {
	if !p.applies(info.Model) {
		return nil
	}
	id, bypassed, err := lookup(ctx)
	if bypassed {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w (%s %s)", err, info.Action, info.Table)
	}
	for _, m := range info.Set {
		if m.Field == p.column || m.Field == p.qualify(info.Table) {
			return fmt.Errorf("%w: %s cannot be changed by an update", ErrCrossTenant, p.column)
		}
	}
	info.Where(q.Eq(p.qualify(info.Table), id))
	return nil
}

func (p *plugin) BeforeWrite(ctx context.Context, w *orm.WriteInfo) error {
	if !p.applies(w.Model) {
		return nil
	}
	if w.Action == orm.ActionUpdate && w.Fields != nil && !contains(w.Fields, p.column) {
		return nil // the column is not written
	}
	cur, _ := w.Get(p.column)
	id, bypassed, err := lookup(ctx)
	if bypassed {
		if w.Action == orm.ActionInsert && isZero(cur) {
			return fmt.Errorf("%w: insert into %s under Bypass must set %s", ErrNoTenant, w.Table, p.column)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w (%s %s)", err, w.Action, w.Table)
	}
	if isZero(cur) {
		return w.Set(p.column, id) // fill in (insert) or keep it (full update)
	}
	// The record names a tenant: it must be the context's. Compare after
	// converting id to the field's type (an int context id vs an int64 field).
	if err := w.Set(p.column, id); err != nil {
		return err
	}
	if now, _ := w.Get(p.column); !equal(cur, now) {
		_ = w.Set(p.column, cur) // leave the record as it was
		return fmt.Errorf("%w: %s %v, context %v", ErrCrossTenant, p.column, cur, id)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func isZero(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		return rv.IsNil() || rv.Elem().IsZero()
	}
	return rv.IsZero()
}

// equal compares two values of the tenant field, looking through pointers.
func equal(a, b any) bool {
	a, b = deref(a), deref(b)
	if a == nil || b == nil {
		return a == b
	}
	if reflect.TypeOf(a).Comparable() && reflect.TypeOf(b).Comparable() {
		return a == b
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
