package kilnbreaker

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newTest(opts ...Option) (*Breaker, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	b := New(opts...)
	b.now = c.now
	return b, c
}

var (
	ctx        = context.Background()
	errDown    = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	errSQL     = errors.New(`pq: syntax error at or near "SELEC"`)
	errTimeout = fmt.Errorf("query: %w", context.DeadlineExceeded)
)

// run does one guarded round trip with the given outcome.
func run(b *Breaker, err error) error {
	if e := b.Allow(ctx); e != nil {
		return e
	}
	b.Done(ctx, err)
	return nil
}

func TestLifecycle(t *testing.T) {
	var changes []string
	b, c := newTest(Failures(3, 10*time.Second), Cooldown(30*time.Second), OnChange(func(from, to State) {
		changes = append(changes, from.String()+"->"+to.String())
	}))

	// Ordinary query errors never count.
	for i := 0; i < 10; i++ {
		_ = run(b, errSQL)
	}
	// Failures spread over more than the window don't open it.
	_ = run(b, errDown)
	_ = run(b, errDown)
	c.advance(11 * time.Second)
	_ = run(b, errDown)
	if b.State() != Closed {
		t.Fatalf("state %v after spread failures", b.State())
	}
	// Three within the window do.
	_ = run(b, errTimeout)
	_ = run(b, errDown)
	if b.State() != Open {
		t.Fatalf("state %v, want open", b.State())
	}
	if err := run(b, nil); !errors.Is(err, ErrOpen) {
		t.Fatalf("open breaker allowed a query: %v", err)
	}

	// After the cooldown: one trial; others wait; a failed trial reopens.
	c.advance(30 * time.Second)
	if b.State() != HalfOpen {
		t.Fatalf("after cooldown: %v", b.State())
	}
	if err := b.Allow(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Allow(ctx); !errors.Is(err, ErrOpen) {
		t.Fatalf("second trial allowed: %v", err)
	}
	b.Done(ctx, errDown)
	if b.State() != Open {
		t.Fatalf("failed trial: %v", b.State())
	}

	// A successful trial closes it.
	c.advance(30 * time.Second)
	if err := run(b, nil); err != nil {
		t.Fatal(err)
	}
	if b.State() != Closed {
		t.Fatalf("after good trial: %v", b.State())
	}
	want := "closed->open open->half-open half-open->open open->half-open half-open->closed"
	if got := fmt.Sprint(changes); got != "["+want+"]" {
		t.Fatalf("changes %v\nwant [%s]", got, want)
	}
	// The window starts fresh after closing.
	_ = run(b, errDown)
	_ = run(b, errDown)
	if b.State() != Closed {
		t.Fatal("old failures counted after closing")
	}
}

func TestTrialsAndOrdinaryErrors(t *testing.T) {
	b, c := newTest(Failures(1, time.Second), Cooldown(time.Second), Trials(2))
	_ = run(b, errDown)
	c.advance(time.Second)
	// Two trials may run at once; both must succeed. A constraint error
	// means the database answered: it counts as a success.
	first, second, third := b.Allow(ctx), b.Allow(ctx), b.Allow(ctx)
	if first != nil || second != nil || third == nil {
		t.Fatal("want exactly two trials")
	}
	b.Done(ctx, errors.New("duplicate key value violates unique constraint"))
	if b.State() != HalfOpen {
		t.Fatal("closed after one of two trials")
	}
	b.Done(ctx, nil)
	if b.State() != Closed {
		t.Fatalf("state %v", b.State())
	}
}

func TestCancelledNotCounted(t *testing.T) {
	b, _ := newTest(Failures(1, time.Second))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_ = b.Allow(cctx)
	b.Done(cctx, fmt.Errorf("read: %w", context.Canceled))
	_ = b.Allow(cctx)
	b.Done(cctx, errDown) // the caller gave up; whatever the driver said
	if b.State() != Closed {
		t.Fatal("cancelled queries opened the breaker")
	}
}

func TestDefaultIsFailure(t *testing.T) {
	for _, err := range []error{errDown, errTimeout, driver.ErrBadConn, errors.New("invalid connection"),
		errors.New("pq: the database system is starting up"), errors.New("Error 1040: Too many connections")} {
		if !DefaultIsFailure(err) {
			t.Errorf("not a failure: %v", err)
		}
	}
	for _, err := range []error{errSQL, context.Canceled, errors.New("sql: no rows in result set"), errors.New("duplicate key")} {
		if DefaultIsFailure(err) {
			t.Errorf("counted: %v", err)
		}
	}
}

func TestConcurrent(t *testing.T) {
	b := New(Failures(50, time.Minute), Cooldown(time.Millisecond), Trials(3))
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				var err error
				if (i+g)%3 == 0 {
					err = errDown
				}
				_ = run(b, err)
				_ = b.State()
			}
		}(g)
	}
	wg.Wait()
	// Recovers once the database is healthy again.
	time.Sleep(2 * time.Millisecond)
	for i := 0; i < 5; i++ {
		_ = run(b, nil)
	}
	if b.State() != Closed {
		t.Fatalf("did not recover: %v", b.State())
	}
}

func BenchmarkClosed(b *testing.B) {
	br := New()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = br.Allow(ctx)
		br.Done(ctx, nil)
	}
}

func BenchmarkOpen(b *testing.B) {
	br := New(Failures(1, time.Second), Cooldown(time.Hour))
	_ = run(br, errDown)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = br.Allow(ctx)
	}
}
