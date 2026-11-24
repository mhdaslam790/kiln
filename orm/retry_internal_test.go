package orm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	ctx := context.Background()
	deadlock := errors.New(`ERROR: deadlock detected (SQLSTATE 40P01)`)

	db, _ := fakeDB(t, 0)
	runs := 0
	failTwice := func(context.Context) error {
		runs++
		if runs <= 2 {
			return deadlock
		}
		return nil
	}

	// Off by default: the error comes back after one run.
	if err := db.Transaction(ctx, failTwice); !errors.Is(err, deadlock) || runs != 1 {
		t.Fatalf("no retry: err=%v runs=%d", err, runs)
	}

	WithRetry(3)(db)
	runs = 0
	if err := db.Transaction(ctx, failTwice); err != nil || runs != 3 {
		t.Fatalf("retry: err=%v runs=%d, want success on run 3", err, runs)
	}

	// Gives up after n retries with the last error.
	runs = 0
	always := func(context.Context) error { runs++; return deadlock }
	if err := db.Transaction(ctx, always); !errors.Is(err, deadlock) || runs != 4 {
		t.Fatalf("exhausted: err=%v runs=%d, want 4 runs", err, runs)
	}

	// Other errors are never retried.
	runs = 0
	boom := errors.New("boom")
	if err := db.Transaction(ctx, func(context.Context) error { runs++; return boom }); err != boom || runs != 1 {
		t.Fatalf("plain error: err=%v runs=%d", err, runs)
	}

	// Nested transactions are not retried on their own: the outer one is.
	runs = 0
	inner := 0
	err := db.Transaction(ctx, func(ctx context.Context) error {
		runs++
		return db.Transaction(ctx, func(context.Context) error {
			inner++
			if inner == 1 {
				return deadlock
			}
			return nil
		})
	})
	if err != nil || runs != 2 || inner != 2 {
		t.Fatalf("nested: err=%v outer=%d inner=%d", err, runs, inner)
	}

	// A done context stops the waiting.
	cctx, cancel := context.WithCancel(ctx)
	runs = 0
	start := time.Now()
	err = db.Transaction(cctx, func(context.Context) error { runs++; cancel(); return deadlock })
	if !errors.Is(err, deadlock) || runs != 1 || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("cancelled ctx: err=%v runs=%d after %v", err, runs, time.Since(start))
	}
}
