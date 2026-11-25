package migrate

import (
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/schema"
)

func TestERD(t *testing.T) {
	st, err := Replay([]Migration{{Version: 1, Name: "init", Up: func(s *schema.Schema) {
		s.CreateTable("users", func(t *schema.Table) {
			t.ID("id")
			t.String("email", schema.Required(), schema.Unique())
			t.Text("bio")
		})
		s.CreateTable("posts", func(t *schema.Table) {
			t.ID("id")
			t.References("user_id", "users", schema.Required())
			t.References("editor_id", "users") // nullable
			t.String("title", schema.Required(), schema.Limit(200))
		})
		s.CreateTable("profiles", func(t *schema.Table) {
			t.ID("id")
			t.References("user_id", "users", schema.Required(), schema.Unique())
		})
		s.CreateTable("roles", func(t *schema.Table) { t.ID("id") })
		s.CreateTable("user_roles", func(t *schema.Table) {
			t.References("user_id", "users", schema.Required())
			t.References("role_id", "roles", schema.Required())
			t.PrimaryKey("user_id", "role_id")
		})
		s.CreateTable("odd table", func(t *schema.Table) { t.ID("id"); t.String("my col") })
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got := ERD(st)
	for _, want := range []string{
		"erDiagram\n",
		"    users {\n        bigint id PK\n        string email UK\n        text bio \"null\"\n    }",
		"        bigint user_id FK\n",
		"        bigint editor_id FK \"null\"\n",
		"        string_200 title\n",
		`    users ||--o{ posts : "user_id"`,
		`    users |o--o{ posts : "editor_id"`,
		`    users ||--o| profiles : "user_id"`,
		"        bigint user_id PK, FK\n",
		`    users ||--o{ user_roles : "user_id"`,
		`    roles ||--o{ user_roles : "role_id"`,
		"    odd_table {\n        bigint id PK\n        string my_col \"null\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Only some tables: relationships to tables left out aren't drawn.
	part := ERD(st, "users", "posts")
	if strings.Contains(part, "profiles") || strings.Contains(part, "roles") || !strings.Contains(part, `users ||--o{ posts`) {
		t.Errorf("filtered ERD:\n%s", part)
	}
}
