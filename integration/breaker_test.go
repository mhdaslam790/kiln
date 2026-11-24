package integration

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/kilnbreaker"
	"github.com/mhdaslam790/kiln/orm"
)

// faultProxy forwards TCP to a database and can be switched to refuse
// connections (down) or to swallow traffic (hang), like a crashed or
// overloaded server.
type faultProxy struct {
	t        *testing.T
	addr     string // what clients dial
	upstream string
	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]bool
	hang     atomic.Bool
}

func newFaultProxy(t *testing.T, upstream string) *faultProxy {
	p := &faultProxy{t: t, upstream: upstream, conns: map[net.Conn]bool{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.down)
	return p
}

func (p *faultProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.track(c, true)
			if p.hang.Load() {
				continue // accept, then never answer
			}
			go p.forward(c)
		}
	}()
}

func (p *faultProxy) forward(c net.Conn) {
	u, err := net.Dial("tcp", p.upstream)
	if err != nil {
		c.Close()
		return
	}
	p.track(u, true)
	go func() { _, _ = io.Copy(u, c); u.Close() }()
	_, _ = io.Copy(c, u)
	c.Close()
}

func (p *faultProxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = true
	} else {
		delete(p.conns, c)
	}
}

func (p *faultProxy) cutAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
		delete(p.conns, c)
	}
}

// down closes every connection and refuses new ones.
func (p *faultProxy) down() {
	p.mu.Lock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	p.mu.Unlock()
	p.cutAll()
}

// up accepts and forwards again, on the same address.
func (p *faultProxy) up() {
	p.hang.Store(false)
	p.mu.Lock()
	running := p.ln != nil
	p.mu.Unlock()
	if running {
		return
	}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port can take a moment to free up
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	must(p.t, err)
	p.serve(ln)
}

// stall drops existing connections and swallows new ones without answering.
func (p *faultProxy) stall() {
	p.hang.Store(true)
	p.cutAll()
}

var hostPort = regexp.MustCompile(`(localhost|127\.0\.0\.1):(\d+)`)

func TestBreaker(t *testing.T) {
	for _, tg := range targets {
		if tg.dsnEnv == "" {
			continue // SQLite has no server to lose
		}
		t.Run(tg.name, func(t *testing.T) {
			dsn := os.Getenv(tg.dsnEnv)
			if dsn == "" {
				t.Skipf("set %s", tg.dsnEnv)
			}
			upstream := hostPort.FindString(dsn)
			if upstream == "" {
				t.Skipf("no localhost:port in %s", tg.dsnEnv)
			}
			proxy := newFaultProxy(t, upstream)
			sqldb, err := sql.Open(tg.driver, hostPort.ReplaceAllString(dsn, proxy.addr))
			must(t, err)
			t.Cleanup(func() { sqldb.Close() })
			sqldb.SetConnMaxIdleTime(time.Minute)

			var changes []string
			var mu sync.Mutex
			breaker := kilnbreaker.New(kilnbreaker.Failures(3, time.Minute), kilnbreaker.Cooldown(300*time.Millisecond),
				kilnbreaker.OnChange(func(from, to kilnbreaker.State) {
					mu.Lock()
					changes = append(changes, to.String())
					mu.Unlock()
				}))
			db := orm.Open(sqldb, tg.d, orm.Use(breaker), orm.WithQueryTimeout(500*time.Millisecond))
			ctx := context.Background()
			ping := func() error {
				var n int64
				return db.Query(ctx, &n, "SELECT 1")
			}
			must(t, ping())
			must(t, db.Transaction(ctx, func(ctx context.Context) error {
				var n int64
				return db.Query(ctx, &n, "SELECT 2")
			}))

			// Ordinary errors don't trip it.
			for i := 0; i < 5; i++ {
				if _, err := db.Exec(ctx, "SELECT * FROM no_such_table_kiln"); err == nil {
					t.Fatal("bad query succeeded")
				}
			}
			if breaker.State() != kilnbreaker.Closed {
				t.Fatalf("ordinary errors opened the breaker")
			}

			// The database goes away: a few failures, then fast ErrOpen.
			proxy.down()
			attempts := 0
			for ; attempts < 10 && breaker.State() == kilnbreaker.Closed; attempts++ {
				if err := ping(); err == nil {
					t.Fatal("query succeeded with the database down")
				}
			}
			if breaker.State() != kilnbreaker.Open || attempts > 3 {
				t.Fatalf("state %v after %d failures", breaker.State(), attempts)
			}
			start := time.Now()
			for i := 0; i < 100; i++ {
				if err := ping(); !errors.Is(err, kilnbreaker.ErrOpen) {
					t.Fatalf("open breaker: %v", err)
				}
			}
			if d := time.Since(start); d > 100*time.Millisecond {
				t.Fatalf("100 refused queries took %v: not failing fast", d)
			}
			if err := db.Transaction(ctx, func(context.Context) error { return nil }); !errors.Is(err, kilnbreaker.ErrOpen) {
				t.Fatalf("transaction while open: %v", err)
			}

			// Still down after the cooldown: the trial fails and it reopens.
			time.Sleep(350 * time.Millisecond)
			if err := ping(); err == nil || errors.Is(err, kilnbreaker.ErrOpen) {
				t.Fatalf("trial query: %v", err)
			}
			if breaker.State() != kilnbreaker.Open {
				t.Fatalf("failed trial left %v", breaker.State())
			}

			// The database is back: after the cooldown one trial closes it.
			proxy.up()
			time.Sleep(350 * time.Millisecond)
			must(t, ping())
			if breaker.State() != kilnbreaker.Closed {
				t.Fatalf("after recovery: %v", breaker.State())
			}

			// A database that hangs: statements time out, which counts too.
			proxy.stall()
			for i := 0; i < 3; i++ {
				if err := ping(); !errors.Is(err, context.DeadlineExceeded) && !kilnbreaker.DefaultIsFailure(err) {
					t.Fatalf("stalled query: %v", err)
				}
			}
			if breaker.State() != kilnbreaker.Open {
				t.Fatalf("after timeouts: %v", breaker.State())
			}
			// Without the breaker each of these would wait the 500ms timeout.
			start = time.Now()
			for i := 0; i < 20; i++ {
				if err := ping(); !errors.Is(err, kilnbreaker.ErrOpen) {
					t.Fatalf("open breaker on a hung database: %v", err)
				}
			}
			if d := time.Since(start); d > 50*time.Millisecond {
				t.Fatalf("20 queries against a hung database took %v (10s without the breaker)", d)
			}
			proxy.up()
			time.Sleep(350 * time.Millisecond)
			must(t, ping())

			mu.Lock()
			defer mu.Unlock()
			want := "[open half-open open half-open closed open half-open closed]"
			if got := fmtList(changes); got != want {
				t.Fatalf("state changes %s, want %s", got, want)
			}
		})
	}
}

func fmtList(s []string) string { return "[" + join(s) + "]" }
