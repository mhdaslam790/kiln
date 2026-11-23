package schema

// ColumnOption configures a Column.
type ColumnOption func(*Column)

// Required makes the column NOT NULL.
func Required() ColumnOption { return func(c *Column) { c.Required = true } }

// Unique adds a single-column UNIQUE constraint.
func Unique() ColumnOption { return func(c *Column) { c.Unique = true } }

// Primary marks the column as the primary key.
func Primary() ColumnOption { return func(c *Column) { c.Primary = true } }

// Unsigned marks an integer column unsigned (MySQL only).
func Unsigned() ColumnOption { return func(c *Column) { c.Unsigned = true } }

// Default sets the column's default; pass schema.Raw for SQL expressions.
func Default(v any) ColumnOption { return func(c *Column) { c.Default = v; c.HasDef = true } }

// Limit sets the length of String columns.
func Limit(n int) ColumnOption { return func(c *Column) { c.Limit = n } }

// Precision sets numeric precision (Decimal) or fractional seconds (DateTime).
func Precision(n int) ColumnOption { return func(c *Column) { c.Precision = n } }

// Scale sets numeric scale (Decimal).
func Scale(n int) ColumnOption { return func(c *Column) { c.Scale = n } }

// SQLType overrides the column type with a literal SQL type.
func SQLType(t string) ColumnOption { return func(c *Column) { c.SQLType = t } }

// ColumnOptions appends raw SQL after the column definition.
func ColumnOptions(sql string) ColumnOption { return func(c *Column) { c.Options = sql } }

func newColumn(name string, typ ColumnType, opts []ColumnOption) Column {
	c := Column{Name: name, Type: typ}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// TableOption configures a CreateTable.
type TableOption func(*CreateTable)

// IfNotExists adds IF NOT EXISTS to CREATE TABLE.
func IfNotExists() TableOption { return func(t *CreateTable) { t.IfNotExists = true } }

// TableOptions appends raw SQL after the table definition.
func TableOptions(sql string) TableOption { return func(t *CreateTable) { t.Options = sql } }

// FKOption configures a foreign key.
type FKOption func(*ForeignKey)

// OnDelete sets the ON DELETE action, e.g. "CASCADE", "SET NULL".
func OnDelete(action string) FKOption { return func(f *ForeignKey) { f.OnDelete = action } }

// OnUpdate sets the ON UPDATE action.
func OnUpdate(action string) FKOption { return func(f *ForeignKey) { f.OnUpdate = action } }

// FKName overrides the constraint name.
func FKName(name string) FKOption { return func(f *ForeignKey) { f.Name = name } }

func newFK(table string, cols []string, refTable string, refCols []string, opts []FKOption) ForeignKey {
	fk := ForeignKey{Columns: cols, RefTable: refTable, RefColumns: refCols}
	for _, o := range opts {
		o(&fk)
	}
	if fk.Name == "" {
		fk.Name = "fk_" + table + "_" + cols[0]
	}
	return fk
}

// IndexOption configures an index.
type IndexOption func(*CreateIndex)

// UniqueIndex makes the index unique.
func UniqueIndex() IndexOption { return func(i *CreateIndex) { i.Unique = true } }

// IndexName sets the index name.
func Name(name string) IndexOption { return func(i *CreateIndex) { i.Name = name } }

// IndexIfNotExists skips creation when the index exists.
func IndexIfNotExists() IndexOption { return func(i *CreateIndex) { i.IfNotExists = true } }

// PartialWhere makes a partial index: WHERE <predicate>.
func PartialWhere(predicate string) IndexOption { return func(i *CreateIndex) { i.Where = predicate } }

// RefOption configures References; both ColumnOption and FKOption satisfy it.
type RefOption interface {
	applyRef(c *Column, fk *ForeignKey)
}

func (o ColumnOption) applyRef(c *Column, _ *ForeignKey) { o(c) }
func (o FKOption) applyRef(_ *Column, fk *ForeignKey)    { o(fk) }

func newRef(table, column, refTable string, opts []RefOption) (Column, ForeignKey) {
	col := Column{Name: column, Type: BigInt}
	fk := ForeignKey{Columns: []string{column}, RefTable: refTable, RefColumns: []string{"id"}}
	for _, o := range opts {
		o.applyRef(&col, &fk)
	}
	if fk.Name == "" {
		fk.Name = "fk_" + table + "_" + column
	}
	return col, fk
}

// RefColumn sets the referenced column (default "id").
func RefColumn(col string) FKOption { return func(f *ForeignKey) { f.RefColumns = []string{col} } }
