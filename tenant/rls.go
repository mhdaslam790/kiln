package tenant

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mhdaslam790/kiln/dialect"
)

// The PostgreSQL settings WithRLS sets in each transaction and RLSPolicy's
// policies read. They are transaction-local (set_config(..., true)), so a
// pooled connection never carries one request's tenant into the next.
const (
	SettingTenant = "app.tenant_id"
	SettingBypass = "app.tenant_bypass"
)

// rlsPlugin is plugin plus AfterBegin; a separate type so that without
// WithRLS no transaction pays for a hook.
type rlsPlugin struct{ *plugin }

// AfterBegin tells PostgreSQL the transaction's tenant. Without a tenant in
// ctx nothing is set and the policies show no rows: it fails safe.
func (p *rlsPlugin) AfterBegin(ctx context.Context, tx *sql.Tx) error {
	switch v := ctx.Value(ctxKey{}).(type) {
	case nil:
		return nil
	case bypass:
		_, err := tx.ExecContext(ctx, "SELECT set_config($1, 'on', true)", SettingBypass)
		return err
	default:
		_, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", SettingTenant, fmt.Sprint(deref(v)))
		return err
	}
}

// RLSPolicy returns the PostgreSQL statements that turn row-level security
// on for table (up) and off again (down). `kiln tenant rls` writes them into
// a migration in your project; use this to write one by hand.
//
// A row is visible and writable when its column equals the transaction's
// tenant, or in a Bypass transaction. The column is compared as text, so
// any tenant id type works; queries keep using the index on the column
// because the plugin's own filter (column = $1) is still in the query.
// FORCE applies the policy to the table's owner too, so the app sees it
// even when it owns the tables (superusers still bypass it).
func RLSPolicy(table, column string) (up, down []string) {
	d := dialect.Postgres()
	t, c := d.Quote(table), d.Quote(column)
	pred := fmt.Sprintf("current_setting('%s', true) = 'on' OR %s::text = current_setting('%s', true)", SettingBypass, c, SettingTenant)
	up = []string{
		"ALTER TABLE " + t + " ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + t + " FORCE ROW LEVEL SECURITY",
		fmt.Sprintf("CREATE POLICY kiln_tenant_isolation ON %s USING (%s) WITH CHECK (%s)", t, pred, pred),
	}
	down = []string{
		"DROP POLICY IF EXISTS kiln_tenant_isolation ON " + t,
		"ALTER TABLE " + t + " NO FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + t + " DISABLE ROW LEVEL SECURITY",
	}
	return up, down
}
