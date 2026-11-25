package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
	"github.com/mhdaslam790/kiln/tenant"
)

// Note combines the model features: timestamps, soft delete, optimistic
// locking and a tenant column.
type Note struct {
	ID        int64
	TenantID  int64
	Title     string
	Views     int
	Version   int `db:"version,lock"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

var notesTable = migrate.Migration{Version: 20990107000001, Name: "create_notes",
	Up: func(s *schema.Schema) {
		s.CreateTable("notes", func(t *schema.Table) {
			t.ID("id")
			t.BigInt("tenant_id", schema.Required())
			t.String("title", schema.Required())
			t.Int("views", schema.Required(), schema.Default(0))
			t.Int("version", schema.Required(), schema.Default(0))
			t.DateTime("created_at", schema.Required())
			t.DateTime("updated_at", schema.Required())
			t.DateTime("deleted_at")
		})
	},
	Down: func(s *schema.Schema) { s.DropTable("notes") },
}

// TestEverydayUse checks that ordinary calls work, with every model
// feature and several DB options on at once.
func TestEverydayUse(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{tenant.Plugin("tenant_id"), orm.WithCache(), orm.WithRetry(2), orm.WithQueryTimeout(10 * time.Second)}
	}, func(t *testing.T, db *orm.DB) {
		bg := context.Background()
		m := migrate.New(db, append(migrations(), notesTable))
		_, err := m.Up(bg)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(bg, 1) })
		ctx := tenant.With(bg, int64(7))
		other := tenant.With(bg, int64(8))
		notes := orm.For[Note](db)
		check := func(what string, cond bool, got ...any) {
			t.Helper()
			if !cond {
				t.Errorf("%s: %v", what, fmt.Sprint(got...))
			}
		}

		// Insert fills id, tenant, version and timestamps.
		a := Note{Title: "a"}
		must(t, notes.Insert(ctx, &a))
		check("insert", a.ID != 0 && a.TenantID == 7 && a.Version == 1 && !a.CreatedAt.IsZero() && !a.UpdatedAt.IsZero(), a)
		got, err := notes.FindByID(ctx, a.ID)
		must(t, err)
		check("read back", got.Title == "a" && got.Version == 1 && got.TenantID == 7, got)

		// InsertAll, then the read methods.
		batch := []Note{{Title: "b"}, {Title: "c"}, {Title: "d"}, {Title: "e"}}
		must(t, notes.InsertAll(ctx, batch))
		for _, n := range batch {
			check("InsertAll fills", n.ID != 0 && n.TenantID == 7 && n.Version == 1 && !n.CreatedAt.IsZero(), n)
		}
		must(t, notes.Insert(other, &Note{Title: "other tenant"}))
		all, err := notes.FindAll(ctx, q.Sort("id"))
		must(t, err)
		check("FindAll", len(all) == 5 && all[0].Title == "a" && all[4].Title == "e", len(all))
		n, err := notes.Count(ctx)
		must(t, err)
		check("Count", n == 5, n)
		ok, err := notes.Exists(ctx, q.Eq("title", "c"))
		must(t, err)
		check("Exists", ok)
		one, err := notes.Find(ctx, q.Eq("title", "d"))
		must(t, err)
		check("Find", one.Title == "d", one)

		// Full update bumps the version; a stale copy is refused.
		stale := got
		got.Title = "a2"
		must(t, notes.Update(ctx, &got))
		check("update version", got.Version == 2, got.Version)
		stale.Title = "lost update"
		err = notes.Update(ctx, &stale)
		check("stale update refused", errors.Is(err, orm.ErrStale), err)
		reread, _ := notes.FindByID(ctx, a.ID)
		check("stale update did not write", reread.Title == "a2" && reread.Version == 2, reread)

		// Partial update, Save (update and insert), UpdateChanged.
		got.Views = 5
		must(t, notes.Update(ctx, &got, "Views"))
		reread, _ = notes.FindByID(ctx, a.ID)
		check("partial update", reread.Views == 5 && reread.Version == 3 && reread.Title == "a2", reread)
		got = reread
		got.Title = "a3"
		must(t, notes.Save(ctx, &got))
		check("Save updates", got.Version == 4, got.Version)
		fresh := Note{Title: "saved"}
		must(t, notes.Save(ctx, &fresh))
		check("Save inserts", fresh.ID != 0 && fresh.TenantID == 7, fresh)
		cs := orm.Track(&got)
		got.Views = 9
		check("Changed", fmt.Sprint(cs.Changed()) == "[Views]", cs.Changed()) // Go field names
		must(t, notes.UpdateChanged(ctx, &got, cs))
		reread, _ = notes.FindByID(ctx, a.ID)
		check("UpdateChanged", reread.Views == 9 && reread.Title == "a3" && reread.Version == got.Version, reread, got.Version)

		// Bulk update / delete count rows of this tenant only.
		k, err := notes.UpdateAny(ctx, q.In("title", "b", "c"), q.Inc("views", 1))
		must(t, err)
		check("UpdateAny", k == 2, k)
		k, err = notes.UpdateAny(other, q.Eq("title", "b"), q.Set("views", 100))
		must(t, err)
		check("UpdateAny other tenant", k == 0, k)

		// Soft delete, unscoped read, restore, hard delete.
		b := batch[0]
		b, _ = notes.FindByID(ctx, b.ID)
		must(t, notes.Delete(ctx, &b))
		_, err = notes.FindByID(ctx, b.ID)
		check("deleted is hidden", errors.Is(err, orm.ErrNotFound), err)
		n, _ = notes.Count(ctx)
		check("Count after delete", n == 5, n) // a, c, d, e, saved
		us, err := notes.FindByID(ctx, b.ID, q.Unscoped())
		must(t, err)
		check("unscoped sees it", us.DeletedAt != nil, us)
		must(t, notes.Restore(ctx, &us))
		_, err = notes.FindByID(ctx, b.ID)
		check("restored", err == nil, err)
		k, err = notes.DeleteAny(ctx, q.Eq("title", "e"))
		must(t, err)
		check("DeleteAny soft", k == 1, k)
		_, err = notes.FindByID(ctx, batch[3].ID, q.Unscoped())
		check("DeleteAny kept the row", err == nil, err)
		c := batch[1]
		c, _ = notes.FindByID(ctx, c.ID)
		must(t, notes.HardDelete(ctx, &c))
		_, err = notes.FindByID(ctx, c.ID, q.Unscoped())
		check("hard deleted", errors.Is(err, orm.ErrNotFound), err)

		// Visible now: a, b, d, saved. Paginate, Each, Cursor agree.
		var want []string
		all, _ = notes.FindAll(ctx, q.Sort("id"))
		for _, x := range all {
			want = append(want, x.Title)
		}
		check("visible", fmt.Sprint(want) == "[a3 b d saved]", want)
		p, err := notes.Paginate(ctx, 2, 3, q.Sort("id"))
		must(t, err)
		check("Paginate", p.Total == 4 && p.TotalPages == 2 && len(p.Items) == 1 && p.Items[0].Title == "saved", p)
		var each []string
		must(t, notes.Each(ctx, 2, func(x Note) error { each = append(each, x.Title); return nil }))
		check("Each", fmt.Sprint(each) == fmt.Sprint(want), each)
		var cur []string
		opts := orm.CursorOpts{Limit: 3}
		for {
			pg, err := notes.Cursor(ctx, opts)
			must(t, err)
			for _, x := range pg.Items {
				cur = append(cur, x.Title)
			}
			if !pg.HasNext {
				break
			}
			opts.After = pg.Next
		}
		check("Cursor", fmt.Sprint(cur) == fmt.Sprint(want), cur)
		var desc []string
		dopts := orm.CursorOpts{Limit: 3, Sort: "-title"}
		for {
			pg, err := notes.Cursor(ctx, dopts)
			must(t, err)
			for _, x := range pg.Items {
				desc = append(desc, x.Title)
			}
			if !pg.HasNext {
				break
			}
			dopts.After = pg.Next
		}
		sorted := append([]string(nil), want...)
		sort.Sort(sort.Reverse(sort.StringSlice(sorted)))
		check("Cursor sorted desc", fmt.Sprint(desc) == fmt.Sprint(sorted), desc)

		// Scope composes with calls.
		popular := notes.Scope(q.Gt("views", 0))
		n, _ = popular.Count(ctx)
		check("Scope", n == 2, n) // a3 (views 9) and b (+1 from UpdateAny, then restored)

		// Cached reads see writes.
		c1, err := notes.FindAll(orm.Cached(ctx, time.Minute), q.Eq("title", "d"))
		must(t, err)
		d := c1[0]
		d.Views = 42
		must(t, notes.Update(ctx, &d))
		c2, err := notes.FindAll(orm.Cached(ctx, time.Minute), q.Eq("title", "d"))
		must(t, err)
		check("cache invalidated on update", c2[0].Views == 42, c2[0].Views)

		// A failed transaction leaves nothing.
		boom := errors.New("boom")
		err = db.Transaction(ctx, func(ctx context.Context) error {
			if err := notes.Insert(ctx, &Note{Title: "tx"}); err != nil {
				return err
			}
			x, _ := notes.FindByID(ctx, a.ID)
			x.Title = "tx changed"
			if err := notes.Update(ctx, &x); err != nil {
				return err
			}
			return boom
		})
		check("tx error", errors.Is(err, boom), err)
		ok, _ = notes.Exists(ctx, q.Eq("title", "tx"))
		reread, _ = notes.FindByID(ctx, a.ID)
		check("tx rolled back", !ok && reread.Title == "a3", ok, reread.Title)
	})
}
