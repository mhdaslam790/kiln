package migrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/schema"
)

// withSQLiteFKsOff runs fn on a pinned connection with foreign key
// enforcement disabled, as SQLite's table-rebuild procedure requires (a
// DROP TABLE would otherwise fire ON DELETE actions on child rows). Foreign
// keys are verified with PRAGMA foreign_key_check before fn's transaction
// commits (see sqliteAlter) and the setting is restored afterwards.
func (m *Migrator) withSQLiteFKsOff(ctx context.Context, fn func(ctx context.Context) error) error {
	if m.db.Dialect().Kind() != dialect.KindSQLite || m.db.InTransaction(ctx) {
		return fn(ctx)
	}
	return m.db.WithConn(ctx, func(ctx context.Context) error {
		var on int
		if err := m.db.Query(ctx, &on, "PRAGMA foreign_keys"); err != nil {
			return err
		}
		if on == 0 {
			return fn(ctx)
		}
		if _, err := m.db.Exec(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return err
		}
		defer m.db.Exec(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON") //nolint:errcheck
		return fn(context.WithValue(ctx, fkCheckKey{}, true))
	})
}

type fkCheckKey struct{}

// sqliteAlter performs operations SQLite's ALTER TABLE cannot do by
// rebuilding the table. It reports whether it handled op.
func (m *Migrator) sqliteAlter(ctx context.Context, op schema.Op) (bool, error) {
	if m.db.Dialect().Kind() != dialect.KindSQLite {
		return false, nil
	}
	var table string
	var mutate func(t *schema.TableState) error
	switch o := op.(type) {
	case schema.ChangeColumn:
		table = o.Table
		mutate = func(t *schema.TableState) error {
			c := t.Column(o.Column.Name)
			if c == nil {
				return fmt.Errorf("column %s.%s does not exist", o.Table, o.Column.Name)
			}
			nc := o.Column
			nc.Primary = c.Primary
			*c = nc
			return nil
		}
	case schema.AddForeignKey:
		table = o.Table
		mutate = func(t *schema.TableState) error { t.ForeignKeys = append(t.ForeignKeys, o.Key); return nil }
	case schema.DropForeignKey:
		table = o.Table
		mutate = func(t *schema.TableState) error {
			return (&schema.State{Tables: map[string]*schema.TableState{t.Name: t}}).Apply(op)
		}
	case schema.DropColumn:
		// Native DROP COLUMN works unless the column is indexed, unique or
		// part of a key.
		st, err := Inspect(ctx, m.db)
		if err != nil {
			return false, err
		}
		t := st.Tables[o.Table]
		if t == nil || !columnConstrained(t, o.Name) {
			return false, nil
		}
		table = o.Table
		mutate = func(t *schema.TableState) error {
			cols := t.Columns[:0]
			for _, c := range t.Columns {
				if c.Name != o.Name {
					cols = append(cols, c)
				}
			}
			t.Columns = cols
			fks := t.ForeignKeys[:0]
			for _, fk := range t.ForeignKeys {
				if !containsStr(fk.Columns, o.Name) {
					fks = append(fks, fk)
				}
			}
			t.ForeignKeys = fks
			idx := t.Indexes[:0]
			for _, ix := range t.Indexes {
				if !containsStr(ix.Columns, o.Name) {
					idx = append(idx, ix)
				}
			}
			t.Indexes = idx
			return nil
		}
	default:
		return false, nil
	}
	return true, m.rebuild(ctx, table, mutate)
}

func columnConstrained(t *schema.TableState, col string) bool {
	if c := t.Column(col); c != nil && (c.Primary || c.Unique) {
		return true
	}
	for _, ix := range t.Indexes {
		if containsStr(ix.Columns, col) {
			return true
		}
	}
	for _, fk := range t.ForeignKeys {
		if containsStr(fk.Columns, col) {
			return true
		}
	}
	return false
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// rebuild implements https://www.sqlite.org/lang_altertable.html#otheralter:
// create the new table, copy rows, drop the old one, rename, re-index.
func (m *Migrator) rebuild(ctx context.Context, table string, mutate func(t *schema.TableState) error) error {
	if ctx.Value(fkCheckKey{}) == nil {
		var on int
		if err := m.db.Query(ctx, &on, "PRAGMA foreign_keys"); err == nil && on == 1 {
			return fmt.Errorf("sqlite: rebuilding %s needs foreign keys disabled; run it through the migrator", table)
		}
	}
	st, err := Inspect(ctx, m.db)
	if err != nil {
		return err
	}
	t := st.Tables[table]
	if t == nil {
		return fmt.Errorf("sqlite: table %s does not exist", table)
	}
	oldCols := map[string]bool{}
	for _, c := range t.Columns {
		oldCols[c.Name] = true
	}
	if err := mutate(t); err != nil {
		return err
	}

	d := m.db.Dialect()
	tmp := "_kiln_rebuild_" + table
	cols := make([]schema.Column, len(t.Columns))
	copy(cols, t.Columns)
	for i := range cols {
		cols[i].Unique = false // recreated from the unique indexes below
	}
	var stmts []dialect.Stmt
	ct, err := d.DDL(schema.CreateTable{Name: tmp, Columns: cols, PrimaryKey: t.PrimaryKey, ForeignKeys: t.ForeignKeys})
	if err != nil {
		return err
	}
	stmts = append(stmts, ct...)

	var common []string
	for _, c := range t.Columns {
		if oldCols[c.Name] {
			common = append(common, d.Quote(c.Name))
		}
	}
	list := strings.Join(common, ", ")
	stmts = append(stmts,
		dialect.Stmt{SQL: "INSERT INTO " + d.Quote(tmp) + " (" + list + ") SELECT " + list + " FROM " + d.Quote(table)},
		dialect.Stmt{SQL: "DROP TABLE " + d.Quote(table)},
		dialect.Stmt{SQL: "ALTER TABLE " + d.Quote(tmp) + " RENAME TO " + d.Quote(table)},
	)
	for _, ix := range t.Indexes {
		ix.Table = table
		is, err := d.DDL(ix)
		if err != nil {
			return err
		}
		stmts = append(stmts, is...)
	}
	for _, s := range stmts {
		if _, err := m.db.Executor(ctx).ExecContext(ctx, s.SQL, s.Args...); err != nil {
			return fmt.Errorf("%w\n  sql: %s", err, s.SQL)
		}
	}
	// With enforcement off, verify integrity explicitly before commit.
	var bad []struct {
		Table string `db:"table"`
	}
	if err := m.db.Query(ctx, &bad, "PRAGMA foreign_key_check("+d.Quote(table)+")"); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("sqlite: rebuilding %s would violate %d foreign key reference(s)", table, len(bad))
	}
	return nil
}
