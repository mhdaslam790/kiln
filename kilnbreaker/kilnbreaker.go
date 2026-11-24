// Package kilnbreaker is a circuit breaker for kiln: when the database is
// down or not answering, queries fail at once with ErrOpen instead of each
// one waiting for its timeout, so requests don't pile up, the connection
// pool isn't exhausted, and the database isn't flooded when it comes back.
//
//	breaker := kilnbreaker.New(kilnbreaker.Failures(5, 10*time.Second))
//	db := orm.Open(sqlDB, dialect.Postgres(), orm.Use(breaker))
//
//	_, err := users.FindByID(ctx, id)
//	if errors.Is(err, kilnbreaker.ErrOpen) {
//		http.Error(w, "try again shortly", http.StatusServiceUnavailable)
//	}
//
// The breaker is closed (queries run) until it sees Failures within the
// window; then it opens (queries fail with ErrOpen) for the cooldown; then
// it is half-open: a few trial queries run, and it closes once they
// succeed or opens again on a failure.
//
// Only infrastructure failures count (see IsFailure): lost or refused
// connections and timeouts. Query errors such as ErrNotFound, constraint
// violations or syntax errors don't, and neither do queries the caller
// cancelled. Cache hits never touch the database and are always served.
//
// One breaker guards one orm.DB, including its read replicas.
//
// The breaker can only count queries that return. Set connect timeouts in
// your DSN: some drivers ignore the context while opening a connection,
// and a server that accepts connections but never answers then blocks
// callers forever. SQL Server (go-mssqldb) needs "dial timeout" and
// "connection timeout" (seconds; the login timeout defaults to none); for
// PostgreSQL and MySQL the context is honoured, "connect_timeout" and
// "timeout" add a bound of their own.
package kilnbreaker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhdaslam790/kiln/orm"
)

// ErrOpen is returned (wrapped) for queries refused while the breaker is
// open.
var ErrOpen = errors.New("kilnbreaker: circuit open: database unavailable")

// State is the breaker's state.
type State int32

const (
	Closed   State = iota // queries run
	Open                  // queries fail with ErrOpen
	HalfOpen              // trial queries run
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	default:
		return "half-open"
	}
}

// Option configures a Breaker.
type Option func(*Breaker)

// Failures opens the breaker after n failures within window (default 5
// within 10s).
func Failures(n int, window time.Duration) Option {
	return func(b *Breaker) { b.threshold, b.window = n, window }
}

// Cooldown is how long the breaker stays open before trying again
// (default 30s).
func Cooldown(d time.Duration) Option { return func(b *Breaker) { b.cooldown = d } }

// Trials is how many queries may run while half-open, and how many must
// succeed to close the breaker (default 1).
func Trials(n int) Option { return func(b *Breaker) { b.trials = n } }

// IsFailure replaces the function that decides which errors count as
// failures (default: DefaultIsFailure).
func IsFailure(fn func(error) bool) Option { return func(b *Breaker) { b.isFailure = fn } }

// OnChange is called after every state change, e.g. to log or alert. It
// runs synchronously, outside the breaker's lock; keep it fast.
func OnChange(fn func(from, to State)) Option { return func(b *Breaker) { b.onChange = fn } }

// Breaker is a circuit breaker. It is an orm plugin: pass it to orm.Use.
// It is safe for concurrent use.
type Breaker struct {
	threshold int
	window    time.Duration
	cooldown  time.Duration
	trials    int
	isFailure func(error) bool
	onChange  func(from, to State)
	now       func() time.Time

	state     atomic.Int32 // State; read without the lock on the fast path
	openUntil atomic.Int64 // unix nanos; while Open

	mu          sync.Mutex
	failures    int       // in the current window (Closed)
	windowStart time.Time // (Closed)
	inflight    int       // trial queries running (HalfOpen)
	succeeded   int       // trial queries that succeeded (HalfOpen)
}

// New returns a closed Breaker.
func New(opts ...Option) *Breaker {
	b := &Breaker{threshold: 5, window: 10 * time.Second, cooldown: 30 * time.Second, trials: 1, isFailure: DefaultIsFailure, now: time.Now}
	for _, o := range opts {
		o(b)
	}
	if b.threshold < 1 {
		b.threshold = 1
	}
	if b.trials < 1 {
		b.trials = 1
	}
	return b
}

// Plugin is a shortcut for orm.Use(New(opts...)).
func Plugin(opts ...Option) orm.Option { return orm.Use(New(opts...)) }

// Name implements orm.Plugin.
func (b *Breaker) Name() string { return "kilnbreaker" }

// State returns the current state, e.g. for a health check. An open
// breaker whose cooldown has passed reports HalfOpen.
func (b *Breaker) State() State {
	s := State(b.state.Load())
	if s == Open && b.now().UnixNano() >= b.openUntil.Load() {
		return HalfOpen
	}
	return s
}

// Allow implements orm.GuardPlugin.
func (b *Breaker) Allow(context.Context) error {
	switch State(b.state.Load()) {
	case Closed:
		return nil // fast path: no lock
	case Open:
		if b.now().UnixNano() < b.openUntil.Load() {
			return ErrOpen
		}
	}
	b.mu.Lock()
	var changed bool
	from := State(b.state.Load())
	switch from {
	case Closed:
		b.mu.Unlock()
		return nil
	case Open:
		if b.now().UnixNano() < b.openUntil.Load() {
			b.mu.Unlock()
			return ErrOpen
		}
		b.set(HalfOpen)
		b.inflight, b.succeeded = 0, 0
		changed = true
	}
	ok := b.inflight < b.trials
	if ok {
		b.inflight++
	}
	b.mu.Unlock()
	if changed {
		b.notify(from, HalfOpen)
	}
	if !ok {
		return ErrOpen
	}
	return nil
}

// Done implements orm.GuardPlugin.
func (b *Breaker) Done(ctx context.Context, err error) {
	failed := err != nil && !errors.Is(err, ErrOpen) && ctx.Err() != context.Canceled && b.isFailure(err)
	s := State(b.state.Load())
	if s == Closed && !failed {
		return // fast path: no lock
	}
	b.mu.Lock()
	from := State(b.state.Load())
	to := from
	switch from {
	case Closed:
		now := b.now()
		if now.Sub(b.windowStart) > b.window {
			b.windowStart, b.failures = now, 0
		}
		b.failures++
		if b.failures >= b.threshold {
			to = b.open(now)
		}
	case HalfOpen:
		if b.inflight > 0 {
			b.inflight--
		}
		if failed {
			to = b.open(b.now())
		} else { // answered (also with an ordinary query error): the database is back
			b.succeeded++
			if b.succeeded >= b.trials {
				b.set(Closed)
				b.failures, b.windowStart = 0, time.Time{}
				to = Closed
			}
		}
	case Open:
		// A query allowed before the breaker opened finished: nothing to do.
	}
	b.mu.Unlock()
	if to != from {
		b.notify(from, to)
	}
}

// open moves to Open; b.mu is held.
func (b *Breaker) open(now time.Time) State {
	b.openUntil.Store(now.Add(b.cooldown).UnixNano())
	b.set(Open)
	return Open
}

func (b *Breaker) set(s State) { b.state.Store(int32(s)) }

func (b *Breaker) notify(from, to State) {
	if b.onChange != nil {
		b.onChange(from, to)
	}
}

// DefaultIsFailure reports whether err means the database is unreachable
// or not answering: network errors, broken or refused connections,
// timeouts (context.DeadlineExceeded) and the "too many connections",
// "starting up" or "shutting down" errors of the supported databases.
func DefaultIsFailure(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range failureMessages {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// failureMessages are errors drivers return as plain strings or their own
// types (lib/pq, pgx, go-sql-driver/mysql, go-mssqldb), lower case.
var failureMessages = []string{
	"connection refused",
	"connection reset",
	"broken pipe",
	"bad connection",
	"invalid connection",                   // mysql
	"too many connections",                 // mysql 1040, postgres 53300
	"the database system is starting up",   // postgres 57P03
	"the database system is shutting down", // postgres 57P03
	"terminating connection",               // postgres 57P01
	"server closed the connection",
	"unexpected eof",
	"i/o timeout",
}
