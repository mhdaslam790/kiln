package orm

import (
	"context"
	"errors"
	"testing"
)

type readRec struct {
	calls int
	err   error
}

func (r *readRec) Name() string { return "read" }
func (r *readRec) AfterRead(_ context.Context, ri *ReadInfo) error {
	r.calls++
	if ri.Table != "bench_users" || ri.Model == nil || ri.Field(0, "nope") != nil {
		return errors.New("bad info")
	}
	for i := 0; i < ri.Len(); i++ {
		*ri.Field(i, "name").(*string) = "seen"
	}
	return r.err
}

func TestReadPlugin(t *testing.T) {
	ctx := context.Background()
	db, _ := fakeDB(t, 3)
	r := &readRec{}
	Use(r)(db)
	users := For[benchUser](db)

	all, err := users.FindAll(ctx)
	must(t, err)
	if len(all) != 3 || all[2].Name != "seen" {
		t.Fatalf("FindAll: %+v", all)
	}
	one, err := users.FindByID(ctx, 1)
	must(t, err)
	if one.Name != "seen" || r.calls != 2 {
		t.Fatalf("FindByID: %+v, calls %d", one, r.calls)
	}
	r.err = errors.New("refused")
	if _, err := users.FindAll(ctx); !errors.Is(err, r.err) {
		t.Fatalf("error not returned: %v", err)
	}
	// Raw SQL is not passed to the plugin.
	r.err = nil
	var raw []benchUser
	must(t, db.Query(ctx, &raw, "SELECT * FROM bench_users"))
	if r.calls != 3 || len(raw) == 0 || raw[0].Name == "seen" {
		t.Fatalf("raw: calls %d", r.calls)
	}
}
