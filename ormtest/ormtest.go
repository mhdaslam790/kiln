// Package ormtest provides a mock orm.Repository[T] for unit tests.
//
//	func TestSignup(t *testing.T) {
//		users := ormtest.New[User](t)
//		users.ExpectExists(q.Eq("email", "a@b.c")).Return(false)
//		users.ExpectInsert().Run(func(u *User) { u.ID = 1 })
//
//		svc := NewService(users, ormtest.Tx{})
//		...
//	}
//
// Expectations are checked automatically when the test ends. Queries are
// matched by the query they build, so q.Eq("a", 1), q.Sort("-id") etc.
// must match exactly; pass ormtest.Any to accept any query.
package ormtest

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// ErrUnexpected is returned by a mock call that matched no expectation.
var ErrUnexpected = errors.New("ormtest: unexpected call")

type anyQuerier struct{}

func (anyQuerier) Build(*q.Query) {}

// Any matches any query when passed to an Expect method.
var Any q.Querier = anyQuerier{}

// Repo is a mock repository implementing orm.Repository[T].
type Repo[T any] struct {
	t    testing.TB
	mu   sync.Mutex
	exps []*Expectation[T]
}

var _ orm.Repository[struct{}] = (*Repo[struct{}])(nil)

// New returns a mock whose expectations are verified at test cleanup.
func New[T any](t testing.TB) *Repo[T] {
	r := &Repo[T]{t: t}
	t.Cleanup(r.AssertExpectations)
	return r
}

// Expectation describes one expected call.
type Expectation[T any] struct {
	method   string
	desc     string
	query    *q.Query // nil: any
	args     []any    // extra exact-match arguments (id, fields, ...); nil: any
	match    func(rec *T) bool
	ret      any
	err      error
	run      func(rec *T)
	runAll   func(recs []T)
	times    int // <0: unlimited
	calls    int
	optional bool
}

// Return sets the value returned by the call (T, []T, int64, bool, Page...).
func (e *Expectation[T]) Return(v any) *Expectation[T] { e.ret = v; return e }

// Error makes the call fail with err.
func (e *Expectation[T]) Error(err error) *Expectation[T] { e.err = err; return e }

// NotFound makes the call fail with orm.ErrNotFound.
func (e *Expectation[T]) NotFound() *Expectation[T] { return e.Error(orm.ErrNotFound) }

// Run is called with the record of write calls (Insert, Update, Save,
// Delete, Preload...), e.g. to set an ID.
func (e *Expectation[T]) Run(fn func(rec *T)) *Expectation[T] { e.run = fn; return e }

// RunAll is called with the records of InsertAll / PreloadAll.
func (e *Expectation[T]) RunAll(fn func(recs []T)) *Expectation[T] { e.runAll = fn; return e }

// Match restricts a write expectation to records accepted by fn.
func (e *Expectation[T]) Match(fn func(rec *T) bool) *Expectation[T] { e.match = fn; return e }

// Times expects exactly n calls (default 1).
func (e *Expectation[T]) Times(n int) *Expectation[T] { e.times = n; return e }

// Always allows any number of calls, including none.
func (e *Expectation[T]) Always() *Expectation[T] { e.times = -1; e.optional = true; return e }

// Maybe makes the expectation optional.
func (e *Expectation[T]) Maybe() *Expectation[T] { e.optional = true; return e }

func buildQuery(qs []q.Querier) *q.Query {
	for _, x := range qs {
		if _, ok := x.(anyQuerier); ok {
			return nil
		}
	}
	qq := q.Apply(q.Query{}, qs...)
	return &qq
}

func describe(qq *q.Query) string {
	if qq == nil {
		return "any query"
	}
	c := *qq
	if c.Table == "" {
		c.Table = "_"
	}
	st, err := dialect.Postgres().Select(c)
	if err != nil {
		return fmt.Sprintf("%+v", *qq)
	}
	s := strings.TrimPrefix(st.SQL, `SELECT * FROM "_"`)
	if s == "" {
		s = " (no conditions)"
	}
	if len(st.Args) > 0 {
		s += fmt.Sprintf(" %v", st.Args)
	}
	return strings.TrimSpace(s)
}

func (r *Repo[T]) expect(method string, qs []q.Querier, args []any) *Expectation[T] {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := &Expectation[T]{method: method, args: args, times: 1}
	if qs != nil {
		e.query = buildQuery(qs)
	}
	e.desc = method
	if e.query != nil || qs != nil {
		e.desc += " " + describe(e.query)
	}
	if args != nil {
		e.desc += fmt.Sprintf(" %v", args)
	}
	r.exps = append(r.exps, e)
	return e
}

// call finds and consumes the first matching expectation.
func (r *Repo[T]) call(method string, qs []q.Querier, hasQuery bool, args []any, rec *T) (*Expectation[T], error) {
	r.t.Helper()
	var built *q.Query
	if hasQuery {
		built = buildQuery(qs)
	}
	r.mu.Lock()
	var found *Expectation[T]
	// Two passes: specific expectations (query / args / matcher) win over
	// catch-all ones, regardless of declaration order.
	for pass := 0; pass < 2 && found == nil; pass++ {
		for _, e := range r.exps {
			if e.method != method || (e.times >= 0 && e.calls >= e.times) {
				continue
			}
			specific := e.query != nil || e.args != nil || e.match != nil
			if pass == 0 && !specific {
				continue
			}
			if hasQuery && e.query != nil && !reflect.DeepEqual(*e.query, *built) {
				continue
			}
			if e.args != nil && !reflect.DeepEqual(e.args, args) {
				continue
			}
			if e.match != nil && rec != nil && !e.match(rec) {
				continue
			}
			found = e
			break
		}
	}
	if found != nil {
		found.calls++
	}
	r.mu.Unlock()

	if found == nil {
		msg := method
		if hasQuery {
			msg += " " + describe(built)
		}
		if args != nil {
			msg += fmt.Sprintf(" %v", args)
		}
		r.t.Errorf("ormtest: unexpected call %s\n  expectations:\n%s", msg, r.pending())
		return nil, fmt.Errorf("%w: %s", ErrUnexpected, msg)
	}
	if found.run != nil && rec != nil {
		found.run(rec)
	}
	return found, found.err
}

func (r *Repo[T]) pending() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.exps {
		fmt.Fprintf(&b, "    %s (called %d/%s)\n", e.desc, e.calls, timesStr(e.times))
	}
	if b.Len() == 0 {
		return "    (none)\n"
	}
	return b.String()
}

func timesStr(n int) string {
	if n < 0 {
		return "any"
	}
	return fmt.Sprint(n)
}

// AssertExpectations fails the test for expectations that were not met.
// It runs automatically at cleanup.
func (r *Repo[T]) AssertExpectations() {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.exps {
		if e.optional || e.times < 0 {
			continue
		}
		if e.calls != e.times {
			r.t.Errorf("ormtest: expected %s to be called %d time(s), got %d", e.desc, e.times, e.calls)
		}
	}
}

// result converts an expectation's return value to V.
func result[T, V any](t testing.TB, e *Expectation[T]) V {
	var zero V
	if e == nil || e.ret == nil {
		return zero
	}
	if v, ok := e.ret.(V); ok {
		return v
	}
	rv := reflect.ValueOf(e.ret)
	if rv.Type().ConvertibleTo(reflect.TypeOf(zero)) {
		return rv.Convert(reflect.TypeOf(zero)).Interface().(V)
	}
	t.Errorf("ormtest: %s: Return(%T) but the method returns %T", e.desc, e.ret, zero)
	return zero
}
