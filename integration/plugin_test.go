package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// scopePlugin is a minimal multi-tenancy plugin, used to test the plugin
// API end to end: it filters every query on a table with a tenant_id
// column, fills tenant_id on insert and refuses updates across tenants.
type scopePlugin struct {
	mu      sync.Mutex
	actions map[string]bool // "update users", ...
	events  int
}

type tenantKey struct{}

var errNoTenant = errors.New("no tenant in context")

func (p *scopePlugin) Name() string { return "test-scope" }

func (p *scopePlugin) BeforeQuery(ctx context.Context, info *orm.QueryInfo) error {
	p.mu.Lock()
	p.actions[info.Action.String()+" "+info.Table] = true
	p.mu.Unlock()
	if info.Model == nil {
		return nil
	}
	if _, ok := info.Model.Column("tenant_id"); !ok {
		return nil
	}
	tid, ok := ctx.Value(tenantKey{}).(int)
	if !ok {
		return errNoTenant
	}
	for _, m := range info.Set {
		if m.Field == "tenant_id" {
			return errors.New("cannot change tenant_id")
		}
	}
	info.Where(q.Eq(info.Table+".tenant_id", tid))
	return nil
}

func (p *scopePlugin) BeforeWrite(ctx context.Context, w *orm.WriteInfo) error {
	if _, ok := w.Model.Column("tenant_id"); !ok {
		return nil
	}
	tid, ok := ctx.Value(tenantKey{}).(int)
	if !ok {
		return errNoTenant
	}
	if w.Action == orm.ActionInsert {
		return w.Set("tenant_id", tid) // int into an int64 field
	}
	if v, _ := w.Get("tenant_id"); v != int64(tid) {
		return fmt.Errorf("record belongs to tenant %v", v)
	}
	return nil
}

func (p *scopePlugin) AfterQuery(ctx context.Context, e orm.Event) {
	p.mu.Lock()
	p.events++
	p.mu.Unlock()
}

func TestPlugins(t *testing.T) {
	var p *scopePlugin
	forEachWith(t, func(t *testing.T) []orm.Option {
		p = &scopePlugin{actions: map[string]bool{}}
		return []orm.Option{orm.Use(p)}
	}, func(t *testing.T, db *orm.DB) {
		users, posts := orm.For[User](db), orm.For[Post](db)
		a := context.WithValue(context.Background(), tenantKey{}, 1)
		b := context.WithValue(context.Background(), tenantKey{}, 2)

		// Inserts get the tenant from the context.
		u := User{Name: "ann", Email: "pl-ann@x.io"}
		must(t, users.Insert(a, &u))
		if u.TenantID != 1 {
			t.Fatalf("Insert: tenant_id = %d, want 1", u.TenantID)
		}
		batch := []User{{Name: "b1", Email: "pl-b1@x.io"}, {Name: "b2", Email: "pl-b2@x.io"}, {Name: "b3", Email: "pl-b3@x.io"}}
		must(t, users.InsertAll(b, batch))
		for _, x := range batch {
			if x.TenantID != 2 {
				t.Fatalf("InsertAll: tenant_id = %d, want 2", x.TenantID)
			}
		}
		must(t, posts.Insert(a, &Post{UserID: u.ID, Title: "hello"})) // posts: no tenant column

		// Reads only see the context's tenant.
		all, err := users.FindAll(a)
		must(t, err)
		if len(all) != 1 || all[0].ID != u.ID {
			t.Fatalf("FindAll(tenant 1) = %v", all)
		}
		if n, err := users.Count(b); err != nil || n != 3 {
			t.Fatalf("Count(tenant 2) = %d, %v", n, err)
		}
		if _, err := users.FindByID(b, u.ID); !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("FindByID across tenants: %v", err)
		}
		if ok, err := users.Exists(b, q.Eq("email", "pl-ann@x.io")); err != nil || ok {
			t.Fatalf("Exists across tenants = %v, %v", ok, err)
		}
		page, err := users.Paginate(b, 1, 2)
		must(t, err)
		if page.Total != 3 || len(page.Items) != 2 {
			t.Fatalf("Paginate = %+v", page)
		}
		withPosts, err := users.FindByID(a, u.ID)
		must(t, err)
		must(t, users.Preload(a, &withPosts, "Posts"))
		if len(withPosts.Posts) != 1 {
			t.Fatalf("Preload: %d posts", len(withPosts.Posts))
		}

		// No tenant: refused before any SQL runs.
		p.mu.Lock()
		before := p.events
		p.mu.Unlock()
		if _, err := users.FindAll(context.Background()); !errors.Is(err, errNoTenant) {
			t.Fatalf("FindAll without tenant: %v", err)
		}
		p.mu.Lock()
		if p.events != before {
			t.Fatalf("a refused query still ran (%d statements)", p.events-before)
		}
		p.mu.Unlock()

		// Writes across tenants are refused or touch nothing.
		if err := users.Update(b, &u); err == nil {
			t.Fatal("Update of another tenant's record succeeded")
		}
		if _, err := users.UpdateAny(b, q.Eq("name", "b1"), q.Set("tenant_id", 1)); err == nil {
			t.Fatal("UpdateAny moving rows to another tenant succeeded")
		}
		n, err := users.UpdateAny(b, q.Like("email", "pl-%"), q.Set("age", 42))
		must(t, err)
		if n != 3 {
			t.Fatalf("UpdateAny(tenant 2) touched %d rows, want 3", n)
		}
		stranger := u
		must(t, users.Delete(b, &stranger)) // soft delete, filtered by tenant 2: no row matches
		if _, err := users.FindByID(a, u.ID); err != nil {
			t.Fatalf("tenant 2 deleted tenant 1's record: %v", err)
		}
		if n, err := users.DeleteAny(b, q.Like("email", "pl-b%")); err != nil || n != 3 {
			t.Fatalf("DeleteAny(tenant 2) = %d, %v", n, err)
		}
		u.Name = "ann 2"
		must(t, users.Update(a, &u, "Name")) // own record: fine

		// Raw SQL is observed but never changed.
		var total int64
		must(t, db.Query(context.Background(), &total, "SELECT COUNT(*) FROM users"))
		if total != 4 {
			t.Fatalf("raw count = %d, want 4", total)
		}

		for _, want := range []string{"select users", "count users", "update users", "delete users", "select posts"} {
			if !p.actions[want] {
				t.Errorf("BeforeQuery never saw %q (saw %v)", want, p.actions)
			}
		}
	})
}
