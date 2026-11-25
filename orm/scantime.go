package orm

import (
	"fmt"
	"strings"
	"time"
)

// nullTime is a NULL-safe time holder that, unlike sql.NullTime, also reads
// times SQLite returns as text: an expression such as MIN(created_at) loses
// the column's declared type, so the driver hands back the stored string.
type nullTime struct {
	Time  time.Time
	Valid bool
}

func (n *nullTime) Scan(v any) error {
	switch x := v.(type) {
	case time.Time:
		n.Time, n.Valid = x, true
	case nil:
		n.Time, n.Valid = time.Time{}, false
	case string:
		return n.parse(x)
	case []byte:
		return n.parse(string(x))
	default:
		return fmt.Errorf("orm: cannot scan %T into a time", v)
	}
	return nil
}

// timeLayouts are the text forms of times: Go's time.String (how the
// SQLite driver stores a time.Time), RFC 3339, and SQLite's own functions.
var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999 -0700 MST",
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02",
}

func (n *nullTime) parse(s string) error {
	if i := strings.Index(s, " m="); i > 0 { // a monotonic clock reading
		s = s[:i]
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			n.Time, n.Valid = t, true
			return nil
		}
	}
	return fmt.Errorf("orm: cannot read %q as a time", s)
}

// scalarDest wraps time destinations so they read text times too; other
// destinations go to the driver unchanged.
func scalarDest(p any) any {
	switch x := p.(type) {
	case *time.Time:
		return &timeDest{p: x}
	case **time.Time:
		return timePtrDest{x}
	}
	return p
}

type timePtrDest struct{ p **time.Time }

func (d timePtrDest) Scan(v any) error {
	var n nullTime
	if err := n.Scan(v); err != nil {
		return err
	}
	if !n.Valid {
		*d.p = nil
		return nil
	}
	t := n.Time
	*d.p = &t
	return nil
}
