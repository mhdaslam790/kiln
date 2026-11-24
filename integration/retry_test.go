package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// Two transactions lock the same two rows in opposite order: the database
// aborts one as a deadlock victim, and WithRetry runs it again.
func TestRetryDeadlock(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{orm.WithRetry(3)}
	}, func(t *testing.T, db *orm.DB) {
		if db.Dialect().Kind() == dialect.KindSQLite {
			t.Skip("SQLite has a single writer: no deadlocks")
		}
		ctx := context.Background()
		users := orm.For[User](db)
		a, b := User{Name: "a", Email: "dl-a@x.io"}, User{Name: "b", Email: "dl-b@x.io"}
		must(t, users.Insert(ctx, &a))
		must(t, users.Insert(ctx, &b))

		var runs atomic.Int32
		var firstRound sync.WaitGroup
		firstRound.Add(2)
		swap := func(first, second int64) error {
			return db.Transaction(ctx, func(ctx context.Context) error {
				n := runs.Add(1)
				if _, err := users.UpdateAny(ctx, q.Eq("id", first), q.Inc("age", 1)); err != nil {
					return err
				}
				if n <= 2 { // first attempts: wait until both hold one row
					firstRound.Done()
					firstRound.Wait()
				}
				_, err := users.UpdateAny(ctx, q.Eq("id", second), q.Inc("age", 1))
				return err
			})
		}
		errs := make(chan error, 2)
		go func() { errs <- swap(a.ID, b.ID) }()
		go func() { errs <- swap(b.ID, a.ID) }()
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("transaction failed despite retry: %v", err)
			}
		}
		if runs.Load() < 3 {
			t.Fatalf("runs = %d: expected a deadlock and a retry", runs.Load())
		}
		var total int64
		must(t, db.Query(ctx, &total, "SELECT SUM(age) FROM users WHERE email LIKE 'dl-%'"))
		if total != 4 { // both transactions committed exactly once
			t.Fatalf("sum(age) = %d, want 4", total)
		}
	})
}
