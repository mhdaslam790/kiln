package migrate

import (
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/schema"
)

func TestLintTenantIndexes(t *testing.T) {
	mig := func(v int64, name string, up func(s *schema.Schema)) Migration {
		return Migration{Version: v, Name: name, Up: up, Down: func(*schema.Schema) {}}
	}
	history := []Migration{
		mig(1, "create_tables", func(s *schema.Schema) {
			s.CreateTable("tenants", func(t *schema.Table) { t.ID("id") }) // no column: ignored
			s.CreateTable("tasks", func(t *schema.Table) {
				t.ID("id")
				t.BigInt("tenant_id", schema.Required())
				t.Index([]string{"tenant_id", "id"}) // leading: fine
			})
			s.CreateTable("notes", func(t *schema.Table) {
				t.ID("id")
				t.BigInt("tenant_id", schema.Required()) // no index: reported
			})
			s.CreateTable("events", func(t *schema.Table) {
				t.ID("id")
				t.BigInt("tenant_id", schema.Required())
				t.DateTime("at")
				t.Index([]string{"at", "tenant_id"}) // not leading: reported
			})
			s.CreateTable("members", func(t *schema.Table) {
				t.BigInt("tenant_id", schema.Primary())
				t.BigInt("user_id", schema.Primary()) // composite PK starts with it: fine
			})
			s.CreateTable("slugs", func(t *schema.Table) {
				t.ID("id")
				t.BigInt("tenant_id", schema.Required())
				t.String("slug")
				t.Unique("tenant_id", "slug") // unique constraint starts with it: fine
			})
		}),
		mig(2, "add_tenant_to_users", func(s *schema.Schema) {
			s.CreateTable("users", func(t *schema.Table) { t.ID("id") })
			s.AddColumn("users", "tenant_id", schema.BigInt, schema.Required(), schema.Default(1))
		}),
		mig(3, "index_users_tenant", func(s *schema.Schema) {
			s.CreateIndex("users", "", []string{"tenant_id"}, schema.Concurrently()) // fixed later: fine
		}),
		mig(4, "add_tenant_to_logs", func(s *schema.Schema) {
			s.CreateTable("logs", func(t *schema.Table) { t.ID("id") })
			s.AddColumn("logs", "tenant_id", schema.BigInt)
			s.AllowUnsafe("logs are only read by admins", "tenant-column-without-index")
		}),
		mig(5, "rename_notes", func(s *schema.Schema) { s.RenameTable("notes", "memos") }),
	}

	fs, err := LintTenantIndexes(history, "tenant_id")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		if f.Check != "tenant-column-without-index" || f.Severity != Warning || f.Column != "tenant_id" || !strings.Contains(f.Fix, "CreateIndex") {
			t.Errorf("finding: %+v", f)
		}
		got = append(got, f.Table+"@"+f.Name)
	}
	want := "events@create_tables memos@create_tables" // sorted by table
	if strings.Join(got, " ") != want {
		t.Fatalf("findings = %q, want %q", strings.Join(got, " "), want)
	}

	if fs, _ := LintTenantIndexes(history, ""); fs != nil {
		t.Fatal("no column must mean no check")
	}
}

func TestTenantTables(t *testing.T) {
	h := []Migration{
		{Version: 1, Name: "a", Up: func(s *schema.Schema) {
			s.CreateTable("tasks", func(t *schema.Table) { t.ID("id"); t.BigInt("tenant_id") })
			s.CreateTable("plans", func(t *schema.Table) { t.ID("id") })
		}},
		{Version: 2, Name: "b", Up: func(s *schema.Schema) {
			s.CreateTable("notes", func(t *schema.Table) { t.ID("id") })
			s.AddColumn("notes", "tenant_id", schema.BigInt)
			s.RenameTable("tasks", "todos")
		}},
	}
	got, err := TenantTables(h, "tenant_id")
	if err != nil || strings.Join(got, ",") != "notes,todos" {
		t.Fatalf("TenantTables = %v, %v", got, err)
	}
}
