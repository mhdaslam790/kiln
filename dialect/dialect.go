// Package dialect compiles q queries and schema operations into SQL for
// PostgreSQL, MySQL, SQLite and Microsoft SQL Server.
//
// Dialects never import a driver: open the *sql.DB with whichever driver
// you prefer (pgx, lib/pq, go-sql-driver/mysql, modernc sqlite, mattn
// sqlite3, go-mssqldb) and pass the matching dialect.
package dialect

import (
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

// Kind identifies a SQL dialect.
type Kind string

const (
	KindPostgres Kind = "postgres"
	KindMySQL    Kind = "mysql"
	KindSQLite   Kind = "sqlite"
	KindMSSQL    Kind = "mssql"
)

// IDMode is how a dialect reports auto-generated primary keys.
type IDMode int

const (
	IDReturning    IDMode = iota // INSERT ... RETURNING / OUTPUT INSERTED
	IDLastInsertID               // sql.Result.LastInsertId
)

// Insert describes an INSERT statement.
type Insert struct {
	Table      string
	Columns    []string
	Rows       [][]any
	Returning  string // column to return (auto-increment primary key)
	OnConflict *q.OnConflict
}

// PositionColumn is the extra result column of a multi-row Insert with
// Returning on SQL Server. Such an insert is compiled to a MERGE that
// returns (position, value) pairs, because the row order of OUTPUT is not
// guaranteed; position is the 0-based index into Rows.
const PositionColumn = "kiln_pos"

// Stmt is a compiled statement.
type Stmt struct {
	SQL  string
	Args []any
}

// Dialect compiles queries and DDL.
type Dialect interface {
	Kind() Kind
	Quote(ident string) string
	// Rebind rewrites '?' placeholders into the dialect's placeholder style.
	Rebind(sql string) string
	// Raw compiles a raw statement, rewriting '?' placeholders and
	// checking the argument count.
	Raw(sql string, args []any) (Stmt, error)
	IDMode() IDMode

	Select(qq q.Query) (Stmt, error)
	Count(qq q.Query) (Stmt, error)
	Insert(ins Insert) (Stmt, error)
	Update(table string, set []q.Mutation, where q.Filter) (Stmt, error)
	Delete(table string, where q.Filter) (Stmt, error)

	// ColumnType returns the concrete SQL type for a column, e.g. "VARCHAR(255)".
	ColumnType(c schema.Column) (string, error)

	// DDL compiles a schema operation. Do operations are not compiled.
	DDL(op schema.Op) ([]Stmt, error)

	Savepoint(name string) string
	RollbackTo(name string) string
	Release(name string) string // empty if not needed

	// Classify inspects a driver error for known constraint violations.
	Classify(err error) ErrorKind
}

// Postgres returns the PostgreSQL dialect ($1 placeholders, RETURNING).
func Postgres() Dialect { return &sqlDialect{kind: KindPostgres} }

// MySQL returns the MySQL/MariaDB dialect. Use parseTime=true in the DSN.
func MySQL() Dialect { return &sqlDialect{kind: KindMySQL} }

// SQLite returns the SQLite dialect (requires SQLite 3.35+ for RETURNING/DROP COLUMN).
func SQLite() Dialect { return &sqlDialect{kind: KindSQLite} }

// MSSQL returns the SQL Server dialect (@p1 placeholders, OUTPUT INSERTED).
func MSSQL() Dialect { return &sqlDialect{kind: KindMSSQL} }

// ByName returns a dialect by name: postgres|postgresql|pg, mysql|mariadb,
// sqlite|sqlite3, mssql|sqlserver. It returns nil for unknown names.
func ByName(name string) Dialect {
	switch name {
	case "postgres", "postgresql", "pg", "pgx":
		return Postgres()
	case "mysql", "mariadb":
		return MySQL()
	case "sqlite", "sqlite3":
		return SQLite()
	case "mssql", "sqlserver":
		return MSSQL()
	}
	return nil
}
