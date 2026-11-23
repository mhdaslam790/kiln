package schema

// Table builds a CREATE TABLE.
type Table struct {
	name        string
	columns     []Column
	primaryKey  []string
	uniques     [][]string
	foreignKeys []ForeignKey
	indexes     []CreateIndex
}

// Column adds a column of any type.
func (t *Table) Column(name string, typ ColumnType, opts ...ColumnOption) {
	t.columns = append(t.columns, newColumn(name, typ, opts))
}

// ID adds an auto-increment big integer primary key.
func (t *Table) ID(name string, opts ...ColumnOption) {
	t.Column(name, ID, append([]ColumnOption{Primary(), Required()}, opts...)...)
}

func (t *Table) Bool(name string, opts ...ColumnOption)     { t.Column(name, Bool, opts...) }
func (t *Table) SmallInt(name string, opts ...ColumnOption) { t.Column(name, SmallInt, opts...) }
func (t *Table) Int(name string, opts ...ColumnOption)      { t.Column(name, Int, opts...) }
func (t *Table) BigInt(name string, opts ...ColumnOption)   { t.Column(name, BigInt, opts...) }
func (t *Table) Float(name string, opts ...ColumnOption)    { t.Column(name, Float, opts...) }
func (t *Table) Decimal(name string, opts ...ColumnOption)  { t.Column(name, Decimal, opts...) }
func (t *Table) String(name string, opts ...ColumnOption)   { t.Column(name, String, opts...) }
func (t *Table) Text(name string, opts ...ColumnOption)     { t.Column(name, Text, opts...) }
func (t *Table) Date(name string, opts ...ColumnOption)     { t.Column(name, Date, opts...) }
func (t *Table) DateTime(name string, opts ...ColumnOption) { t.Column(name, DateTime, opts...) }
func (t *Table) Time(name string, opts ...ColumnOption)     { t.Column(name, Time, opts...) }
func (t *Table) Bytes(name string, opts ...ColumnOption)    { t.Column(name, Bytes, opts...) }
func (t *Table) JSON(name string, opts ...ColumnOption)     { t.Column(name, JSON, opts...) }
func (t *Table) UUID(name string, opts ...ColumnOption)     { t.Column(name, UUID, opts...) }

// Timestamps adds required created_at and updated_at columns.
func (t *Table) Timestamps() {
	t.DateTime("created_at", Required())
	t.DateTime("updated_at", Required())
}

// SoftDelete adds a nullable deleted_at column (pairs with a DeletedAt
// *time.Time field on the model).
func (t *Table) SoftDelete() { t.DateTime("deleted_at") }

// References adds a BIGINT column, a foreign key to refTable(id), and an
// index. It accepts both column options and foreign key options:
//
//	t.References("user_id", "users", schema.Required(), schema.OnDelete("CASCADE"))
func (t *Table) References(column, refTable string, opts ...RefOption) {
	col, fk := newRef(t.name, column, refTable, opts)
	t.columns = append(t.columns, col)
	t.foreignKeys = append(t.foreignKeys, fk)
	t.Index([]string{column})
}

// PrimaryKey declares a (composite) primary key.
func (t *Table) PrimaryKey(columns ...string) { t.primaryKey = columns }

// Unique adds a (composite) unique constraint.
func (t *Table) Unique(columns ...string) { t.uniques = append(t.uniques, columns) }

// ForeignKey adds a foreign key constraint.
func (t *Table) ForeignKey(column, refTable, refColumn string, opts ...FKOption) {
	t.foreignKeys = append(t.foreignKeys, newFK(t.name, []string{column}, refTable, []string{refColumn}, opts))
}

// Index adds an index created right after the table.
func (t *Table) Index(columns []string, opts ...IndexOption) {
	idx := CreateIndex{Table: t.name, Columns: columns}
	for _, o := range opts {
		o(&idx)
	}
	if idx.Name == "" {
		idx.Name = IndexName(t.name, columns, idx.Unique)
	}
	t.indexes = append(t.indexes, idx)
}

// AlterTable records column changes on an existing table.
type AlterTable struct {
	name string
	s    *Schema
}

func (a *AlterTable) Column(name string, typ ColumnType, opts ...ColumnOption) {
	a.s.AddColumn(a.name, name, typ, opts...)
}
func (a *AlterTable) Bool(name string, opts ...ColumnOption)     { a.Column(name, Bool, opts...) }
func (a *AlterTable) SmallInt(name string, opts ...ColumnOption) { a.Column(name, SmallInt, opts...) }
func (a *AlterTable) Time(name string, opts ...ColumnOption)     { a.Column(name, Time, opts...) }
func (a *AlterTable) Int(name string, opts ...ColumnOption)      { a.Column(name, Int, opts...) }
func (a *AlterTable) BigInt(name string, opts ...ColumnOption)   { a.Column(name, BigInt, opts...) }
func (a *AlterTable) Float(name string, opts ...ColumnOption)    { a.Column(name, Float, opts...) }
func (a *AlterTable) Decimal(name string, opts ...ColumnOption)  { a.Column(name, Decimal, opts...) }
func (a *AlterTable) String(name string, opts ...ColumnOption)   { a.Column(name, String, opts...) }
func (a *AlterTable) Text(name string, opts ...ColumnOption)     { a.Column(name, Text, opts...) }
func (a *AlterTable) Date(name string, opts ...ColumnOption)     { a.Column(name, Date, opts...) }
func (a *AlterTable) DateTime(name string, opts ...ColumnOption) { a.Column(name, DateTime, opts...) }
func (a *AlterTable) Bytes(name string, opts ...ColumnOption)    { a.Column(name, Bytes, opts...) }
func (a *AlterTable) JSON(name string, opts ...ColumnOption)     { a.Column(name, JSON, opts...) }
func (a *AlterTable) UUID(name string, opts ...ColumnOption)     { a.Column(name, UUID, opts...) }
func (a *AlterTable) DropColumn(name string)                     { a.s.DropColumn(a.name, name) }
func (a *AlterTable) RenameColumn(name, newName string)          { a.s.RenameColumn(a.name, name, newName) }
func (a *AlterTable) ChangeColumn(name string, typ ColumnType, opts ...ColumnOption) {
	a.s.ChangeColumn(a.name, name, typ, opts...)
}

// References adds a BIGINT column with a foreign key to refTable(id) and an
// index. On SQLite the key is declared inline in ADD COLUMN.
func (a *AlterTable) References(column, refTable string, opts ...RefOption) {
	col, fk := newRef(a.name, column, refTable, opts)
	col.Ref = &fk
	a.s.add(AddColumn{Table: a.name, Column: col})
	a.s.CreateIndex(a.name, "", []string{column})
}

func (a *AlterTable) Index(columns []string, opts ...IndexOption) {
	a.s.CreateIndex(a.name, "", columns, opts...)
}
func (a *AlterTable) ForeignKey(column, refTable, refColumn string, opts ...FKOption) {
	a.s.AddForeignKey(a.name, column, refTable, refColumn, opts...)
}
