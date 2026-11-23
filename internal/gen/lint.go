package gen

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// LintOptions configures Lint.
type LintOptions struct {
	All    bool   // every migration, not just pending / the newest
	Strict bool   // fail on warnings too
	DSN    string // database URL; with one, pending migrations are checked
}

// lintFinding mirrors migrate.Finding as decoded from the helper program.
type lintFinding struct {
	Version  int64
	Name     string
	Check    string
	Severity int // 0 info, 1 warning, 2 error
	Table    string
	Column   string
	Message  string
	Fix      string
}

type lintOutput struct {
	Findings []lintFinding `json:"findings"`
	Checked  int           `json:"checked"`
	Error    string        `json:"error"`
}

// Lint checks migrations for risky operations. With a database URL
// (--dsn or DATABASE_URL) it checks the pending migrations, otherwise the
// newest one; --all checks every migration.
func (p *Project) Lint(o LintOptions) error {
	if err := p.ensureMigrationsPkg(); err != nil {
		return err
	}
	dsn := o.DSN
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	useDB := dsn != "" && !o.All
	d := drivers[p.Config.Dialect]

	var src strings.Builder
	src.WriteString("package main\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n\t\"os\"\n\n")
	fmt.Fprintf(&src, "\t%q\n\t%q\n\t%q\n", libPath+"/migrate", libPath+"/orm", libPath+"/dialect")
	fmt.Fprintf(&src, "\tmigs %q\n", p.importPath(p.Config.Migrations))
	if useDB {
		fmt.Fprintf(&src, "\t_ %q\n", d.Import)
	}
	src.WriteString(")\n\nvar _ = orm.Open\nvar _ = context.Background\n\n")
	src.WriteString(`func main() {
	out := map[string]any{}
	defer func() { json.NewEncoder(os.Stdout).Encode(out) }()
	list := migs.Set.List()
	var fs []migrate.Finding
	var err error
`)
	switch {
	case useDB:
		fmt.Fprintf(&src, `	ctx := context.Background()
	db, cerr := orm.Connect(ctx, %q, os.Getenv("DATABASE_URL"), dialect.%s())
	if cerr != nil {
		out["error"] = cerr.Error()
		return
	}
	defer db.Close()
	m := migrate.New(db, list)
	pending, perr := m.Pending(ctx)
	if perr != nil {
		out["error"] = perr.Error()
		return
	}
	out["checked"] = len(pending)
	fs, err = m.LintPending(ctx)
`, d.Name, d.Dialect)
	case o.All:
		fmt.Fprintf(&src, "\tout[\"checked\"] = len(list)\n\tfs, err = migrate.Lint(dialect.Kind(%q), list, nil)\n", p.Config.Dialect)
	default: // newest migration only
		fmt.Fprintf(&src, `	var newest int64
	for _, m := range list {
		if m.Version > newest {
			newest = m.Version
		}
	}
	if len(list) > 0 {
		out["checked"] = 1
	}
	fs, err = migrate.Lint(dialect.Kind(%q), list, func(m migrate.Migration) bool { return m.Version == newest })
`, p.Config.Dialect)
	}
	src.WriteString(`	if err != nil {
		out["error"] = err.Error()
	}
	out["findings"] = fs
}
`)
	var env []string
	if o.DSN != "" {
		env = append(env, "DATABASE_URL="+o.DSN)
	}
	var out lintOutput
	if err := p.runProgram(src.String(), env, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	scope := "the newest migration"
	switch {
	case useDB:
		scope = fmt.Sprintf("%d pending migration(s)", out.Checked)
	case o.All:
		scope = fmt.Sprintf("%d migration(s)", out.Checked)
	}
	if out.Checked == 0 {
		p.Out("no migrations to check")
		return nil
	}
	p.printFindings(out.Findings)
	errs, warns, infos := countFindings(out.Findings)
	p.Out("checked %s: %d error(s), %d warning(s), %d info", scope, errs, warns, infos)
	if !o.All && !useDB {
		p.Out("(set DATABASE_URL in .env to check pending migrations, or use --all)")
	}
	if errs > 0 || (o.Strict && warns > 0) {
		return errors.New("lint failed")
	}
	return nil
}

var severityName = []string{"INFO", "WARN", "ERROR"}

// printFindings prints findings grouped by migration.
func (p *Project) printFindings(fs []lintFinding) {
	last := int64(-1)
	for _, f := range fs {
		if f.Version != last {
			if f.Version == 0 {
				p.Out("\nnew migration")
			} else {
				p.Out("\n%d_%s", f.Version, f.Name)
			}
			last = f.Version
		}
		target := f.Table
		if f.Column != "" {
			target += "." + f.Column
		}
		sev := "INFO"
		if f.Severity >= 0 && f.Severity < len(severityName) {
			sev = severityName[f.Severity]
		}
		line := fmt.Sprintf("  %-5s %s", sev, f.Check)
		if target != "" {
			line += "  " + target
		}
		p.Out("%s\n        %s", line, f.Message)
		if f.Fix != "" {
			p.Out("        fix: %s", f.Fix)
		}
		p.Out("        (reviewed? add s.AllowUnsafe(\"why\", %q) to the migration)", f.Check)
	}
	if len(fs) > 0 {
		p.Out("")
	}
}

func countFindings(fs []lintFinding) (errs, warns, infos int) {
	for _, f := range fs {
		switch f.Severity {
		case 2:
			errs++
		case 1:
			warns++
		default:
			infos++
		}
	}
	return
}
