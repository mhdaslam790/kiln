package orm

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/q"
)

type orderPlugin struct {
	name string
	log  *[]string
	err  error
}

func (p orderPlugin) Name() string { return p.name }
func (p orderPlugin) BeforeQuery(_ context.Context, info *QueryInfo) error {
	*p.log = append(*p.log, p.name+":"+info.Action.String()+" "+info.Table)
	return p.err
}
func (p orderPlugin) BeforeWrite(_ context.Context, w *WriteInfo) error {
	*p.log = append(*p.log, p.name+":write "+w.Action.String())
	return w.Set("name", p.name)
}

type nameOnly struct{}

func (nameOnly) Name() string { return "empty" }

func TestPluginOrderAndAbort(t *testing.T) {
	var log []string
	var stmts int
	sqldb := sql.OpenDB(fakeConnector{&fakeDriver{rows: 1}})
	defer sqldb.Close()
	db := Open(sqldb, dialect.Postgres(),
		Use(orderPlugin{name: "first", log: &log}),
		Use(orderPlugin{name: "second", log: &log}),
		WithHook(func(context.Context, Event) { stmts++ }),
	)
	ctx := context.Background()
	users := For[benchUser](db)

	if _, err := users.FindByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	u := benchUser{Name: "x"}
	if err := users.Insert(ctx, &u); err != nil {
		t.Fatal(err)
	}
	if u.Name != "second" { // both ran, in order; the last one wins
		t.Fatalf("name = %q", u.Name)
	}
	want := "first:select bench_users,second:select bench_users,first:write insert,second:write insert"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
	if stmts != 2 {
		t.Fatalf("WithHook saw %d statements, want 2", stmts)
	}

	boom := errors.New("boom")
	db2 := Open(sqldb, dialect.Postgres(), Use(orderPlugin{name: "a", log: &log, err: boom}), WithHook(func(context.Context, Event) { stmts++ }))
	stmts = 0
	if _, err := For[benchUser](db2).FindAll(ctx); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, err := For[benchUser](db2).UpdateAny(ctx, q.Eq("id", 1), q.Set("age", 1)); !errors.Is(err, boom) {
		t.Fatalf("UpdateAny err = %v, want boom", err)
	}
	if stmts != 0 {
		t.Fatalf("aborted operations ran %d statements", stmts)
	}
	// Raw SQL skips BeforeQuery.
	var n int
	if err := db2.Query(ctx, &n, "SELECT 1"); errors.Is(err, boom) {
		t.Fatal("raw SQL went through BeforeQuery")
	}
}

func TestUsePanicsWithoutHooks(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), `"empty"`) {
			t.Fatalf("recover = %v", r)
		}
	}()
	Open(nil, dialect.Postgres(), Use(nameOnly{}))
}

func TestWriteInfoSet(t *testing.T) {
	type rec struct {
		ID    int64
		Org   int64
		Label string
		Note  *string
	}
	r := rec{}
	v := reflect.ValueOf(&r).Elem()
	m := For[rec](Open(nil, dialect.Postgres())).m
	w := &WriteInfo{m: m, v: v}

	for _, tc := range []struct {
		col  string
		val  any
		fail bool
	}{
		{"org", 7, false},     // int -> int64
		{"label", "x", false}, // same type
		{"note", "hi", false}, // value into pointer field
		{"org", "7", true},    // string -> int64: refused
		{"label", 65, true},   // int -> string would be "A": refused
		{"missing", 1, true},  // unknown column
		{"note", nil, false},  // nil clears
	} {
		err := w.Set(tc.col, tc.val)
		if (err != nil) != tc.fail {
			t.Fatalf("Set(%q, %#v): err = %v, want fail=%v", tc.col, tc.val, err, tc.fail)
		}
	}
	if r.Org != 7 || r.Label != "x" || r.Note != nil {
		t.Fatalf("record = %+v", r)
	}
	if got, ok := w.Get("org"); !ok || got != int64(7) {
		t.Fatalf("Get(org) = %v, %v", got, ok)
	}
	if _, ok := w.Get("missing"); ok {
		t.Fatal("Get(missing) ok")
	}
}
