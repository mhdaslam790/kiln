package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// InsertAll sends rows in batches on every database and must give each
// record the id the database actually stored for it.
func TestInsertAllAssignsIDs(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		insertAndCheck(t, ctx, users, "bulk", 2500) // several chunks on SQL Server (1,000 rows)

		if db.Dialect().Kind() == dialect.KindMySQL {
			// Multi-primary setups (Galera, group replication) number rows
			// in steps; ids must follow the step.
			must(t, db.WithConn(ctx, func(ctx context.Context) error {
				if _, err := db.Exec(ctx, "SET SESSION auto_increment_increment = 3"); err != nil {
					return err
				}
				defer db.Exec(ctx, "SET SESSION auto_increment_increment = 1")
				insertAndCheck(t, ctx, users, "step", 50)
				return nil
			}))
		}
	})
}

func insertAndCheck(t *testing.T, ctx context.Context, users *orm.Repo[User], prefix string, n int) {
	t.Helper()
	batch := make([]User, n)
	for i := range batch {
		batch[i] = User{Name: fmt.Sprintf("%s %d", prefix, i), Email: fmt.Sprintf("%s-%d@EXAMPLE.com", prefix, i), Age: i % 90}
	}
	must(t, users.InsertAll(ctx, batch))

	stored, err := users.FindAll(ctx, q.Like("email", prefix+"-%"))
	must(t, err)
	if len(stored) != n {
		t.Fatalf("%s: stored %d rows, want %d", prefix, len(stored), n)
	}
	byEmail := make(map[string]int64, n)
	for _, u := range stored {
		byEmail[u.Email] = u.ID
	}
	seen := make(map[int64]bool, n)
	for i, u := range batch {
		if u.ID == 0 || seen[u.ID] {
			t.Fatalf("%s: row %d has id %d (zero or duplicate)", prefix, i, u.ID)
		}
		seen[u.ID] = true
		// BeforeInsert lowercases the email; the hook ran on every row.
		if want := byEmail[u.Email]; want != u.ID {
			t.Fatalf("%s: row %d (%s) got id %d, database has %d", prefix, i, u.Email, u.ID, want)
		}
		if u.CreatedAt.IsZero() {
			t.Fatalf("%s: row %d has no created_at", prefix, i)
		}
	}
}
