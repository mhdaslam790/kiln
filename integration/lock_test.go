package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

type Doc struct {
	ID        int64
	Title     string
	Body      string
	Version   int        `db:"version,lock"`
	DeletedAt *time.Time `db:"deleted_at"`
}

func TestOptimisticLock(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		mig := migrate.Migration{Version: 20990103000001, Name: "create_docs",
			Up: func(s *schema.Schema) {
				s.CreateTable("docs", func(t *schema.Table) {
					t.ID("id")
					t.String("title")
					t.Text("body")
					t.Int("version", schema.Required(), schema.Default(0))
					t.DateTime("deleted_at")
				})
			},
			Down: func(s *schema.Schema) { s.DropTable("docs") },
		}
		m := migrate.New(db, append(migrations(), mig))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })
		docs := orm.For[Doc](db)

		d := Doc{Title: "plan"}
		must(t, docs.Insert(ctx, &d))
		if d.Version != 1 {
			t.Fatalf("new record version = %d, want 1", d.Version)
		}

		// Two users read the same version.
		alice, err := docs.FindByID(ctx, d.ID)
		must(t, err)
		bob, err := docs.FindByID(ctx, d.ID)
		must(t, err)

		alice.Body = "alice's edit"
		must(t, docs.Update(ctx, &alice))
		if alice.Version != 2 {
			t.Fatalf("after update version = %d, want 2", alice.Version)
		}

		// Bob's save would overwrite Alice's: refused, nothing written.
		bob.Body = "bob's edit"
		if err := docs.Update(ctx, &bob); !errors.Is(err, orm.ErrStale) {
			t.Fatalf("stale full update: %v", err)
		}
		if err := docs.Update(ctx, &bob, "Body"); !errors.Is(err, orm.ErrStale) {
			t.Fatalf("stale partial update: %v", err)
		}
		if bob.Version != 1 {
			t.Fatalf("refused update changed bob's version to %d", bob.Version)
		}
		got, err := docs.FindByID(ctx, d.ID)
		must(t, err)
		if got.Body != "alice's edit" || got.Version != 2 {
			t.Fatalf("row = %+v", got)
		}

		// Reload and retry: fine. Partial updates are checked and bump too.
		bob = got
		bob.Body = "bob's edit"
		must(t, docs.Update(ctx, &bob, "Body"))
		if bob.Version != 3 {
			t.Fatalf("partial update version = %d, want 3", bob.Version)
		}
		// The version cannot be written by hand.
		bob.Version = 99
		if err := docs.Update(ctx, &bob, "Version", "Title"); !errors.Is(err, orm.ErrStale) {
			t.Fatalf("hand-written version: %v", err)
		}
		bob.Version = 3

		// Deletes are checked too; a stale soft delete leaves the record as is.
		if err := docs.Delete(ctx, &alice); !errors.Is(err, orm.ErrStale) {
			t.Fatalf("stale delete: %v", err)
		}
		if alice.DeletedAt != nil {
			t.Fatal("refused soft delete marked the record deleted")
		}
		must(t, docs.Delete(ctx, &bob))
		if bob.DeletedAt == nil || bob.Version != 4 {
			t.Fatalf("soft delete: %+v", bob)
		}
		must(t, docs.HardDelete(ctx, &bob))
		if err := docs.HardDelete(ctx, &bob); !errors.Is(err, orm.ErrStale) {
			t.Fatalf("second delete: %v", err)
		}

		// Bulk writes are explicit and not version-checked.
		must(t, docs.InsertAll(ctx, []Doc{{Title: "a"}, {Title: "b"}}))
		n, err := docs.UpdateAny(ctx, q.Eq("title", "a"), q.Set("body", "x"))
		if err != nil || n != 1 {
			t.Fatalf("UpdateAny = %d, %v", n, err)
		}
	})
}
