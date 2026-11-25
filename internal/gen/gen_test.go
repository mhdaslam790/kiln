package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseField(t *testing.T) {
	f, err := ParseField("displayName:string:unique:limit=80:default=anon")
	if err != nil {
		t.Fatal(err)
	}
	if f.Column != "display_name" || f.GoName() != "DisplayName" || f.GoType() != "string" || !f.Unique || f.Limit != 80 {
		t.Fatalf("%+v", f)
	}
	if got := f.DSL("t")[0]; got != `t.String("display_name", schema.Required(), schema.Unique(), schema.Limit(80), schema.Default("anon"))` {
		t.Errorf("dsl: %s", got)
	}

	r, _ := ParseField("author:references:to=users")
	if r.Column != "author_id" || r.RefTable != "users" || r.GoType() != "int64" {
		t.Fatalf("%+v", r)
	}
	if got := r.DSL("t"); len(got) != 1 || got[0] != `t.References("author_id", "users", schema.Required())` {
		t.Errorf("ref dsl %v", got)
	}

	n, _ := ParseField("born:date:null")
	if n.GoType() != "*time.Time" {
		t.Errorf("nullable type %s", n.GoType())
	}

	c, _ := ParseField("score:int")
	c.PrepareForAdd()
	if c.Default == nil || *c.Default != "0" {
		t.Error("add-column should default NOT NULL ints to 0")
	}

	for _, bad := range []string{"x", "x:wat", "x:int:limit=abc", "x:int:bogus"} {
		if _, err := ParseField(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func newProject(t *testing.T) *Project {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n\ngo 1.23\n"), 0o644)
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	p.Out = func(string, ...any) {}
	return p
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGenerators(t *testing.T) {
	p := newProject(t)
	if err := p.Init(Config{Dialect: "pg", Migrations: "db/migrations", Models: "models", Runner: "cmd/migrate"}, false); err != nil {
		t.Fatal(err)
	}
	runner := read(t, p.path("cmd/migrate/main.go"))
	if !strings.Contains(runner, `"example.com/app/db/migrations"`) || !strings.Contains(runner, "dialect.Postgres()") {
		t.Fatalf("runner:\n%s", runner)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.Create("BlogPost", []string{"title:string", "published_at:datetime:null"}, CreateOptions{SoftDelete: true}))
	model := read(t, p.path("models/blog_post.go"))
	model = strings.Join(strings.Fields(model), " ")
	for _, want := range []string{"type BlogPost struct", "PublishedAt *time.Time `db:\"published_at\"`", "DeletedAt *time.Time"} {
		if !strings.Contains(model, want) {
			t.Errorf("model missing %q:\n%s", want, model)
		}
	}
	must(p.Create("Report", []string{"table_name:string"}, CreateOptions{Table: "report_x"}))
	if r := strings.Join(strings.Fields(read(t, p.path("models/report.go"))), " "); !strings.Contains(r, "TableNameCol string `db:\"table_name\"`") {
		t.Errorf("table_name column clashes with TableName():\n%s", r)
	}
	if err := p.Create("BlogPost", nil, CreateOptions{}); err == nil {
		t.Error("duplicate create should fail")
	}

	must(p.AddColumns("BlogPost", []string{"views:int", "meta:json", "summary:text:null"}))
	must(p.RenameColumn("BlogPost", "views", "view_count"))
	must(p.RenameColumn("BlogPost", "summary", "abstract")) // keeps schema:"type:text" (kiln diff saw a change otherwise)
	must(p.DropColumns("BlogPost", []string{"published_at"}))
	must(p.AddIndex("BlogPost", []string{"title"}, true))

	model = strings.Join(strings.Fields(read(t, p.path("models/blog_post.go"))), " ")
	if !strings.Contains(model, "ViewCount int `db:\"view_count\"`") || strings.Contains(model, "PublishedAt") ||
		!strings.Contains(model, "Meta map[string]any `db:\"meta,json\"`") ||
		!strings.Contains(model, "Abstract *string `db:\"abstract\" schema:\"type:text\"`") {
		t.Errorf("model after edits:\n%s", model)
	}

	entries, _ := os.ReadDir(p.path("db/migrations"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{
		"20260102030405_create_blog_posts.go",
		"20260102030406_create_report_x.go",
		"20260102030407_add_views_meta_summary_to_blog_posts.go",
		"20260102030408_rename_views_to_view_count_in_blog_posts.go",
		"20260102030409_rename_summary_to_abstract_in_blog_posts.go",
		"20260102030410_drop_published_at_from_blog_posts.go",
		"20260102030411_add_index_uniq_blog_posts_title.go",
		"migrations.go",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("migrations:\n%s", strings.Join(names, "\n"))
	}
	drop := read(t, p.path("db/migrations", want[5]))
	if !strings.Contains(drop, `a.DateTime("published_at")`) {
		t.Errorf("drop-column down should restore a nullable datetime:\n%s", drop)
	}
}

func TestRenameTagKeepsEverything(t *testing.T) {
	for _, c := range []struct{ old, want string }{
		{"`db:\"bio\" schema:\"type:text\"`", "`db:\"about\" schema:\"type:text\"`"},
		{"`db:\"version,lock\"`", "`db:\"rev,lock\"`"},
		{"`db:\"email\" validate:\"required,email\" mask:\"email\"`", "`db:\"mail\" validate:\"required,email\" mask:\"email\"`"},
		{"`db:\"prefs,json\" schema:\"type:text\"`", "`db:\"settings,json\" schema:\"type:text\"`"},
		{"`mask:\"full\"`", "`db:\"token\" mask:\"full\"`"},
		{"", "`db:\"token\"`"},
	} {
		col := map[string]string{"`db:\"bio\" schema:\"type:text\"`": "about", "`db:\"version,lock\"`": "rev",
			"`db:\"email\" validate:\"required,email\" mask:\"email\"`": "mail", "`db:\"prefs,json\" schema:\"type:text\"`": "settings"}[c.old]
		if col == "" {
			col = "token"
		}
		if got := renameTag(c.old, col, false); got != c.want {
			t.Errorf("renameTag(%s) = %s, want %s", c.old, got, c.want)
		}
	}
}
