package orm

import (
	"context"
	"errors"
	"testing"
)

type valueKey struct{}

type afterRec struct {
	seen    []string
	carried []any
	inTx    []bool
	fail    bool
	db      *DB
}

func (p *afterRec) Name() string { return "after-rec" }

func (p *afterRec) BeforeWrite(_ context.Context, w *WriteInfo) error {
	w.SetValue(valueKey{}, "from-before:"+w.Action.String())
	return nil
}

func (p *afterRec) AfterWrite(ctx context.Context, w *WriteInfo) error {
	p.seen = append(p.seen, w.Action.String()+" "+w.Table)
	p.carried = append(p.carried, w.Value(valueKey{}))
	p.inTx = append(p.inTx, p.db.InTransaction(ctx))
	if _, ok := w.Record.(*benchUser); !ok {
		return errors.New("record is not *benchUser")
	}
	if p.fail {
		return errors.New("audit store down")
	}
	return nil
}

func TestAfterWrite(t *testing.T) {
	ctx := context.Background()
	db, _ := fakeDB(t, 1)
	p := &afterRec{db: db}
	Use(p)(db)
	users := For[benchUser](db)

	u := benchUser{ID: 7, Name: "ann"}
	must(t, users.Insert(ctx, &u))
	must(t, users.Update(ctx, &u))
	must(t, users.Update(ctx, &u, "Name"))
	must(t, users.HardDelete(ctx, &u))

	want := []string{"insert bench_users", "update bench_users", "update bench_users", "delete bench_users"}
	if len(p.seen) != len(want) {
		t.Fatalf("seen %q", p.seen)
	}
	for i := range want {
		if p.seen[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, p.seen[i], want[i])
		}
		if !p.inTx[i] {
			t.Errorf("call %d ran outside a transaction", i)
		}
	}
	// BeforeWrite's value reaches AfterWrite; deletes have no BeforeWrite.
	if p.carried[0] != "from-before:insert" || p.carried[1] != "from-before:update" || p.carried[3] != nil {
		t.Errorf("carried values: %v", p.carried)
	}

	// An AfterWrite error fails the write.
	p.fail = true
	if err := users.Update(ctx, &u); err == nil || !errors.Is(err, err) || err.Error() != "orm: plugin after-rec: audit store down" {
		t.Fatalf("failing AfterWrite: %v", err)
	}
}
