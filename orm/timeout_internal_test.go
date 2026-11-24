package orm

import (
	"context"
	"testing"
	"time"
)

func TestStmtCtx(t *testing.T) {
	bg := context.Background()

	off := &DB{}
	if ctx, _ := off.stmtCtx(bg); ctx != bg {
		t.Fatal("no timeout must return ctx unchanged")
	}
	if n := testing.AllocsPerRun(100, func() { _, c := off.stmtCtx(bg); c() }); n != 0 {
		t.Fatalf("no timeout allocates %v", n)
	}

	on := &DB{timeout: time.Second}
	ctx, cancel := on.stmtCtx(bg)
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > time.Second {
		t.Fatalf("deadline = %v, %v", dl, ok)
	}
	cancel()
	if ctx.Err() == nil {
		t.Fatal("cancel must release the timer")
	}

	// An earlier context deadline wins and is not wrapped.
	short, c := context.WithTimeout(bg, 10*time.Millisecond)
	defer c()
	if got, _ := on.stmtCtx(short); got != short {
		t.Fatal("earlier ctx deadline must be kept as is")
	}
	// A later one is shortened.
	long, c2 := context.WithTimeout(bg, time.Hour)
	defer c2()
	got, c3 := on.stmtCtx(long)
	defer c3()
	if dl, _ := got.Deadline(); time.Until(dl) > time.Second {
		t.Fatal("query timeout must cap a later ctx deadline")
	}
}
