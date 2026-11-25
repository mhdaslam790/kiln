package gen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mhdaslam790/kiln/internal/meta"
	"github.com/mhdaslam790/kiln/schema"
)

// modelInfo is a struct found in the models package.
type modelInfo struct {
	Name  string
	Table string
	File  string
}

// listModels finds model-like structs: exported structs with db tags, an
// ID field, or a TableName method.
func (p *Project) listModels() ([]modelInfo, error) {
	dir := p.path(p.Config.Models)
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	structs := map[string]*modelInfo{}
	likely := map[string]bool{}
	tables := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if !ok || !ts.Name.IsExported() || ts.TypeParams != nil {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					structs[ts.Name.Name] = &modelInfo{Name: ts.Name.Name, Table: meta.Plural(meta.Snake(ts.Name.Name)), File: path}
					for _, fld := range st.Fields.List {
						for _, n := range fld.Names {
							if n.Name == "ID" {
								likely[ts.Name.Name] = true
							}
						}
						if fld.Tag != nil && strings.Contains(fld.Tag.Value, `db:"`) {
							likely[ts.Name.Name] = true
						}
					}
				}
			case *ast.FuncDecl:
				if d.Name.Name == "TableName" && d.Recv != nil && len(d.Recv.List) == 1 {
					r := recvName(d.Recv.List[0].Type)
					likely[r] = true
					if t := returnedString(d); t != "" {
						tables[r] = t
					}
				}
			}
		}
	}
	var out []modelInfo
	for name, mi := range structs {
		if !likely[name] {
			continue
		}
		if t := tables[name]; t != "" {
			mi.Table = t
		}
		out = append(out, *mi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// runProgram writes a temporary main package inside the module, runs it
// with go run and decodes its JSON output into v.
func (p *Project) runProgram(src string, env []string, v any) error {
	if err := p.checkImports([]byte(src)); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(p.Root, "kiln_tmp_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := p.writeGo(filepath.Join(dir, "main.go"), src, true); err != nil {
		return err
	}
	cmd := exec.Command("go", "run", "./"+filepath.Base(dir))
	cmd.Dir = p.Root
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), v); err != nil {
		return fmt.Errorf("unexpected output: %w\n%s", err, stdout.String())
	}
	return nil
}

func (p *Project) importPath(dir string) string {
	return p.Module + "/" + filepath.ToSlash(strings.TrimPrefix(dir, "./"))
}

// DiffOptions configures Diff.
type DiffOptions struct {
	Name   string // migration name (default "auto")
	DB     bool   // compare against the live database instead of the migrations
	DSN    string
	DryRun bool // print instead of writing a file
}

type diffOutput struct {
	Up       []string      `json:"up"`
	Down     []string      `json:"down"`
	Notes    []string      `json:"notes"`
	Findings []lintFinding `json:"findings"`
	Error    string        `json:"error"`
}

// Diff compares the models with the migrations (or the live database) and
// writes a migration with the differences.
func (p *Project) Diff(o DiffOptions) error {
	models, err := p.listModels()
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no models found in %s", p.Config.Models)
	}
	if err := p.ensureMigrationsPkg(); err != nil {
		return err
	}
	var src strings.Builder
	modPkg := lastSegment(p.Config.Models)
	src.WriteString("package main\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n\t\"os\"\n\n")
	fmt.Fprintf(&src, "\t%q\n\t%q\n\t%q\n\t%q\n", libPath+"/migrate", libPath+"/orm", libPath+"/dialect", libPath+"/schema")
	fmt.Fprintf(&src, "\tmigs %q\n\t%s %q\n", p.importPath(p.Config.Migrations), modPkg, p.importPath(p.Config.Models))
	d := drivers[p.Config.Dialect]
	if o.DB {
		fmt.Fprintf(&src, "\t_ %q\n", d.Import)
	}
	src.WriteString(")\n\n")
	src.WriteString("var _ = orm.Open\nvar _ = dialect.SQLite\nvar _ = context.Background\nvar _ = migs.Set\n\n")
	src.WriteString("func main() {\n\tmodels := []any{\n")
	for _, m := range models {
		fmt.Fprintf(&src, "\t\t%s.%s{},\n", modPkg, m.Name)
	}
	src.WriteString("\t}\n\tvar res migrate.DiffResult\n\tvar err error\n\tvar before *schema.State\n")
	if o.DB {
		fmt.Fprintf(&src, `	ctx := context.Background()
	db, err := orm.Connect(ctx, %q, os.Getenv("DATABASE_URL"), dialect.%s())
	if err == nil {
		defer db.Close()
		state, ierr := migrate.Inspect(ctx, db)
		if ierr != nil {
			err = ierr
		} else {
			res, err = migrate.DiffFor(db.Dialect(), state, models...)
			before = state
		}
	}
`, d.Name, d.Dialect)
	} else {
		src.WriteString("\tres, err = migrate.DiffMigrations(migs.Set.List(), models...)\n")
	}
	fmt.Fprintf(&src, "\tkind := dialect.Kind(%q)\n", p.Config.Dialect)
	src.WriteString(`	out := map[string]any{"up": migrate.RenderGo(res.Up), "down": migrate.RenderGo(res.Down), "notes": res.Notes}
	if err != nil {
		out["error"] = err.Error()
	} else if len(res.Up) > 0 {
		// Lint the new migration against the schema it will run on.
		up := res.Up
		next := migrate.Migration{Version: 1 << 62, Name: "new",
			Up: func(s *schema.Schema) { s.Ops = append(s.Ops, up...) }, Down: func(*schema.Schema) {}}
		var fs []migrate.Finding
		if before != nil {
			fs = migrate.LintOps(kind, before, next, up)
		} else {
			fs, _ = migrate.Lint(kind, append(migs.Set.List(), next), func(m migrate.Migration) bool { return m.Version == next.Version })
		}
		for i := range fs {
			fs[i].Version, fs[i].Name = 0, ""
		}
		out["findings"] = fs
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
	var out diffOutput
	if err := p.runProgram(src.String(), env, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	for _, n := range out.Notes {
		p.Out("note: %s", n)
	}
	if len(out.Up) == 0 {
		p.Out("schema is up to date with the models")
		return nil
	}
	if o.DryRun {
		p.Out("-- up\n%s\n-- down\n%s", strings.Join(out.Up, "\n"), strings.Join(out.Down, "\n"))
	} else {
		name := o.Name
		if name == "" {
			name = "auto"
		}
		if _, err = p.writeMigration(name, out.Up, out.Down); err != nil {
			return err
		}
	}
	if len(out.Findings) > 0 {
		p.printFindings(out.Findings)
		errs, warns, _ := countFindings(out.Findings)
		if errs+warns > 0 {
			p.Out("review the migration before applying it (%d error(s), %d warning(s)); `kiln lint` re-checks it", errs, warns)
		}
	}
	return nil
}

// PullOptions configures Pull.
type PullOptions struct {
	DSN    string
	Tables []string // only these tables (default: all without a model)
	Force  bool     // overwrite existing model files
}

// Pull generates model structs from the live database for tables that do
// not have a model yet.
func (p *Project) Pull(o PullOptions) error {
	d := drivers[p.Config.Dialect]
	src := fmt.Sprintf(`package main

import (
	"context"
	"encoding/json"
	"os"

	_ %q

	"%[2]s/dialect"
	"%[2]s/migrate"
	"%[2]s/orm"
)

func main() {
	ctx := context.Background()
	out := map[string]any{}
	db, err := orm.Connect(ctx, %q, os.Getenv("DATABASE_URL"), dialect.%s())
	if err == nil {
		defer db.Close()
		var st any
		st, err = migrate.Inspect(ctx, db)
		out["state"] = st
	}
	if err != nil {
		out["error"] = err.Error()
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
`, d.Import, libPath, d.Name, d.Dialect)
	var env []string
	if o.DSN != "" {
		env = append(env, "DATABASE_URL="+o.DSN)
	} else if os.Getenv("DATABASE_URL") == "" {
		return errors.New("pull needs DATABASE_URL (in .env or the environment) or --dsn")
	}
	var out struct {
		State *schema.State `json:"state"`
		Error string        `json:"error"`
	}
	if err := p.runProgram(src, env, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	existing, err := p.listModels()
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, m := range existing {
		have[m.Table] = true
	}
	only := map[string]bool{}
	for _, t := range o.Tables {
		only[t] = true
	}
	var pulled []string
	taken := map[string]bool{}
	for _, m := range existing {
		taken[m.Name] = true
	}
	for _, name := range out.State.TableNames() {
		if len(only) > 0 && !only[name] {
			continue
		}
		if have[name] && !o.Force {
			p.Out("skip %s (model exists)", name)
			continue
		}
		pulled = append(pulled, name)
		taken[meta.Camel(meta.Singular(name))] = true
	}
	// Enum types used by the pulled tables become named string types.
	enums := map[string]string{}
	for _, name := range pulled {
		for _, c := range out.State.Tables[name].Columns {
			base := enumName(c.SQLType)
			if _, ok := out.State.Enums[base]; ok && enums[base] == "" {
				g := meta.Camel(base)
				if taken[g] {
					g += "Type"
				}
				taken[g] = true
				enums[base] = g
			}
		}
	}
	pkg := lastSegment(p.Config.Models)
	usesPG := false
	for _, name := range pulled {
		t := out.State.Tables[name]
		model := meta.Camel(meta.Singular(name))
		path := p.path(p.Config.Models, meta.Snake(model)+".go")
		src := modelFromTable(pkg, model, t, enums)
		usesPG = usesPG || strings.Contains(src, kilnpgPath)
		if err := p.writeGo(path, src, o.Force); err != nil {
			return err
		}
		p.Out("created %s", p.rel(path))
	}
	enumNames := make([]string, 0, len(enums))
	for e := range enums {
		enumNames = append(enumNames, e)
	}
	sort.Strings(enumNames)
	for _, e := range enumNames {
		path := p.path(p.Config.Models, "enum_"+meta.Snake(enums[e])+".go")
		if _, err := os.Stat(path); err == nil && !o.Force {
			p.Out("skip %s (exists)", p.rel(path))
			continue
		}
		if err := p.writeGo(path, enumSource(pkg, enums[e], e, out.State.Enums[e]), o.Force); err != nil {
			return err
		}
		p.Out("created %s", p.rel(path))
	}
	if len(pulled) == 0 {
		p.Out("no new models")
	}
	if usesPG {
		if b, err := os.ReadFile(p.path("go.mod")); err == nil && !slices.Contains(modules(b), kilnpgPath) {
			p.Out("array columns use kilnpg.Array; run: go get %s", kilnpgPath)
		}
	}
	return nil
}

const kilnpgPath = libPath + "/kilnpg"

// enumSource renders a Go string type with one constant per value for a
// PostgreSQL enum type.
func enumSource(pkg, goName, sqlName string, values []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n// %s is the PostgreSQL enum type %s (generated by kiln pull).\ntype %s string\n", pkg, goName, sqlName, goName)
	if len(values) == 0 {
		return b.String()
	}
	b.WriteString("\nconst (\n")
	used := map[string]bool{}
	for _, v := range values {
		c := goName + meta.Camel(v)
		if c == goName+"X" || used[c] { // a value with no letters or digits, or a clash
			for i := 2; used[c] || c == goName+"X"; i++ {
				c = fmt.Sprintf("%s%s%d", goName, meta.Camel(v), i)
			}
		}
		used[c] = true
		fmt.Fprintf(&b, "\t%s %s = %q\n", c, goName, v)
	}
	b.WriteString(")\n")
	return b.String()
}

// modelFromTable renders a model struct for an inspected table.
func modelFromTable(pkg, name string, t *schema.TableState, enums map[string]string) string {
	imports := map[string]bool{}
	pks := map[string]bool{}
	for _, c := range t.PrimaryKey {
		pks[c] = true
	}
	singlePK := len(t.PrimaryKey) == 0
	fks := map[string]schema.ForeignKey{}
	for _, fk := range t.ForeignKeys {
		fks[fk.Columns[0]] = fk
	}
	// Index tags: single-column plain indexes use "index", others a named group.
	idxTags := map[string][]string{}
	for _, ix := range t.Indexes {
		if ix.Unique && len(ix.Columns) == 1 {
			continue // reported as Column.Unique
		}
		if len(ix.Columns) == 1 && !ix.Unique {
			if _, isFK := fks[ix.Columns[0]]; isFK {
				continue // implied by references
			}
			idxTags[ix.Columns[0]] = append(idxTags[ix.Columns[0]], "index")
			continue
		}
		key := "index:"
		if ix.Unique {
			key = "uniqueindex:"
		}
		for _, c := range ix.Columns {
			idxTags[c] = append(idxTags[c], key+ix.Name)
		}
	}

	var b strings.Builder
	var fields []string
	for _, c := range t.Columns {
		goType, st := goTypeFor(c, imports, enums)
		var dbOpts []string
		if pks[c.Name] || (singlePK && c.Primary) {
			dbOpts = append(dbOpts, "primary")
			if c.Type != schema.ID && (c.Type == schema.Int || c.Type == schema.BigInt || c.Type == schema.SmallInt) && singlePK {
				dbOpts = append(dbOpts, "noauto")
			}
		}
		if c.Unique {
			st = append(st, "unique")
		}
		if fk, ok := fks[c.Name]; ok {
			ref := "references:" + fk.RefTable
			if fk.RefColumns[0] != "id" {
				ref += "." + fk.RefColumns[0]
			}
			st = append(st, ref)
			if fk.OnDelete != "" {
				st = append(st, "ondelete:"+fk.OnDelete)
			}
		}
		st = append(st, idxTags[c.Name]...)
		tag := `db:"` + strings.Join(append([]string{c.Name}, dbOpts...), ",") + `"`
		if len(st) > 0 {
			tag += ` schema:"` + strings.Join(st, ",") + `"`
		}
		fields = append(fields, fmt.Sprintf("\t%s %s `%s`\n", fieldName(c.Name), goType, tag))
	}
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	if len(imports) > 0 {
		b.WriteString("import (\n")
		var ims []string
		for im := range imports {
			ims = append(ims, im)
		}
		sort.Strings(ims)
		for _, im := range ims {
			b.WriteString("\t" + strconv.Quote(im) + "\n")
		}
		b.WriteString(")\n\n")
	}
	fmt.Fprintf(&b, "// %s maps table %s (generated by kiln pull).\ntype %s struct {\n", name, t.Name, name)
	for _, f := range fields {
		b.WriteString(f)
	}
	b.WriteString("}\n")
	if meta.Plural(meta.Snake(name)) != t.Name {
		fmt.Fprintf(&b, "\nfunc (%s) TableName() string { return %q }\n", name, t.Name)
	}
	return b.String()
}

// enumName is the enum type name in an enum (or enum array) column type:
// "order status"[] -> order status.
func enumName(sqlType string) string {
	base := strings.TrimSuffix(sqlType, "[]")
	if len(base) > 1 && base[0] == '"' && base[len(base)-1] == '"' {
		return strings.ReplaceAll(base[1:len(base)-1], `""`, `"`)
	}
	return base
}

// tagValue escapes double quotes for use inside a struct tag's value.
func tagValue(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }

// arrayElem is the Go element type for a PostgreSQL array of base, or "".
func arrayElem(base string, enums map[string]string) string {
	if g := enums[base]; g != "" {
		return g
	}
	switch strings.ToLower(base) {
	case "smallint":
		return "int16"
	case "integer":
		return "int"
	case "bigint":
		return "int64"
	case "real":
		return "float32"
	case "double precision", "numeric":
		return "float64"
	case "boolean":
		return "bool"
	case "text", "varchar", "character varying", "char", "character", "citext", "uuid":
		return "string"
	}
	return ""
}

// goTypeFor picks the Go type and the schema tag entries needed so that a
// diff of the generated model against the database is empty.
func goTypeFor(c schema.Column, imports map[string]bool, enums map[string]string) (string, []string) {
	var t string
	var st []string
	ptr := !c.Required
	if base, arr := strings.CutSuffix(c.SQLType, "[]"); arr {
		if elem := arrayElem(enumName(base), enums); elem != "" {
			imports[kilnpgPath] = true
			st = append(st, "sqltype:"+tagValue(c.SQLType))
			if c.Required {
				st = append(st, "notnull")
			}
			return "kilnpg.Array[" + elem + "]", st
		}
	} else if g := enums[enumName(c.SQLType)]; g != "" {
		t = g
		if ptr {
			t = "*" + t
		}
		return t, []string{"type:text", "sqltype:" + tagValue(c.SQLType)}
	}
	switch c.Type {
	case schema.ID, schema.BigInt:
		t = "int64"
	case schema.Int:
		t = "int"
	case schema.SmallInt:
		t = "int16"
	case schema.Bool:
		t = "bool"
	case schema.Float:
		t = "float64"
	case schema.Decimal:
		t = "float64"
		st = append(st, "type:decimal")
		if c.Precision > 0 {
			st = append(st, fmt.Sprintf("precision:%d", c.Precision), fmt.Sprintf("scale:%d", c.Scale))
		}
	case schema.String:
		t = "string"
		if c.Limit > 0 && c.Limit != 255 {
			st = append(st, fmt.Sprintf("limit:%d", c.Limit))
		}
	case schema.Text, schema.UUID:
		t = "string"
		st = append(st, "type:"+string(c.Type))
	case schema.Date, schema.Time:
		t = "time.Time"
		imports["time"] = true
		st = append(st, "type:"+string(c.Type))
	case schema.DateTime:
		t = "time.Time"
		imports["time"] = true
	case schema.Bytes:
		t, ptr = "[]byte", false
		if c.Required {
			st = append(st, "notnull")
		}
	case schema.JSON:
		t, ptr = "json.RawMessage", false
		imports["encoding/json"] = true
		st = append(st, "type:json")
		if c.Required {
			st = append(st, "notnull")
		}
	default:
		t = "string"
		st = append(st, "type:text")
	}
	if c.SQLType != "" {
		st = append(st, "sqltype:"+tagValue(c.SQLType))
	}
	if ptr {
		t = "*" + t
	}
	return t, st
}
