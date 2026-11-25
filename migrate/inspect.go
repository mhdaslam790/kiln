package migrate

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
)

// Inspect reads the live database schema (tables, columns, primary keys,
// indexes and single-column foreign keys) of the current schema/database.
func Inspect(ctx context.Context, db *orm.DB) (*schema.State, error) {
	var (
		cols []colRow
		idx  []idxRow
		fks  []fkRow
		err  error
	)
	switch db.Dialect().Kind() {
	case dialect.KindSQLite:
		cols, idx, fks, err = inspectSQLite(ctx, db)
	case dialect.KindPostgres:
		cols, idx, fks, err = inspectQueries(ctx, db, pgColumns, pgIndexes, pgFKs)
	case dialect.KindMySQL:
		cols, idx, fks, err = inspectQueries(ctx, db, myColumns, myIndexes, myFKs)
	case dialect.KindMSSQL:
		cols, idx, fks, err = inspectQueries(ctx, db, msColumns, msIndexes, msFKs)
	default:
		return nil, fmt.Errorf("migrate: inspect is not supported for %s", db.Dialect().Kind())
	}
	if err != nil {
		return nil, fmt.Errorf("migrate: inspect: %w", err)
	}
	st := buildState(cols, idx, fks)
	if db.Dialect().Kind() == dialect.KindPostgres {
		if err := pgEnums(ctx, db, st); err != nil {
			return nil, fmt.Errorf("migrate: inspect: %w", err)
		}
	}
	return st, nil
}

// pgArrayElem spells PostgreSQL's internal element type names (int8 in
// _int8) the way migrations write them.
var pgArrayElem = map[string]string{
	"int2": "SMALLINT", "int4": "INTEGER", "int8": "BIGINT", "float4": "REAL",
	"float8": "DOUBLE PRECISION", "bool": "BOOLEAN", "bpchar": "CHAR",
}

// pgTypeName quotes a type name that PostgreSQL would otherwise misread
// (spaces, capitals, ...), as DDL needs it: order status -> "order status".
func pgTypeName(name string) string {
	if plainName.MatchString(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

var plainName = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// pgEnums loads the schema's enum types and names enum and array columns
// the way migrations write them (mood, mood[], BIGINT[]).
func pgEnums(ctx context.Context, db *orm.DB, st *schema.State) error {
	var rows []struct {
		Name  string `db:"name"`
		Label string `db:"label"`
	}
	if err := db.Query(ctx, &rows, `
SELECT t.typname AS name, e.enumlabel AS label
FROM pg_type t
JOIN pg_enum e ON e.enumtypid = t.oid
JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = current_schema()
ORDER BY t.typname, e.enumsortorder`); err != nil {
		return err
	}
	byFold := map[string]string{}
	for _, r := range rows {
		if st.Enums == nil {
			st.Enums = map[string][]string{}
		}
		st.Enums[r.Name] = append(st.Enums[r.Name], r.Label)
		byFold[strings.ToLower(r.Name)] = r.Name
	}
	for _, t := range st.Tables {
		for i := range t.Columns {
			c := &t.Columns[i]
			base, arr := strings.CutSuffix(c.SQLType, "[]")
			if c.SQLType == "" {
				continue
			}
			if name, ok := byFold[strings.ToLower(base)]; ok {
				base = pgTypeName(name)
			} else if arr {
				if n, ok := pgArrayElem[strings.ToLower(base)]; ok {
					base = n
				}
			}
			if arr {
				base += "[]"
			}
			c.SQLType = base
		}
	}
	return nil
}

type colRow struct {
	Table     string  `db:"tbl"`
	Name      string  `db:"col"`
	Type      string  `db:"typ"`
	Nullable  bool    `db:"nullable"`
	Default   *string `db:"def"`
	Length    *int64  `db:"len"`
	Precision *int64  `db:"prec"`
	Scale     *int64  `db:"scale"`
	Primary   bool    `db:"pk"`
	Auto      bool    `db:"auto"`
	Position  int64   `db:"pos"`
}

type idxRow struct {
	Table  string `db:"tbl"`
	Name   string `db:"name"`
	Unique bool   `db:"uniq"`
	Column string `db:"col"`
	Seq    int64  `db:"seq"`
}

type fkRow struct {
	Table     string  `db:"tbl"`
	Name      string  `db:"name"`
	Column    string  `db:"col"`
	RefTable  string  `db:"ref_tbl"`
	RefColumn string  `db:"ref_col"`
	OnDelete  *string `db:"on_delete"`
	OnUpdate  *string `db:"on_update"`
}

func inspectQueries(ctx context.Context, db *orm.DB, colSQL, idxSQL, fkSQL string) ([]colRow, []idxRow, []fkRow, error) {
	var cols []colRow
	var idx []idxRow
	var fks []fkRow
	if err := db.Query(ctx, &cols, colSQL); err != nil {
		return nil, nil, nil, err
	}
	if err := db.Query(ctx, &idx, idxSQL); err != nil {
		return nil, nil, nil, err
	}
	if err := db.Query(ctx, &fks, fkSQL); err != nil {
		return nil, nil, nil, err
	}
	return cols, idx, fks, nil
}

func buildState(cols []colRow, idx []idxRow, fks []fkRow) *schema.State {
	st := schema.NewState()
	sort.SliceStable(cols, func(i, j int) bool {
		if cols[i].Table != cols[j].Table {
			return cols[i].Table < cols[j].Table
		}
		return cols[i].Position < cols[j].Position
	})
	for _, c := range cols {
		if c.Table == "schema_migrations" || c.Table == sqliteLockTable {
			continue
		}
		t := st.Tables[c.Table]
		if t == nil {
			t = &schema.TableState{Name: c.Table}
			st.Tables[c.Table] = t
		}
		col := sqlColumn(c)
		if c.Primary {
			t.PrimaryKey = append(t.PrimaryKey, c.Name)
		}
		t.Columns = append(t.Columns, col)
	}
	for _, t := range st.Tables {
		if len(t.PrimaryKey) > 1 {
			for i := range t.Columns {
				if t.Columns[i].Type == schema.ID {
					t.Columns[i].Type = schema.BigInt
				}
			}
		} else {
			t.PrimaryKey = nil
		}
	}

	sort.SliceStable(idx, func(i, j int) bool {
		if idx[i].Table != idx[j].Table || idx[i].Name != idx[j].Name {
			return idx[i].Table+"\x00"+idx[i].Name < idx[j].Table+"\x00"+idx[j].Name
		}
		return idx[i].Seq < idx[j].Seq
	})
	for _, r := range idx {
		t := st.Tables[r.Table]
		if t == nil {
			continue
		}
		n := len(t.Indexes)
		if n > 0 && t.Indexes[n-1].Name == r.Name {
			t.Indexes[n-1].Columns = append(t.Indexes[n-1].Columns, r.Column)
			continue
		}
		t.Indexes = append(t.Indexes, schema.CreateIndex{Table: r.Table, Name: r.Name, Unique: r.Unique, Columns: []string{r.Column}})
	}
	// Single-column unique indexes are reported on the column too.
	for _, t := range st.Tables {
		for _, ix := range t.Indexes {
			if ix.Unique && len(ix.Columns) == 1 {
				if c := t.Column(ix.Columns[0]); c != nil {
					c.Unique = true
				}
			}
		}
	}

	for _, r := range fks {
		t := st.Tables[r.Table]
		if t == nil {
			continue
		}
		name := r.Name
		if name == "" {
			name = "fk_" + r.Table + "_" + r.Column
		}
		t.ForeignKeys = append(t.ForeignKeys, schema.ForeignKey{
			Name: name, Columns: []string{r.Column}, RefTable: r.RefTable, RefColumns: []string{r.RefColumn},
			OnDelete: fkAction(r.OnDelete), OnUpdate: fkAction(r.OnUpdate),
		})
	}
	return st
}

func fkAction(p *string) string {
	if p == nil {
		return ""
	}
	switch a := strings.ToUpper(strings.ReplaceAll(*p, "_", " ")); a {
	case "", "NO ACTION", "A":
		return ""
	case "C":
		return "CASCADE"
	case "N":
		return "SET NULL"
	case "D":
		return "SET DEFAULT"
	case "R":
		return "RESTRICT"
	default:
		return a
	}
}

var typeArgs = regexp.MustCompile(`^([a-z0-9 _]+?)\s*(?:\(([^)]*)\))?(\s+unsigned)?$`)

// sqlColumn maps a database column back to the abstract schema types.
func sqlColumn(c colRow) schema.Column {
	col := schema.Column{Name: c.Name, Required: !c.Nullable || c.Primary, Primary: c.Primary}
	raw := strings.ToLower(strings.TrimSpace(c.Type))
	base, argList := raw, ""
	if m := typeArgs.FindStringSubmatch(raw); m != nil {
		base, argList = strings.TrimSpace(m[1]), m[2]
		col.Unsigned = m[3] != ""
	}
	var args []int
	for _, a := range strings.Split(argList, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(a)); err == nil {
			args = append(args, n)
		}
	}
	argOr := func(i int, p *int64) int {
		if i < len(args) {
			return args[i]
		}
		if p != nil {
			return int(*p)
		}
		return 0
	}

	switch base {
	case "bool", "boolean", "bit":
		col.Type = schema.Bool
	case "tinyint":
		col.Type = schema.SmallInt
		if argOr(0, nil) == 1 {
			col.Type = schema.Bool
		}
	case "smallint", "int2", "smallserial":
		col.Type = schema.SmallInt
	case "int", "integer", "int4", "mediumint", "serial":
		col.Type = schema.Int
	case "bigint", "int8", "bigserial":
		col.Type = schema.BigInt
	case "real", "float", "double", "double precision", "float4", "float8":
		col.Type = schema.Float
	case "decimal", "numeric", "money", "smallmoney":
		col.Type = schema.Decimal
		col.Precision, col.Scale = argOr(0, c.Precision), argOr(1, c.Scale)
	case "varchar", "character varying", "nvarchar", "char", "nchar", "character", "varchar2":
		col.Type = schema.String
		n := argOr(0, c.Length)
		if argList == "max" || n < 0 {
			col.Type = schema.Text
		} else if n != 255 {
			col.Limit = n
		}
	case "text", "tinytext", "mediumtext", "longtext", "ntext", "clob", "citext":
		col.Type = schema.Text
	case "date":
		col.Type = schema.Date
	case "timestamp", "timestamptz", "timestamp with time zone", "timestamp without time zone",
		"datetime", "datetime2", "datetimeoffset", "smalldatetime":
		col.Type = schema.DateTime
	case "time", "time without time zone", "time with time zone", "timetz":
		col.Type = schema.Time
	case "bytea", "blob", "tinyblob", "mediumblob", "longblob", "binary", "varbinary", "image":
		col.Type = schema.Bytes
	case "json", "jsonb":
		col.Type = schema.JSON
	case "uuid", "uniqueidentifier":
		col.Type = schema.UUID
	default:
		col.Type, col.SQLType = schema.Text, strings.ToUpper(c.Type)
	}
	if c.Default != nil {
		d := strings.TrimSpace(*c.Default)
		if strings.HasPrefix(strings.ToLower(d), "nextval(") {
			c.Auto = true
		} else if d != "" && !strings.EqualFold(d, "null") {
			col.Default, col.HasDef = parseSQLDefault(d, col.Type), true
		}
	}
	if c.Auto && c.Primary && (col.Type == schema.Int || col.Type == schema.BigInt) {
		col.Type = schema.ID
	}
	return col
}

var castSuffix = regexp.MustCompile(`::[a-z ]+(\[\])?$`)

func parseSQLDefault(d string, t schema.ColumnType) any {
	for strings.HasPrefix(d, "(") && strings.HasSuffix(d, ")") {
		d = d[1 : len(d)-1]
	}
	d = castSuffix.ReplaceAllString(d, "")
	if len(d) >= 2 && (d[0] == '\'' || (d[0] == 'N' && d[1] == '\'')) {
		s := strings.TrimPrefix(d, "N")
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	switch t {
	case schema.Bool:
		switch strings.ToLower(d) {
		case "true", "1", "b'1'":
			return true
		case "false", "0", "b'0'":
			return false
		}
	case schema.SmallInt, schema.Int, schema.BigInt:
		if n, err := strconv.ParseInt(d, 10, 64); err == nil {
			return n
		}
	case schema.Float, schema.Decimal:
		if n, err := strconv.ParseFloat(d, 64); err == nil {
			return n
		}
	case schema.String, schema.Text:
		if !strings.ContainsAny(d, "()") {
			return d // MySQL reports string defaults unquoted
		}
	}
	return schema.Raw(d)
}

// ---- SQLite ---------------------------------------------------------------------

func inspectSQLite(ctx context.Context, db *orm.DB) ([]colRow, []idxRow, []fkRow, error) {
	var tables []struct {
		Name string `db:"name"`
		SQL  string `db:"sql"`
	}
	if err := db.Query(ctx, &tables, "SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'"); err != nil {
		return nil, nil, nil, err
	}
	var cols []colRow
	var idx []idxRow
	var fks []fkRow
	for _, t := range tables {
		qt := db.Dialect().Quote(t.Name)
		var info []struct {
			CID     int64   `db:"cid"`
			Name    string  `db:"name"`
			Type    string  `db:"type"`
			NotNull bool    `db:"notnull"`
			Default *string `db:"dflt_value"`
			PK      int64   `db:"pk"`
		}
		if err := db.Query(ctx, &info, "PRAGMA table_info("+qt+")"); err != nil {
			return nil, nil, nil, err
		}
		pks := 0
		for _, c := range info {
			if c.PK > 0 {
				pks++
			}
		}
		for _, c := range info {
			auto := c.PK > 0 && pks == 1 && strings.EqualFold(c.Type, "integer")
			cols = append(cols, colRow{Table: t.Name, Name: c.Name, Type: c.Type, Nullable: !c.NotNull,
				Default: c.Default, Primary: c.PK > 0, Auto: auto, Position: c.CID})
		}

		var list []struct {
			Name   string `db:"name"`
			Unique bool   `db:"unique"`
			Origin string `db:"origin"`
		}
		if err := db.Query(ctx, &list, "PRAGMA index_list("+qt+")"); err != nil {
			return nil, nil, nil, err
		}
		for _, ix := range list {
			if ix.Origin == "pk" {
				continue
			}
			var info []struct {
				Seq  int64  `db:"seqno"`
				Name string `db:"name"`
			}
			if err := db.Query(ctx, &info, "PRAGMA index_info("+db.Dialect().Quote(ix.Name)+")"); err != nil {
				return nil, nil, nil, err
			}
			name := ix.Name
			if strings.HasPrefix(name, "sqlite_autoindex_") {
				cs := make([]string, len(info))
				for i, c := range info {
					cs[i] = c.Name
				}
				name = schema.IndexName(t.Name, cs, true)
			}
			for _, c := range info {
				idx = append(idx, idxRow{Table: t.Name, Name: name, Unique: ix.Unique, Column: c.Name, Seq: c.Seq})
			}
		}

		var fkl []struct {
			ID       int64  `db:"id"`
			Seq      int64  `db:"seq"`
			Table    string `db:"table"`
			From     string `db:"from"`
			To       string `db:"to"`
			OnUpdate string `db:"on_update"`
			OnDelete string `db:"on_delete"`
		}
		if err := db.Query(ctx, &fkl, "PRAGMA foreign_key_list("+qt+")"); err != nil {
			return nil, nil, nil, err
		}
		for _, f := range fkl {
			if f.Seq > 0 {
				continue // composite keys: first column only
			}
			od, ou := f.OnDelete, f.OnUpdate
			fks = append(fks, fkRow{Table: t.Name, Name: sqliteFKName(t.SQL, f.From, t.Name), Column: f.From,
				RefTable: f.Table, RefColumn: f.To, OnDelete: &od, OnUpdate: &ou})
		}
	}
	return cols, idx, fks, nil
}

var sqliteConstraint = regexp.MustCompile(`(?i)CONSTRAINT\s+["\x60\[]?([A-Za-z0-9_]+)["\x60\]]?\s+FOREIGN\s+KEY\s*\(\s*["\x60\[]?([A-Za-z0-9_]+)`)

func sqliteFKName(createSQL, col, table string) string {
	for _, m := range sqliteConstraint.FindAllStringSubmatch(createSQL, -1) {
		if m[2] == col {
			return m[1]
		}
	}
	return "fk_" + table + "_" + col
}

// ---- PostgreSQL -------------------------------------------------------------------

const pgColumns = `
SELECT c.table_name AS tbl, c.column_name AS col,
       CASE WHEN c.data_type = 'ARRAY' THEN substr(c.udt_name, 2) || '[]'
            WHEN c.data_type = 'USER-DEFINED' THEN c.udt_name ELSE c.data_type END AS typ,
       c.is_nullable = 'YES' AS nullable, c.column_default AS def,
       c.character_maximum_length AS len, c.numeric_precision AS prec, c.numeric_scale AS scale,
       EXISTS (SELECT 1 FROM information_schema.table_constraints tc
               JOIN information_schema.key_column_usage k ON k.constraint_name = tc.constraint_name AND k.table_schema = tc.table_schema
               WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = c.table_schema
                 AND k.table_name = c.table_name AND k.column_name = c.column_name) AS pk,
       (c.is_identity = 'YES' OR c.column_default LIKE 'nextval(%') AS auto,
       c.ordinal_position AS pos
FROM information_schema.columns c
JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
WHERE c.table_schema = current_schema()`

const pgIndexes = `
SELECT t.relname AS tbl, i.relname AS name, ix.indisunique AS uniq, a.attname AS col,
       array_position(ix.indkey::int2[], a.attnum) AS seq
FROM pg_index ix
JOIN pg_class i ON i.oid = ix.indexrelid
JOIN pg_class t ON t.oid = ix.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(ix.indkey)
WHERE n.nspname = current_schema() AND NOT ix.indisprimary`

const pgFKs = `
SELECT t.relname AS tbl, c.conname AS name, a.attname AS col, rt.relname AS ref_tbl, ra.attname AS ref_col,
       c.confdeltype::text AS on_delete, c.confupdtype::text AS on_update
FROM pg_constraint c
JOIN pg_class t ON t.oid = c.conrelid
JOIN pg_class rt ON rt.oid = c.confrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
JOIN pg_attribute ra ON ra.attrelid = c.confrelid AND ra.attnum = c.confkey[1]
WHERE c.contype = 'f' AND n.nspname = current_schema()`

// ---- MySQL ---------------------------------------------------------------------------

const myColumns = `
SELECT c.TABLE_NAME AS tbl, c.COLUMN_NAME AS col, c.COLUMN_TYPE AS typ,
       c.IS_NULLABLE = 'YES' AS nullable, c.COLUMN_DEFAULT AS def,
       c.CHARACTER_MAXIMUM_LENGTH AS len, c.NUMERIC_PRECISION AS prec, c.NUMERIC_SCALE AS scale,
       c.COLUMN_KEY = 'PRI' AS pk, c.EXTRA LIKE '%auto_increment%' AS auto, c.ORDINAL_POSITION AS pos
FROM information_schema.COLUMNS c
JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME AND t.TABLE_TYPE = 'BASE TABLE'
WHERE c.TABLE_SCHEMA = DATABASE()`

const myIndexes = `
SELECT TABLE_NAME AS tbl, INDEX_NAME AS name, NON_UNIQUE = 0 AS uniq, COLUMN_NAME AS col, SEQ_IN_INDEX AS seq
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = DATABASE() AND INDEX_NAME <> 'PRIMARY'`

const myFKs = `
SELECT k.TABLE_NAME AS tbl, k.CONSTRAINT_NAME AS name, k.COLUMN_NAME AS col,
       k.REFERENCED_TABLE_NAME AS ref_tbl, k.REFERENCED_COLUMN_NAME AS ref_col,
       r.DELETE_RULE AS on_delete, r.UPDATE_RULE AS on_update
FROM information_schema.KEY_COLUMN_USAGE k
JOIN information_schema.REFERENTIAL_CONSTRAINTS r
  ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME
WHERE k.TABLE_SCHEMA = DATABASE() AND k.REFERENCED_TABLE_NAME IS NOT NULL AND k.ORDINAL_POSITION = 1`

// ---- SQL Server -------------------------------------------------------------------------

const msColumns = `
SELECT c.TABLE_NAME AS tbl, c.COLUMN_NAME AS col, c.DATA_TYPE AS typ,
       CAST(CASE WHEN c.IS_NULLABLE = 'YES' THEN 1 ELSE 0 END AS BIT) AS nullable, c.COLUMN_DEFAULT AS def,
       CAST(c.CHARACTER_MAXIMUM_LENGTH AS BIGINT) AS len, CAST(c.NUMERIC_PRECISION AS BIGINT) AS prec, CAST(c.NUMERIC_SCALE AS BIGINT) AS scale,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
               JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE k ON k.CONSTRAINT_NAME = tc.CONSTRAINT_NAME AND k.TABLE_SCHEMA = tc.TABLE_SCHEMA
               WHERE tc.CONSTRAINT_TYPE = 'PRIMARY KEY' AND tc.TABLE_SCHEMA = c.TABLE_SCHEMA
                 AND k.TABLE_NAME = c.TABLE_NAME AND k.COLUMN_NAME = c.COLUMN_NAME) THEN 1 ELSE 0 END AS BIT) AS pk,
       CAST(COLUMNPROPERTY(OBJECT_ID(c.TABLE_SCHEMA + '.' + c.TABLE_NAME), c.COLUMN_NAME, 'IsIdentity') AS BIT) AS auto,
       CAST(c.ORDINAL_POSITION AS BIGINT) AS pos
FROM INFORMATION_SCHEMA.COLUMNS c
JOIN INFORMATION_SCHEMA.TABLES t ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME AND t.TABLE_TYPE = 'BASE TABLE'
WHERE c.TABLE_SCHEMA = SCHEMA_NAME()`

const msIndexes = `
SELECT t.name AS tbl, i.name AS name, i.is_unique AS uniq, c.name AS col, CAST(ic.key_ordinal AS BIGINT) AS seq
FROM sys.indexes i
JOIN sys.tables t ON t.object_id = i.object_id
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.is_primary_key = 0 AND i.type > 0 AND SCHEMA_NAME(t.schema_id) = SCHEMA_NAME()`

const msFKs = `
SELECT t.name AS tbl, fk.name AS name, c.name AS col, rt.name AS ref_tbl, rc.name AS ref_col,
       fk.delete_referential_action_desc AS on_delete, fk.update_referential_action_desc AS on_update
FROM sys.foreign_keys fk
JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id AND fkc.constraint_column_id = 1
JOIN sys.tables t ON t.object_id = fk.parent_object_id
JOIN sys.columns c ON c.object_id = fkc.parent_object_id AND c.column_id = fkc.parent_column_id
JOIN sys.tables rt ON rt.object_id = fk.referenced_object_id
JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
WHERE SCHEMA_NAME(t.schema_id) = SCHEMA_NAME()`
