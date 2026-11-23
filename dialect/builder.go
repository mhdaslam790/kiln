package dialect

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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
	case KindMySQL:
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
	if ident == "*" || ident == "" {
		return ident
	}
	if strings.ContainsAny(ident, "() '\"`[]+-/,:") || (ident[0] >= '0' && ident[0] <= '9') {
		return ident
	}
	if q, ok := d.quoted.Load(ident); ok {
		return q.(string)
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
			b.w(d.Quote(s.Field))
			if s.Desc {
				b.w(" DESC")
			} else {
				b.w(" ASC")
			}
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
	b.w("INSERT INTO ", d.Quote(ins.Table))

	ret := ins.Returning
	if d.kind == KindMySQL {
		ret = ""
	}

	if len(ins.Columns) == 0 {
		switch d.kind {
		case KindMySQL:
			b.w(" () VALUES ()")
		case KindMSSQL:
			if ret != "" {
				b.w(" OUTPUT INSERTED.", d.Quote(ret))
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
			b.w(d.Quote(c))
		}
		b.w(")")
		if d.kind == KindMSSQL && ret != "" {
			b.w(" OUTPUT INSERTED.", d.Quote(ret))
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
				qc := d.Quote(c)
				b.w(" ON DUPLICATE KEY UPDATE ", qc, " = ", qc)
			} else {
				b.w(" ON DUPLICATE KEY UPDATE ")
				for i, c := range oc.Update {
					if i > 0 {
						b.w(", ")
					}
					qc := d.Quote(c)
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
					b.w(d.Quote(c))
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
					qc := d.Quote(c)
					b.w(qc, " = EXCLUDED.", qc)
				}
			}
		}
	}

	if ret != "" && d.kind != KindMSSQL {
		b.w(" RETURNING ", d.Quote(ret))
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
		f := d.Quote(m.Field)
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
