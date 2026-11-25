package gen

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mhdaslam790/kiln/tenant"
)

// TenantRLSOptions configures TenantRLS.
type TenantRLSOptions struct {
	// Column overrides kiln.json's tenant_column.
	Column string
}

// TenantRLS writes a migration that turns on PostgreSQL row-level security
// for the given tables, or for every table with the tenant column. The
// migration is plain SQL in your project: read it, edit it, own it.
func (p *Project) TenantRLS(tables []string, o TenantRLSOptions) error {
	if p.Config.Dialect != "postgres" {
		return fmt.Errorf("row-level security is PostgreSQL only (dialect is %q)", p.Config.Dialect)
	}
	col := o.Column
	if col == "" {
		col = p.Config.TenantColumn
	}
	if col == "" {
		return errors.New(`no tenant column: set "tenant_column" in kiln.json or pass --column`)
	}
	if len(tables) == 0 {
		found, err := p.tenantTables(col)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("no table has a %s column yet", col)
		}
		tables = found
	}

	up := []string{
		fmt.Sprintf("// PostgreSQL row-level security on %s (kiln tenant rls): the database", col),
		"// itself hides other tenants' rows. Needs tenant.Plugin(..., tenant.WithRLS())",
		"// and an app role that is not a superuser and has no BYPASSRLS; cross-tenant",
		"// work (tenant.Bypass) connects as a separate role that has BYPASSRLS.",
	}
	var down []string
	for i, t := range tables {
		tu, td := tenant.RLSPolicy(t, col)
		if i > 0 {
			up = append(up, "")
		}
		for _, q := range tu {
			up = append(up, "s.Exec(`"+q+"`)")
		}
		var d []string
		for _, q := range td {
			d = append(d, "s.Exec(`"+q+"`)")
		}
		down = append(d, down...) // undo in reverse table order
	}
	if _, err := p.writeMigration("tenant_rls", up, down); err != nil {
		return err
	}
	p.Out("row-level security for: %s", strings.Join(tables, ", "))
	p.Out("next:")
	p.Out("  1. kiln migrate up")
	p.Out(`  2. orm.Open(..., tenant.Plugin(%q, tenant.WithRLS()))`, col)
	p.Out("  3. run tenant work inside db.Transaction (e.g. one per request)")
	p.Out("  4. connect as a role that is not a superuser (superusers skip RLS)")
	p.Out("  5. tenant.Bypass work: a second *orm.DB connecting as a role WITH BYPASSRLS")
	p.Out("new tenant tables later: kiln tenant rls <table>")
	return nil
}

// tenantTables lists the tables with col after all migrations, by running
// a small program against the project's migrations package.
func (p *Project) tenantTables(col string) ([]string, error) {
	if err := p.ensureMigrationsPkg(); err != nil {
		return nil, err
	}
	src := fmt.Sprintf(`package main

import (
	"encoding/json"
	"os"

	%q
	migs %q
)

func main() {
	out := map[string]any{}
	tables, err := migrate.TenantTables(migs.Set.List(), %q)
	if err != nil {
		out["error"] = err.Error()
	}
	out["tables"] = tables
	json.NewEncoder(os.Stdout).Encode(out)
}
`, libPath+"/migrate", p.importPath(p.Config.Migrations), col)
	var out struct {
		Tables []string `json:"tables"`
		Error  string   `json:"error"`
	}
	if err := p.runProgram(src, nil, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	return out.Tables, nil
}
