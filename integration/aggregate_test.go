package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/tenant"
)

var uc = struct {
	Age       q.Col[int]
	Name      q.Col[string]
	Bio       q.Col[string]
	TenantID  q.Col[int64]
	CreatedAt q.Col[time.Time]
	Email     q.Col[string]
}{"age", "name", "bio", "tenant_id", "created_at", "email"}

func TestAggregates(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{tenant.Plugin("tenant_id")}
	}, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		acme, globex := tenant.With(ctx, int64(1)), tenant.With(ctx, int64(2))
		users := orm.For[User](db)

		// Nothing yet: zero sums, ErrNotFound for min/max, empty maps.
		if s, err := orm.Sum(acme, users, uc.Age); err != nil || s != 0 {
			t.Fatalf("empty Sum = %v, %v", s, err)
		}
		if _, err := orm.Max(acme, users, uc.Age); !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("empty Max: %v", err)
		}
		if m, err := orm.CountBy(acme, users, uc.Name); err != nil || len(m) != 0 {
			t.Fatalf("empty CountBy = %v, %v", m, err)
		}

		bio := "x"
		for i, u := range []User{
			{Name: "ann", Age: 30, Bio: &bio}, {Name: "bob", Age: 25}, {Name: "ann", Age: 40, Bio: &bio}, {Name: "cy", Age: 2},
		} {
			u.Email = fmt.Sprintf("agg%d@x.io", i)
			must(t, users.Insert(acme, &u))
		}
		must(t, users.Insert(globex, &User{Name: "ann", Age: 1000, Email: "agg-g@x.io"}))
		gone := User{Name: "dee", Age: 500, Email: "agg-d@x.io"}
		must(t, users.Insert(acme, &gone))
		must(t, users.Delete(acme, &gone)) // soft deleted: not counted

		if s, err := orm.Sum(acme, users, uc.Age); err != nil || s != 97 {
			t.Errorf("Sum = %v, %v (want 97: own tenant, no soft-deleted rows)", s, err)
		}
		if s, err := orm.Sum(acme, users, uc.Age, q.Eq("name", "ann")); err != nil || s != 70 {
			t.Errorf("filtered Sum = %v, %v", s, err)
		}
		if a, err := orm.Avg(acme, users, uc.Age); err != nil || a != 24.25 {
			t.Errorf("Avg = %v, %v (integers must not round)", a, err)
		}
		if v, err := orm.Min(acme, users, uc.Age); err != nil || v != 2 {
			t.Errorf("Min = %v, %v", v, err)
		}
		if v, err := orm.Max(acme, users, uc.Name); err != nil || v != "cy" {
			t.Errorf("Max(name) = %v, %v", v, err)
		}
		first, err := orm.Min(acme, users, uc.CreatedAt)
		if err != nil || !first.Equal(clock) {
			t.Errorf("Min(created_at) = %v, %v (want %v)", first, err, clock)
		}
		// Raw aggregates over time columns scan into time.Time on every database.
		var last time.Time
		var lastPtr *time.Time
		must(t, db.Query(acme, &last, "SELECT MAX(created_at) FROM users"))
		must(t, db.Query(acme, &lastPtr, "SELECT MAX(created_at) FROM users"))
		if !last.Equal(clock) || lastPtr == nil || !lastPtr.Equal(clock) {
			t.Errorf("raw MAX(created_at) = %v / %v", last, lastPtr)
		}
		var stamps []time.Time
		must(t, db.Query(acme, &stamps, "SELECT MIN(created_at) FROM users GROUP BY name"))
		// ann, bob, cy, dee: raw SQL has no tenant or soft-delete filter.
		if len(stamps) != 4 || !stamps[0].Equal(clock) {
			t.Errorf("raw time slice = %v", stamps)
		}
		if m, err := orm.CountBy(acme, users, uc.Name); err != nil || len(m) != 3 || m["ann"] != 2 || m["bob"] != 1 {
			t.Errorf("CountBy = %v, %v", m, err)
		}
		if m, err := orm.CountBy(acme, users, uc.Bio); err != nil || len(m) != 1 || m["x"] != 2 {
			t.Errorf("CountBy nullable = %v, %v (NULLs left out)", m, err)
		}
		if m, err := orm.SumBy(acme, users, uc.Name, uc.Age, q.Sort("name"), q.Limit(1)); err != nil || m["ann"] != 70 || m["cy"] != 2 || len(m) != 3 {
			t.Errorf("SumBy = %v, %v (sort/limit ignored)", m, err)
		}
		if s, err := orm.Sum(tenant.Bypass(ctx), users, uc.Age); err != nil || s != 1097 {
			t.Errorf("Bypass Sum = %v, %v", s, err)
		}
		if _, err := orm.Sum(ctx, users, uc.Age); !errors.Is(err, tenant.ErrNoTenant) {
			t.Errorf("Sum without tenant: %v", err)
		}
		if _, err := orm.Sum(acme, users, q.Col[int]("age) FROM users; --")); err == nil {
			t.Error("unknown field accepted")
		}
	})
}
