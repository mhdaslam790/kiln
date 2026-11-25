package kilnpg

import (
	"strings"

	"github.com/mhdaslam790/kiln/schema"
)

// CreateEnum creates a PostgreSQL enum type in a migration:
//
//	kilnpg.CreateEnum(s, "mood", "happy", "sad")
//	s.CreateTable("people", func(t *schema.Table) {
//		t.ID("id")
//		t.String("mood", schema.Required(), kilnpg.EnumType("mood"))
//	})
//
// In Go the column is a string type (type Mood string). Drop it in Down
// with DropEnum, after the tables that use it.
func CreateEnum(s *schema.Schema, name string, values ...string) {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = literal(v)
	}
	s.Exec("CREATE TYPE " + pg.QuoteName(name) + " AS ENUM (" + strings.Join(quoted, ", ") + ")")
}

// AddEnumValue adds a value to an enum type (PostgreSQL 12+ runs it inside
// the migration's transaction, but the new value can only be used after
// that transaction commits: use it in a later migration).
func AddEnumValue(s *schema.Schema, name, value string) {
	s.Exec("ALTER TYPE " + pg.QuoteName(name) + " ADD VALUE IF NOT EXISTS " + literal(value))
}

// DropEnum drops an enum type.
func DropEnum(s *schema.Schema, name string) {
	s.Exec("DROP TYPE IF EXISTS " + pg.QuoteName(name))
}

// EnumType is the column option for an enum type made by CreateEnum.
func EnumType(name string) schema.ColumnOption { return schema.SQLType(pg.QuoteName(name)) }

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
