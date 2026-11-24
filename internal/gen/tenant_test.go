package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTenantRLSEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go build")
	}
	t.Setenv("DATABASE_URL", "")
	p := linkedProject(t)
	var log strings.Builder
	p.Out = func(f string, a ...any) { fmt.Fprintf(&log, f+"\n", a...) }
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
	}
	must(p.Init(Config{Dialect: "postgres", Migrations: "db/migrations", Models: "models", Runner: "cmd/migrate", Seeds: "db/seeds"}, false))
	must(p.Create("User", []string{"email:string"}, CreateOptions{}))
	must(os.WriteFile(p.path("db", "migrations", "20270102000000_create_tasks.go"), []byte(`package migrations

import "github.com/mhdaslam790/kiln/schema"

func init() {
	Set.Add(20270102000000, "create_tasks", func(s *schema.Schema) {
		s.CreateTable("tasks", func(t *schema.Table) {
			t.ID("id")
			t.BigInt("tenant_id", schema.Required())
		})
	}, func(s *schema.Schema) { s.DropTable("tasks") })
}
`), 0o644))

	if err := p.TenantRLS(nil, TenantRLSOptions{}); err == nil || !strings.Contains(err.Error(), "tenant_column") {
		t.Fatalf("no column configured: %v", err)
	}

	p.Config.TenantColumn = "tenant_id"
	must(p.TenantRLS(nil, TenantRLSOptions{}))
	files, _ := filepath.Glob(p.path("db", "migrations", "*_tenant_rls.go"))
	if len(files) != 1 {
		t.Fatalf("migration files: %v\n%s", files, log.String())
	}
	src, _ := os.ReadFile(files[0])
	for _, want := range []string{
		"s.Exec(`ALTER TABLE \"tasks\" ENABLE ROW LEVEL SECURITY`)",
		"s.Exec(`ALTER TABLE \"tasks\" FORCE ROW LEVEL SECURITY`)",
		"CREATE POLICY kiln_tenant_isolation ON \"tasks\"",
		"s.Exec(`DROP POLICY IF EXISTS kiln_tenant_isolation ON \"tasks\"`)",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("migration lacks %q:\n%s", want, src)
		}
	}
	if strings.Contains(string(src), `"users"`) {
		t.Errorf("users has no tenant column but got a policy:\n%s", src)
	}
	if !strings.Contains(log.String(), "tenant.WithRLS()") {
		t.Errorf("next steps missing:\n%s", log.String())
	}

	// The generated migration compiles with the rest (lint builds them).
	log.Reset()
	must(p.Lint(LintOptions{All: true}))
	if !strings.Contains(log.String(), "raw-sql") {
		t.Errorf("lint should list the raw SQL steps for review:\n%s", log.String())
	}

	// Explicit tables; other dialects are refused.
	must(p.TenantRLS([]string{"projects"}, TenantRLSOptions{Column: "org_id"}))
	p.Config.Dialect = "mysql"
	if err := p.TenantRLS(nil, TenantRLSOptions{}); err == nil || !strings.Contains(err.Error(), "PostgreSQL only") {
		t.Fatalf("mysql: %v", err)
	}
}
