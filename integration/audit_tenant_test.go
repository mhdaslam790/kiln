package integration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mhdaslam790/kiln/audit"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
	"github.com/mhdaslam790/kiln/tenant"
)

// TestAuditTenant: audit.Tenant stores the record's tenant, else the
// context's, else NULL, as text on every database.
func TestAuditTenant(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		return []orm.Option{audit.Plugin(audit.Tenant("tenant_id"))}
	}, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		table := migrate.Migration{Version: 20990110000002, Name: "audit_tenant",
			Up: func(s *schema.Schema) {
				s.CreateTable("audit_log", func(t *schema.Table) {
					t.ID("id")
					t.String("table_name", schema.Required())
					t.String("record_id", schema.Required())
					t.String("action", schema.Required())
					t.String("actor")
					t.Text("changes")
					t.DateTime("created_at", schema.Required())
					t.String("tenant_id")
				})
			},
			Down: func(s *schema.Schema) { s.DropTable("audit_log") },
		}
		m := migrate.New(db, append(migrations(), table))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })

		u := User{TenantID: 7, Name: "ann", Email: "audit-tenant@x.io"}
		must(t, orm.For[User](db).Insert(ctx, &u))                                                     // the record's own tenant
		must(t, orm.For[Post](db).Insert(tenant.With(ctx, int64(9)), &Post{UserID: u.ID, Title: "p"})) // no column: the context's
		must(t, orm.For[Post](db).Insert(ctx, &Post{UserID: u.ID, Title: "q"}))                        // neither: NULL

		var rows []struct {
			Table  string         `db:"table_name"`
			Tenant sql.NullString `db:"tenant_id"`
		}
		must(t, db.Query(ctx, &rows, "SELECT table_name, tenant_id FROM audit_log ORDER BY id"))
		if len(rows) != 3 || rows[0].Tenant.String != "7" || rows[1].Tenant.String != "9" || rows[2].Tenant.Valid {
			t.Fatalf("audit tenants: %+v", rows)
		}
	})
}
