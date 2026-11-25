package gen

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/schema"
)

// The audit table has a table_name column: its field must not clash with
// the TableName method (it did: pulled models failed to compile).
func TestPullTableNameColumn(t *testing.T) {
	src := modelFromTable("domain", "AuditLog", &schema.TableState{
		Name: "audit_log",
		Columns: []schema.Column{
			{Name: "id", Type: schema.ID, Primary: true},
			{Name: "table_name", Type: schema.String, Required: true},
			{Name: "changes", Type: schema.Text},
		},
		PrimaryKey: []string{"id"},
	}, nil)
	if _, err := parser.ParseFile(token.NewFileSet(), "audit_log.go", src, 0); err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	flat := strings.Join(strings.Fields(src), " ")
	for _, want := range []string{"TableNameCol string `db:\"table_name\"`", `func (AuditLog) TableName() string { return "audit_log" }`} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
	if strings.Contains(flat, "\tTableName string") || strings.Contains(flat, " TableName string `") {
		t.Errorf("field named TableName:\n%s", src)
	}
}

func TestPullArraysAndEnums(t *testing.T) {
	enums := map[string]string{"kiln mood": "KilnMood"}
	src := modelFromTable("domain", "Post", &schema.TableState{
		Name: "posts",
		Columns: []schema.Column{
			{Name: "id", Type: schema.ID, Primary: true},
			{Name: "tags", Type: schema.Text, SQLType: "TEXT[]"},
			{Name: "scores", Type: schema.Text, SQLType: "BIGINT[]", Required: true},
			{Name: "moods", Type: schema.Text, SQLType: "kiln mood[]"},
			{Name: "mood", Type: schema.Text, SQLType: "kiln mood", Required: true},
			{Name: "old_mood", Type: schema.Text, SQLType: "kiln mood"},
			{Name: "boxes", Type: schema.Text, SQLType: "BOX[]"},
		},
		PrimaryKey: []string{"id"},
	}, enums)
	if _, err := parser.ParseFile(token.NewFileSet(), "post.go", src, 0); err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	for _, want := range []string{
		`"github.com/mhdaslam790/kiln/kilnpg"`,
		"Tags kilnpg.Array[string] `db:\"tags\" schema:\"sqltype:TEXT[]\"`",
		"Scores kilnpg.Array[int64] `db:\"scores\" schema:\"sqltype:BIGINT[],notnull\"`",
		"Moods kilnpg.Array[KilnMood]",
		"Mood KilnMood `db:\"mood\" schema:\"type:text,sqltype:kiln mood\"`",
		"OldMood *KilnMood",
		"Boxes *string", // no Go element type: text, as before
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
	e := enumSource("domain", "KilnMood", "kiln mood", []string{"happy", "it's ok", "It's OK", "!!"})
	if _, err := parser.ParseFile(token.NewFileSet(), "enum.go", e, 0); err != nil {
		t.Fatalf("%v\n%s", err, e)
	}
	for _, want := range []string{"type KilnMood string", `KilnMoodHappy KilnMood = "happy"`, `KilnMoodItSOk KilnMood = "it's ok"`, `KilnMoodItSOK KilnMood = "It's OK"`, `KilnMoodX2 KilnMood = "!!"`} {
		if !strings.Contains(e, want) {
			t.Errorf("missing %q in\n%s", want, e)
		}
	}
}
