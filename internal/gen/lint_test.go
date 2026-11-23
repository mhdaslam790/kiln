package gen

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLintEndToEnd(t *testing.T) {
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
	must(p.Create("User", []string{"email:string", "age:int"}, CreateOptions{}))

	// A new table is never risky.
	log.Reset()
	must(p.Lint(LintOptions{}))
	if !strings.Contains(log.String(), "0 error(s), 0 warning(s)") {
		t.Fatalf("create should be clean:\n%s", log.String())
	}

	// Dropping a column is a warning: fine by default, fails with --strict.
	must(p.DropColumns("User", []string{"age"}))
	log.Reset()
	must(p.Lint(LintOptions{}))
	out := log.String()
	for _, want := range []string{"WARN  drop-column  users.age", "fix: ", `s.AllowUnsafe("why", "drop-column")`, "1 warning(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if err := p.Lint(LintOptions{Strict: true}); err == nil {
		t.Error("--strict should fail on a warning")
	}

	// --all covers every migration.
	log.Reset()
	must(p.Lint(LintOptions{All: true}))
	if !strings.Contains(log.String(), "checked 2 migration(s)") {
		t.Errorf("--all:\n%s", log.String())
	}

	// kiln diff reports findings for the migration it writes: removing a
	// field from the model generates a column drop.
	model := read(t, p.path("models", "user.go"))
	var kept []string
	for _, l := range strings.Split(model, "\n") {
		if !strings.Contains(l, `db:"email"`) {
			kept = append(kept, l)
		}
	}
	must(os.WriteFile(p.path("models", "user.go"), []byte(strings.Join(kept, "\n")), 0o644))
	log.Reset()
	must(p.Diff(DiffOptions{Name: "drop_email"}))
	if out := log.String(); !strings.Contains(out, "WARN  drop-column  users.email") ||
		!strings.Contains(out, "review the migration before applying it") {
		t.Errorf("diff findings:\n%s", out)
	}

	// Errors fail lint; a reviewed AllowUnsafe silences them.
	mig := `package migrations

import "github.com/mhdaslam790/kiln/schema"

func init() {
	Set.Add(20270101000000, "add_status", func(s *schema.Schema) {
		%s
		s.AddColumn("users", "status", schema.String, schema.Required())
	}, func(s *schema.Schema) { s.DropColumn("users", "status") })
}
`
	path := p.path("db", "migrations", "20270101000000_add_status.go")
	must(os.WriteFile(path, []byte(fmt.Sprintf(mig, "")), 0o644))
	log.Reset()
	if err := p.Lint(LintOptions{}); err == nil || !strings.Contains(log.String(), "ERROR not-null-without-default  users.status") {
		t.Errorf("lint should fail on the error (err=%v):\n%s", err, log.String())
	}
	must(os.WriteFile(path, []byte(fmt.Sprintf(mig, `s.AllowUnsafe("users table is empty in every environment", "not-null-without-default")`)), 0o644))
	log.Reset()
	must(p.Lint(LintOptions{Strict: true}))
}
