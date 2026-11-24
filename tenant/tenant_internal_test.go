package tenant

import (
	"context"
	"errors"
	"testing"
)

func TestContext(t *testing.T) {
	bg := context.Background()
	if _, ok := From(bg); ok {
		t.Fatal("From(background) ok")
	}
	if id, ok := From(With(bg, "acme")); !ok || id != "acme" {
		t.Fatalf("From = %v, %v", id, ok)
	}
	if _, ok := From(Bypass(With(bg, "acme"))); ok {
		t.Fatal("Bypass still reports a tenant")
	}
	if id, ok := From(With(Bypass(bg), 7)); !ok || id != 7 {
		t.Fatal("With after Bypass must set a tenant again")
	}
	if _, _, err := lookup(bg); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("lookup(background) err = %v", err)
	}
	if _, bypassed, err := lookup(Bypass(bg)); !bypassed || err != nil {
		t.Fatalf("lookup(bypass) = %v, %v", bypassed, err)
	}
}

func TestValueHelpers(t *testing.T) {
	s, empty := "a", ""
	type uuid [2]byte
	for _, tc := range []struct {
		v    any
		zero bool
	}{{nil, true}, {0, true}, {int64(3), false}, {"", true}, {"x", false}, {&s, false}, {&empty, true}, {(*string)(nil), true}, {uuid{}, true}, {uuid{1}, false}} {
		if got := isZero(tc.v); got != tc.zero {
			t.Errorf("isZero(%#v) = %v", tc.v, got)
		}
	}
	other := "a"
	for _, tc := range []struct {
		a, b any
		eq   bool
	}{{&s, &other, true}, {&s, "a", true}, {int64(1), int64(1), true}, {int64(1), int64(2), false}, {[]byte("k"), []byte("k"), true}, {uuid{1}, uuid{2}, false}, {nil, (*string)(nil), true}} {
		if got := equal(tc.a, tc.b); got != tc.eq {
			t.Errorf("equal(%#v, %#v) = %v", tc.a, tc.b, got)
		}
	}
}
