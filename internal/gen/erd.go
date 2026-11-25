package gen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ERDOptions configures ERD.
type ERDOptions struct {
	Tables []string // only these tables (default: all)
	DB     bool     // read the live database instead of replaying the migrations
	DSN    string
	Out    string // write here (.md: in a mermaid code block) instead of printing
}

// ERD draws the schema as a Mermaid entity-relationship diagram, from the
// migrations (no database needed) or with DB from the live database.
func (p *Project) ERD(o ERDOptions) error {
	if err := p.ensureMigrationsPkg(); err != nil {
		return err
	}
	var src strings.Builder
	src.WriteString("package main\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"os\"\n\n")
	fmt.Fprintf(&src, "\t%q\n\t%q\n\t%q\n\t%q\n", libPath+"/migrate", libPath+"/orm", libPath+"/dialect", libPath+"/schema")
	fmt.Fprintf(&src, "\tmigs %q\n", p.importPath(p.Config.Migrations))
	d := drivers[p.Config.Dialect]
	if o.DB {
		fmt.Fprintf(&src, "\t_ %q\n", d.Import)
	}
	src.WriteString(")\n\nvar _ = orm.Open\nvar _ = dialect.SQLite\nvar _ = context.Background\nvar _ = migs.Set\n\n")
	fmt.Fprintf(&src, "func main() {\n\ttables := %#v\n\tvar st *schema.State\n\tvar err error\n", o.Tables)
	if o.DB {
		fmt.Fprintf(&src, `	ctx := context.Background()
	var db *orm.DB
	if db, err = orm.Connect(ctx, %q, os.Getenv("DATABASE_URL"), dialect.%s()); err == nil {
		defer db.Close()
		st, err = migrate.Inspect(ctx, db)
	}
`, d.Name, d.Dialect)
	} else {
		src.WriteString("\tst, err = migrate.Replay(migs.Set.List())\n")
	}
	src.WriteString(`	out := map[string]any{}
	for _, t := range tables {
		if err == nil && st.Tables[t] == nil {
			err = fmt.Errorf("no table %q", t)
		}
	}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["erd"] = migrate.ERD(st, tables...)
		out["opaque"] = st.Opaque
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
`)
	var env []string
	if o.DSN != "" {
		env = append(env, "DATABASE_URL="+o.DSN)
	}
	if o.DB && o.DSN == "" && os.Getenv("DATABASE_URL") == "" {
		return errors.New("--db needs DATABASE_URL (in .env or the environment) or --dsn")
	}
	var out struct {
		ERD    string `json:"erd"`
		Opaque int    `json:"opaque"`
		Error  string `json:"error"`
	}
	if err := p.runProgram(src.String(), env, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	if out.Opaque > 0 && !o.DB {
		p.Out("note: %d migration step(s) are raw SQL or Go code and aren't drawn; `kiln erd --db` reads the database instead", out.Opaque)
	}
	if o.Out == "" {
		fmt.Fprint(os.Stdout, out.ERD)
		return nil
	}
	body := out.ERD
	if strings.EqualFold(filepath.Ext(o.Out), ".md") {
		body = "```mermaid\n" + out.ERD + "```\n"
	}
	path := o.Out
	if !filepath.IsAbs(path) {
		path = p.path(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	p.Out("wrote %s", p.rel(path))
	return nil
}
