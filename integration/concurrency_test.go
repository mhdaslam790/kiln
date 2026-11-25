package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/tenant"
)

// TestConcurrentUse runs reads, writes and transactions from many
// goroutines with the tenant plugin, cache and statement cache on.
// Run with -race.
func TestConcurrentUse(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{tenant.Plugin("tenant_id"), orm.WithCache(), orm.WithStmtCache(32), orm.WithRetry(5)}
	}, func(t *testing.T, db *orm.DB) {
		bg := context.Background()
		m := migrate.New(db, append(migrations(), notesTable))
		_, err := m.Up(bg)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(bg, 1) })
		notes := orm.For[Note](db)
		const workers, each = 8, 15
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				ctx := tenant.With(bg, int64(w%2+1))
				for i := 0; i < each; i++ {
					err := db.Transaction(ctx, func(ctx context.Context) error {
						n := Note{Title: fmt.Sprintf("w%d-%d", w, i)}
						if err := notes.Insert(ctx, &n); err != nil {
							return err
						}
						n.Views = i
						return notes.Update(ctx, &n)
					})
					if err != nil {
						errs <- fmt.Errorf("worker %d tx %d: %w", w, i, err)
						return
					}
					if _, err := notes.FindAll(orm.Cached(ctx, time.Minute), q.Sort("-id"), q.Limit(5)); err != nil {
						errs <- err
						return
					}
					if _, err := notes.Count(ctx); err != nil {
						errs <- err
						return
					}
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		for tid := int64(1); tid <= 2; tid++ {
			ctx := tenant.With(bg, tid)
			n, err := notes.Count(ctx)
			must(t, err)
			if n != workers/2*each {
				t.Errorf("tenant %d: %d notes, want %d", tid, n, workers/2*each)
			}
			// The cache must not serve a list from before the last write.
			cached, err := notes.Count(orm.Cached(ctx, time.Minute))
			must(t, err)
			if cached != n {
				t.Errorf("tenant %d: cached count %d, real %d", tid, cached, n)
			}
		}
	})
}

// TestMigrationRoundTrip: every migration can be undone and redone.
func TestMigrationRoundTrip(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		all := append(migrations(), notesTable)
		m := migrate.New(db, all)
		_, err := m.Up(ctx)
		must(t, err)
		if _, err := m.Down(ctx, len(all)); err != nil {
			t.Fatalf("down all: %v", err)
		}
		pending, err := m.Pending(ctx)
		must(t, err)
		if len(pending) != len(all) {
			t.Fatalf("after down all: %d pending, want %d", len(pending), len(all))
		}
		if _, err := m.Up(ctx); err != nil {
			t.Fatalf("up again: %v", err)
		}
		if err := m.Redo(ctx); err != nil {
			t.Fatalf("redo: %v", err)
		}
		pending, _ = m.Pending(ctx)
		if len(pending) != 0 {
			t.Fatalf("%d pending after redo", len(pending))
		}
		_, _ = m.Down(ctx, 1)
	})
}
