// Package gen implements the kiln code generators.
package gen

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mhdaslam790/kiln/env"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConfigFile is the per-project configuration written by `kiln init`.
const ConfigFile = "kiln.json"

// Config describes where generated code lives.
type Config struct {
	Dialect    string `json:"dialect"`
	Migrations string `json:"migrations"`
	Models     string `json:"models"`
	Runner     string `json:"runner"`
	Seeds      string `json:"seeds"`
	// Where `kiln scaffold` writes repository implementations and usecases.
	Repositories string `json:"repositories,omitempty"`
	Usecases     string `json:"usecases,omitempty"`
	// TenantColumn enables the `kiln lint` check for tenant tables without
	// an index on this column (set it when you use the tenant plugin).
	TenantColumn string `json:"tenant_column,omitempty"`
}

// DefaultConfig is used when no kiln.json exists.
func DefaultConfig() Config {
	return Config{
		Dialect: "postgres", Migrations: "database/migrations", Models: "internal/domain",
		Runner: "cmd/migrate", Seeds: "database/seeds",
		Repositories: DefaultRepositories, Usecases: DefaultUsecases,
	}
}

// FlatConfig is the layout without layers: models/ at the top level.
func FlatConfig() Config {
	c := DefaultConfig()
	c.Models, c.Repositories, c.Usecases = "models", "", ""
	return c
}

// Project is the user's Go module.
type Project struct {
	Root   string
	Module string
	Config Config
	// Out receives progress lines.
	Out func(format string, args ...any)
	// Now is the clock used for migration versions.
	Now func() time.Time
}

// Load finds the module root from dir upward and reads kiln.json.
func Load(dir string) (*Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root := abs
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return nil, errors.New("go.mod not found; run kiln inside a Go module (go mod init ...)")
		}
		root = parent
	}
	mod, err := modulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, err
	}
	p := &Project{
		Root: root, Module: mod, Config: DefaultConfig(),
		Out: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
		Now: time.Now,
	}
	if b, err := os.ReadFile(filepath.Join(root, ConfigFile)); err == nil {
		if err := json.Unmarshal(b, &p.Config); err != nil {
			return nil, fmt.Errorf("%s: %w", ConfigFile, err)
		}
		if p.Config.Seeds == "" {
			p.Config.Seeds = DefaultConfig().Seeds
		}
	}
	// .env in the project root supplies DATABASE_URL etc.; real
	// environment variables win. Programs kiln runs inherit them.
	if err := env.Load(filepath.Join(root, ".env")); err != nil {
		return nil, err
	}
	return p, nil
}

func modulePath(gomod string) (string, error) {
	f, err := os.Open(gomod)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module")), `"`), nil
		}
	}
	return "", errors.New("module directive not found in go.mod")
}

func (p *Project) path(parts ...string) string {
	return filepath.Join(append([]string{p.Root}, parts...)...)
}

func (p *Project) rel(path string) string {
	if r, err := filepath.Rel(p.Root, path); err == nil {
		return r
	}
	return path
}

// writeGo formats and writes a Go file. It refuses to overwrite unless force.
func (p *Project) writeGo(path, src string, force bool) error {
	out, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("generated invalid Go for %s: %w\n%s", p.rel(path), err, src)
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", p.rel(path))
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// ensureMigrationsPkg creates the migrations package registry if missing.
func (p *Project) ensureMigrationsPkg() error {
	path := p.path(p.Config.Migrations, "migrations.go")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := p.writeGo(path, migrationsPkgTmpl(filepath.Base(p.Config.Migrations)), false); err != nil {
		return err
	}
	p.Out("created %s", p.rel(path))
	return nil
}

// ensureSeedsPkg creates the seeds package registry if missing.
func (p *Project) ensureSeedsPkg() error {
	path := p.path(p.Config.Seeds, "seeds.go")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := p.writeGo(path, seedsPkgTmpl(filepath.Base(p.Config.Seeds)), false); err != nil {
		return err
	}
	p.Out("created %s", p.rel(path))
	return nil
}

// NewSeed creates a seed file.
func (p *Project) NewSeed(name string) error {
	if err := p.ensureSeedsPkg(); err != nil {
		return err
	}
	name = sanitizeName(name)
	v := p.Now().UTC().Format("20060102150405")
	file := p.path(p.Config.Seeds, v+"_"+name+".go")
	if err := p.writeGo(file, seedTmpl(filepath.Base(p.Config.Seeds), name, p.importPath(p.Config.Models), lastSegment(p.Config.Models)), false); err != nil {
		return err
	}
	p.Out("created %s", p.rel(file))
	return nil
}

// nextVersion returns a timestamp version not used by an existing file.
func (p *Project) nextVersion() (int64, error) {
	dir := p.path(p.Config.Migrations)
	used := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if v, _, ok := strings.Cut(e.Name(), "_"); ok {
			used[v] = true
		}
	}
	t := p.Now().UTC()
	for i := 0; i < 1000; i++ {
		v := t.Add(time.Duration(i) * time.Second).Format("20060102150405")
		if !used[v] {
			var n int64
			fmt.Sscan(v, &n)
			return n, nil
		}
	}
	return 0, errors.New("could not find a free migration version")
}

// writeMigration writes a migration file with the given Up/Down bodies.
func (p *Project) writeMigration(name string, up, down []string) (string, error) {
	if err := p.ensureMigrationsPkg(); err != nil {
		return "", err
	}
	v, err := p.nextVersion()
	if err != nil {
		return "", err
	}
	name = sanitizeName(name)
	file := p.path(p.Config.Migrations, fmt.Sprintf("%d_%s.go", v, name))
	src := migrationTmpl(filepath.Base(p.Config.Migrations), v, name, up, down)
	if err := p.writeGo(file, src, false); err != nil {
		return "", err
	}
	p.Out("created %s", p.rel(file))
	return file, nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
