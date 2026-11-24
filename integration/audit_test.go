package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mhdaslam790/kiln/audit"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
)

type actorKey struct{}

// auditTable is what `kiln audit init` generates.
var auditTable = migrate.Migration{Version: 20990105000001, Name: "create_audit_log",
	Up: func(s *schema.Schema) {
		s.CreateTable("audit_log", func(t *schema.Table) {
			t.ID("id")
			t.String("table_name", schema.Required())
			t.String("record_id", schema.Required())
			t.String("action", schema.Required())
			t.String("actor")
			t.Text("changes")
			t.DateTime("created_at", schema.Required())
			t.Index([]string{"table_name", "record_id"})
			t.Index([]string{"created_at"})
		})
	},
	Down: func(s *schema.Schema) { s.DropTable("audit_log") },
}

type auditRow struct {
	ID       int64  `db:"id"`
	Table    string `db:"table_name"`
	RecordID string `db:"record_id"`
	Action   string `db:"action"`
	Actor    string `db:"actor"`
	Changes  string `db:"changes"`
}

func TestAudit(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{audit.Plugin(
			audit.Actor(func(ctx context.Context) string { s, _ := ctx.Value(actorKey{}).(string); return s }),
			audit.Redact("bio"),
		)}
	}, func(t *testing.T, db *orm.DB) {
		ctx := context.WithValue(context.Background(), actorKey{}, "admin@acme")
		m := migrate.New(db, append(migrations(), auditTable))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })
		users := orm.For[User](db)
		entries := func() []auditRow {
			t.Helper()
			var rows []auditRow
			must(t, db.Query(ctx, &rows, "SELECT id, table_name, record_id, action, actor, changes FROM audit_log ORDER BY id"))
			return rows
		}
		changes := func(r auditRow) map[string]any {
			t.Helper()
			var c map[string]any
			must(t, json.Unmarshal([]byte(r.Changes), &c))
			return c
		}

		secret := "secret bio"
		u := User{TenantID: 1, Name: "ann", Email: "audit-ann@x.io", Age: 30, Bio: &secret}
		must(t, users.Insert(ctx, &u))
		u.Age = 31
		must(t, users.Update(ctx, &u))
		must(t, users.Update(ctx, &u)) // nothing changed: no entry
		u.Name = "ann b"
		u.Email = "not written" // not in the field list: neither written nor recorded
		must(t, users.Update(ctx, &u, "Name"))
		must(t, users.Delete(ctx, &u)) // soft delete
		must(t, users.InsertAll(ctx, []User{{TenantID: 1, Name: "b1", Email: "audit-b1@x.io"}, {TenantID: 1, Name: "b2", Email: "audit-b2@x.io"}}))

		rows := entries()
		var got []string
		for _, r := range rows {
			got = append(got, r.Action)
			if r.Table != "users" || r.Actor != "admin@acme" {
				t.Errorf("entry %+v", r)
			}
		}
		if want := "insert update update delete insert insert"; join(got) != want {
			t.Fatalf("actions = %q, want %q", join(got), want)
		}
		if rows[0].RecordID == "" || rows[0].RecordID != rows[1].RecordID {
			t.Errorf("record ids: %q %q", rows[0].RecordID, rows[1].RecordID)
		}
		ins := changes(rows[0])
		if ins["email"] != "audit-ann@x.io" || ins["bio"] != "[redacted]" {
			t.Errorf("insert changes: %v", ins)
		}
		upd := changes(rows[1])
		if age, ok := upd["age"].([]any); !ok || len(upd) != 1 || age[0] != float64(30) || age[1] != float64(31) {
			t.Errorf("update changes = %v, want only age [30 31]", upd)
		}
		if part := changes(rows[2]); len(part) != 1 || part["name"] == nil {
			t.Errorf("partial update changes = %v, want only name", part)
		}
		if del := changes(rows[3]); del["name"] != "ann b" {
			t.Errorf("delete changes: %v", del)
		}

		// Atomic with the change: a rolled-back transaction leaves no entry...
		before := len(entries())
		boom := errors.New("boom")
		err = db.Transaction(ctx, func(ctx context.Context) error {
			must(t, users.Insert(ctx, &User{TenantID: 1, Name: "gone", Email: "audit-gone@x.io"}))
			return boom
		})
		if !errors.Is(err, boom) || len(entries()) != before {
			t.Fatalf("rolled-back insert left an entry (%v)", err)
		}
		// ...and a failing audit write undoes the change.
		broken := orm.Open(db.SQL(), db.Dialect(), audit.Plugin(audit.Table("no_such_audit_table")))
		if err := orm.For[User](broken).Insert(ctx, &User{TenantID: 1, Name: "x", Email: "audit-x@x.io"}); err == nil {
			t.Fatal("insert succeeded although its audit entry failed")
		}
		var n int64
		must(t, db.Query(ctx, &n, "SELECT COUNT(*) FROM users WHERE email = ?", "audit-x@x.io"))
		if n != 0 {
			t.Fatal("the change was kept without its audit entry")
		}
	})
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
