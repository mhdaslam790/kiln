package orm

import (
	"context"
	"sync"
	"testing"

	"github.com/mhdaslam790/kiln/q"
)

func TestEventTableAction(t *testing.T) {
	ctx := context.Background()
	db, _ := fakeDB(t, 1)
	var (
		mu  sync.Mutex
		got []string
	)
	WithHook(func(_ context.Context, e Event) {
		mu.Lock()
		got = append(got, e.Action.String()+" "+e.Table)
		mu.Unlock()
	})(db)
	users := For[benchUser](db)

	u, err := users.FindByID(ctx, int64(1))
	must(t, err)
	_, _ = users.Count(ctx) // the fake driver cannot scan a count; the event is what matters
	must(t, users.Update(ctx, &u))
	_, err = users.UpdateAny(ctx, q.Eq("id", 1), q.Set("name", "x"))
	must(t, err)
	must(t, users.HardDelete(ctx, &u))
	var raw []benchUser
	must(t, db.Query(ctx, &raw, rawSelect))
	_, err = db.Exec(ctx, "SELECT 1")
	must(t, err)

	want := []string{"select bench_users", "count bench_users", "update bench_users",
		"update bench_users", "delete bench_users", "raw ", "raw "}
	if len(got) != len(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
