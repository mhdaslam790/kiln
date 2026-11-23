package migrate

import (
	"sort"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/schema"
)

func lintBase() Migration {
	return Migration{Version: 1, Name: "create_users",
		Up: func(s *schema.Schema) {
			s.CreateTable("users", func(t *schema.Table) {
				t.ID("id")
				t.String("email", schema.Required())
				t.String("nick")
				t.Int("age")
			})
		},
		Down: func(s *schema.Schema) { s.DropTable("users") },
	}
}

func checksOf(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Check)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func lintOne(t *testing.T, kind dialect.Kind, up func(*schema.Schema)) []Finding {
	t.Helper()
	m := Migration{Version: 2, Name: "change", Up: up, Down: func(*schema.Schema) {}}
	fs, err := Lint(kind, []Migration{m, lintBase()}, func(x Migration) bool { return x.Version == 2 })
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestLintChecks(t *testing.T) {
	cases := []struct {
		name string
		kind dialect.Kind
		up   func(*schema.Schema)
		want string // sorted check names
	}{
		{"safe additions", "", func(s *schema.Schema) {
			s.AlterTable("users", func(a *schema.AlterTable) {
				a.String("bio")                                      // nullable
				a.Int("score", schema.Required(), schema.Default(0)) // NOT NULL with default
				a.String("slug", schema.Unique())                    // unique but NULL for old rows
			})
		}, ""},
		{"not null without default", "", func(s *schema.Schema) {
			s.AlterTable("users", func(a *schema.AlterTable) { a.String("status", schema.Required()) })
		}, "not-null-without-default"},
		{"unique with default collides", "", func(s *schema.Schema) {
			s.AlterTable("users", func(a *schema.AlterTable) { a.String("code", schema.Unique(), schema.Default("x")) })
		}, "unique-on-existing"},
		{"destructive", "", func(s *schema.Schema) {
			s.DropColumn("users", "nick")
			s.RenameColumn("users", "age", "years")
			s.RenameTable("users", "people")
		}, "drop-column rename-column rename-table"},
		{"drop table", "", func(s *schema.Schema) { s.DropTable("users") }, "drop-table"},
		{"change type and nullability", "", func(s *schema.Schema) {
			s.ChangeColumn("users", "age", schema.BigInt, schema.Required())
		}, "change-column-type set-not-null"},
		{"unique index on existing", "", func(s *schema.Schema) {
			s.CreateUniqueIndex("users", "", []string{"email"})
		}, "unique-on-existing"},
		{"postgres index locks writes", dialect.KindPostgres, func(s *schema.Schema) {
			s.CreateIndex("users", "", []string{"age"})
		}, "index-locks-writes"},
		{"mysql index is fine", dialect.KindMySQL, func(s *schema.Schema) {
			s.CreateIndex("users", "", []string{"age"})
		}, ""},
		{"concurrent index alone is fine", dialect.KindPostgres, func(s *schema.Schema) {
			s.CreateIndex("users", "", []string{"age"}, schema.Concurrently())
		}, ""},
		{"concurrent index mixed", dialect.KindPostgres, func(s *schema.Schema) {
			s.CreateIndex("users", "", []string{"age"}, schema.Concurrently())
			s.AlterTable("users", func(a *schema.AlterTable) { a.String("bio") })
		}, "concurrent-index-mixed"},
		{"new table is never risky", dialect.KindPostgres, func(s *schema.Schema) {
			s.CreateTable("posts", func(t *schema.Table) { t.ID("id"); t.String("title") })
			s.AlterTable("posts", func(a *schema.AlterTable) { a.String("slug", schema.Required()) })
			s.CreateUniqueIndex("posts", "", []string{"slug"})
			s.DropColumn("posts", "title")
		}, ""},
		{"raw sql", "", func(s *schema.Schema) { s.Exec("UPDATE users SET age = 0") }, "raw-sql"},
		{"allow unsafe", "", func(s *schema.Schema) {
			s.AllowUnsafe("nick unused since v2.3", "drop-column")
			s.DropColumn("users", "nick")
			s.RenameColumn("users", "age", "years") // not allowed: still reported
		}, "rename-column"},
		{"allow everything", "", func(s *schema.Schema) {
			s.AllowUnsafe("reviewed with the DBA")
			s.DropTable("users")
		}, ""},
		{"allow needs a reason", "", func(s *schema.Schema) {
			s.AllowUnsafe(" ", "drop-column")
			s.DropColumn("users", "nick")
		}, "allow-unsafe-no-reason"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := lintOne(t, c.kind, c.up)
			if got := checksOf(fs); got != c.want {
				for _, f := range fs {
					t.Log(f)
				}
				t.Fatalf("checks = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLintScopeAndIrreversible(t *testing.T) {
	drop := Migration{Version: 2, Name: "drop_nick", Up: func(s *schema.Schema) { s.DropColumn("users", "nick") }}
	all, err := Lint("", []Migration{lintBase(), drop}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := checksOf(all); got != "drop-column irreversible" {
		t.Fatalf("all: %q", got)
	}
	only1, _ := Lint("", []Migration{lintBase(), drop}, func(m Migration) bool { return m.Version == 1 })
	if len(only1) != 0 {
		t.Fatalf("version 1 should be clean: %v", only1)
	}
	if !HasErrors([]Finding{{Severity: Warning}}, true) || HasErrors([]Finding{{Severity: Warning}}, false) {
		t.Fatal("HasErrors strict handling")
	}
	f := all[0]
	if f.Version != 2 || f.Table != "users" || f.Column != "nick" || !strings.Contains(f.String(), "fix: ") {
		t.Fatalf("finding fields: %+v\n%s", f, f)
	}
}
