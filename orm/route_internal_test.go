package orm

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
)

type routeRec struct {
	wrote, asked atomic.Int64
	primary      atomic.Bool
}

func (r *routeRec) Name() string                     { return "route" }
func (r *routeRec) Wrote(context.Context)            { r.wrote.Add(1) }
func (r *routeRec) ReadPrimary(context.Context) bool { r.asked.Add(1); return r.primary.Load() }

func TestRoutePlugin(t *testing.T) {
	ctx := context.Background()
	db, _ := fakeDB(t, 3) // primary: three rows
	replica := sql.OpenDB(fakeConnector{&fakeDriver{rows: 1}})
	t.Cleanup(func() { replica.Close() })
	r := &routeRec{}
	WithReplicas(replica)(db)
	Use(r)(db)
	users := For[benchUser](db)

	from := func(ctx context.Context) string {
		t.Helper()
		all, err := users.FindAll(ctx)
		must(t, err)
		if len(all) == 3 {
			return "primary"
		}
		return "replica"
	}
	if got := from(ctx); got != "replica" {
		t.Fatalf("read went to the %s", got)
	}
	r.primary.Store(true)
	if got := from(ctx); got != "primary" {
		t.Fatalf("ReadPrimary true: read went to the %s", got)
	}
	if r.asked.Load() != 2 || r.wrote.Load() != 0 {
		t.Fatalf("asked %d, wrote %d", r.asked.Load(), r.wrote.Load())
	}

	u := benchUser{ID: 1}
	must(t, users.Update(ctx, &u))
	_, err := db.Exec(ctx, "UPDATE bench_users SET name = ?", "x")
	must(t, err)
	if r.wrote.Load() != 2 {
		t.Fatalf("wrote %d, want 2", r.wrote.Load())
	}

	// Transactions and UsePrimary don't ask.
	r.primary.Store(false)
	must(t, db.Transaction(ctx, func(ctx context.Context) error {
		if got := from(ctx); got != "primary" {
			t.Errorf("read in a transaction went to the %s", got)
		}
		return nil
	}))
	if got := from(UsePrimary(ctx)); got != "primary" {
		t.Fatalf("UsePrimary read went to the %s", got)
	}
	if r.asked.Load() != 2 {
		t.Fatalf("asked %d times, want still 2", r.asked.Load())
	}

	// A transaction that wrote tells the plugins again at commit (the
	// changes become visible then); a read-only or rolled-back one doesn't.
	before := r.wrote.Load()
	must(t, db.Transaction(ctx, func(ctx context.Context) error {
		_, err := db.Exec(ctx, "UPDATE bench_users SET name = ?", "y")
		return err
	}))
	if got := r.wrote.Load() - before; got != 2 {
		t.Fatalf("write + commit: wrote %d, want 2", got)
	}
	before = r.wrote.Load()
	must(t, db.Transaction(ctx, func(ctx context.Context) error { _, err := users.FindAll(ctx); return err }))
	_ = db.Transaction(ctx, func(ctx context.Context) error {
		_, _ = db.Exec(ctx, "UPDATE bench_users SET name = ?", "z")
		return errors.New("rollback")
	})
	if got := r.wrote.Load() - before; got != 1 {
		t.Fatalf("read-only + rolled back: wrote %d, want 1 (the rolled-back statement)", got)
	}
}
