package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
	"github.com/mhdaslam790/kiln/tenant"
)

// Row-level security is enforced by PostgreSQL itself: raw SQL that skips
// the plugin still only sees, and can only write, the context's tenant.
func TestTenantRLS(t *testing.T) {
	forEachWith(t, func(t *testing.T) []orm.Option {
		if !strings.HasSuffix(t.Name(), "/postgres") {
			return nil
		}
		return []orm.Option{tenant.Plugin("tenant_id", tenant.WithRLS())}
	}, func(t *testing.T, db *orm.DB) {
		if !strings.HasSuffix(t.Name(), "/postgres") {
			t.Skip("row-level security is PostgreSQL only")
		}
		ctx := context.Background()
		up, down := tenant.RLSPolicy("users", "tenant_id")
		mig := migrate.Migration{Version: 20990104000001, Name: "tenant_rls",
			Up: func(s *schema.Schema) {
				for _, q := range up {
					s.Exec(q)
				}
				// Superusers bypass RLS: the test acts as an ordinary role.
				s.Exec(`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kiln_rls_app') THEN CREATE ROLE kiln_rls_app NOLOGIN; END IF; END $$`)
				s.Exec(`GRANT SELECT, INSERT, UPDATE, DELETE ON users TO kiln_rls_app`)
				s.Exec(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO kiln_rls_app`)
			},
			Down: func(s *schema.Schema) {
				s.Exec(`REVOKE ALL ON users FROM kiln_rls_app`)
				for _, q := range down {
					s.Exec(q)
				}
			},
		}
		m := migrate.New(db, append(migrations(), mig))
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() { _, _ = m.Down(ctx, 1) })

		users := orm.For[User](db)
		asApp := func(ctx context.Context, fn func(ctx context.Context) error) error {
			return db.Transaction(ctx, func(ctx context.Context) error {
				if _, err := db.Exec(ctx, "SET LOCAL ROLE kiln_rls_app"); err != nil {
					return err
				}
				return fn(ctx)
			})
		}
		rawCount := func(ctx context.Context) (n int64) {
			t.Helper()
			must(t, db.Query(ctx, &n, "SELECT COUNT(*) FROM users"))
			return n
		}

		must(t, asApp(tenant.Bypass(ctx), func(ctx context.Context) error {
			return users.InsertAll(ctx, []User{
				{TenantID: 1, Name: "a1", Email: "rls-a1@x.io"}, {TenantID: 1, Name: "a2", Email: "rls-a2@x.io"},
				{TenantID: 2, Name: "g1", Email: "rls-g1@x.io"},
			})
		}))

		acme := tenant.With(ctx, 1)
		must(t, asApp(acme, func(ctx context.Context) error {
			if n, err := users.Count(ctx); err != nil || n != 2 {
				t.Errorf("ORM count = %d, %v", n, err)
			}
			if n := rawCount(ctx); n != 2 { // the plugin never sees raw SQL: this is RLS
				t.Errorf("raw count for tenant 1 = %d, want 2", n)
			}
			return nil
		}))

		// Writing another tenant's row through raw SQL is refused by the database.
		err = asApp(acme, func(ctx context.Context) error {
			_, err := db.Exec(ctx, "INSERT INTO users (tenant_id, name, email, age) VALUES (2, 'x', 'rls-x@x.io', 0)")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Fatalf("cross-tenant raw insert: %v", err)
		}
		var moved int64
		must(t, asApp(acme, func(ctx context.Context) error {
			res, err := db.Exec(ctx, "UPDATE users SET name = 'hacked' WHERE tenant_id = 2")
			if err == nil {
				moved, err = res.RowsAffected()
			}
			return err
		}))
		if moved != 0 {
			t.Fatalf("raw update touched %d rows of another tenant", moved)
		}

		// No tenant: nothing visible (fail safe). Bypass: everything.
		must(t, asApp(ctx, func(ctx context.Context) error {
			if n := rawCount(ctx); n != 0 {
				t.Errorf("raw count without tenant = %d, want 0", n)
			}
			return nil
		}))
		must(t, asApp(tenant.Bypass(ctx), func(ctx context.Context) error {
			if n := rawCount(ctx); n != 3 {
				t.Errorf("raw count with Bypass = %d, want 3", n)
			}
			return nil
		}))

		// Outside a transaction the database would not know the tenant.
		if _, err := users.Count(acme); !errors.Is(err, tenant.ErrNoTransaction) {
			t.Fatalf("query outside a transaction: %v", err)
		}
		// A write kiln runs in its own transaction (User has After hooks) is
		// fine: that transaction gets the tenant too.
		n := User{Name: "n", Email: "rls-n@x.io"}
		must(t, users.Insert(acme, &n))
		if n.TenantID != 1 {
			t.Fatalf("insert in kiln's own transaction: tenant %d", n.TenantID)
		}
	})
}
