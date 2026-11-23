package orm

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/q"
)

func TestCacheCodecRoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 23, 10, 30, 0, 123, time.FixedZone("x", 3600))
	in := &cachedResult{
		cols: []string{"id", "name", "score", "ok", "blob", "at", "nothing"},
		rows: [][]any{
			{int64(-42), "ann", 3.5, true, []byte{0, 1, 2}, ts, nil},
			{int64(1 << 62), "", math.Copysign(0, -1), false, []byte{}, time.Time{}, nil},
		},
	}
	b, err := encodeResult(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.cols, out.cols) || len(out.rows) != 2 {
		t.Fatalf("%+v", out)
	}
	for i := range in.rows {
		for j := range in.rows[i] {
			a, b := in.rows[i][j], out.rows[i][j]
			if ta, ok := a.(time.Time); ok {
				if !ta.Equal(b.(time.Time)) {
					t.Fatalf("[%d][%d] %v != %v", i, j, a, b)
				}
				continue
			}
			if fa, ok := a.(float64); ok && math.Signbit(fa) != math.Signbit(b.(float64)) {
				t.Fatalf("[%d][%d] sign of %v lost", i, j, a)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("[%d][%d] %#v != %#v", i, j, a, b)
			}
		}
	}

	// Truncated or corrupted entries are rejected, never misread.
	for n := 0; n < len(b); n++ {
		if _, err := decodeResult(b[:n]); err == nil {
			t.Fatalf("truncated entry of %d/%d bytes accepted", n, len(b))
		}
	}
	if _, err := decodeResult(append(b, 0)); err == nil {
		t.Fatal("trailing garbage accepted")
	}

	// Unknown driver types make the result uncacheable instead of wrong.
	if _, err := encodeResult(&cachedResult{cols: []string{"x"}, rows: [][]any{{struct{}{}}}}); err == nil {
		t.Fatal("unsupported type encoded")
	}
}

func TestQueryTables(t *testing.T) {
	sub := q.From("orders").Select("user_id").Where(q.Gt("total", 10))
	qq := q.From("users u").Where(q.Or(q.In("id", sub), q.Not(q.Eq("x", q.From("carts").Select("id")))))
	qq.Joins = append(qq.Joins, q.JoinClause{Table: "profiles AS p"})
	got := dedupe(queryTables(qq))
	want := []string{"users", "profiles", "orders", "carts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := queryTables(q.SQL("SELECT 1")); len(got) != 0 {
		t.Fatalf("raw SQL tables: %v", got)
	}
}
