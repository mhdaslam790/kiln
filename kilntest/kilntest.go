// Package kilntest helps you test code that uses a real database.
//
// Each test runs in its own transaction that is rolled back when the test
// ends, so tests never see each other's data and need no cleanup:
//
//	func TestSignup(t *testing.T) {
//		ctx := kilntest.Tx(t, db) // rolled back when the test ends
//		u := users.Create(t, ctx) // a factory, see NewFactory
//
//		err := svc.Rename(ctx, u.ID, "Ada") // pass ctx to the code under test
//		...
//	}
//
// The code under test must use the ctx it is given. A call made with a new
// context.Background() runs outside the test's transaction: it cannot see
// the test's rows and its writes are not rolled back.
//
// Code that calls db.Transaction keeps working: inside a test it becomes a
// savepoint, so an error still rolls back only that block.
package kilntest

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
)

// Tx starts a transaction for the test and returns a context that carries
// it. The transaction is rolled back when the test (and its subtests) end.
func Tx(t testing.TB, db *orm.DB) context.Context {
	t.Helper()
	tx, err := db.Begin(context.Background(), nil)
	if err != nil {
		t.Fatalf("kilntest: begin transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("kilntest: roll back transaction: %v", err)
		}
	})
	return tx.Context()
}

// Migrate applies pending migrations, failing the test on error. When
// nothing is pending it only reads the migrations table (about 15 µs on
// SQLite, 100 µs on a server), so every test can call it. It keeps no state
// between calls.
func Migrate(t testing.TB, db *orm.DB, migrations []migrate.Migration) {
	t.Helper()
	if _, err := migrate.New(db, migrations).Up(context.Background()); err != nil {
		t.Fatalf("kilntest: migrate: %v", err)
	}
}

// Factory builds and inserts test records with sensible defaults.
//
//	var users = kilntest.NewFactory(db, func(n int) User {
//		return User{Name: "User", Email: fmt.Sprintf("u%d@test.dev", n)}
//	})
//
//	u := users.Create(t, ctx)                                       // defaults
//	admin := users.Create(t, ctx, func(u *User) { u.Role = "admin" }) // override
//	list := users.CreateMany(t, ctx, 10)
type Factory[T any] struct {
	db   *orm.DB
	fn   func(n int) T
	next atomic.Int64
}

// NewFactory returns a factory whose fn builds a record with defaults. n
// counts up from 1 for each record the factory builds; use it to keep
// unique columns (emails, slugs) unique.
func NewFactory[T any](db *orm.DB, fn func(n int) T) *Factory[T] {
	return &Factory[T]{db: db, fn: fn}
}

// Build returns a record with defaults and the overrides applied, without
// inserting it.
func (f *Factory[T]) Build(overrides ...func(*T)) T {
	rec := f.fn(int(f.next.Add(1)))
	for _, o := range overrides {
		o(&rec)
	}
	return rec
}

// Create builds a record and inserts it, failing the test on error. The
// returned record has its primary key and timestamps filled in.
func (f *Factory[T]) Create(t testing.TB, ctx context.Context, overrides ...func(*T)) T {
	t.Helper()
	rec := f.Build(overrides...)
	if err := orm.For[T](f.db).Insert(ctx, &rec); err != nil {
		t.Fatalf("kilntest: create %T: %v", rec, err)
	}
	return rec
}

// CreateMany builds n records, applies the overrides to each and inserts
// them in one batch, failing the test on error.
func (f *Factory[T]) CreateMany(t testing.TB, ctx context.Context, n int, overrides ...func(*T)) []T {
	t.Helper()
	recs := make([]T, n)
	for i := range recs {
		recs[i] = f.Build(overrides...)
	}
	if n == 0 {
		return recs
	}
	if err := orm.For[T](f.db).InsertAll(ctx, recs); err != nil {
		var zero T
		t.Fatalf("kilntest: create %d %T: %v", n, zero, err)
	}
	return recs
}
