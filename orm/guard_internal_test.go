package orm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type countGuard struct {
	name          string
	allowed, done atomic.Int64
	refuse        error
	lastErr       atomic.Value
}

func (g *countGuard) Name() string { return g.name }

func (g *countGuard) Allow(context.Context) error {
	if g.refuse != nil {
		return g.refuse
	}
	g.allowed.Add(1)
	return nil
}

func (g *countGuard) Done(_ context.Context, err error) {
	g.done.Add(1)
	g.lastErr.Store(errBox{err})
}

type errBox struct{ err error }

func TestGuardPlugin(t *testing.T) {
	ctx := context.Background()
	db, _ := fakeDB(t, 1)
	g := &countGuard{name: "g"}
	Use(g)(db)
	users := For[benchUser](db)

	_, err := users.FindByID(ctx, 1) // query
	must(t, err)
	u := benchUser{ID: 1, Name: "a"}
	must(t, users.Update(ctx, &u))                                // exec (no transaction: no after hooks)
	must(t, db.Transaction(ctx, func(ctx context.Context) error { // begin + exec inside
		_, err := db.Exec(ctx, "UPDATE bench_users SET name = ?", "b")
		return err
	}))
	if a, d := g.allowed.Load(), g.done.Load(); a != 4 || d != 4 {
		t.Fatalf("allowed %d, done %d; want 4 each", a, d)
	}

	// A refusing guard stops the round trip; earlier guards are balanced.
	db2, _ := fakeDB(t, 1)
	first, stop := &countGuard{name: "first"}, &countGuard{name: "stop", refuse: errors.New("circuit open")}
	Use(first)(db2)
	Use(stop)(db2)
	_, err = For[benchUser](db2).FindByID(ctx, 1)
	if err == nil || err.Error() != "orm: plugin stop: circuit open" {
		t.Fatalf("refused query: %v", err)
	}
	if err := db2.Transaction(ctx, func(context.Context) error { return nil }); err == nil {
		t.Fatal("refused begin ran")
	}
	if first.allowed.Load() != 2 || first.done.Load() != 2 {
		t.Fatalf("first guard unbalanced: %d/%d", first.allowed.Load(), first.done.Load())
	}
	if b := first.lastErr.Load().(errBox); b.err == nil {
		t.Fatal("first guard was told the refused statement succeeded")
	}
}
