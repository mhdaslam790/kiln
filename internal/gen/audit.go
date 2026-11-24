package gen

import "fmt"

// AuditInitOptions configures AuditInit.
type AuditInitOptions struct {
	Table string // default "audit_log"
}

// AuditInit writes a migration creating the table the audit module
// (github.com/mhdaslam790/kiln/audit) writes to. The migration is yours to
// change: e.g. make changes JSONB on PostgreSQL, or add a tenant column.
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
		`	t.Index([]string{"table_name", "record_id"}) // history of one record`,
		`	t.Index([]string{"created_at"})`,
		"})",
	}
	down := []string{fmt.Sprintf("s.DropTable(%q)", table)}
	if _, err := p.writeMigration("create_"+table, up, down); err != nil {
		return err
	}
	p.Out("next:")
	p.Out("  1. kiln migrate up")
	p.Out("  2. go get github.com/mhdaslam790/kiln/audit")
	if table == "audit_log" {
		p.Out("  3. orm.Open(..., audit.Plugin(audit.Actor(userFromCtx)))")
	} else {
		p.Out("  3. orm.Open(..., audit.Plugin(audit.Table(%q), audit.Actor(userFromCtx)))", table)
	}
	return nil
}
