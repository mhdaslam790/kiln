// Package schema is a database-agnostic DSL for describing schema changes.
//
//	func(s *schema.Schema) {
//		s.CreateTable("users", func(t *schema.Table) {
//			t.ID("id")
//			t.String("email", schema.Required(), schema.Unique())
//			t.Int("age", schema.Default(0))
//			t.Timestamps()
//		})
//		s.CreateIndex("users", "idx_users_age", []string{"age"})
//	}
//
// A Schema only records operations; a dialect turns them into SQL.
package schema

import (
	"context"
	"database/sql"
)

// ColumnType is an abstract column type; each dialect maps it to a concrete type.
type ColumnType string

const (
	ID       ColumnType = "id"       // auto-increment big integer primary key
	Bool     ColumnType = "bool"     //
	SmallInt ColumnType = "smallint" //
	Int      ColumnType = "int"      //
	BigInt   ColumnType = "bigint"   //
	Float    ColumnType = "float"    // double precision
	Decimal  ColumnType = "decimal"  // uses Precision / Scale
	String   ColumnType = "string"   // varchar, uses Limit (default 255)
	Text     ColumnType = "text"     //
	Date     ColumnType = "date"     //
	DateTime ColumnType = "datetime" // timestamp with time zone where available
	Time     ColumnType = "time"     //
	Bytes    ColumnType = "bytes"    //
	JSON     ColumnType = "json"     //
	UUID     ColumnType = "uuid"     //
)

// Raw is an unescaped SQL literal, e.g. Default(schema.Raw("CURRENT_TIMESTAMP")).
type Raw string

// Column describes one column.
type Column struct {
	Name      string
	Type      ColumnType
	Required  bool // NOT NULL
	Unique    bool
	Primary   bool
	Unsigned  bool
	Default   any // nil means no default; use Raw for expressions
	HasDef    bool
	Limit     int
	Precision int
	Scale     int
	// SQLType overrides the dialect's type mapping entirely, e.g. "CITEXT".
	SQLType string
	// Options is appended verbatim after the column definition.
	Options string
	// Ref is a foreign key created together with the column (AddColumn only).
	Ref *ForeignKey
}

// ForeignKey describes a foreign key constraint.
type ForeignKey struct {
	Name       string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
	OnUpdate   string
}

// Op is a single schema operation.
type Op interface{ isOp() }

type CreateTable struct {
	Name        string
	Columns     []Column
	PrimaryKey  []string // composite primary key
	Uniques     [][]string
	ForeignKeys []ForeignKey
	IfNotExists bool
	Options     string // appended after the closing paren, e.g. "ENGINE=InnoDB"
}

type DropTable struct {
	Name     string
	IfExists bool
}

type RenameTable struct{ Name, NewName string }

type AddColumn struct {
	Table  string
	Column Column
}

type DropColumn struct{ Table, Name string }

type RenameColumn struct{ Table, Name, NewName string }

// ChangeColumn alters a column's type/nullability/default. Not supported on SQLite.
type ChangeColumn struct {
	Table  string
	Column Column
}

type AddForeignKey struct {
	Table string
	Key   ForeignKey
}

type DropForeignKey struct{ Table, Name string }

type CreateIndex struct {
	Table       string
	Name        string
	Columns     []string
	Unique      bool
	IfNotExists bool
	Where       string // partial index predicate (Postgres, SQLite, MSSQL)
}

type DropIndex struct {
	Table    string
	Name     string
	IfExists bool
}

// Exec runs raw SQL.
type Exec struct {
	SQL  string
	Args []any
}

// Executor is what a Do function receives: the migration's transaction.
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Do runs arbitrary Go code (data migrations) inside the migration.
type Do struct {
	Fn func(ctx context.Context, tx Executor) error
}

func (CreateTable) isOp()    {}
func (DropTable) isOp()      {}
func (RenameTable) isOp()    {}
func (AddColumn) isOp()      {}
func (DropColumn) isOp()     {}
func (RenameColumn) isOp()   {}
func (ChangeColumn) isOp()   {}
func (AddForeignKey) isOp()  {}
func (DropForeignKey) isOp() {}
func (CreateIndex) isOp()    {}
func (DropIndex) isOp()      {}
func (Exec) isOp()           {}
func (Do) isOp()             {}

// Schema accumulates operations in order.
type Schema struct {
	Ops []Op
}

func (s *Schema) add(op Op) { s.Ops = append(s.Ops, op) }

// CreateTable creates a table. Indexes declared with t.Index are emitted
// as separate CREATE INDEX statements after the table.
func (s *Schema) CreateTable(name string, fn func(t *Table), opts ...TableOption) {
	t := &Table{name: name}
	fn(t)
	ct := CreateTable{
		Name:        name,
		Columns:     t.columns,
		PrimaryKey:  t.primaryKey,
		Uniques:     t.uniques,
		ForeignKeys: t.foreignKeys,
	}
	for _, o := range opts {
		o(&ct)
	}
	s.add(ct)
	for _, idx := range t.indexes {
		s.add(idx)
	}
}

// CreateTableIfNotExists is CreateTable with IF NOT EXISTS semantics.
func (s *Schema) CreateTableIfNotExists(name string, fn func(t *Table), opts ...TableOption) {
	s.CreateTable(name, fn, append(opts, IfNotExists())...)
}

// AlterTable groups column changes on an existing table.
func (s *Schema) AlterTable(name string, fn func(t *AlterTable)) {
	fn(&AlterTable{name: name, s: s})
}

func (s *Schema) DropTable(name string)         { s.add(DropTable{Name: name}) }
func (s *Schema) DropTableIfExists(name string) { s.add(DropTable{Name: name, IfExists: true}) }
func (s *Schema) RenameTable(name, newName string) {
	s.add(RenameTable{Name: name, NewName: newName})
}

func (s *Schema) AddColumn(table, name string, typ ColumnType, opts ...ColumnOption) {
	s.add(AddColumn{Table: table, Column: newColumn(name, typ, opts)})
}
func (s *Schema) DropColumn(table, name string) { s.add(DropColumn{Table: table, Name: name}) }
func (s *Schema) RenameColumn(table, name, newName string) {
	s.add(RenameColumn{Table: table, Name: name, NewName: newName})
}
func (s *Schema) ChangeColumn(table, name string, typ ColumnType, opts ...ColumnOption) {
	s.add(ChangeColumn{Table: table, Column: newColumn(name, typ, opts)})
}

// CreateIndex creates an index. Name may be empty to derive idx_<table>_<cols>.
func (s *Schema) CreateIndex(table, name string, columns []string, opts ...IndexOption) {
	idx := CreateIndex{Table: table, Name: name, Columns: columns}
	for _, o := range opts {
		o(&idx)
	}
	if idx.Name == "" {
		idx.Name = IndexName(table, columns, idx.Unique)
	}
	s.add(idx)
}

// CreateUniqueIndex creates a unique index.
func (s *Schema) CreateUniqueIndex(table, name string, columns []string, opts ...IndexOption) {
	s.CreateIndex(table, name, columns, append(opts, UniqueIndex())...)
}

func (s *Schema) DropIndex(table, name string) { s.add(DropIndex{Table: table, Name: name}) }
func (s *Schema) DropIndexIfExists(table, name string) {
	s.add(DropIndex{Table: table, Name: name, IfExists: true})
}

func (s *Schema) AddForeignKey(table, column, refTable, refColumn string, opts ...FKOption) {
	s.add(AddForeignKey{Table: table, Key: newFK(table, []string{column}, refTable, []string{refColumn}, opts)})
}
func (s *Schema) DropForeignKey(table, name string) { s.add(DropForeignKey{Table: table, Name: name}) }

// Exec runs raw SQL ('?' placeholders are rewritten per dialect).
func (s *Schema) Exec(sql string, args ...any) { s.add(Exec{SQL: sql, Args: args}) }

// Do runs Go code inside the migration's transaction (data migrations).
func (s *Schema) Do(fn func(ctx context.Context, tx Executor) error) { s.add(Do{Fn: fn}) }

// IndexName derives a conventional index name.
func IndexName(table string, columns []string, unique bool) string {
	prefix := "idx_"
	if unique {
		prefix = "uniq_"
	}
	n := prefix + table
	for _, c := range columns {
		n += "_" + c
	}
	return n
}
