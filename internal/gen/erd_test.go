package gen

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestERDCommand(t *testing.T) {
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
	must(p.Create("User", []string{"email:string:unique"}, CreateOptions{}))
	must(p.Create("Post", []string{"title:string", "user:references"}, CreateOptions{}))

	must(p.ERD(ERDOptions{Out: "docs/schema.md"}))
	b, err := os.ReadFile(p.path("docs", "schema.md"))
	must(err)
	for _, want := range []string{"```mermaid\nerDiagram\n", "    users {", "string email UK", `users ||--o{ posts : "user_id"`, "```\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q:\n%s", want, b)
		}
	}
	must(p.ERD(ERDOptions{Out: "docs/schema.mmd", Tables: []string{"users"}}))
	b, _ = os.ReadFile(p.path("docs", "schema.mmd"))
	if strings.Contains(string(b), "```") || strings.Contains(string(b), "posts") {
		t.Errorf(".mmd with one table:\n%s", b)
	}
	if err := p.ERD(ERDOptions{Tables: []string{"nope"}}); err == nil || !strings.Contains(err.Error(), `no table "nope"`) {
		t.Errorf("unknown table: %v", err)
	}
}
