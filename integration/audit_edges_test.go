package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/audit"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/tenant"
)

type auditEntry struct {
	TableName string    `db:"table_name"`
	RecordID  string    `db:"record_id"`
	Action    string    `db:"action"`
	Changes   string    `db:"changes"`
	CreatedAt time.Time `db:"created_at"`
}

// TestAuditEdges: audit must not change what callers see, and must log
// exactly the writes that happened.
func TestAuditEdges(t *testing.T) {
	clock := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{audit.Plugin(), tenant.Plugin("tenant_id"), orm.WithClock(func() time.Time { return clock })}
	}, func(t *testing.T, db *orm.DB) {
		bg := context.Background()
		m := migrate.New(db, append(migrations(), auditTable, notesTable))
		_, err := m.Up(bg)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(bg, 2) })
		ctx := tenant.With(bg, int64(1))
		notes, tags := orm.For[Note](db), orm.For[PostTag](db)
		logged := func() []auditEntry {
			var rows []auditEntry
			must(t, db.Query(bg, &rows, "SELECT table_name, record_id, action, changes, created_at FROM audit_log ORDER BY id"))
			return rows
		}
		probe := func(name string, fn func() error) {
			t.Helper()
			if err := fn(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}

		n := Note{Title: "a"}
		must(t, notes.Insert(ctx, &n))
		probe("audit time uses the DB clock (WithClock)", func() error {
			r := logged()
			if len(r) != 1 || !r[0].CreatedAt.Equal(clock) {
				return fmt.Errorf("%+v", r)
			}
			return nil
		})
		probe("update of a missing row behaves as without audit", func() error {
			err := notes.Update(ctx, &Note{ID: 999999, Title: "ghost", Version: 1})
			if !errors.Is(err, orm.ErrStale) { // Note has a version lock
				return fmt.Errorf("got %v, want ErrStale", err)
			}
			if len(logged()) != 1 {
				return errors.New("logged an update that wrote nothing")
			}
			return nil
		})
		probe("update of a missing row without a lock: nil, not logged", func() error {
			before := len(logged())
			err := orm.For[User](db).Update(ctx, &User{ID: 999999, Name: "ghost", Email: "g@x.io"})
			if err != nil {
				return err
			}
			if len(logged()) != before {
				return errors.New("logged an update of a row that doesn't exist")
			}
			return nil
		})
		probe("stale update is ErrStale and not logged", func() error {
			stale := n
			stale.Version = 0
			stale.Title = "lost"
			if err := notes.Update(ctx, &stale); !errors.Is(err, orm.ErrStale) {
				return fmt.Errorf("got %v", err)
			}
			if len(logged()) != 1 {
				return errors.New("logged a refused update")
			}
			return nil
		})
		probe("update that changes nothing logs nothing", func() error {
			cur, _ := notes.FindByID(ctx, n.ID)
			before := len(logged())
			if err := notes.Update(ctx, &cur, "Title"); err != nil {
				return err
			}
			if got := logged(); len(got) != before {
				return fmt.Errorf("logged %s", got[len(got)-1].Changes)
			}
			return nil
		})
		probe("another tenant's update is refused and not logged", func() error {
			cur, _ := notes.FindByID(ctx, n.ID)
			cur.Title = "hijack"
			before := len(logged())
			err := notes.Update(tenant.With(bg, int64(2)), &cur)
			if err == nil {
				return errors.New("cross-tenant update succeeded")
			}
			if len(logged()) != before {
				return errors.New("logged a refused update")
			}
			return nil
		})
		probe("failed write inside a transaction leaves no audit row", func() error {
			before := len(logged())
			boom := errors.New("boom")
			err := db.Transaction(ctx, func(ctx context.Context) error {
				if err := notes.Insert(ctx, &Note{Title: "tx"}); err != nil {
					return err
				}
				return boom
			})
			if !errors.Is(err, boom) || len(logged()) != before {
				return fmt.Errorf("err %v, rows %d -> %d", err, before, len(logged()))
			}
			return nil
		})
		probe("InsertAll logs each record with its id", func() error {
			before := len(logged())
			batch := []Note{{Title: "x"}, {Title: "y"}}
			if err := notes.InsertAll(ctx, batch); err != nil {
				return err
			}
			r := logged()[before:]
			if len(r) != 2 || r[0].RecordID != fmt.Sprint(batch[0].ID) || r[1].RecordID != fmt.Sprint(batch[1].ID) {
				return fmt.Errorf("%+v", r)
			}
			return nil
		})
		probe("composite key record id", func() error {
			users := orm.For[User](db)
			u := User{Name: "u", Email: "u@x.io"}
			if err := users.Insert(ctx, &u); err != nil {
				return err
			}
			p := Post{UserID: u.ID, Title: "p"}
			if err := orm.For[Post](db).Insert(ctx, &p); err != nil {
				return err
			}
			before := len(logged())
			if err := tags.Insert(ctx, &PostTag{PostID: p.ID, Tag: "go"}); err != nil {
				return err
			}
			r := logged()[before:]
			if len(r) != 1 || r[0].RecordID != fmt.Sprint(p.ID)+",go" {
				return fmt.Errorf("%+v", r)
			}
			return nil
		})
		probe("soft delete logs a delete", func() error {
			before := len(logged())
			cur, _ := notes.FindByID(ctx, n.ID)
			if err := notes.Delete(ctx, &cur); err != nil {
				return err
			}
			r := logged()[before:]
			if len(r) != 1 || r[0].Action != "delete" {
				return fmt.Errorf("%+v", r)
			}
			return nil
		})
		probe("UpdateAny is not logged (documented) and still works", func() error {
			k, err := notes.UpdateAny(ctx, q.Eq("title", "x"), q.Set("views", 3))
			if err != nil || k != 1 {
				return fmt.Errorf("%d %v", k, err)
			}
			return nil
		})
	})
}
