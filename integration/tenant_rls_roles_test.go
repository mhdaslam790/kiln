package integration

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/mhdaslam790/kiln/audit"
	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
	"github.com/mhdaslam790/kiln/tenant"
)

// TestTenantRLSRoles connects as the two roles a real deployment uses: an
// app role subject to row-level security, and an admin role with
// BYPASSRLS for cross-tenant work. Audit entries carry their tenant and are
// kept apart by the same policies.
func TestTenantRLSRoles(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run PostgreSQL tests")
	}
	ctx := context.Background()
	owner := open(t, targets[1]) // the test's (superuser) connection owns the tables

	usersUp, usersDown := tenant.RLSPolicy("users", "tenant_id")
	auditUp, auditDown := tenant.RLSPolicy("audit_log", "tenant_id")
	mig := migrate.Migration{Version: 20990110000001, Name: "rls_roles",
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
			for _, q := range append(usersUp, auditUp...) {
				s.Exec(q)
			}
			for name, attr := range map[string]string{"kiln_rls_user": "NOBYPASSRLS", "kiln_rls_admin": "BYPASSRLS"} {
				s.Exec(`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + name + `') THEN CREATE ROLE ` + name + ` LOGIN PASSWORD 'kiln' ` + attr + `; END IF; END $$`)
				s.Exec(`GRANT SELECT, INSERT, UPDATE, DELETE ON users, audit_log TO ` + name)
				s.Exec(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO ` + name)
			}
		},
		Down: func(s *schema.Schema) {
			s.Exec(`REVOKE ALL ON users FROM kiln_rls_user, kiln_rls_admin`)
			for _, q := range append(auditDown, usersDown...) {
				s.Exec(q)
			}
			s.DropTable("audit_log")
		},
	}
	m := migrate.New(owner, append(migrations(), mig))
	_, err := m.Up(ctx)
	must(t, err)
	t.Cleanup(func() { _, _ = m.Down(ctx, 1) })

	as := func(role string, opts ...orm.Option) *orm.DB {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Skipf("PG_DSN is not a URL: %v", err)
		}
		u.User = url.UserPassword(role, "kiln")
		sqlDB, err := sql.Open("pgx", u.String())
		must(t, err)
		t.Cleanup(func() { sqlDB.Close() })
		if err := sqlDB.PingContext(ctx); err != nil {
			t.Skipf("cannot log in as %s (pg_hba?): %v", role, err)
		}
		return orm.Open(sqlDB, dialect.Postgres(), opts...)
	}
	auditing := audit.Plugin(audit.Tenant("tenant_id"))
	app := as("kiln_rls_user", tenant.Plugin("tenant_id", tenant.WithRLS()), auditing)
	admin := as("kiln_rls_admin", tenant.Plugin("tenant_id"), auditing)

	acme, globex := tenant.With(ctx, 1), tenant.With(ctx, 2)
	users := orm.For[User](app)
	for _, c := range []struct {
		ctx   context.Context
		email string
	}{{acme, "roles-a@x.io"}, {acme, "roles-b@x.io"}, {globex, "roles-g@x.io"}} {
		must(t, app.Transaction(c.ctx, func(ctx context.Context) error {
			return users.Insert(ctx, &User{Name: "u", Email: c.email})
		}))
	}

	// The app role can't bypass: refused up front, not an empty result.
	err = app.Transaction(tenant.Bypass(ctx), func(ctx context.Context) error {
		_, err := users.Count(ctx)
		return err
	})
	if !errors.Is(err, tenant.ErrBypassRole) {
		t.Fatalf("Bypass as the app role: %v", err)
	}

	// Each tenant sees its own audit history, even through raw SQL.
	count := func(db *orm.DB, ctx context.Context, sql string) (n int64) {
		t.Helper()
		must(t, db.Transaction(ctx, func(ctx context.Context) error { return db.Query(ctx, &n, sql) }))
		return n
	}
	if n := count(app, acme, "SELECT COUNT(*) FROM audit_log"); n != 2 {
		t.Errorf("acme sees %d audit entries, want 2", n)
	}
	if n := count(app, globex, "SELECT COUNT(*) FROM audit_log WHERE tenant_id = '1'"); n != 0 {
		t.Errorf("globex sees %d of acme's audit entries", n)
	}

	// The admin connection works across tenants; its own writes are audited too.
	adminUsers := orm.For[User](admin)
	if n, err := adminUsers.Count(tenant.Bypass(ctx)); err != nil || n != 3 {
		t.Fatalf("admin Bypass count = %d, %v", n, err)
	}
	must(t, adminUsers.Insert(tenant.Bypass(ctx), &User{TenantID: 2, Name: "by admin", Email: "roles-admin@x.io"}))
	if n := count(admin, tenant.Bypass(ctx), "SELECT COUNT(*) FROM audit_log WHERE tenant_id = '2'"); n != 2 {
		t.Errorf("globex audit entries = %d, want 2 (one by the admin)", n)
	}
	if n := count(app, globex, "SELECT COUNT(*) FROM users"); n != 2 {
		t.Errorf("globex sees %d users, want 2", n)
	}
}
