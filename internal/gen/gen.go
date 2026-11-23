package gen

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/internal/meta"
)

// Init writes kiln.json, the migrations package and the runner command.
func (p *Project) Init(cfg Config, force bool) error {
	if dialect.ByName(cfg.Dialect) == nil {
		return fmt.Errorf("unknown dialect %q (postgres, mysql, sqlite, mssql)", cfg.Dialect)
	}
	cfg.Dialect = string(dialect.ByName(cfg.Dialect).Kind())
	def := DefaultConfig()
	for _, f := range []struct {
		dst *string
		def string
	}{
		{&cfg.Migrations, def.Migrations}, {&cfg.Models, def.Models}, {&cfg.Runner, def.Runner}, {&cfg.Seeds, def.Seeds},
	} {
		if *f.dst == "" {
			*f.dst = f.def
		}
	}
	p.Config = cfg

	b, _ := json.MarshalIndent(cfg, "", "  ")
	cfgPath := p.path(ConfigFile)
	if _, err := os.Stat(cfgPath); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", ConfigFile)
	}
	if err := os.WriteFile(cfgPath, append(b, '\n'), 0o644); err != nil {
		return err
	}
	p.Out("created %s", ConfigFile)

	if err := p.ensureMigrationsPkg(); err != nil {
		return err
	}
	if err := p.ensureSeedsPkg(); err != nil {
		return err
	}
	d := drivers[cfg.Dialect]
	runner := p.path(cfg.Runner, "main.go")
	if err := p.writeGo(runner, runnerTmpl(p.Module, cfg, d), force); err != nil {
		return err
	}
	p.Out("created %s", p.rel(runner))
	p.Out("\nnext steps:\n  go get %s@latest %s\n  kiln create User name:string email:string:unique\n  DATABASE_URL=%q kiln migrate up",
		libPath, d.Import, d.DSNExample)
	return nil
}

// New creates an empty migration.
func (p *Project) New(name string) error {
	_, err := p.writeMigration(name, nil, nil)
	return err
}

// CreateOptions tweak model generation.
type CreateOptions struct {
	NoTimestamps bool
	SoftDelete   bool
	Table        string
}

// Create generates a model file and its create-table migration.
func (p *Project) Create(model string, args []string, o CreateOptions) error {
	model = meta.Camel(meta.Snake(model))
	fields, err := parseFields(args)
	if err != nil {
		return err
	}
	table := o.Table
	if table == "" {
		table = meta.Plural(meta.Snake(model))
	}
	modelPath := p.path(p.Config.Models, meta.Snake(model)+".go")
	if _, err := p.findModel(model); err == nil {
		return fmt.Errorf("model %s already exists", model)
	}

	src := modelTmpl(lastSegment(p.Config.Models), model, fields, !o.NoTimestamps, o.SoftDelete)
	if o.Table != "" {
		src += fmt.Sprintf("\nfunc (%s) TableName() string { return %q }\n", model, o.Table)
	}
	if err := p.writeGo(modelPath, src, false); err != nil {
		return err
	}
	p.Out("created %s", p.rel(modelPath))

	up := []string{fmt.Sprintf("s.CreateTable(%q, func(t *schema.Table) {", table), "\tt.ID(\"id\")"}
	for _, f := range fields {
		for _, l := range f.DSL("t") {
			up = append(up, "\t"+l)
		}
	}
	if !o.NoTimestamps {
		up = append(up, "\tt.Timestamps()")
	}
	if o.SoftDelete {
		up = append(up, "\tt.SoftDelete()")
	}
	up = append(up, "})")
	if _, err = p.writeMigration("create_"+table, up, []string{fmt.Sprintf("s.DropTable(%q)", table)}); err == nil {
		p.regenCols()
	}
	return err
}

// AddColumns adds fields to a model and writes the migration.
func (p *Project) AddColumns(model string, args []string) error {
	mf, err := p.findModel(model)
	if err != nil {
		return err
	}
	fields, err := parseFields(args)
	if err != nil {
		return err
	}
	var up, down []string
	up = append(up, fmt.Sprintf("s.AlterTable(%q, func(a *schema.AlterTable) {", mf.table))
	for i := range fields {
		fields[i].PrepareForAdd()
		for _, l := range fields[i].DSL("a") {
			up = append(up, "\t"+l)
		}
	}
	up = append(up, "})")
	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		if f.Kind == "references" && p.Config.Dialect != "sqlite" {
			down = append(down, fmt.Sprintf("s.DropForeignKey(%q, %q)", mf.table, "fk_"+mf.table+"_"+f.Column))
		}
		if f.Index || f.Kind == "references" {
			down = append(down, fmt.Sprintf("s.DropIndex(%q, %q)", mf.table, "idx_"+mf.table+"_"+f.Column))
		}
		down = append(down, fmt.Sprintf("s.DropColumn(%q, %q)", mf.table, f.Column))
	}
	if err := mf.addFields(fields); err != nil {
		return err
	}
	p.Out("updated %s", p.rel(mf.path))
	if _, err = p.writeMigration(fmt.Sprintf("add_%s_to_%s", joinCols(fields), mf.table), up, down); err == nil {
		p.regenCols()
	}
	return err
}

// DropColumns removes fields from a model and writes the migration. The
// Down side re-creates the columns from their Go types.
func (p *Project) DropColumns(model string, columns []string) error {
	var up, down []string
	var table, path string
	for _, c := range columns {
		mf, err := p.findModel(model)
		if err != nil {
			return err
		}
		table, path = mf.table, mf.path
		_, col, _ := mf.findField(meta.Snake(c))
		if col == "" {
			col = meta.Snake(c)
		}
		goType, isJSON, err := mf.removeField(col)
		if err != nil {
			return err
		}
		up = append(up, fmt.Sprintf("s.DropColumn(%q, %q)", table, col))
		spec := SpecFromGo(col, goType, isJSON)
		spec.PrepareForAdd()
		down = append(down, fmt.Sprintf("s.AlterTable(%q, func(a *schema.AlterTable) { %s })", table, spec.DSL("a")[0]))
	}
	p.Out("updated %s", p.rel(path))
	_, err := p.writeMigration("drop_"+strings.Join(snakeAll(columns), "_")+"_from_"+table, up, down)
	if err == nil {
		p.regenCols()
	}
	return err
}

// RenameColumn renames a model field's column and writes the migration.
func (p *Project) RenameColumn(model, from, to string) error {
	mf, err := p.findModel(model)
	if err != nil {
		return err
	}
	from, to = meta.Snake(from), meta.Snake(to)
	f, col, isJSON := mf.findField(from)
	if f == nil {
		return fmt.Errorf("no field for column %q in model %s", from, model)
	}
	start, end := mf.offset(f.Names[0].Pos()), mf.offset(f.End())
	tag := "`db:\"" + to + "\"`"
	if isJSON {
		tag = "`db:\"" + to + ",json\"`"
	}
	line := meta.Camel(to) + " " + mf.exprString(f.Type) + " " + tag
	src := string(mf.src[:start]) + line + string(mf.src[end:])
	if err := writeFormatted(mf.path, src); err != nil {
		return err
	}
	p.Out("updated %s", p.rel(mf.path))
	_, err = p.writeMigration(fmt.Sprintf("rename_%s_to_%s_in_%s", col, to, mf.table),
		[]string{fmt.Sprintf("s.RenameColumn(%q, %q, %q)", mf.table, col, to)},
		[]string{fmt.Sprintf("s.RenameColumn(%q, %q, %q)", mf.table, to, col)})
	if err == nil {
		p.regenCols()
	}
	return err
}

// AddIndex writes a create-index migration.
func (p *Project) AddIndex(model string, columns []string, unique bool) error {
	table, err := p.tableOf(model)
	if err != nil {
		return err
	}
	cols := snakeAll(columns)
	opt := ""
	if unique {
		opt = ", schema.UniqueIndex()"
	}
	name := indexName(table, cols, unique)
	_, err = p.writeMigration("add_index_"+name,
		[]string{fmt.Sprintf("s.CreateIndex(%q, %q, %s%s)", table, name, goStrings(cols), opt)},
		[]string{fmt.Sprintf("s.DropIndex(%q, %q)", table, name)})
	return err
}

// DropIndex writes a drop-index migration (Down re-creates it).
func (p *Project) DropIndex(model string, columns []string, unique bool) error {
	table, err := p.tableOf(model)
	if err != nil {
		return err
	}
	cols := snakeAll(columns)
	opt := ""
	if unique {
		opt = ", schema.UniqueIndex()"
	}
	name := indexName(table, cols, unique)
	_, err = p.writeMigration("drop_index_"+name,
		[]string{fmt.Sprintf("s.DropIndex(%q, %q)", table, name)},
		[]string{fmt.Sprintf("s.CreateIndex(%q, %q, %s%s)", table, name, goStrings(cols), opt)})
	return err
}

// Migrate runs the project's migration runner with go run.
func (p *Project) Migrate(args []string, dsn string) error {
	runner := p.path(p.Config.Runner)
	if _, err := os.Stat(filepath.Join(runner, "main.go")); err != nil {
		return fmt.Errorf("%s/main.go not found; run kiln init first", p.Config.Runner)
	}
	cmd := exec.Command("go", append([]string{"run", "./" + filepath.ToSlash(p.Config.Runner)}, args...)...)
	cmd.Dir = p.Root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if dsn != "" {
		cmd.Env = append(cmd.Env, "DATABASE_URL="+dsn)
	}
	return cmd.Run()
}

func (p *Project) tableOf(model string) (string, error) {
	if mf, err := p.findModel(model); err == nil {
		return mf.table, nil
	}
	// Allow passing a table name directly.
	if strings.ToLower(model) == model {
		return model, nil
	}
	return "", fmt.Errorf("model %s not found in %s", model, p.Config.Models)
}

func parseFields(args []string) ([]FieldSpec, error) {
	out := make([]FieldSpec, 0, len(args))
	seen := map[string]bool{}
	for _, a := range args {
		f, err := ParseField(a)
		if err != nil {
			return nil, err
		}
		if f.Column == "id" || seen[f.Column] {
			return nil, fmt.Errorf("duplicate or reserved column %q", f.Column)
		}
		seen[f.Column] = true
		out = append(out, f)
	}
	return out, nil
}

func indexName(table string, cols []string, unique bool) string {
	prefix := "idx_"
	if unique {
		prefix = "uniq_"
	}
	return prefix + table + "_" + strings.Join(cols, "_")
}

func snakeAll(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = meta.Snake(c)
	}
	return out
}

func joinCols(fs []FieldSpec) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = f.Column
	}
	return strings.Join(names, "_")
}

func goStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[]string{" + strings.Join(q, ", ") + "}"
}
