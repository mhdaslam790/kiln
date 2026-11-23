package migrate

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mhdaslam790/kiln/schema"
)

// RenderGo renders schema operations as Go statements using the schema
// DSL with receiver s, ready to paste into a migration function.
func RenderGo(ops []schema.Op) []string {
	var out []string
	for _, op := range ops {
		out = append(out, renderOp(op)...)
	}
	return out
}

var typeConst = map[schema.ColumnType]string{
	schema.ID: "schema.ID", schema.Bool: "schema.Bool", schema.SmallInt: "schema.SmallInt", schema.Int: "schema.Int",
	schema.BigInt: "schema.BigInt", schema.Float: "schema.Float", schema.Decimal: "schema.Decimal",
	schema.String: "schema.String", schema.Text: "schema.Text", schema.Date: "schema.Date",
	schema.DateTime: "schema.DateTime", schema.Time: "schema.Time", schema.Bytes: "schema.Bytes",
	schema.JSON: "schema.JSON", schema.UUID: "schema.UUID",
}

var tableMethod = map[schema.ColumnType]string{
	schema.Bool: "Bool", schema.SmallInt: "SmallInt", schema.Int: "Int", schema.BigInt: "BigInt",
	schema.Float: "Float", schema.Decimal: "Decimal", schema.String: "String", schema.Text: "Text",
	schema.Date: "Date", schema.DateTime: "DateTime", schema.Time: "Time", schema.Bytes: "Bytes",
	schema.JSON: "JSON", schema.UUID: "UUID",
}

func q(s string) string { return strconv.Quote(s) }

func strList(ss []string) string {
	qs := make([]string, len(ss))
	for i, s := range ss {
		qs[i] = q(s)
	}
	return "[]string{" + strings.Join(qs, ", ") + "}"
}

func literal(v any) string {
	switch x := v.(type) {
	case nil:
		return "nil"
	case schema.Raw:
		return "schema.Raw(" + q(string(x)) + ")"
	case string:
		return q(x)
	case time.Time:
		return q(x.UTC().Format("2006-01-02 15:04:05"))
	case []byte:
		return q(string(x))
	}
	return fmt.Sprint(v)
}

func colOpts(c schema.Column, withPrimary bool) []string {
	var o []string
	if c.Required && !(c.Type == schema.ID) {
		o = append(o, "schema.Required()")
	}
	if withPrimary && c.Primary && c.Type != schema.ID {
		o = append(o, "schema.Primary()")
	}
	if c.Unique {
		o = append(o, "schema.Unique()")
	}
	if c.Unsigned {
		o = append(o, "schema.Unsigned()")
	}
	if c.Limit > 0 {
		o = append(o, fmt.Sprintf("schema.Limit(%d)", c.Limit))
	}
	if c.Precision > 0 {
		o = append(o, fmt.Sprintf("schema.Precision(%d)", c.Precision))
	}
	if c.Scale > 0 {
		o = append(o, fmt.Sprintf("schema.Scale(%d)", c.Scale))
	}
	if c.HasDef {
		o = append(o, "schema.Default("+literal(c.Default)+")")
	}
	if c.SQLType != "" {
		o = append(o, "schema.SQLType("+q(c.SQLType)+")")
	}
	if c.Options != "" {
		o = append(o, "schema.ColumnOptions("+q(c.Options)+")")
	}
	return o
}

func fkOpts(fk schema.ForeignKey, defaultName string) []string {
	var o []string
	if fk.OnDelete != "" {
		o = append(o, "schema.OnDelete("+q(fk.OnDelete)+")")
	}
	if fk.OnUpdate != "" {
		o = append(o, "schema.OnUpdate("+q(fk.OnUpdate)+")")
	}
	if fk.Name != "" && fk.Name != defaultName {
		o = append(o, "schema.FKName("+q(fk.Name)+")")
	}
	return o
}

func args(parts ...[]string) string {
	var all []string
	for _, p := range parts {
		all = append(all, p...)
	}
	return strings.Join(all, ", ")
}

func renderOp(op schema.Op) []string {
	switch o := op.(type) {
	case schema.CreateTable:
		lines := []string{"s.CreateTable(" + q(o.Name) + ", func(t *schema.Table) {"}
		for _, c := range o.Columns {
			if c.Type == schema.ID && c.Primary {
				lines = append(lines, "\tt.ID("+q(c.Name)+")")
				continue
			}
			if m := tableMethod[c.Type]; m != "" {
				lines = append(lines, "\tt."+m+"("+args([]string{q(c.Name)}, colOpts(c, true))+")")
			} else {
				lines = append(lines, "\tt.Column("+args([]string{q(c.Name), typeConst[c.Type]}, colOpts(c, true))+")")
			}
		}
		if len(o.PrimaryKey) > 1 {
			qs := make([]string, len(o.PrimaryKey))
			for i, c := range o.PrimaryKey {
				qs[i] = q(c)
			}
			lines = append(lines, "\tt.PrimaryKey("+strings.Join(qs, ", ")+")")
		}
		for _, u := range o.Uniques {
			qs := make([]string, len(u))
			for i, c := range u {
				qs[i] = q(c)
			}
			lines = append(lines, "\tt.Unique("+strings.Join(qs, ", ")+")")
		}
		for _, fk := range o.ForeignKeys {
			lines = append(lines, "\tt.ForeignKey("+args([]string{q(fk.Columns[0]), q(fk.RefTable), q(fk.RefColumns[0])},
				fkOpts(fk, "fk_"+o.Name+"_"+fk.Columns[0]))+")")
		}
		return append(lines, "})")
	case schema.DropTable:
		if o.IfExists {
			return []string{"s.DropTableIfExists(" + q(o.Name) + ")"}
		}
		return []string{"s.DropTable(" + q(o.Name) + ")"}
	case schema.RenameTable:
		return []string{"s.RenameTable(" + q(o.Name) + ", " + q(o.NewName) + ")"}
	case schema.AddColumn:
		c := o.Column
		if c.Ref != nil && c.Type == schema.BigInt && len(c.Ref.RefColumns) == 1 {
			opts := colOpts(c, false)
			if c.Ref.RefColumns[0] != "id" {
				opts = append(opts, "schema.RefColumn("+q(c.Ref.RefColumns[0])+")")
			}
			opts = append(opts, fkOpts(*c.Ref, "fk_"+o.Table+"_"+c.Name)...)
			return []string{"s.AlterTable(" + q(o.Table) + ", func(a *schema.AlterTable) { a.References(" +
				args([]string{q(c.Name), q(c.Ref.RefTable)}, opts) + ") })"}
		}
		lines := []string{"s.AddColumn(" + args([]string{q(o.Table), q(c.Name), typeConst[c.Type]}, colOpts(c, false)) + ")"}
		if c.Ref != nil {
			lines = append(lines, renderOp(schema.AddForeignKey{Table: o.Table, Key: *c.Ref})...)
		}
		return lines
	case schema.DropColumn:
		return []string{"s.DropColumn(" + q(o.Table) + ", " + q(o.Name) + ")"}
	case schema.RenameColumn:
		return []string{"s.RenameColumn(" + q(o.Table) + ", " + q(o.Name) + ", " + q(o.NewName) + ")"}
	case schema.ChangeColumn:
		return []string{"s.ChangeColumn(" + args([]string{q(o.Table), q(o.Column.Name), typeConst[o.Column.Type]}, colOpts(o.Column, false)) + ")"}
	case schema.AddForeignKey:
		return []string{"s.AddForeignKey(" + args([]string{q(o.Table), q(o.Key.Columns[0]), q(o.Key.RefTable), q(o.Key.RefColumns[0])},
			fkOpts(o.Key, "fk_"+o.Table+"_"+o.Key.Columns[0])) + ")"}
	case schema.DropForeignKey:
		return []string{"s.DropForeignKey(" + q(o.Table) + ", " + q(o.Name) + ")"}
	case schema.CreateIndex:
		var opts []string
		if o.Unique {
			opts = append(opts, "schema.UniqueIndex()")
		}
		if o.Where != "" {
			opts = append(opts, "schema.PartialWhere("+q(o.Where)+")")
		}
		return []string{"s.CreateIndex(" + args([]string{q(o.Table), q(o.Name), strList(o.Columns)}, opts) + ")"}
	case schema.DropIndex:
		return []string{"s.DropIndex(" + q(o.Table) + ", " + q(o.Name) + ")"}
	case schema.Exec:
		return []string{"s.Exec(" + q(o.SQL) + ")"}
	}
	return []string{fmt.Sprintf("// unsupported operation %T", op)}
}
