package integration

import (
	"context"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
)

// A concurrent index must run outside a transaction on PostgreSQL (which
// rejects CREATE INDEX CONCURRENTLY in one) and is a plain index elsewhere.
func TestConcurrentIndexMigration(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		idx := migrate.Migration{Version: 20990101000001, Name: "index_posts_views",
			Up: func(s *schema.Schema) {
				s.CreateIndex("posts", "idx_posts_views", []string{"views"}, schema.Concurrently())
			},
			Down: func(s *schema.Schema) { s.DropIndex("posts", "idx_posts_views") },
		}
		all := append(migrations(), idx)
		m := migrate.New(db, all)

		fs, err := m.LintPending(ctx)
		must(t, err)
		if len(fs) != 0 {
			t.Fatalf("a lone concurrent index should lint clean: %v", fs)
		}

		_, err = m.Up(ctx)
		must(t, err)
		applied := true
		t.Cleanup(func() {
			if applied {
				_, _ = m.Down(ctx, 1)
			}
		})
		st, err := migrate.Inspect(ctx, db)
		must(t, err)
		if !st.Tables["posts"].HasIndexOn([]string{"views"}, false) {
			t.Fatalf("index not created: %+v", st.Tables["posts"].Indexes)
		}
		_, err = m.Down(ctx, 1)
		must(t, err)
		applied = false

		// Mixed with other changes, lint reports it on PostgreSQL.
		mixed := idx
		mixed.Up = func(s *schema.Schema) {
			s.CreateIndex("posts", "idx_posts_views", []string{"views"}, schema.Concurrently())
			s.AddColumn("posts", "summary", schema.Text)
		}
		fs, err = migrate.New(db, append(migrations(), mixed)).LintPending(ctx)
		must(t, err)
		if want := db.Dialect().Kind() == dialect.KindPostgres; (len(fs) == 1 && fs[0].Check == "concurrent-index-mixed") != want {
			t.Fatalf("mixed findings on %s: %v", db.Dialect().Kind(), fs)
		}
	})
}
