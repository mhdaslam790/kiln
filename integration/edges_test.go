package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/tenant"
)

// TestEdges probes boundary values and feature combinations that the
// per-feature tests don't cover. Each probe reports on its own.
func TestEdges(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		m := migrate.New(db, append(migrations(), notesTable))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })
		users := orm.For[User](db)
		posts := orm.For[Post](db)
		notes := orm.For[Note](db)
		probe := func(name string, fn func() error) {
			t.Helper()
			if err := fn(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}

		probe("InsertAll with no records", func() error { return users.InsertAll(ctx, nil) })
		probe("InsertAll with one record", func() error {
			one := []User{{TenantID: 1, Name: "solo", Email: "solo@x.io"}}
			if err := users.InsertAll(ctx, one); err != nil {
				return err
			}
			if one[0].ID == 0 {
				return errors.New("no id")
			}
			return nil
		})
		probe("FindAll on an empty result is an empty slice", func() error {
			got, err := users.FindAll(ctx, q.Eq("name", "nobody"))
			if err != nil {
				return err
			}
			if got == nil || len(got) != 0 {
				return fmt.Errorf("got %#v", got)
			}
			return nil
		})
		probe("In with no values matches nothing", func() error {
			got, err := users.FindAll(ctx, q.In("id"))
			if err != nil {
				return err
			}
			if len(got) != 0 {
				return fmt.Errorf("%d rows", len(got))
			}
			return nil
		})
		probe("Count/Exists/Paginate past the end", func() error {
			p, err := users.Paginate(ctx, 99, 10)
			if err != nil {
				return err
			}
			if len(p.Items) != 0 || p.Total == 0 || p.Page != 99 {
				return fmt.Errorf("%+v", p)
			}
			return nil
		})
		probe("Update with a zero primary key is an error, not a no-op", func() error {
			if err := users.Update(ctx, &User{Name: "x"}); err == nil {
				return errors.New("no error")
			}
			return nil
		})
		probe("Update of a missing row writes nothing and returns nil (documented)", func() error {
			if err := users.Update(ctx, &User{ID: 987654, Name: "ghost", Email: "g@x.io"}); err != nil {
				return fmt.Errorf("got %v", err)
			}
			if _, err := users.FindByID(ctx, 987654); !errors.Is(err, orm.ErrNotFound) {
				return fmt.Errorf("a row appeared: %v", err)
			}
			return nil
		})
		probe("Delete twice (soft) / Restore of a live row", func() error {
			u := User{TenantID: 1, Name: "twice", Email: "twice@x.io"}
			if err := users.Insert(ctx, &u); err != nil {
				return err
			}
			if err := users.Restore(ctx, &u); err != nil {
				return fmt.Errorf("restore live: %w", err)
			}
			if err := users.Delete(ctx, &u); err != nil {
				return err
			}
			err := users.Delete(ctx, &u)
			if err != nil && !errors.Is(err, orm.ErrNotFound) {
				return fmt.Errorf("second delete: %w", err)
			}
			return nil
		})
		probe("Unicode and long text round-trip", func() error {
			bio := strings.Repeat("ünïcødé ✓ 漢字 ", 500)
			u := User{TenantID: 1, Name: "Zoë ✓", Email: "zoe@x.io", Bio: &bio}
			if err := users.Insert(ctx, &u); err != nil {
				return err
			}
			got, err := users.FindByID(ctx, u.ID)
			if err != nil {
				return err
			}
			if got.Name != "Zoë ✓" || got.Bio == nil || *got.Bio != bio {
				return fmt.Errorf("name %q, bio len %d", got.Name, len(deref(got.Bio)))
			}
			return nil
		})
		probe("nil pointer and JSON fields", func() error {
			u := User{TenantID: 1, Name: "nil", Email: "nil@x.io", Prefs: map[string]any{"a": 1.5, "b": []any{"x"}}}
			if err := users.Insert(ctx, &u); err != nil {
				return err
			}
			got, err := users.FindByID(ctx, u.ID)
			if err != nil {
				return err
			}
			if got.Bio != nil || got.Prefs["a"] != 1.5 || fmt.Sprint(got.Prefs["b"]) != "[x]" {
				return fmt.Errorf("%+v", got)
			}
			got.Prefs = nil
			if err := users.Update(ctx, &got); err != nil {
				return err
			}
			again, _ := users.FindByID(ctx, u.ID)
			if again.Prefs != nil {
				return fmt.Errorf("prefs after nil update: %#v", again.Prefs)
			}
			return nil
		})
		probe("times keep their instant across databases", func() error {
			loc := time.FixedZone("UTC+5", 5*3600)
			at := time.Date(2026, 3, 4, 5, 6, 7, 0, loc)
			n := Note{TenantID: 1, Title: "tz", CreatedAt: at}
			if err := notes.Insert(ctx, &n); err != nil {
				return err
			}
			got, err := notes.FindByID(ctx, n.ID)
			if err != nil {
				return err
			}
			if !got.CreatedAt.Equal(at) {
				return fmt.Errorf("stored %v, read %v", at, got.CreatedAt)
			}
			return nil
		})
		probe("Cursor with many equal sort values visits each row once", func() error {
			for i := 0; i < 7; i++ {
				if err := notes.Insert(ctx, &Note{TenantID: 2, Title: "same"}); err != nil {
					return err
				}
			}
			seen := map[int64]int{}
			o := orm.CursorOpts{Limit: 2, Sort: "-title"}
			for i := 0; i < 20; i++ {
				pg, err := notes.Cursor(ctx, o, q.Eq("tenant_id", 2))
				if err != nil {
					return err
				}
				for _, x := range pg.Items {
					seen[x.ID]++
				}
				if !pg.HasNext {
					break
				}
				o.After = pg.Next
			}
			if len(seen) != 7 {
				return fmt.Errorf("saw %d distinct of 7", len(seen))
			}
			for id, c := range seen {
				if c != 1 {
					return fmt.Errorf("id %d seen %d times", id, c)
				}
			}
			return nil
		})
		probe("Cursor backwards (Prev) returns the previous page", func() error {
			o := orm.CursorOpts{Limit: 3, Sort: "title"}
			first, err := notes.Cursor(ctx, o, q.Eq("tenant_id", 2))
			if err != nil || !first.HasNext {
				return fmt.Errorf("first: %v %v", err, first.HasNext)
			}
			second, err := notes.Cursor(ctx, orm.CursorOpts{Limit: 3, Sort: "title", After: first.Next}, q.Eq("tenant_id", 2))
			if err != nil {
				return err
			}
			back, err := notes.Cursor(ctx, orm.CursorOpts{Limit: 3, Sort: "title", Before: second.Prev}, q.Eq("tenant_id", 2))
			if err != nil {
				return err
			}
			if ids(back.Items) != ids(first.Items) {
				return fmt.Errorf("back %v, first %v", ids(back.Items), ids(first.Items))
			}
			return nil
		})
		probe("Each stops on the callback's error", func() error {
			stop := errors.New("stop")
			calls := 0
			err := notes.Each(ctx, 2, func(Note) error { calls++; return stop })
			if !errors.Is(err, stop) || calls != 1 {
				return fmt.Errorf("err %v, calls %d", err, calls)
			}
			return nil
		})
		probe("nested transaction: inner rollback keeps outer work", func() error {
			inner := errors.New("inner")
			err := db.Transaction(ctx, func(ctx context.Context) error {
				if err := notes.Insert(ctx, &Note{TenantID: 3, Title: "outer"}); err != nil {
					return err
				}
				if err := db.Transaction(ctx, func(ctx context.Context) error {
					if err := notes.Insert(ctx, &Note{TenantID: 3, Title: "inner"}); err != nil {
						return err
					}
					return inner
				}); !errors.Is(err, inner) {
					return fmt.Errorf("inner err %v", err)
				}
				return nil
			})
			if err != nil {
				return err
			}
			got, _ := notes.FindAll(ctx, q.Eq("tenant_id", 3))
			if len(got) != 1 || got[0].Title != "outer" {
				return fmt.Errorf("got %v", got)
			}
			return nil
		})
		probe("panic in a transaction rolls back and re-panics", func() error {
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_ = db.Transaction(ctx, func(ctx context.Context) error {
					_ = notes.Insert(ctx, &Note{TenantID: 4, Title: "panic"})
					panic("boom")
				})
			}()
			n, err := notes.Count(ctx, q.Eq("tenant_id", 4))
			if recovered == nil || err != nil || n != 0 {
				return fmt.Errorf("recovered %v, count %d, err %v", recovered, n, err)
			}
			return nil
		})
		probe("cancelled context fails fast and leaves the pool usable", func() error {
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := users.FindAll(cctx); !errors.Is(err, context.Canceled) {
				return fmt.Errorf("cancelled: %v", err)
			}
			_, err := users.Count(ctx)
			return err
		})
		probe("Preload hides soft-deleted parents? (user of a post)", func() error {
			u := User{TenantID: 1, Name: "author", Email: "author@x.io"}
			if err := users.Insert(ctx, &u); err != nil {
				return err
			}
			p := Post{UserID: u.ID, Title: "by author"}
			if err := posts.Insert(ctx, &p); err != nil {
				return err
			}
			if err := users.Delete(ctx, &u); err != nil {
				return err
			}
			got, err := posts.FindByID(ctx, p.ID)
			if err != nil {
				return err
			}
			if err := posts.Preload(ctx, &got, "User"); err != nil {
				return err
			}
			if got.User != nil {
				return fmt.Errorf("preloaded a soft-deleted user: %+v", got.User)
			}
			return nil
		})
		probe("tenant + soft delete + Restore across tenants", func() error {
			tdb := orm.Open(db.SQL(), db.Dialect(), tenant.Plugin("tenant_id"))
			tn := orm.For[Note](tdb)
			a, b := tenant.With(ctx, int64(50)), tenant.With(ctx, int64(51))
			n := Note{Title: "mine"}
			if err := tn.Insert(a, &n); err != nil {
				return err
			}
			if err := tn.Delete(a, &n); err != nil {
				return err
			}
			if err := tn.Restore(b, &n); err != nil && !errors.Is(err, orm.ErrNotFound) {
				return fmt.Errorf("restore by other tenant: %w", err)
			}
			if _, err := tn.FindByID(a, n.ID); !errors.Is(err, orm.ErrNotFound) {
				return fmt.Errorf("another tenant restored the row (err %v)", err)
			}
			return tn.Restore(a, &n)
		})
		probe("Select specific columns into the model", func() error {
			got, err := users.FindAll(ctx, q.Select("id", "name"), q.Limit(1))
			if err != nil {
				return err
			}
			if len(got) != 1 || got[0].Name == "" || got[0].Email != "" {
				return fmt.Errorf("%+v", got)
			}
			return nil
		})
	})
}

func ids(ns []Note) string {
	var s []string
	for _, n := range ns {
		s = append(s, fmt.Sprint(n.ID))
	}
	return strings.Join(s, ",")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
