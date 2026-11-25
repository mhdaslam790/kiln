package gen

import (
	"fmt"
	"strings"
)

// AuditInitOptions configures AuditInit.
type AuditInitOptions struct {
	Table string // default "audit_log"
}

// AuditInit writes a migration creating the table the audit module
// (github.com/mhdaslam790/kiln/audit) writes to. The migration is yours to
// change: e.g. make changes JSONB on PostgreSQL. With a tenant_column in
// kiln.json the table gets that column too (filled by audit.Tenant), so
// `kiln tenant rls` covers it and each tenant sees only its own history.
func (p *Project) AuditInit(o AuditInitOptions) error {
	table := o.Table
	if table == "" {
		table = "audit_log"
	}
	up := []string{
		"// Audit entries written by github.com/mhdaslam790/kiln/audit (kiln audit init).",
		fmt.Sprintf("s.CreateTable(%q, func(t *schema.Table) {", table),
		`	t.ID("id")`,
		`	t.String("table_name", schema.Required())`,
		`	t.String("record_id", schema.Required())`,
		`	t.String("action", schema.Required()) // insert, update or delete`,
		`	t.String("actor")`,
		`	t.Text("changes") // JSON`,
		`	t.DateTime("created_at", schema.Required())`,
	}
	tc := p.Config.TenantColumn
	if tc != "" {
		up = append(up,
			fmt.Sprintf("	t.String(%q) // the entry's tenant (audit.Tenant); NULL for cross-tenant work", tc),
			fmt.Sprintf("	t.Index([]string{%q, \"created_at\"}) // a tenant's history", tc))
	}
	up = append(up,
		`	t.Index([]string{"table_name", "record_id"}) // history of one record`,
		`	t.Index([]string{"created_at"})`,
		"})",
	)
	down := []string{fmt.Sprintf("s.DropTable(%q)", table)}
	if _, err := p.writeMigration("create_"+table, up, down); err != nil {
		return err
	}
	p.Out("next:")
	p.Out("  1. kiln migrate up")
	p.Out("  2. go get github.com/mhdaslam790/kiln/audit")
	var opts []string
	if table != "audit_log" {
		opts = append(opts, fmt.Sprintf("audit.Table(%q)", table))
	}
	if tc != "" {
		opts = append(opts, fmt.Sprintf("audit.Tenant(%q)", tc))
	}
	opts = append(opts, "audit.Actor(userFromCtx)")
	p.Out("  3. orm.Open(..., audit.Plugin(%s))", strings.Join(opts, ", "))
	if tc != "" && p.Config.Dialect == "postgres" {
		p.Out("  with row-level security: kiln tenant rls %s", table)
	}
	return nil
}
