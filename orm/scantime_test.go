package orm

import (
	"testing"
	"time"
)

func TestNullTimeText(t *testing.T) {
	want := time.Date(2026, 11, 23, 10, 0, 0, 500, time.UTC)
	for _, s := range []string{
		"2026-11-23 10:00:00.0000005 +0000 UTC",
		"2026-11-23 10:00:00.0000005 +0000 UTC m=+0.000123",
		"2026-11-23T10:00:00.0000005Z",
		"2026-11-23 10:00:00.0000005+00:00",
		"2026-11-23 10:00:00.0000005",
	} {
		var n nullTime
		if err := n.Scan(s); err != nil || !n.Valid || !n.Time.Equal(want) {
			t.Errorf("%q: %v %v %v", s, n.Time, n.Valid, err)
		}
		if err := n.Scan([]byte(s)); err != nil || !n.Time.Equal(want) {
			t.Errorf("[]byte %q: %v", s, err)
		}
	}
	var n nullTime
	if err := n.Scan(nil); err != nil || n.Valid {
		t.Error("nil")
	}
	if err := n.Scan("yesterday"); err == nil {
		t.Error("garbage accepted")
	}
	if err := n.Scan(42); err == nil {
		t.Error("int accepted")
	}
}
