package kilnpg

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/q"
)

func TestArrayRoundTrip(t *testing.T) {
	strs := Array[string]{"go", `a "quoted" word`, `back\slash`, "comma,inside", " spaced ", "NULL", "{brace}", ""}
	v, err := strs.Value()
	if err != nil {
		t.Fatal(err)
	}
	var back Array[string]
	if err := back.Scan(v); err != nil || !reflect.DeepEqual(back, strs) {
		t.Fatalf("strings: %q -> %q, %v", v, back, err)
	}
	ints := Array[int64]{1, -2, 300}
	v, _ = ints.Value()
	var bi Array[int64]
	if err := bi.Scan([]byte(v.(string))); err != nil || !reflect.DeepEqual(bi, ints) || v != "{1,-2,300}" {
		t.Fatalf("ints: %v -> %v, %v", v, bi, err)
	}
	var bb Array[bool]
	if err := bb.Scan("{t,f,true}"); err != nil || !reflect.DeepEqual(bb, Array[bool]{true, false, true}) {
		t.Fatalf("bools: %v %v", bb, err)
	}
	var bf Array[float64]
	if err := bf.Scan("{1.5,-2,3e2}"); err != nil || !reflect.DeepEqual(bf, Array[float64]{1.5, -2, 300}) {
		t.Fatalf("floats: %v %v", bf, err)
	}
	// NULL and empty.
	var nilArr Array[string]
	if v, _ := nilArr.Value(); v != nil {
		t.Errorf("nil Array = %v, want NULL", v)
	}
	if v, _ := (Array[string]{}).Value(); v != "{}" {
		t.Errorf("empty Array = %v", v)
	}
	back = Array[string]{"x"}
	if err := back.Scan(nil); err != nil || back != nil {
		t.Errorf("Scan(nil) = %v %v", back, err)
	}
	if err := back.Scan("{}"); err != nil || back == nil || len(back) != 0 {
		t.Errorf("Scan({}) = %#v %v", back, err)
	}
	// Errors, not silent garbage.
	for _, bad := range []string{"{a,NULL}", "{{1,2},{3,4}}", `{"open}`, "nope", "{1,x}"} {
		var a Array[int64]
		if bad == "{a,NULL}" {
			var s Array[string]
			if err := s.Scan(bad); err == nil {
				t.Errorf("%q: no error", bad)
			}
			continue
		}
		if err := a.Scan(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	// Lower bounds prefix.
	if err := bi.Scan("[0:1]={5,6}"); err != nil || !reflect.DeepEqual(bi, Array[int64]{5, 6}) {
		t.Errorf("bounds: %v %v", bi, err)
	}
}

func TestFiltersQuoteColumn(t *testing.T) {
	f := Contains("my tags", "a").(q.ExprFilter)
	if !strings.HasPrefix(f.SQL, `"my tags" @> `) {
		t.Errorf("SQL %q", f.SQL)
	}
	h := Has(`x") OR 1=1 --`, "a").(q.ExprFilter)
	if !strings.Contains(h.SQL, `ANY("x"") OR 1=1 --")`) {
		t.Errorf("column not quoted safely: %q", h.SQL)
	}
}
