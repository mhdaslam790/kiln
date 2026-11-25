package orm

import (
	"context"
	"os"
	"testing"

	"github.com/mhdaslam790/kiln/q"
)

// TestAllocGuard fails when a change makes common operations allocate more,
// e.g. a new feature or plugin hook that costs something even when nobody
// uses it. Numbers are allocations per call against an instant in-memory
// driver, without plugins, measured with the current Go release (older
// compilers allocate a little more; CI runs this on Go stable only). If a
// change lowers them, lower the limits too;
// raise them only on purpose, with the reason in the commit message.
// KILN_ALLOC_PRINT=1 go test -run AllocGuard -v ./orm prints every count.
func TestAllocGuard(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("allocation counts differ under -short / -race")
	}
	ctx := context.Background()
	db1, _ := fakeDB(t, 1)
	db100, _ := fakeDB(t, 100)
	users1, users100 := For[benchUser](db1), For[benchUser](db100)
	u := benchUser{ID: 1, Name: "n", Email: "e", Age: 3}
	pusers := For[benchUser](pluginDB(t))
	buf := make([]benchUser, 100)
	make100 := func() []benchUser {
		for i := range buf {
			buf[i] = benchUser{Name: "n", Email: "e", Age: i}
		}
		return buf
	}

	for _, tc := range []struct {
		name string
		max  float64
		fn   func()
	}{
		{"FindByID", 19, func() { _, _ = users1.FindByID(ctx, 1) }},
		{"FindAll100", 319, func() { _, _ = users100.FindAll(ctx, q.Eq("age", 3)) }},
		{"Count", 18, func() { _, _ = users1.Count(ctx, q.Eq("age", 3)) }},
		{"Insert", 26, func() { x := benchUser{Name: "n"}; _ = users1.Insert(ctx, &x) }},
		{"Update", 13, func() { _ = users1.Update(ctx, &u) }},
		{"UpdateFields", 11, func() { _ = users1.Update(ctx, &u, "Age") }},
		{"UpdateAny", 11, func() { _, _ = users1.UpdateAny(ctx, q.Eq("age", 3), q.Set("age", 4)) }},
		{"HardDelete", 6, func() { _ = users1.HardDelete(ctx, &u) }},
		{"Transaction", 9, func() { _ = db1.Transaction(ctx, func(context.Context) error { return nil }) }},
		// Batch limits allow 1%: database/sql pool bookkeeping moves the
		// count by ±2, while a per-record cost would add 100 or more.
		{"InsertAll100", 655, func() { _ = users1.InsertAll(ctx, make100()) }},
		// With a plugin installed, each hook costs at most one allocation
		// per call (not per record).
		{"Plugin/FindByID", 20, func() { _, _ = pusers.FindByID(ctx, 1) }},
		{"Plugin/InsertAll100", 655, func() { _ = pusers.InsertAll(ctx, make100()) }},
	} {
		got := testing.AllocsPerRun(200, tc.fn)
		if os.Getenv("KILN_ALLOC_PRINT") != "" {
			t.Logf("%s: %.0f allocs (limit %.0f)", tc.name, got, tc.max)
		}
		if got > tc.max {
			t.Errorf("%s: %.0f allocs per call, limit %.0f", tc.name, got, tc.max)
		}
	}
}
