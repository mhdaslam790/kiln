package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

// TestConcurrentMigrators: several instances starting at once must apply
// each migration exactly once and all succeed.
func TestConcurrentMigrators(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		count := 0
		var mu sync.Mutex
		all := append(migrations(), migrate.Migration{Version: 20990109000001, Name: "counted",
			Up: func(s *schema.Schema) {
				s.Do(func(ctx context.Context, tx schema.Executor) error {
					mu.Lock()
					count++
					mu.Unlock()
					return nil
				})
				s.CreateTable("counted", func(t *schema.Table) { t.ID("id") })
			},
			Down: func(s *schema.Schema) { s.DropTable("counted") },
		})
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := migrate.New(db, all).Up(ctx); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("concurrent Up: %v", err)
		}
		if count != 1 {
			t.Errorf("migration ran %d times", count)
		}
		t.Cleanup(func() { _, _ = migrate.New(db, all).Down(ctx, 1) })
	})
}

type nsort struct {
	ID    int64
	Title string
	Rank  *int
}

func (nsort) TableName() string { return "nsorts" }

// TestCursorNullSort: paging by a column with NULLs visits every row once.
func TestCursorNullSort(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		mig := migrate.Migration{Version: 20990109000002, Name: "nsorts",
			Up: func(s *schema.Schema) {
				s.CreateTable("nsorts", func(t *schema.Table) {
					t.ID("id")
					t.String("title", schema.Required())
					t.Int("rank")
				})
			},
			Down: func(s *schema.Schema) { s.DropTable("nsorts") },
		}
		m := migrate.New(db, append(migrations(), mig))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })
		r := orm.For[nsort](db)
		one, two := 1, 2
		for i, rk := range []*int{&two, nil, &one, nil, &two, nil, &one} {
			must(t, r.Insert(ctx, &nsort{Title: fmt.Sprint(i), Rank: rk}))
		}
		for _, sort := range []string{"rank", "-rank"} {
			seen := map[int64]int{}
			o := orm.CursorOpts{Limit: 2, Sort: sort}
			var pages [][]int64
			var last orm.CursorPage[nsort]
			for i := 0; i < 20; i++ {
				pg, err := r.Cursor(ctx, o)
				if err != nil {
					t.Fatalf("%s: %v", sort, err)
				}
				var ids []int64
				for _, x := range pg.Items {
					seen[x.ID]++
					ids = append(ids, x.ID)
				}
				pages = append(pages, ids)
				last = pg
				if !pg.HasNext {
					break
				}
				o.After = pg.Next
			}
			// Walking back from the last page gives the same pages in reverse.
			back := orm.CursorOpts{Limit: 2, Sort: sort, Before: last.Prev}
			for i := len(pages) - 2; i >= 0 && back.Before != ""; i-- {
				pg, err := r.Cursor(ctx, back)
				if err != nil {
					t.Fatalf("%s back: %v", sort, err)
				}
				var ids []int64
				for _, x := range pg.Items {
					ids = append(ids, x.ID)
				}
				if fmt.Sprint(ids) != fmt.Sprint(pages[i]) {
					t.Errorf("sort %s: back page %d = %v, forward was %v", sort, i, ids, pages[i])
				}
				back.Before = pg.Prev
			}
			if len(seen) != 7 {
				t.Errorf("sort %s: saw %d of 7 rows", sort, len(seen))
			}
			for id, c := range seen {
				if c != 1 {
					t.Errorf("sort %s: row %d seen %d times", sort, id, c)
				}
			}
		}
	})
}

// TestPreloadSoftDeletedChildren: soft-deleted children are not preloaded.
func TestPreloadSoftDeletedChildren(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		_, err := migrate.New(db, migrations()).Up(ctx)
		must(t, err)
		users := orm.For[User](db)
		owner := User{TenantID: 1, Name: "o", Email: "o@x.io"}
		must(t, users.Insert(ctx, &owner))
		// users soft-delete; posts don't: check users preloaded as a child via Post.User and
		// a parent's has-many of soft-deletable rows.
		p := Post{UserID: owner.ID, Title: "t"}
		must(t, orm.For[Post](db).Insert(ctx, &p))
		got, _ := orm.For[Post](db).FindByID(ctx, p.ID)
		must(t, users.Delete(ctx, &owner))
		must(t, orm.For[Post](db).Preload(ctx, &got, "User"))
		if got.User != nil {
			t.Error("soft-deleted user preloaded (belongs-to)")
		}
		var list []Post
		must(t, db.FindAll(ctx, &list, q.Eq("id", p.ID)))
		must(t, orm.For[Post](db).PreloadAll(ctx, list, "User"))
		if list[0].User != nil {
			t.Error("soft-deleted user preloaded (PreloadAll)")
		}
		if _, err := users.FindByID(ctx, owner.ID); !errors.Is(err, orm.ErrNotFound) {
			t.Error("soft-deleted user found")
		}
	})
}
