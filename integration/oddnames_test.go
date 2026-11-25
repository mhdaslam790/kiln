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

// Legacy is a table with column names that need quoting: a hyphen, a
// space, a leading digit and SQL keywords.
type Legacy struct {
	ID        int64
	Code      string     `db:"item-code"`
	Label     string     `db:"display name"`
	X3d       int        `db:"3d"`
	Order     int        `db:"order"`
	Version   int        `db:"version,lock"`
	DeletedAt *time.Time `db:"deleted_at"`
}

func (Legacy) TableName() string { return "legacy-items" }

func TestOddColumnNames(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		mig := migrate.Migration{Version: 20990108000001, Name: "create_legacy",
			Up: func(s *schema.Schema) {
				s.CreateTable("legacy-items", func(t *schema.Table) {
					t.ID("id")
					t.String("item-code", schema.Required())
					t.String("display name", schema.Required())
					t.Int("3d", schema.Required())
					t.Int("order", schema.Required())
					t.Int("version", schema.Required(), schema.Default(0))
					t.DateTime("deleted_at")
				})
				s.CreateIndex("legacy-items", "idx_legacy_code", []string{"item-code"})
			},
			Down: func(s *schema.Schema) { s.DropTable("legacy-items") },
		}
		m := migrate.New(db, append(migrations(), mig))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })
		items := orm.For[Legacy](db)

		a := Legacy{Code: "A-1", Label: "first", X3d: 3, Order: 2}
		must(t, items.Insert(ctx, &a))
		must(t, items.InsertAll(ctx, []Legacy{{Code: "B-2", Label: "second", X3d: 1, Order: 1}}))
		got, err := items.FindByID(ctx, a.ID)
		must(t, err)
		if got.Code != "A-1" || got.Label != "first" || got.X3d != 3 || got.Version != 1 {
			t.Fatalf("read back %+v", got)
		}
		byCode, err := items.Find(ctx, q.Eq("item-code", "B-2"))
		must(t, err)
		if byCode.Label != "second" {
			t.Fatalf("filter on item-code: %+v", byCode)
		}
		sorted, err := items.FindAll(ctx, q.Sort("display name"))
		must(t, err)
		if len(sorted) != 2 || sorted[0].Label != "first" {
			t.Fatalf("sort by display name: %+v", sorted)
		}
		sorted, err = items.FindAll(ctx, q.Sort("-3d", "order"))
		must(t, err)
		if sorted[0].X3d != 3 {
			t.Fatalf("sort by 3d: %+v", sorted)
		}
		got.Label = "renamed"
		must(t, items.Update(ctx, &got))
		stale := got
		stale.Version--
		if err := items.Update(ctx, &stale); !errors.Is(err, orm.ErrStale) {
			t.Fatalf("version lock: %v", err)
		}
		n, err := items.UpdateAny(ctx, q.Eq("item-code", "B-2"), q.Set("display name", "two"), q.Inc("3d", 1))
		must(t, err)
		if n != 1 {
			t.Fatalf("UpdateAny = %d", n)
		}
		must(t, items.Delete(ctx, &got))
		if c, _ := items.Count(ctx); c != 1 {
			t.Fatalf("count after soft delete = %d", c)
		}
		pg, err := items.Cursor(ctx, orm.CursorOpts{Limit: 5, Sort: "item-code"}, q.Unscoped())
		must(t, err)
		if len(pg.Items) != 2 {
			t.Fatalf("cursor: %d", len(pg.Items))
		}
	})
}
