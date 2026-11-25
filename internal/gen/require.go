package gen

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
)

// Version is the version of this kiln binary ("latest" for a development
// build, including one from a modified checkout: "v1.10.1+dirty"), so
// generated hints install the matching library.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path == libPath && bi.Main.Version != "" &&
		bi.Main.Version != "(devel)" && !strings.Contains(bi.Main.Version, "+") {
		return bi.Main.Version
	}
	return "latest"
}

// checkImports returns a clear error naming the go get command when a
// program kiln is about to run imports a package that no module in go.mod
// provides, instead of the go tool's errors about a generated file.
func (p *Project) checkImports(src []byte) error {
	b, err := os.ReadFile(p.path("go.mod"))
	if err != nil {
		return nil // no go.mod here: let the go tool explain
	}
	mods := modules(b)
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var missing []string
	for _, im := range f.Imports {
		path, _ := strconv.Unquote(im.Path.Value)
		if !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") || within(path, p.Module) {
			continue // standard library or this module
		}
		found := false
		for _, m := range mods {
			if within(path, m) {
				found = true
				break
			}
		}
		if !found {
			if within(path, libPath) {
				path = libPath + "@" + Version()
			}
			missing = append(missing, path)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("go.mod is missing packages kiln needs; run:\n  go get %s", strings.Join(dedupe(missing), " "))
}

// modules lists the module paths go.mod requires.
func modules(gomod []byte) []string {
	var out []string
	block := false
	for _, line := range strings.Split(string(gomod), "\n") {
		f := strings.Fields(strings.SplitN(line, "//", 2)[0])
		switch {
		case len(f) == 0:
		case f[0] == "require" && len(f) > 1 && f[1] == "(":
			block = true
		case f[0] == ")":
			block = false
		case f[0] == "require" && len(f) >= 3:
			out = append(out, f[1])
		case block && len(f) >= 2:
			out = append(out, f[0])
		}
	}
	return out
}

func within(pkg, mod string) bool { return pkg == mod || strings.HasPrefix(pkg, mod+"/") }

func dedupe(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// ExitError carries the exit code of a program kiln ran (the migration
// runner), which has already printed its own error.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

func exitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ExitError{Code: ee.ExitCode()}
	}
	return err
}
