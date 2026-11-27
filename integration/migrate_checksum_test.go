package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
)

func checksumSet(col string) []migrate.Migration {
	var set migrate.Set
	set.Add(20990201000001, "create_widgets", func(s *schema.Schema) {
		s.CreateTable("widgets", func(t *schema.Table) {
			t.ID("id")
			t.String(col)
		})
	}, func(s *schema.Schema) { s.DropTable("widgets") })
	return set.List()
}

type migRow struct {
	Version  int64  `db:"version"`
	File     string `db:"file"`
	Checksum string `db:"checksum"`
}

// TestMigrationFileAndChecksum: applied migrations record their source file
// and checksum, and editing an applied migration is reported.
func TestMigrationFileAndChecksum(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		var logs []string
		logf := migrate.WithLog(func(f string, a ...any) { logs = append(logs, f) })
		m := migrate.New(db, checksumSet("name"), migrate.WithTable("mig_checksum"), logf)
		_, err := m.Up(ctx)
		must(t, err)
		t.Cleanup(func() {
			_, _ = m.Down(ctx, 1)
			_, _ = db.Exec(ctx, "DROP TABLE mig_checksum")
		})

		var rows []migRow
		must(t, db.Query(ctx, &rows, metaSQL(db, "mig_checksum")))
		if len(rows) != 1 || rows[0].File != "migrate_checksum_test.go" || len(rows[0].Checksum) != 64 {
			t.Fatalf("recorded %+v", rows)
		}
		st, err := m.Status(ctx)
		must(t, err)
		if st[0].Changed || st[0].Checksum != rows[0].Checksum || st[0].File != "migrate_checksum_test.go" {
			t.Fatalf("unchanged migration: %+v", st[0])
		}

		// The same version with an edited Up.
		edited := migrate.New(db, checksumSet("title"), migrate.WithTable("mig_checksum"), logf)
		st, err = edited.Status(ctx)
		must(t, err)
		if !st[0].Changed {
			t.Fatalf("edit not detected: %+v", st[0])
		}
		logs = nil
		_, err = edited.Up(ctx)
		must(t, err)
		if len(logs) != 1 || !strings.Contains(logs[0], "edited after it was applied") {
			t.Fatalf("logs %q", logs)
		}
	})
}

// TestMigrationTableUpgrade: a bookkeeping table without file and checksum
// columns gets them, filled in from the code, on the next Up.
func TestMigrationTableUpgrade(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		var s schema.Schema
		s.CreateTable("mig_old", func(t *schema.Table) {
			t.BigInt("version", schema.Primary(), schema.Required())
			t.String("name", schema.Required())
			t.DateTime("applied_at", schema.Required())
		})
		s.CreateTable("widgets", func(t *schema.Table) {
			t.ID("id")
			t.String("name")
		})
		old := migrate.Migration{Version: 1, Name: "old", Up: func(sc *schema.Schema) { sc.Ops = append(sc.Ops, s.Ops...) }}
		must(t, applyScratch(ctx, db, old))
		t.Cleanup(func() {
			_, _ = db.Exec(ctx, "DROP TABLE widgets")
			_, _ = db.Exec(ctx, "DROP TABLE mig_old")
		})
		_, err := db.Exec(ctx, "INSERT INTO mig_old (version, name, applied_at) VALUES (?, ?, ?)",
			int64(20990201000001), "create_widgets", time.Now().UTC())
		must(t, err)

		m := migrate.New(db, checksumSet("name"), migrate.WithTable("mig_old"))
		st, err := m.Status(ctx) // works before the upgrade
		must(t, err)
		if !st[0].Applied || st[0].Changed {
			t.Fatalf("before upgrade: %+v", st[0])
		}
		_, err = m.Up(ctx)
		must(t, err)
		var rows []migRow
		must(t, db.Query(ctx, &rows, metaSQL(db, "mig_old")))
		if len(rows) != 1 || rows[0].File != "migrate_checksum_test.go" || rows[0].Checksum != st[0].Checksum {
			t.Fatalf("after upgrade %+v", rows)
		}
	})
}

// applyScratch applies a migration's Up without bookkeeping.
func applyScratch(ctx context.Context, db *orm.DB, mg migrate.Migration) error {
	m := migrate.New(db, []migrate.Migration{mg}, migrate.WithTable("mig_scratch"))
	if _, err := m.Up(ctx); err != nil {
		return err
	}
	_, err := db.Exec(ctx, "DROP TABLE mig_scratch")
	return err
}

// metaSQL selects the bookkeeping columns, quoted (FILE is reserved in SQL Server).
func metaSQL(db *orm.DB, table string) string {
	d := db.Dialect()
	return "SELECT version, " + d.Quote("file") + ", " + d.Quote("checksum") + " FROM " + d.Quote(table)
}
