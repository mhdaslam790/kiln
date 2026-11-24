package integration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/mhdaslam790/kiln/kilntest"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/tenant"
)

func TestTenant(t *testing.T) {
	var cacheHits atomic.Int64
	forEachWith(t, func(t *testing.T) []orm.Option {
		cacheHits.Store(0)
		return []orm.Option{tenant.Plugin("tenant_id"), orm.WithCache(), orm.WithHook(func(_ context.Context, e orm.Event) {
			if e.Cached {
				cacheHits.Add(1)
			}
		})}
	}, func(t *testing.T, db *orm.DB) {
		users, posts := orm.For[User](db), orm.For[Post](db)
		acme := tenant.With(context.Background(), 1) // int; the column is int64
		globex := tenant.With(context.Background(), int64(2))

		// Inserts take the tenant from the context.
		ann := User{Name: "ann", Email: "ann@acme.io"}
		must(t, users.Insert(acme, &ann))
		if ann.TenantID != 1 {
			t.Fatalf("Insert: tenant_id = %d", ann.TenantID)
		}
		staff := []User{{Name: "g1", Email: "g1@globex.io"}, {Name: "g2", Email: "g2@globex.io"}}
		must(t, users.InsertAll(globex, staff))
		if staff[0].TenantID != 2 || staff[1].TenantID != 2 {
			t.Fatalf("InsertAll: %+v", staff)
		}
		must(t, posts.Insert(acme, &Post{UserID: ann.ID, Title: "no tenant column"}))

		// An explicit, matching tenant is fine; a different one is refused.
		must(t, users.Insert(acme, &User{TenantID: 1, Name: "explicit", Email: "x@acme.io"}))
		if err := users.Insert(acme, &User{TenantID: 2, Name: "sneaky", Email: "s@acme.io"}); !errors.Is(err, tenant.ErrCrossTenant) {
			t.Fatalf("insert into another tenant: %v", err)
		}

		// Reads, counts, pages, existence and preloads see one tenant.
		if n, err := users.Count(acme); err != nil || n != 2 {
			t.Fatalf("Count(acme) = %d, %v", n, err)
		}
		if n, err := users.Count(globex); err != nil || n != 2 {
			t.Fatalf("Count(globex) = %d, %v", n, err)
		}
		if _, err := users.FindByID(globex, ann.ID); !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("FindByID across tenants: %v", err)
		}
		if ok, err := users.Exists(globex, q.Eq("email", "ann@acme.io")); err != nil || ok {
			t.Fatalf("Exists across tenants: %v %v", ok, err)
		}
		got, err := users.FindByID(acme, ann.ID)
		must(t, err)
		must(t, users.Preload(acme, &got, "Posts"))
		if len(got.Posts) != 1 {
			t.Fatalf("Preload: %d posts", len(got.Posts))
		}

		// Cached results are kept per tenant (the filter is in the key).
		for i := 0; i < 2; i++ {
			a, err := users.FindAll(orm.Cached(acme, 0))
			must(t, err)
			g, err := users.FindAll(orm.Cached(globex, 0))
			must(t, err)
			if len(a) != 2 || len(g) != 2 || a[0].TenantID != 1 || g[0].TenantID != 2 {
				t.Fatalf("cached reads mixed tenants: %v / %v", a, g)
			}
		}
		if n := cacheHits.Load(); n != 2 {
			t.Fatalf("cache hits = %d, want 2 (second round from the cache)", n)
		}

		// Fails safe without a tenant.
		if _, err := users.FindAll(context.Background()); !errors.Is(err, tenant.ErrNoTenant) {
			t.Fatalf("FindAll without tenant: %v", err)
		}
		if err := users.Insert(context.Background(), &User{Name: "x", Email: "none@x.io"}); !errors.Is(err, tenant.ErrNoTenant) {
			t.Fatalf("Insert without tenant: %v", err)
		}
		if _, err := posts.FindAll(context.Background()); err != nil {
			t.Fatalf("table without the column must not need a tenant: %v", err)
		}

		// Writes cannot cross tenants.
		moved := got
		moved.TenantID = 2
		if err := users.Update(acme, &moved); !errors.Is(err, tenant.ErrCrossTenant) {
			t.Fatalf("Update moving to another tenant: %v", err)
		}
		if moved.TenantID != 2 {
			t.Fatal("refused update changed the record")
		}
		if _, err := users.UpdateAny(acme, q.Eq("name", "ann"), q.Set("tenant_id", 2)); !errors.Is(err, tenant.ErrCrossTenant) {
			t.Fatalf("UpdateAny of tenant_id: %v", err)
		}
		if err := users.Update(globex, &got); !errors.Is(err, tenant.ErrCrossTenant) {
			t.Fatalf("Update of another tenant's record: %v", err)
		}
		got.Name = "ann b"
		must(t, users.Update(acme, &got, "Name")) // column not written: fine
		if n, err := users.UpdateAny(globex, q.Like("email", "%"), q.Set("age", 9)); err != nil || n != 2 {
			t.Fatalf("UpdateAny(globex) = %d, %v", n, err)
		}
		victim := ann
		must(t, users.HardDelete(globex, &victim)) // matches nothing for globex
		if _, err := users.FindByID(acme, ann.ID); err != nil {
			t.Fatalf("globex deleted acme's record: %v", err)
		}

		// Bypass: no filter; inserts must name the tenant.
		admin := tenant.Bypass(context.Background())
		if n, err := users.Count(admin); err != nil || n != 4 {
			t.Fatalf("Count(bypass) = %d, %v", n, err)
		}
		if err := users.Insert(admin, &User{Name: "y", Email: "y@x.io"}); !errors.Is(err, tenant.ErrNoTenant) {
			t.Fatalf("Bypass insert without tenant: %v", err)
		}
		must(t, users.Insert(admin, &User{TenantID: 3, Name: "z", Email: "z@initech.io"}))
		if _, ok := tenant.From(admin); ok {
			t.Fatal("From(bypass) reported a tenant")
		}
		if id, ok := tenant.From(acme); !ok || id != 1 {
			t.Fatalf("From(acme) = %v, %v", id, ok)
		}

		// Raw SQL is never rewritten.
		var total int64
		must(t, db.Query(context.Background(), &total, "SELECT COUNT(*) FROM users"))
		if total != 5 {
			t.Fatalf("raw count = %d, want 5", total)
		}

		// Works together with kilntest: the test transaction carries the tenant.
		t.Run("kilntest", func(t *testing.T) {
			ctx := tenant.With(kilntest.Tx(t, db), 1)
			must(t, users.Insert(ctx, &User{Name: "tmp", Email: "tmp@acme.io"}))
			if n, err := users.Count(ctx); err != nil || n != 3 {
				t.Fatalf("Count in test tx = %d, %v", n, err)
			}
		})
	})
}
