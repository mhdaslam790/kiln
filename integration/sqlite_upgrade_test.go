package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
)

func TestSQLiteDatabaseFromOlderKiln(t *testing.T) {
	ctx := context.Background()
	sqldb, _ := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db"))
	defer sqldb.Close()
	// As kiln <= v1.8.3 created it (double-quoted names).
	for _, s := range []string{
		`CREATE TABLE "things" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "name" VARCHAR(255) NOT NULL, "old-col" TEXT, "price" INTEGER DEFAULT 0)`,
		`CREATE INDEX "idx_things_name" ON "things" ("name")`,
		`INSERT INTO "things" ("name", "old-col", "price") VALUES ('a', 'x', 5)`,
	} {
		if _, err := sqldb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db := orm.Open(sqldb, dialect.SQLite())
	st, err := migrate.Inspect(ctx, db)
	must(t, err)
	tb := st.Tables["things"]
	if tb == nil || len(tb.Columns) != 4 {
		t.Fatalf("inspect: %+v", tb)
	}
	m := migrate.New(db, []migrate.Migration{{Version: 1, Name: "alter",
		Up: func(s *schema.Schema) {
			s.ChangeColumn("things", "price", schema.BigInt, schema.Required(), schema.Default(1))
			s.DropColumn("things", "old-col")
			s.AddColumn("things", "new-col", schema.String)
		},
		Down: func(s *schema.Schema) {},
	}})
	_, err = m.Up(ctx)
	must(t, err)
	var name string
	var price int64

	if err := sqldb.QueryRow("SELECT name, price FROM things").Scan(&name, &price); err != nil || name != "a" || price != 5 {
		t.Fatalf("data after rebuild: %q %d %v", name, price, err)
	}
	var idx int
	sqldb.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_things_name'").Scan(&idx)
	if idx != 1 {
		t.Fatal("index lost in rebuild")
	}
}
