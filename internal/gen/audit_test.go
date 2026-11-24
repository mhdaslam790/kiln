package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditInit(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go build")
	}
	t.Setenv("DATABASE_URL", "")
	p := linkedProject(t)
	var log strings.Builder
	p.Out = func(f string, a ...any) { fmt.Fprintf(&log, f+"\n", a...) }
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	if err := p.Init(Config{Dialect: "postgres", Migrations: "db/migrations", Models: "models", Runner: "cmd/migrate", Seeds: "db/seeds"}, false); err != nil {
		t.Fatal(err)
	}
	if err := p.AuditInit(AuditInitOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	files, _ := filepath.Glob(p.path("db", "migrations", "*_create_audit_log.go"))
	if len(files) != 1 {
		t.Fatalf("files: %v", files)
	}
	src, _ := os.ReadFile(files[0])
	for _, want := range []string{`s.CreateTable("audit_log"`, `t.Text("changes")`, `s.DropTable("audit_log")`} {
		if !strings.Contains(string(src), want) {
			t.Errorf("missing %q:\n%s", want, src)
		}
	}
	// It compiles and is a plain new table: lint is clean.
	log.Reset()
	if err := p.Lint(LintOptions{All: true, Strict: true}); err != nil {
		t.Fatalf("lint: %v\n%s", err, log.String())
	}
}
