package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/mhdaslam790/kiln/dialect"
)

// SettingTenant is the PostgreSQL setting WithRLS sets in each transaction
// and RLSPolicy's policies read. It is transaction-local
// (set_config(..., true)), so a pooled connection never carries one
// request's tenant into the next.
const SettingTenant = "app.tenant_id"

// ErrBypassRole is returned with WithRLS for a Bypass transaction on a
// connection whose role is subject to row-level security: the policies
// have no bypass switch (any SQL could flip one), so cross-tenant work
// needs a second *orm.DB that connects as a role with BYPASSRLS.
var ErrBypassRole = errors.New("tenant: Bypass with row-level security needs a connection as a role with BYPASSRLS; open a separate admin *orm.DB for it")

// rlsPlugin is plugin plus AfterBegin; a separate type so that without
// WithRLS no transaction pays for a hook.
type rlsPlugin struct {
	*plugin
	canBypass atomic.Bool // the role was seen to have BYPASSRLS (or be a superuser)
}

// AfterBegin tells PostgreSQL the transaction's tenant. Without a tenant in
// ctx nothing is set and the policies show no rows: it fails safe.
func (p *rlsPlugin) AfterBegin(ctx context.Context, tx *sql.Tx) error {
	switch v := ctx.Value(ctxKey{}).(type) {
	case nil:
		return nil
	case bypass:
		if p.canBypass.Load() {
			return nil
		}
		var ok bool
		err := tx.QueryRowContext(ctx, "SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&ok)
		if err != nil {
			return fmt.Errorf("tenant: checking the role for Bypass: %w", err)
		}
		if !ok {
			return ErrBypassRole
		}
		p.canBypass.Store(true)
		return nil
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
// tenant. There is deliberately no bypass switch in the policy: a setting
// that turns it off could be set by any SQL the app runs, injected SQL
// included. Cross-tenant work connects as a role with BYPASSRLS instead.
//
// The column is compared as text, so any tenant id type works; queries keep
// using the index on the column because the plugin's own filter (column =
// $1) is still in the query. FORCE applies the policy to the table's owner
// too, so the app sees it even when it owns the tables.
func RLSPolicy(table, column string) (up, down []string) {
	d := dialect.Postgres()
	t, c := d.Quote(table), d.Quote(column)
	pred := fmt.Sprintf("%s::text = current_setting('%s', true)", c, SettingTenant)
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
