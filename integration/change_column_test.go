package integration

import (
	"context"
	"testing"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
)

// ChangeColumn describes the whole new column: a definition without a
// default removes the old default on every database. (PostgreSQL used to
// keep it, so rows written without the column silently got the old value.)
func TestChangeColumnDropsDefault(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		add := migrate.Migration{Version: 20990102000001, Name: "add_priority",
			Up: func(s *schema.Schema) {
				s.AddColumn("posts", "priority", schema.Int, schema.Required(), schema.Default(5))
			},
			Down: func(s *schema.Schema) { s.DropColumn("posts", "priority") },
		}
		change := migrate.Migration{Version: 20990102000002, Name: "priority_no_default",
			Up: func(s *schema.Schema) { s.ChangeColumn("posts", "priority", schema.Int, schema.Required()) },
			Down: func(s *schema.Schema) {
				s.ChangeColumn("posts", "priority", schema.Int, schema.Required(), schema.Default(5))
			},
		}
		m := migrate.New(db, append(migrations(), add, change))
		insert := func() error {
			_, err := db.Exec(ctx, "INSERT INTO posts (title, views) VALUES (?, ?)", "p", 0)
			return err
		}

		_, err := m.UpTo(ctx, add.Version)
		must(t, err)
		applied := 1
		t.Cleanup(func() { _, _ = m.Down(ctx, applied) })

		must(t, insert()) // the default fills priority
		var p int
		must(t, db.Query(ctx, &p, "SELECT priority FROM posts WHERE title = ?", "p"))
		if p != 5 {
			t.Fatalf("priority = %d, want the default 5", p)
		}

		_, err = m.Up(ctx)
		must(t, err)
		applied = 2
		if err := insert(); err == nil {
			t.Fatal("insert without priority succeeded: the old default is still there")
		}

		// Rolling back restores the default.
		_, err = m.Down(ctx, 1)
		must(t, err)
		applied = 1
		must(t, insert())
	})
}
