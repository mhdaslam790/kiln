package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
)

// slowSQL returns a statement that runs for about two seconds.
func slowSQL(k dialect.Kind) string {
	switch k {
	case dialect.KindPostgres:
		return "SELECT 1 FROM pg_sleep(2)"
	case dialect.KindMySQL:
		return "SELECT SLEEP(2)"
	case dialect.KindMSSQL:
		return "WAITFOR DELAY '00:00:02'; SELECT 1"
	default: // SQLite: a long recursive count, interrupted on cancel
		return "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c LIMIT 1000000000) SELECT MAX(x) FROM c"
	}
}

func TestQueryTimeout(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{orm.WithQueryTimeout(200 * time.Millisecond)}
	}, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		slow := slowSQL(db.Dialect().Kind())

		start := time.Now()
		var n int64
		err := db.Query(ctx, &n, slow)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("slow query: err = %v (after %v), want DeadlineExceeded", err, time.Since(start))
		}
		if d := time.Since(start); d > 1500*time.Millisecond {
			t.Fatalf("slow query stopped after %v, want about 200ms", d)
		}

		// The pool keeps working, and fast statements are unaffected.
		users := orm.For[User](db)
		must(t, users.Insert(ctx, &User{Name: "t", Email: "timeout@x.io"}))
		if c, err := users.Count(ctx); err != nil || c != 1 {
			t.Fatalf("Count after timeout = %d, %v", c, err)
		}

		// A context deadline that is earlier wins.
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		start = time.Now()
		if err := db.Query(short, &n, slow); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ctx deadline: %v", err)
		}
		if d := time.Since(start); d > 1000*time.Millisecond {
			t.Fatalf("ctx deadline not honoured: %v", d)
		}

		// The timeout is per statement: a transaction may run longer than
		// it as long as each statement is quick.
		must(t, db.Transaction(ctx, func(ctx context.Context) error {
			for i := 0; i < 3; i++ {
				if _, err := users.Count(ctx); err != nil {
					return err
				}
				time.Sleep(100 * time.Millisecond)
			}
			return nil
		}))
	})
}
