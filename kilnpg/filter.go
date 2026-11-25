package kilnpg

import (
	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/q"
)

var pg = dialect.Postgres()

func arrayOp[T Elem](col, op string, vals []T) q.Filter {
	return q.Expr(pg.QuoteName(col)+" "+op+" ?", Array[T](append([]T{}, vals...)))
}

// Has matches rows whose array column col contains v (v = ANY(col)).
func Has[T Elem](col string, v T) q.Filter {
	return q.Expr("? = ANY("+pg.QuoteName(col)+")", v)
}

// Contains matches rows whose array column col contains all of vals (@>).
func Contains[T Elem](col string, vals ...T) q.Filter { return arrayOp(col, "@>", vals) }

// ContainedBy matches rows whose array column col has only elements from
// vals (<@).
func ContainedBy[T Elem](col string, vals ...T) q.Filter { return arrayOp(col, "<@", vals) }

// Overlaps matches rows whose array column col shares an element with vals
// (&&).
func Overlaps[T Elem](col string, vals ...T) q.Filter { return arrayOp(col, "&&", vals) }
