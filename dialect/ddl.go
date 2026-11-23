package dialect

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mhdaslam790/kiln/schema"
)

// Raw compiles a raw statement, rewriting '?' placeholders.
func (d *sqlDialect) Raw(sql string, args []any) (Stmt, error) {
	b := &buf{d: d}
	if err := b.raw(sql, args, true); err != nil {
		return Stmt{}, err
	}
	return b.stmt(), nil
}

func (d *sqlDialect) DDL(op schema.Op) ([]Stmt, error) {
	one := func(s string) ([]Stmt, error) { return []Stmt{{SQL: s}}, nil }
	t := d.Quote
	switch o := op.(type) {
	case schema.CreateTable:
		return d.createTable(o)

	case schema.DropTable:
		s := "DROP TABLE "
		if o.IfExists {
			s += "IF EXISTS "
		}
		return one(s + t(o.Name))

	case schema.RenameTable:
		switch d.kind {
		case KindMySQL:
			return one("RENAME TABLE " + t(o.Name) + " TO " + t(o.NewName))
		case KindMSSQL:
			return one("EXEC sp_rename " + sqlString(o.Name) + ", " + sqlString(o.NewName))
		}
		return one("ALTER TABLE " + t(o.Name) + " RENAME TO " + t(o.NewName))

	case schema.AddColumn:
		if o.Column.Unique && d.kind == KindSQLite {
			// SQLite cannot ADD COLUMN ... UNIQUE; add a unique index instead.
			c := o.Column
			c.Unique = false
			stmts, err := d.DDL(schema.AddColumn{Table: o.Table, Column: c})
			if err != nil {
				return nil, err
			}
			ix, err := d.createIndex(schema.CreateIndex{Table: o.Table, Name: schema.IndexName(o.Table, []string{c.Name}, true), Columns: []string{c.Name}, Unique: true})
			return append(stmts, ix...), err
		}
		def, err := d.columnDef(o.Column, true)
		if err != nil {
			return nil, err
		}
		ref := o.Column.Ref
		if ref != nil && d.kind == KindSQLite {
			// SQLite can only add a foreign key inline with the column.
			def += " REFERENCES " + t(ref.RefTable) + " (" + d.quoteList(ref.RefColumns) + ")"
			if ref.OnDelete != "" {
				def += " ON DELETE " + ref.OnDelete
			}
			if ref.OnUpdate != "" {
				def += " ON UPDATE " + ref.OnUpdate
			}
			ref = nil
		}
		add := "ALTER TABLE " + t(o.Table) + " ADD COLUMN " + def
		if d.kind == KindMSSQL {
			add = "ALTER TABLE " + t(o.Table) + " ADD " + def
		}
		stmts := []Stmt{{SQL: add}}
		if ref != nil {
			stmts = append(stmts, Stmt{SQL: "ALTER TABLE " + t(o.Table) + " ADD " + d.fkDef(*ref)})
		}
		return stmts, nil

	case schema.DropColumn:
		drop := Stmt{SQL: "ALTER TABLE " + t(o.Table) + " DROP COLUMN " + t(o.Name)}
		if d.kind == KindMSSQL {
			// SQL Server refuses to drop a column that still has a default
			// constraint (often auto-named, e.g. DF__users__age__3A81B327).
			return []Stmt{d.mssqlDropDefault(o.Table, o.Name), drop}, nil
		}
		return []Stmt{drop}, nil

	case schema.RenameColumn:
		if d.kind == KindMSSQL {
			return one("EXEC sp_rename " + sqlString(o.Table+"."+o.Name) + ", " + sqlString(o.NewName) + ", 'COLUMN'")
		}
		return one("ALTER TABLE " + t(o.Table) + " RENAME COLUMN " + t(o.Name) + " TO " + t(o.NewName))

	case schema.ChangeColumn:
		return d.changeColumn(o)

	case schema.AddForeignKey:
		if d.kind == KindSQLite {
			return nil, errors.New("dialect: SQLite cannot add a foreign key to an existing table; declare it in CreateTable")
		}
		return one("ALTER TABLE " + t(o.Table) + " ADD " + d.fkDef(o.Key))

	case schema.DropForeignKey:
		switch d.kind {
		case KindSQLite:
			return nil, errors.New("dialect: SQLite cannot drop a foreign key from an existing table")
		case KindMySQL:
			return one("ALTER TABLE " + t(o.Table) + " DROP FOREIGN KEY " + t(o.Name))
		}
		return one("ALTER TABLE " + t(o.Table) + " DROP CONSTRAINT " + t(o.Name))

	case schema.CreateIndex:
		return d.createIndex(o)

	case schema.DropIndex:
		switch d.kind {
		case KindMySQL:
			return one("DROP INDEX " + t(o.Name) + " ON " + t(o.Table))
		case KindMSSQL:
			s := "DROP INDEX "
			if o.IfExists {
				s += "IF EXISTS "
			}
			return one(s + t(o.Name) + " ON " + t(o.Table))
		}
		s := "DROP INDEX "
		if o.IfExists {
			s += "IF EXISTS "
		}
		return one(s + t(o.Name))

	case schema.AllowUnsafe:
		return nil, nil // lint marker, no SQL

	case schema.Exec:
		st, err := d.Raw(o.SQL, o.Args)
		if err != nil {
			return nil, err
		}
		return []Stmt{st}, nil
	}
	return nil, fmt.Errorf("dialect: cannot compile %T", op)
}

func (d *sqlDialect) createTable(o schema.CreateTable) ([]Stmt, error) {
	var sb strings.Builder
	if o.IfNotExists && d.kind == KindMSSQL {
		sb.WriteString("IF OBJECT_ID(" + sqlString(o.Name) + ", 'U') IS NULL ")
	}
	sb.WriteString("CREATE TABLE ")
	if o.IfNotExists && d.kind != KindMSSQL {
		sb.WriteString("IF NOT EXISTS ")
	}
	sb.WriteString(d.Quote(o.Name))
	sb.WriteString(" (")

	pk := o.PrimaryKey
	if len(pk) == 0 {
		for _, c := range o.Columns {
			if c.Primary {
				pk = append(pk, c.Name)
			}
		}
	}
	inlinePK := len(pk) == 1

	var parts []string
	for _, c := range o.Columns {
		c.Primary = inlinePK && c.Name == pk[0]
		def, err := d.columnDef(c, false)
		if err != nil {
			return nil, err
		}
		parts = append(parts, def)
	}
	if !inlinePK && len(pk) > 0 {
		parts = append(parts, "PRIMARY KEY ("+d.quoteList(pk)+")")
	}
	for _, u := range o.Uniques {
		parts = append(parts, "CONSTRAINT "+d.Quote(schema.IndexName(o.Name, u, true))+" UNIQUE ("+d.quoteList(u)+")")
	}
	for _, fk := range o.ForeignKeys {
		parts = append(parts, d.fkDef(fk))
	}
	sb.WriteString(strings.Join(parts, ", "))
	sb.WriteString(")")
	if o.Options != "" {
		sb.WriteString(" " + o.Options)
	}
	return []Stmt{{SQL: sb.String()}}, nil
}

func (d *sqlDialect) quoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = d.Quote(c)
	}
	return strings.Join(q, ", ")
}

func (d *sqlDialect) fkDef(fk schema.ForeignKey) string {
	s := "CONSTRAINT " + d.Quote(fk.Name) + " FOREIGN KEY (" + d.quoteList(fk.Columns) + ") REFERENCES " +
		d.Quote(fk.RefTable) + " (" + d.quoteList(fk.RefColumns) + ")"
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}

func (d *sqlDialect) createIndex(o schema.CreateIndex) ([]Stmt, error) {
	if o.Where != "" && d.kind == KindMySQL {
		return nil, errors.New("dialect: MySQL does not support partial indexes")
	}
	var sb strings.Builder
	if o.IfNotExists && d.kind == KindMSSQL {
		sb.WriteString("IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = " + sqlString(o.Name) +
			" AND object_id = OBJECT_ID(" + sqlString(o.Table) + ")) ")
	}
	sb.WriteString("CREATE ")
	if o.Unique {
		sb.WriteString("UNIQUE ")
	}
	sb.WriteString("INDEX ")
	if o.Concurrently && d.kind == KindPostgres {
		sb.WriteString("CONCURRENTLY ")
	}
	// MySQL has no CREATE INDEX IF NOT EXISTS; the flag is ignored there.
	if o.IfNotExists && (d.kind == KindPostgres || d.kind == KindSQLite) {
		sb.WriteString("IF NOT EXISTS ")
	}
	sb.WriteString(d.Quote(o.Name) + " ON " + d.Quote(o.Table) + " (" + d.quoteList(o.Columns) + ")")
	if o.Where != "" {
		sb.WriteString(" WHERE " + o.Where)
	}
	return []Stmt{{SQL: sb.String()}}, nil
}

func (d *sqlDialect) changeColumn(o schema.ChangeColumn) ([]Stmt, error) {
	t, c := d.Quote(o.Table), o.Column
	switch d.kind {
	case KindSQLite:
		return nil, errors.New("dialect: SQLite cannot change a column; recreate the table")
	case KindMySQL:
		def, err := d.columnDef(c, true)
		if err != nil {
			return nil, err
		}
		return []Stmt{{SQL: "ALTER TABLE " + t + " MODIFY COLUMN " + def}}, nil
	case KindMSSQL:
		typ, err := d.columnType(c)
		if err != nil {
			return nil, err
		}
		s := "ALTER TABLE " + t + " ALTER COLUMN " + d.Quote(c.Name) + " " + typ
		if c.Required {
			s += " NOT NULL"
		} else {
			s += " NULL"
		}
		// A default constraint blocks ALTER COLUMN, and SQL Server keeps
		// defaults as separate constraints: drop it, alter, then re-add.
		stmts := []Stmt{d.mssqlDropDefault(o.Table, c.Name), {SQL: s}}
		if c.HasDef {
			lit, err := d.literal(c.Default)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, Stmt{SQL: "ALTER TABLE " + t + " ADD CONSTRAINT " +
				d.Quote("DF_"+o.Table+"_"+c.Name) + " DEFAULT " + lit + " FOR " + d.Quote(c.Name)})
		}
		return stmts, nil
	}
	typ, err := d.columnType(c)
	if err != nil {
		return nil, err
	}
	col := d.Quote(c.Name)
	parts := []string{"ALTER COLUMN " + col + " TYPE " + typ}
	if c.Required {
		parts = append(parts, "ALTER COLUMN "+col+" SET NOT NULL")
	} else {
		parts = append(parts, "ALTER COLUMN "+col+" DROP NOT NULL")
	}
	if c.HasDef {
		lit, err := d.literal(c.Default)
		if err != nil {
			return nil, err
		}
		parts = append(parts, "ALTER COLUMN "+col+" SET DEFAULT "+lit)
	}
	return []Stmt{{SQL: "ALTER TABLE " + t + " " + strings.Join(parts, ", ")}}, nil
}

func (d *sqlDialect) columnDef(c schema.Column, alter bool) (string, error) {
	typ, err := d.columnType(c)
	if err != nil {
		return "", err
	}
	s := d.Quote(c.Name) + " " + typ
	if c.Unsigned && d.kind == KindMySQL {
		s += " UNSIGNED"
	}
	if c.Required || c.Primary {
		s += " NOT NULL"
	}
	if c.Type == schema.ID {
		switch d.kind {
		case KindMySQL:
			s += " AUTO_INCREMENT"
		case KindMSSQL:
			s += " IDENTITY(1,1)"
		}
	}
	if c.HasDef {
		lit, err := d.literal(c.Default)
		if err != nil {
			return "", err
		}
		s += " DEFAULT " + lit
	}
	if c.Unique {
		s += " UNIQUE"
	}
	if c.Primary {
		s += " PRIMARY KEY"
		if c.Type == schema.ID && d.kind == KindSQLite {
			s += " AUTOINCREMENT"
		}
	} else if alter && c.Type == schema.ID {
		return "", errors.New("dialect: ID columns can only be added in CreateTable")
	}
	if c.Options != "" {
		s += " " + c.Options
	}
	return s, nil
}

func (d *sqlDialect) ColumnType(c schema.Column) (string, error) { return d.columnType(c) }

func (d *sqlDialect) columnType(c schema.Column) (string, error) {
	if c.SQLType != "" {
		return c.SQLType, nil
	}
	pg, my, lite := d.kind == KindPostgres, d.kind == KindMySQL, d.kind == KindSQLite
	pick := func(p, m, s, x string) string {
		switch {
		case pg:
			return p
		case my:
			return m
		case lite:
			return s
		}
		return x
	}
	prec := func(base string) string {
		if c.Precision > 0 {
			return base + "(" + strconv.Itoa(c.Precision) + ")"
		}
		return base
	}
	switch c.Type {
	case schema.ID:
		return pick("BIGSERIAL", "BIGINT", "INTEGER", "BIGINT"), nil
	case schema.Bool:
		return pick("BOOLEAN", "BOOL", "BOOLEAN", "BIT"), nil
	case schema.SmallInt:
		return "SMALLINT", nil
	case schema.Int:
		return pick("INTEGER", "INT", "INTEGER", "INT"), nil
	case schema.BigInt:
		return "BIGINT", nil
	case schema.Float:
		return pick("DOUBLE PRECISION", "DOUBLE", "REAL", "FLOAT"), nil
	case schema.Decimal:
		if c.Precision > 0 {
			return "DECIMAL(" + strconv.Itoa(c.Precision) + "," + strconv.Itoa(c.Scale) + ")", nil
		}
		return "DECIMAL", nil
	case schema.String:
		n := c.Limit
		if n <= 0 {
			n = 255
		}
		return pick("VARCHAR", "VARCHAR", "VARCHAR", "NVARCHAR") + "(" + strconv.Itoa(n) + ")", nil
	case schema.Text:
		return pick("TEXT", "TEXT", "TEXT", "NVARCHAR(MAX)"), nil
	case schema.Date:
		return "DATE", nil
	case schema.DateTime:
		return prec(pick("TIMESTAMPTZ", "DATETIME", "DATETIME", "DATETIME2")), nil
	case schema.Time:
		return prec("TIME"), nil
	case schema.Bytes:
		return pick("BYTEA", "LONGBLOB", "BLOB", "VARBINARY(MAX)"), nil
	case schema.JSON:
		return pick("JSONB", "JSON", "TEXT", "NVARCHAR(MAX)"), nil
	case schema.UUID:
		return pick("UUID", "CHAR(36)", "TEXT", "UNIQUEIDENTIFIER"), nil
	}
	return "", fmt.Errorf("dialect: unknown column type %q for %s", c.Type, c.Name)
}

func (d *sqlDialect) literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case schema.Raw:
		return string(x), nil
	case string:
		return sqlString(x), nil
	case bool:
		if d.kind == KindSQLite || d.kind == KindMSSQL {
			if x {
				return "1", nil
			}
			return "0", nil
		}
		if x {
			return "TRUE", nil
		}
		return "FALSE", nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", x), nil
	case float32, float64:
		return fmt.Sprintf("%v", x), nil
	case time.Time:
		return sqlString(x.UTC().Format("2006-01-02 15:04:05")), nil
	}
	return "", fmt.Errorf("dialect: unsupported default value %T", v)
}

// mssqlDropDefault drops the default constraint of table.column, whatever
// its name, if there is one.
func (d *sqlDialect) mssqlDropDefault(table, column string) Stmt {
	// EXEC('...' + f()) does not allow function calls, so the statement is
	// built in a variable and run with sp_executesql.
	return Stmt{SQL: "DECLARE @df sysname, @sql nvarchar(max); " +
		"SELECT @df = dc.name FROM sys.default_constraints dc " +
		"JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id " +
		"WHERE dc.parent_object_id = OBJECT_ID(" + sqlString(d.Quote(table)) + ") AND c.name = " + sqlString(column) + "; " +
		"IF @df IS NOT NULL BEGIN " +
		"SET @sql = N" + sqlString("ALTER TABLE "+d.Quote(table)+" DROP CONSTRAINT ") + " + QUOTENAME(@df); " +
		"EXEC sp_executesql @sql; END"}
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
