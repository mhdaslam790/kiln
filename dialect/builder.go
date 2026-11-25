package dialect

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhdaslam790/kiln/q"
)

type sqlDialect struct {
	kind Kind
	// quoted caches Quote results; identifiers are few and reused on every
	// query. Bounded so dynamic identifiers cannot grow it without limit.
	quoted  sync.Map
	nQuoted atomic.Int32
}

const maxQuoteCache = 4096

func (d *sqlDialect) Kind() Kind { return d.kind }

func (d *sqlDialect) IDMode() IDMode {
	if d.kind == KindMySQL {
		return IDLastInsertID
	}
	return IDReturning
}

func (d *sqlDialect) placeholder(n int) string {
	switch d.kind {
	case KindPostgres:
		return "$" + strconv.Itoa(n)
	case KindMSSQL:
		return "@p" + strconv.Itoa(n)
	}
	return "?"
}

func (d *sqlDialect) quoteOne(s string) string {
	switch d.kind {
	case KindMySQL, KindSQLite:
		// SQLite: a "double-quoted" name that is not a column silently
		// becomes a string literal; a `backquoted` one is an error.
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	case KindMSSQL:
		return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// Quote quotes an identifier. Dotted names are quoted per part, '*' is kept,
// and anything that looks like an expression (parens, spaces, quotes) is
// passed through untouched so callers can select e.g. "COUNT(*) AS n".
func (d *sqlDialect) Quote(ident string) string {
	if q, ok := d.quoted.Load(ident); ok { // only names are cached
		return q.(string)
	}
	if ident == "*" || ident == "" || isExpr(ident) {
		return ident
	}
	parts := strings.Split(ident, ".")
	for i, p := range parts {
		if p != "*" {
			parts[i] = d.quoteOne(p)
		}
	}
	q := strings.Join(parts, ".")
	if d.nQuoted.Load() < maxQuoteCache {
		if _, loaded := d.quoted.LoadOrStore(ident, q); !loaded {
			d.nQuoted.Add(1)
		}
	}
	return q
}

// isExpr reports whether Quote passes s through as SQL: it has expression
// syntax (parens, spaces, quotes, operators) or is a number.
func isExpr(s string) bool {
	for i := 0; i < len(s); i++ {
		if exprByte[s[i]] {
			return true
		}
	}
	return isNumber(s)
}

var exprByte = func() (t [256]bool) {
	for _, c := range []byte("() '\"`[]+/,:\t\n\r") {
		t[c] = true
	}
	return t
}()

// ident quotes a name in a position that can only hold a name (sort field,
// SET target, insert/returning column, column definition): always quoted
// and escaped, so it is safe for any input and works for any column name.
func (d *sqlDialect) ident(s string) string {
	if q, ok := d.quoted.Load(s); ok {
		return q.(string)
	}
	if !isExpr(s) {
		return d.Quote(s) // quotes and caches
	}
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = d.quoteOne(p)
	}
	return strings.Join(parts, ".")
}

func (d *sqlDialect) QuoteName(name string) string { return d.ident(name) }

func (d *sqlDialect) Rebind(sql string) string {
	b := d.newBuf()
	// Rebind on its own does not validate arg counts; feed nil args.
	_ = b.raw(sql, nil, false)
	return b.sb.String()
}

func (d *sqlDialect) Savepoint(name string) string {
	if d.kind == KindMSSQL {
		return "SAVE TRANSACTION " + name
	}
	return "SAVEPOINT " + name
}

func (d *sqlDialect) RollbackTo(name string) string {
	if d.kind == KindMSSQL {
		return "ROLLBACK TRANSACTION " + name
	}
	return "ROLLBACK TO SAVEPOINT " + name
}

func (d *sqlDialect) Release(name string) string {
	if d.kind == KindMSSQL {
		return ""
	}
	return "RELEASE SAVEPOINT " + name
}

// ---- buffer ---------------------------------------------------------------

type buf struct {
	d    *sqlDialect
	sb   strings.Builder
	args []any
}

func (b *buf) w(s ...string) {
	for _, x := range s {
		b.sb.WriteString(x)
	}
}

func (b *buf) arg(v any) {
	if b.d.kind == KindSQLite || b.d.kind == KindMSSQL {
		v = utcArg(v)
	}
	b.args = append(b.args, v)
	n := len(b.args)
	switch b.d.kind {
	case KindPostgres:
		b.sb.WriteByte('$')
	case KindMSSQL:
		b.sb.WriteString("@p")
	default:
		b.sb.WriteByte('?')
		return
	}
	// Append the number without allocating a string.
	var tmp [20]byte
	b.sb.Write(strconv.AppendInt(tmp[:0], int64(n), 10))
}

// newBuf returns a builder sized for a typical statement, so building it
// does not grow (and copy) the buffer several times.
func (d *sqlDialect) newBuf() *buf {
	b := &buf{d: d, args: make([]any, 0, 8)}
	b.sb.Grow(256)
	return b
}

// reserve makes room for n more args and about bytes more SQL, so large
// statements (multi-row inserts) do not grow their buffers step by step.
func (b *buf) reserve(n, bytes int) {
	if need := len(b.args) + n; need > cap(b.args) {
		args := make([]any, len(b.args), need)
		copy(args, b.args)
		b.args = args
	}
	b.sb.Grow(bytes)
}

func (b *buf) stmt() Stmt { return Stmt{SQL: b.sb.String(), Args: b.args} }

// raw copies a SQL fragment, turning each '?' outside of quotes into a
// placeholder and appending args. '??' yields a literal '?'.
func (b *buf) raw(sql string, args []any, check bool) error {
	n := 0
	var quote byte
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if quote != 0 {
			b.sb.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
			b.sb.WriteByte(c)
		case '?':
			if i+1 < len(sql) && sql[i+1] == '?' {
				b.sb.WriteByte('?')
				i++
				continue
			}
			if check {
				if n >= len(args) {
					return fmt.Errorf("dialect: not enough arguments for %q", sql)
				}
				b.arg(args[n])
			} else {
				b.args = append(b.args, nil)
				b.sb.WriteString(b.d.placeholder(len(b.args)))
			}
			n++
		default:
			b.sb.WriteByte(c)
		}
	}
	if check && n != len(args) {
		return fmt.Errorf("dialect: %d placeholders but %d arguments in %q", n, len(args), sql)
	}
	return nil
}

// ---- filters --------------------------------------------------------------

var opSQL = map[q.Op]string{
	q.OpEq: " = ", q.OpNe: " <> ", q.OpLt: " < ", q.OpLte: " <= ",
	q.OpGt: " > ", q.OpGte: " >= ", q.OpLike: " LIKE ", q.OpNotLike: " NOT LIKE ",
}

func (b *buf) value(v any) error {
	switch x := v.(type) {
	case q.F:
		b.w(b.d.Quote(string(x)))
	case q.Query:
		b.w("(")
		if err := b.selectQuery(x); err != nil {
			return err
		}
		b.w(")")
	default:
		b.arg(v)
	}
	return nil
}

func (b *buf) filter(f q.Filter) error {
	switch x := f.(type) {
	case nil:
		b.w("1=1")
	case q.Cond:
		return b.cond(x)
	case q.AndFilter:
		return b.join(x.Inner, " AND ")
	case q.OrFilter:
		return b.join(x.Inner, " OR ")
	case q.NotFilter:
		b.w("NOT (")
		if err := b.filter(x.Inner); err != nil {
			return err
		}
		b.w(")")
	case q.ExprFilter:
		return b.raw(x.SQL, x.Args, true)
	default:
		return fmt.Errorf("dialect: unsupported filter %T", f)
	}
	return nil
}

func (b *buf) join(fs []q.Filter, sep string) error {
	b.w("(")
	for i, f := range fs {
		if i > 0 {
			b.w(sep)
		}
		if err := b.filter(f); err != nil {
			return err
		}
	}
	b.w(")")
	return nil
}

func (b *buf) cond(c q.Cond) error {
	field := b.d.Quote(c.Field)
	switch c.Op {
	case q.OpNull:
		b.w(field, " IS NULL")
		return nil
	case q.OpNotNull:
		b.w(field, " IS NOT NULL")
		return nil
	case q.OpIn, q.OpNotIn:
		not := c.Op == q.OpNotIn
		if len(c.Values) == 1 {
			if sub, ok := c.Values[0].(q.Query); ok {
				b.w(field)
				if not {
					b.w(" NOT")
				}
				b.w(" IN ")
				return b.value(sub)
			}
		}
		if len(c.Values) == 0 {
			if not {
				b.w("1=1")
			} else {
				b.w("1=0")
			}
			return nil
		}
		b.w(field)
		if not {
			b.w(" NOT")
		}
		b.w(" IN (")
		for i, v := range c.Values {
			if i > 0 {
				b.w(", ")
			}
			if err := b.value(v); err != nil {
				return err
			}
		}
		b.w(")")
		return nil
	}
	if c.Value == nil {
		switch c.Op {
		case q.OpEq:
			b.w(field, " IS NULL")
			return nil
		case q.OpNe:
			b.w(field, " IS NOT NULL")
			return nil
		}
	}
	op, ok := opSQL[c.Op]
	if !ok {
		return fmt.Errorf("dialect: unknown operator %d", c.Op)
	}
	b.w(field, op)
	return b.value(c.Value)
}

// ---- select ---------------------------------------------------------------

func (d *sqlDialect) Select(qq q.Query) (Stmt, error) {
	b := d.newBuf()
	if err := b.selectQuery(qq); err != nil {
		return Stmt{}, err
	}
	return b.stmt(), nil
}

func (d *sqlDialect) Count(qq q.Query) (Stmt, error) {
	b := d.newBuf()
	if qq.RawSQL != "" {
		b.w("SELECT COUNT(*) FROM (")
		if err := b.raw(qq.RawSQL, qq.RawArgs, true); err != nil {
			return Stmt{}, err
		}
		b.w(") ", d.Quote("t"))
		return b.stmt(), nil
	}
	qq.Sorts, qq.LimitN, qq.OffsetN, qq.Lock = nil, 0, 0, false
	if qq.IsDistinct || len(qq.GroupBy) > 0 {
		b.w("SELECT COUNT(*) FROM (")
		if err := b.selectQuery(qq); err != nil {
			return Stmt{}, err
		}
		b.w(") ", d.Quote("t"))
		return b.stmt(), nil
	}
	qq.Fields = []string{"COUNT(*)"}
	if err := b.selectQuery(qq); err != nil {
		return Stmt{}, err
	}
	return b.stmt(), nil
}

func (b *buf) selectQuery(qq q.Query) error {
	d := b.d
	if qq.RawSQL != "" {
		return b.raw(qq.RawSQL, qq.RawArgs, true)
	}
	if qq.Table == "" {
		return errors.New("dialect: query has no table")
	}
	b.w("SELECT ")
	if qq.IsDistinct {
		b.w("DISTINCT ")
	}
	mssqlTop := d.kind == KindMSSQL && qq.LimitN > 0 && qq.OffsetN == 0
	if mssqlTop {
		b.w("TOP (", strconv.Itoa(qq.LimitN), ") ")
	}
	if len(qq.Fields) == 0 {
		b.w("*")
	} else {
		for i, f := range qq.Fields {
			if i > 0 {
				b.w(", ")
			}
			b.w(d.Quote(f))
		}
	}
	b.w(" FROM ", d.Quote(qq.Table))
	if qq.Lock && d.kind == KindMSSQL {
		b.w(" WITH (UPDLOCK, ROWLOCK)")
	}
	for _, j := range qq.Joins {
		b.w(" ", string(j.Kind), " ", d.Quote(j.Table), " ON ")
		if err := b.raw(j.On, j.Args, true); err != nil {
			return err
		}
	}
	if qq.WhereCond != nil {
		b.w(" WHERE ")
		if err := b.filter(qq.WhereCond); err != nil {
			return err
		}
	}
	if len(qq.GroupBy) > 0 {
		b.w(" GROUP BY ")
		for i, g := range qq.GroupBy {
			if i > 0 {
				b.w(", ")
			}
			b.w(d.Quote(g))
		}
	}
	if qq.HavingCond != nil {
		b.w(" HAVING ")
		if err := b.filter(qq.HavingCond); err != nil {
			return err
		}
	}
	if len(qq.Sorts) > 0 {
		b.w(" ORDER BY ")
		for i, s := range qq.Sorts {
			if i > 0 {
				b.w(", ")
			}
			dir := " ASC"
			if s.Desc {
				dir = " DESC"
			}
			if s.NullsLarge { // portable: no NULLS FIRST/LAST in MySQL or SQL Server
				b.w("CASE WHEN ", d.ident(s.Field), " IS NULL THEN 1 ELSE 0 END", dir, ", ")
			}
			b.w(d.ident(s.Field), dir)
		}
	}
	switch {
	case d.kind == KindMSSQL:
		if qq.OffsetN > 0 {
			if len(qq.Sorts) == 0 {
				b.w(" ORDER BY (SELECT NULL)")
			}
			b.w(" OFFSET ", strconv.Itoa(qq.OffsetN), " ROWS")
			if qq.LimitN > 0 {
				b.w(" FETCH NEXT ", strconv.Itoa(qq.LimitN), " ROWS ONLY")
			}
		}
	default:
		if qq.LimitN > 0 {
			b.w(" LIMIT ", strconv.Itoa(qq.LimitN))
		} else if qq.OffsetN > 0 && d.kind != KindPostgres {
			// MySQL and SQLite require LIMIT before OFFSET.
			if d.kind == KindMySQL {
				b.w(" LIMIT 18446744073709551615")
			} else {
				b.w(" LIMIT -1")
			}
		}
		if qq.OffsetN > 0 {
			b.w(" OFFSET ", strconv.Itoa(qq.OffsetN))
		}
	}
	if qq.Lock && (d.kind == KindPostgres || d.kind == KindMySQL) {
		b.w(" FOR UPDATE")
	}
	return nil
}

// ---- insert / update / delete --------------------------------------------

func (d *sqlDialect) Insert(ins Insert) (Stmt, error) {
	b := d.newBuf()
	oc := ins.OnConflict
	if oc != nil && d.kind == KindMSSQL {
		return Stmt{}, errors.New("dialect: OnConflict is not supported on SQL Server; use a MERGE via raw SQL")
	}
	if oc != nil && (d.kind == KindPostgres || d.kind == KindSQLite) && len(oc.Columns) == 0 && !oc.DoNothing {
		return Stmt{}, errors.New("dialect: OnConflict.Update requires conflict Columns on this database")
	}
	if len(ins.Rows) > 1 {
		n := len(ins.Rows) * len(ins.Columns)
		b.reserve(n, n*6+len(ins.Columns)*24) // "$123, " per value, quoted column list
	}
	b.w("INSERT INTO ", d.Quote(ins.Table))

	ret := ins.Returning
	if d.kind == KindMySQL {
		ret = ""
	}
	if d.kind == KindMSSQL && ret != "" && len(ins.Rows) > 1 && len(ins.Columns) > 0 {
		return d.mssqlMergeInsert(ins)
	}

	if len(ins.Columns) == 0 {
		switch d.kind {
		case KindMySQL:
			b.w(" () VALUES ()")
		case KindMSSQL:
			if ret != "" {
				b.w(" OUTPUT INSERTED.", d.ident(ret))
			}
			b.w(" DEFAULT VALUES")
		default:
			b.w(" DEFAULT VALUES")
		}
	} else {
		b.w(" (")
		for i, c := range ins.Columns {
			if i > 0 {
				b.w(", ")
			}
			b.w(d.ident(c))
		}
		b.w(")")
		if d.kind == KindMSSQL && ret != "" {
			b.w(" OUTPUT INSERTED.", d.ident(ret))
		}
		b.w(" VALUES ")
		for r, row := range ins.Rows {
			if len(row) != len(ins.Columns) {
				return Stmt{}, fmt.Errorf("dialect: row %d has %d values, want %d", r, len(row), len(ins.Columns))
			}
			if r > 0 {
				b.w(", ")
			}
			b.w("(")
			for i, v := range row {
				if i > 0 {
					b.w(", ")
				}
				b.arg(v)
			}
			b.w(")")
		}
	}

	if oc != nil {
		switch d.kind {
		case KindMySQL:
			if oc.DoNothing || len(oc.Update) == 0 {
				// A self-assignment ignores only duplicate-key conflicts,
				// unlike INSERT IGNORE which also hides other errors.
				c := ""
				if len(oc.Columns) > 0 {
					c = oc.Columns[0]
				} else if len(ins.Columns) > 0 {
					c = ins.Columns[0]
				}
				if c == "" {
					return Stmt{}, errors.New("dialect: OnConflict needs at least one column on MySQL")
				}
				qc := d.ident(c)
				b.w(" ON DUPLICATE KEY UPDATE ", qc, " = ", qc)
			} else {
				b.w(" ON DUPLICATE KEY UPDATE ")
				for i, c := range oc.Update {
					if i > 0 {
						b.w(", ")
					}
					qc := d.ident(c)
					b.w(qc, " = VALUES(", qc, ")")
				}
			}
		default:
			b.w(" ON CONFLICT")
			if len(oc.Columns) > 0 {
				b.w(" (")
				for i, c := range oc.Columns {
					if i > 0 {
						b.w(", ")
					}
					b.w(d.ident(c))
				}
				b.w(")")
			}
			if oc.DoNothing || len(oc.Update) == 0 {
				b.w(" DO NOTHING")
			} else {
				b.w(" DO UPDATE SET ")
				for i, c := range oc.Update {
					if i > 0 {
						b.w(", ")
					}
					qc := d.ident(c)
					b.w(qc, " = EXCLUDED.", qc)
				}
			}
		}
	}

	if ret != "" && d.kind != KindMSSQL {
		b.w(" RETURNING ", d.ident(ret))
	}
	return b.stmt(), nil
}

func (d *sqlDialect) Update(table string, set []q.Mutation, where q.Filter) (Stmt, error) {
	if len(set) == 0 {
		return Stmt{}, errors.New("dialect: update without SET")
	}
	b := d.newBuf()
	b.w("UPDATE ", d.Quote(table), " SET ")
	for i, m := range set {
		if i > 0 {
			b.w(", ")
		}
		f := d.ident(m.Field)
		b.w(f, " = ")
		switch m.Kind {
		case q.MutSet:
			if err := b.value(m.Value); err != nil {
				return Stmt{}, err
			}
		case q.MutInc:
			b.w(f, " + ")
			b.arg(m.Value)
		case q.MutExpr:
			if err := b.raw(m.SQL, m.Args, true); err != nil {
				return Stmt{}, err
			}
		}
	}
	if where != nil {
		b.w(" WHERE ")
		if err := b.filter(where); err != nil {
			return Stmt{}, err
		}
	}
	return b.stmt(), nil
}

func (d *sqlDialect) Delete(table string, where q.Filter) (Stmt, error) {
	b := d.newBuf()
	b.w("DELETE FROM ", d.Quote(table))
	if where != nil {
		b.w(" WHERE ")
		if err := b.filter(where); err != nil {
			return Stmt{}, err
		}
	}
	return b.stmt(), nil
}

// mssqlMergeInsert inserts several rows and returns (position, id) pairs:
//
//	MERGE INTO [t] USING (VALUES (@p1, @p2, 0), (@p3, @p4, 1)) AS s ([a], [b], [kiln_pos])
//	ON 1 = 0 WHEN NOT MATCHED THEN INSERT ([a], [b]) VALUES (s.[a], s.[b])
//	OUTPUT s.[kiln_pos], INSERTED.[id];
//
// A plain INSERT ... OUTPUT does not guarantee that rows come back in
// VALUES order; MERGE can output the source position next to the new id.
func (d *sqlDialect) mssqlMergeInsert(ins Insert) (Stmt, error) {
	b := d.newBuf()
	n := len(ins.Rows) * len(ins.Columns)
	b.reserve(n, n*8+len(ins.Columns)*48)
	pos := d.ident(PositionColumn)
	b.w("MERGE INTO ", d.Quote(ins.Table), " USING (VALUES ")
	for r, row := range ins.Rows {
		if len(row) != len(ins.Columns) {
			return Stmt{}, fmt.Errorf("dialect: row %d has %d values, want %d", r, len(row), len(ins.Columns))
		}
		if r > 0 {
			b.w(", ")
		}
		b.w("(")
		for _, v := range row {
			b.arg(v)
			b.w(", ")
		}
		b.w(strconv.Itoa(r), ")")
	}
	b.w(") AS s (")
	for _, c := range ins.Columns {
		b.w(d.ident(c), ", ")
	}
	b.w(pos, ") ON 1 = 0 WHEN NOT MATCHED THEN INSERT (", d.quoteList(ins.Columns), ") VALUES (")
	for i, c := range ins.Columns {
		if i > 0 {
			b.w(", ")
		}
		b.w("s.", d.ident(c))
	}
	b.w(") OUTPUT s.", pos, ", INSERTED.", d.ident(ins.Returning), ";")
	return b.stmt(), nil
}

// utcArg sends times in UTC (the same instant) where the column keeps no
// zone. SQLite stores text: the driver can't read back a fixed-offset zone
// (what time.Parse returns for "...+05:00", e.g. from JSON), and mixed
// offsets sort wrongly. SQL Server's DATETIME2 drops the offset, so a
// +05:00 time would read back 5 hours off. (PostgreSQL uses TIMESTAMPTZ;
// the MySQL driver converts to its connection zone itself.)
func utcArg(v any) any {
	switch t := v.(type) {
	case time.Time:
		if t.Location() != time.UTC {
			return t.UTC()
		}
	case *time.Time:
		if t != nil && t.Location() != time.UTC {
			u := t.UTC()
			return &u
		}
	case sql.NullTime:
		if t.Valid && t.Time.Location() != time.UTC {
			return sql.NullTime{Time: t.Time.UTC(), Valid: true}
		}
	}
	return v
}

// isNumber reports whether s is a numeric literal such as 1 or 2.5 (kept
// unquoted, e.g. in a select list); "3d_model" is an identifier.
func isNumber(s string) bool {
	if len(s) > 1 && s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && s[i] != '.' {
			return false
		}
	}
	return true
}
