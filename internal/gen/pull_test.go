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
	})
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
