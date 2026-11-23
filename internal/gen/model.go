package gen

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/mhdaslam790/kiln/internal/meta"
)

// modelFile locates the file declaring `type <name> struct` in the models dir.
type modelFile struct {
	path  string
	src   []byte
	fset  *token.FileSet
	file  *ast.File
	st    *ast.StructType
	table string
}

func (p *Project) findModel(name string) (*modelFile, error) {
	dir := p.path(p.Config.Models)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("model %s not found: %w", name, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		mf, err := parseModel(path, name)
		if err != nil {
			return nil, err
		}
		if mf != nil {
			return mf, nil
		}
	}
	return nil, fmt.Errorf("model %s not found in %s (create it with: kiln create %s)", name, p.Config.Models, name)
}

func parseModel(path, name string) (*modelFile, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	mf := &modelFile{path: path, src: src, fset: fset, file: f, table: meta.Plural(meta.Snake(name))}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			for _, s := range d.Specs {
				ts, ok := s.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					return nil, fmt.Errorf("%s is not a struct", name)
				}
				mf.st = st
			}
		case *ast.FuncDecl:
			// func (X) TableName() string { return "..." }
			if d.Name.Name == "TableName" && d.Recv != nil && len(d.Recv.List) == 1 && recvName(d.Recv.List[0].Type) == name {
				if t := returnedString(d); t != "" {
					mf.table = t
				}
			}
		}
	}
	if mf.st == nil {
		return nil, nil
	}
	return mf, nil
}

func recvName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func returnedString(fn *ast.FuncDecl) string {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return ""
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return ""
	}
	lit, ok := ret.Results[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, _ := strconv.Unquote(lit.Value)
	return s
}

func (mf *modelFile) offset(pos token.Pos) int { return mf.fset.Position(pos).Offset }

// column returns the column name and json flag of a struct field.
func fieldColumn(f *ast.Field, name string) (string, bool) {
	col, isJSON := meta.Snake(name), false
	if f.Tag != nil {
		tag, _ := strconv.Unquote(f.Tag.Value)
		if db, ok := reflect.StructTag(tag).Lookup("db"); ok {
			parts := strings.Split(db, ",")
			if parts[0] != "" {
				col = parts[0]
			}
			for _, o := range parts[1:] {
				if o == "json" {
					isJSON = true
				}
			}
		}
	}
	return col, isJSON
}

// findField returns the struct field mapped to column (or Go name).
func (mf *modelFile) findField(column string) (*ast.Field, string, bool) {
	for _, f := range mf.st.Fields.List {
		for _, n := range f.Names {
			col, isJSON := fieldColumn(f, n.Name)
			if col == column || n.Name == column {
				return f, col, isJSON
			}
		}
	}
	return nil, "", false
}

func (mf *modelFile) exprString(e ast.Expr) string {
	return string(mf.src[mf.offset(e.Pos()):mf.offset(e.End())])
}

// addFields inserts fields before the struct's closing brace.
func (mf *modelFile) addFields(fields []FieldSpec) error {
	var b strings.Builder
	needTime := false
	for _, f := range fields {
		if existing, _, _ := mf.findField(f.Column); existing != nil {
			return fmt.Errorf("%s already has a field for column %q", filepath.Base(mf.path), f.Column)
		}
		fmt.Fprintf(&b, "\t%s %s %s\n", f.GoName(), f.GoType(), f.Tag())
		if strings.Contains(f.GoType(), "time.") {
			needTime = true
		}
	}
	end := mf.offset(mf.st.Fields.Closing)
	src := string(mf.src[:end]) + b.String() + string(mf.src[end:])
	if needTime {
		src = ensureImport(src, "time")
	}
	return writeFormatted(mf.path, src)
}

// removeField deletes the field for column and returns its Go type text.
func (mf *modelFile) removeField(column string) (goType string, isJSON bool, err error) {
	f, _, isJSON := mf.findField(column)
	if f == nil {
		return "", false, fmt.Errorf("no field for column %q in %s", column, filepath.Base(mf.path))
	}
	if len(f.Names) > 1 {
		return "", false, fmt.Errorf("field for %q shares a line with other fields; split it first", column)
	}
	goType = mf.exprString(f.Type)
	start := mf.offset(f.Pos())
	if f.Doc != nil {
		start = mf.offset(f.Doc.Pos())
	}
	end := mf.offset(f.End())
	if f.Comment != nil {
		end = mf.offset(f.Comment.End())
	}
	// Extend to whole lines.
	for start > 0 && mf.src[start-1] != '\n' {
		start--
	}
	for end < len(mf.src) && mf.src[end] != '\n' {
		end++
	}
	if end < len(mf.src) {
		end++
	}
	src := string(mf.src[:start]) + string(mf.src[end:])
	src = dropUnusedImport(src, "time")
	return goType, isJSON, writeFormatted(mf.path, src)
}

func writeFormatted(path, src string) error {
	out, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("formatting %s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o644)
}

func ensureImport(src, pkg string) string {
	quoted := strconv.Quote(pkg)
	if strings.Contains(src, quoted) {
		return src
	}
	if i := strings.Index(src, "import ("); i >= 0 {
		j := i + len("import (")
		return src[:j] + "\n\t" + quoted + src[j:]
	}
	if i := strings.Index(src, "\nimport "); i >= 0 {
		return src[:i+1] + "import " + quoted + "\n" + src[i+1:]
	}
	// After the package clause.
	if i := strings.Index(src, "\n"); i >= 0 && strings.HasPrefix(strings.TrimSpace(src), "package") {
		return src[:i+1] + "\nimport " + quoted + "\n" + src[i+1:]
	}
	return src
}

func dropUnusedImport(src, pkg string) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ImportsOnly)
	if err != nil {
		return src
	}
	if len(f.Decls) == 0 {
		return src
	}
	importEnd := fset.Position(f.Decls[len(f.Decls)-1].End()).Offset
	if strings.Contains(src[importEnd:], pkg+".") {
		return src
	}
	quoted := strconv.Quote(pkg)
	lines := strings.Split(src, "\n")
	out := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == quoted || t == "import "+quoted {
			continue
		}
		out = append(out, l)
	}
	return emptyImport.ReplaceAllString(strings.Join(out, "\n"), "")
}

var emptyImport = regexp.MustCompile(`(?m)^import \(\s*\)\n`)
