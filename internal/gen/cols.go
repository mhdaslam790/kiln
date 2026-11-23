package gen

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ColsFile is the file `kiln gen` writes into the models package.
const ColsFile = "kiln_cols.go"

// GenOptions configures Gen.
type GenOptions struct {
	// Check reports an error instead of writing when the file is stale
	// (for CI).
	Check bool
	// Quiet suppresses the "wrote" message (used after other generators).
	Quiet bool
}

type colsModel struct {
	Name   string      `json:"name"`
	Table  string      `json:"table"`
	Fields []colsField `json:"fields"`
}

type colsField struct {
	Name     string `json:"name"`
	Column   string `json:"column"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

type colsOutput struct {
	Models  []colsModel       `json:"models"`
	Imports map[string]string `json:"imports"` // path -> package name
	Error   string            `json:"error"`
}

// Gen writes typed column references for every model into ColsFile, e.g.
//
//	var UserCols = struct {
//		ID    q.Col[int64]
//		Email q.Col[string]
//	}{ID: "id", Email: "email"}
func (p *Project) Gen(o GenOptions) error {
	models, err := p.listModels()
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no models found in %s", p.Config.Models)
	}
	dir := p.path(p.Config.Models)
	pkg, err := packageName(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ColsFile)

	old, readErr := os.ReadFile(path)
	restore := func() {
		if readErr == nil {
			_ = os.WriteFile(path, old, 0o644)
		}
	}

	// Compile the models with the current file first: other files in the
	// package may use the generated columns. If that fails, the file may be
	// stale (referencing a type that no longer exists), so retry without it.
	var out colsOutput
	prog := p.colsProgram(models)
	if err := p.runProgram(prog, nil, &out); err != nil {
		if readErr != nil {
			return err
		}
		if rmErr := os.Remove(path); rmErr != nil {
			return err
		}
		out = colsOutput{}
		if err2 := p.runProgram(prog, nil, &out); err2 != nil {
			restore()
			return fmt.Errorf("%w\n(retried without %s: %v)", err, ColsFile, err2)
		}
	}
	if out.Error != "" {
		restore()
		return errors.New(out.Error)
	}
	src, err := renderCols(pkg, out)
	if err != nil {
		restore()
		return err
	}
	if o.Check {
		restore()
		if !bytes.Equal(old, src) {
			return fmt.Errorf("%s is out of date; run kiln gen", p.rel(path))
		}
		if !o.Quiet {
			p.Out("%s is up to date", p.rel(path))
		}
		return nil
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		restore()
		return err
	}
	if !o.Quiet {
		p.Out("wrote %s (%d models)", p.rel(path), len(out.Models))
	}
	return nil
}

// regenCols refreshes ColsFile after a generator changed the models, if the
// project uses it. Failures are reported but do not undo the change.
func (p *Project) regenCols() {
	if _, err := os.Stat(p.path(p.Config.Models, ColsFile)); err != nil {
		return
	}
	if err := p.Gen(GenOptions{Quiet: true}); err != nil {
		p.Out("warning: could not refresh %s: %v", ColsFile, err)
		return
	}
	p.Out("updated %s", p.rel(p.path(p.Config.Models, ColsFile)))
}

func (p *Project) colsProgram(models []modelInfo) string {
	modPath := p.importPath(p.Config.Models)
	var b strings.Builder
	fmt.Fprintf(&b, `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	%q
	models %q
)

const modelsPath = %q

func main() {
	out := map[string]any{}
	imports := map[string]string{}
	var list []map[string]any
	for _, m := range []any{
`, libPath+"/orm", modPath, modPath)
	for _, m := range models {
		fmt.Fprintf(&b, "\t\tmodels.%s{},\n", m.Name)
	}
	b.WriteString(`	} {
		info, err := orm.Describe(m)
		if err != nil {
			out["error"] = err.Error()
			json.NewEncoder(os.Stdout).Encode(out)
			return
		}
		var fields []map[string]any
		for _, f := range info.Fields {
			t, nullable := f.Type, false
			if t.Kind() == reflect.Pointer {
				t, nullable = t.Elem(), true
			}
			fields = append(fields, map[string]any{
				"name": f.Name, "column": f.Column, "type": typeStr(t, imports), "nullable": nullable,
			})
		}
		list = append(list, map[string]any{"name": info.Name, "table": info.Table, "fields": fields})
	}
	out["models"] = list
	out["imports"] = imports
	json.NewEncoder(os.Stdout).Encode(out)
}

// typeStr renders t as Go source in the models package, recording imports.
func typeStr(t reflect.Type, imports map[string]string) string {
	if t.Name() != "" {
		switch t.PkgPath() {
		case "":
			return t.Name()
		case modelsPath:
			return t.Name()
		}
		s := t.String()
		if i := strings.IndexByte(s, '.'); i > 0 {
			imports[t.PkgPath()] = s[:i]
		}
		return s
	}
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + typeStr(t.Elem(), imports)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 && t.Elem().Name() == "uint8" {
			return "[]byte"
		}
		return "[]" + typeStr(t.Elem(), imports)
	case reflect.Array:
		return fmt.Sprintf("[%d]%s", t.Len(), typeStr(t.Elem(), imports))
	case reflect.Map:
		return "map[" + typeStr(t.Key(), imports) + "]" + typeStr(t.Elem(), imports)
	case reflect.Interface:
		if t.NumMethod() == 0 {
			return "any"
		}
	}
	return t.String()
}
`)
	return b.String()
}

func renderCols(pkg string, out colsOutput) ([]byte, error) {
	var b strings.Builder
	b.WriteString("// Code generated by kiln gen. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	// Standard library first, then everything else, as goimports does.
	var std, other []string
	for path := range out.Imports {
		if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
			other = append(other, path)
		} else {
			std = append(std, path)
		}
	}
	other = append(other, libPath+"/q")
	sort.Strings(std)
	sort.Strings(other)
	b.WriteString("import (\n")
	for i, group := range [][]string{std, other} {
		if i == 1 && len(std) > 0 {
			b.WriteString("\n")
		}
		for _, path := range group {
			name, last := out.Imports[path], path[strings.LastIndex(path, "/")+1:]
			if name != "" && name != last {
				fmt.Fprintf(&b, "\t%s %q\n", name, path)
			} else {
				fmt.Fprintf(&b, "\t%q\n", path)
			}
		}
	}
	b.WriteString(")\n")
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Name < out.Models[j].Name })
	for _, m := range out.Models {
		v := m.Name + "Cols"
		typ := strings.ToLower(v[:1]) + v[1:]
		fmt.Fprintf(&b, "\n// %s holds the typed columns of %s (table %s).\nvar %s = %s{\n", v, m.Name, m.Table, v, typ)
		for _, f := range m.Fields {
			fmt.Fprintf(&b, "\t%s: %q,\n", f.Name, f.Column)
		}
		fmt.Fprintf(&b, "}\n\ntype %s struct {\n", typ)
		for _, f := range m.Fields {
			comment := ""
			if f.Nullable {
				comment = " // nullable"
			}
			fmt.Fprintf(&b, "\t%s q.Col[%s]%s\n", f.Name, f.Type, comment)
		}
		b.WriteString("}\n")
	}
	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("generated invalid Go: %w\n%s", err, b.String())
	}
	return src, nil
}

// packageName returns the Go package name of the .go files in dir.
func packageName(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == ColsFile {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.PackageClauseOnly)
		if err != nil {
			return "", err
		}
		return f.Name.Name, nil
	}
	return lastSegment(dir), nil
}
