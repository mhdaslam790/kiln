package migrate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/schema"
)

// Severity ranks a lint finding.
type Severity int

const (
	// Info is worth knowing but usually fine.
	Info Severity = iota
	// Warning may break production: data loss, locks or a deploy-time
	// incompatibility with the running code.
	Warning
	// Error will fail or corrupt data on a realistic database.
	Error
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	}
	return "info"
}

// Finding is one problem found by Lint.
type Finding struct {
	Version  int64
	Name     string // migration name
	Check    string // e.g. "drop-column"; silence it with s.AllowUnsafe(reason, check)
	Severity Severity
	Table    string
	Column   string
	Message  string
	Fix      string // how to do it safely
}

func (f Finding) String() string {
	target := f.Table
	if f.Column != "" {
		target += "." + f.Column
	}
	s := fmt.Sprintf("%s %s: %s: %s", f.Severity, f.Check, target, f.Message)
	if f.Fix != "" {
		s += "\n  fix: " + f.Fix
	}
	return s
}

// Checks lists every lint check with its severity, for documentation.
var Checks = map[string]Severity{
	"not-null-without-default": Error,
	"concurrent-index-mixed":   Error,
	"drop-table":               Warning,
	"drop-column":              Warning,
	"rename-table":             Warning,
	"rename-column":            Warning,
	"change-column-type":       Warning,
	"set-not-null":             Warning,
	"unique-on-existing":       Warning,
	"index-locks-writes":       Warning,
	"foreign-key-on-existing":  Info,
	"raw-sql":                  Info,
	"irreversible":             Info,
	"allow-unsafe-no-reason":   Error,
}

// Lint checks migrations for operations that are risky on a production
// database: data loss, long locks, statements that fail once tables hold
// rows, and changes that break the previous app version during a deploy.
//
// history is every migration (in any order); only those for which check
// returns true are reported (nil checks all). kind enables
// database-specific checks; pass "" for portable checks only.
func Lint(kind dialect.Kind, history []Migration, check func(Migration) bool) ([]Finding, error) {
	sorted := append([]Migration(nil), history...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	state := schema.NewState()
	var out []Finding
	for _, mg := range sorted {
		var s schema.Schema
		mg.Up(&s)
		if check == nil || check(mg) {
			out = append(out, lintOps(kind, state, mg, s.Ops)...)
		}
		for _, op := range s.Ops {
			if err := state.Apply(op); err != nil {
				return out, fmt.Errorf("replaying %d_%s: %w", mg.Version, mg.Name, err)
			}
		}
	}
	return out, nil
}

// LintOps checks the operations of one migration against the schema
// produced by the migrations before it.
func LintOps(kind dialect.Kind, before *schema.State, mg Migration, ops []schema.Op) []Finding {
	return lintOps(kind, before, mg, ops)
}

func lintOps(kind dialect.Kind, before *schema.State, mg Migration, ops []schema.Op) []Finding {
	var out []Finding
	created := map[string]bool{} // tables created by this migration: empty, no risk
	allowed := map[string]bool{}
	allowAll := false
	add := func(check, table, column, msg, fix string) {
		out = append(out, Finding{
			Version: mg.Version, Name: mg.Name, Check: check, Severity: Checks[check],
			Table: table, Column: column, Message: msg, Fix: fix,
		})
	}
	existing := func(table string) bool { return before.Tables[table] != nil && !created[table] }
	column := func(table, name string) *schema.Column {
		if t := before.Tables[table]; t != nil {
			return t.Column(name)
		}
		return nil
	}

	concurrent, others := 0, 0
	for _, op := range ops {
		switch o := op.(type) {
		case schema.AllowUnsafe:
			if strings.TrimSpace(o.Reason) == "" {
				add("allow-unsafe-no-reason", "", "", "AllowUnsafe needs a reason", `s.AllowUnsafe("why this is safe", "check-name")`)
			}
			if len(o.Checks) == 0 {
				allowAll = true
			}
			for _, c := range o.Checks {
				allowed[c] = true
			}
			continue
		case schema.CreateIndex:
			if o.Concurrently {
				concurrent++
			} else {
				others++
			}
		default:
			others++
		}

		switch o := op.(type) {
		case schema.CreateTable:
			created[o.Name] = true

		case schema.DropTable:
			if before.Tables[o.Name] != nil {
				add("drop-table", o.Name, "", "drops the table and all its data",
					"deploy code that no longer uses it first; consider renaming it to keep the data for a while")
			}

		case schema.RenameTable:
			if existing(o.Name) {
				add("rename-table", o.Name, "", "renaming breaks the app version still running during the deploy",
					"create the new table, copy and dual-write, switch the code, then drop the old one")
			}

		case schema.AddColumn:
			c := o.Column
			if existing(o.Table) && c.Required && !c.HasDef && !c.Primary {
				add("not-null-without-default", o.Table, c.Name,
					"adding a NOT NULL column without a default fails as soon as the table has rows",
					"add a default (schema.Default(...)), or add it as NULL, backfill, then make it NOT NULL")
			}
			if existing(o.Table) && c.Unique && c.HasDef {
				add("unique-on-existing", o.Table, c.Name,
					"every existing row gets the same default, so the unique constraint fails once the table has two rows",
					"add the column without unique, backfill distinct values, then add a unique index")
			}
			if existing(o.Table) && c.Ref != nil {
				add("foreign-key-on-existing", o.Table, c.Name, "adding a foreign key checks every existing row while holding locks",
					"run it when traffic is low on big tables")
			}

		case schema.DropColumn:
			if column(o.Table, o.Name) != nil && existing(o.Table) {
				add("drop-column", o.Table, o.Name, "drops the column and its data; the app version still running during the deploy breaks if it uses it",
					"first deploy code that no longer reads or writes it, then drop it in a later release")
			}

		case schema.RenameColumn:
			if existing(o.Table) {
				add("rename-column", o.Table, o.Name, fmt.Sprintf("renaming to %q breaks the app version still running during the deploy", o.NewName),
					"add the new column, backfill and write both, switch reads, then drop the old one")
			}

		case schema.ChangeColumn:
			cur := column(o.Table, o.Column.Name)
			if cur == nil || !existing(o.Table) {
				break
			}
			if cur.Type != o.Column.Type || cur.SQLType != o.Column.SQLType || cur.Limit != o.Column.Limit ||
				cur.Precision != o.Column.Precision || cur.Scale != o.Column.Scale {
				add("change-column-type", o.Table, o.Column.Name,
					fmt.Sprintf("changing the type (%s) rewrites the table under a lock and can lose data if it narrows", typeDesc(cur, &o.Column)),
					"add a new column, backfill it in batches, switch the code, then drop the old one")
			}
			if o.Column.Required && !cur.Required {
				add("set-not-null", o.Table, o.Column.Name, "making the column NOT NULL fails if any row still has NULL",
					fmt.Sprintf("backfill first: UPDATE %s SET %s = ... WHERE %s IS NULL", o.Table, o.Column.Name, o.Column.Name))
			}

		case schema.CreateIndex:
			if !existing(o.Table) {
				break
			}
			if o.Unique {
				add("unique-on-existing", o.Table, strings.Join(o.Columns, ","),
					"a unique index on an existing table fails if duplicates exist",
					fmt.Sprintf("check first: SELECT %s, COUNT(*) FROM %s GROUP BY %s HAVING COUNT(*) > 1",
						strings.Join(o.Columns, ", "), o.Table, strings.Join(o.Columns, ", ")))
			}
			if kind == dialect.KindPostgres && !o.Concurrently {
				add("index-locks-writes", o.Table, strings.Join(o.Columns, ","),
					"CREATE INDEX blocks writes to the table until it finishes",
					"use schema.Concurrently() in a migration of its own")
			}

		case schema.AddForeignKey:
			if existing(o.Table) {
				add("foreign-key-on-existing", o.Table, strings.Join(o.Key.Columns, ","),
					"adding a foreign key checks every existing row while holding locks",
					"run it when traffic is low on big tables")
			}

		case schema.Exec, schema.Do:
			add("raw-sql", "", "", "raw SQL / Go steps cannot be checked", "review it by hand")
		}
	}

	if concurrent > 0 && others > 0 && kind == dialect.KindPostgres {
		add("concurrent-index-mixed", "", "",
			"a concurrent index runs outside a transaction, so the other changes in this migration are not atomic with it",
			"move the concurrent index into a migration of its own")
	}
	if mg.Down == nil {
		add("irreversible", "", "", "the migration has no Down and cannot be rolled back", "")
	}

	if allowAll || len(allowed) > 0 {
		kept := out[:0]
		for _, f := range out {
			if f.Check == "allow-unsafe-no-reason" || (!allowAll && !allowed[f.Check]) {
				kept = append(kept, f)
			}
		}
		out = kept
	}
	return out
}

func typeDesc(from, to *schema.Column) string {
	d := func(c *schema.Column) string {
		s := string(c.Type)
		if c.SQLType != "" {
			s = c.SQLType
		}
		switch {
		case c.Precision > 0:
			s += fmt.Sprintf("(%d,%d)", c.Precision, c.Scale)
		case c.Limit > 0:
			s += fmt.Sprintf("(%d)", c.Limit)
		}
		return s
	}
	return d(from) + " → " + d(to)
}

// HasErrors reports whether any finding is an error (or, with strict, a
// warning).
func HasErrors(fs []Finding, strict bool) bool {
	for _, f := range fs {
		if f.Severity == Error || (strict && f.Severity == Warning) {
			return true
		}
	}
	return false
}

// LintPending lints the migrations not yet applied to the database.
func (m *Migrator) LintPending(ctx context.Context) ([]Finding, error) {
	pending, err := m.Pending(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[int64]bool, len(pending))
	for _, p := range pending {
		want[p.Version] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	return Lint(m.db.Dialect().Kind(), m.list, func(mg Migration) bool { return want[mg.Version] })
}
