package tenant

import "testing"

func TestSameTenant(t *testing.T) {
	one, s := int64(1), "acme"
	for _, c := range []struct {
		v, id any
		want  bool
	}{
		{int64(1), 1, true}, {int32(1), int64(1), true}, {&one, 1, true}, {"acme", "acme", true}, {&s, "acme", true},
		{int64(2), 1, false}, {"acme", "globex", false}, {nil, 1, false}, {int64(1), nil, false},
		{int8(1), 257, false}, // 257 doesn't fit: no false match through overflow
	} {
		if got := sameTenant(c.v, c.id); got != c.want {
			t.Errorf("sameTenant(%v, %v) = %v, want %v", c.v, c.id, got, c.want)
		}
	}
}
