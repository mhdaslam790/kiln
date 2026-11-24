// Package kilnrw gives read-your-writes consistency with read replicas:
// after a user writes, their reads go to the primary for a short window,
// so they never see data older than their own change because a replica
// is a few milliseconds behind. Everyone else keeps reading from replicas.
//
//	rw := kilnrw.New(kilnrw.Key(func(ctx context.Context) string { return userID(ctx) }))
//	db := orm.Open(primary, dialect.Postgres(), orm.WithReplicas(replica), orm.Use(rw))
//	handler = kilnrw.Middleware(handler)
//
// Two ways to know "this user just wrote", use one or both:
//
//   - Within one request: Middleware (or Track) puts a tracker in the
//     context; after a write, later reads in the same request go to the
//     primary. Nothing else to configure.
//   - Across requests (save, then redirect and reload): Key names who is
//     asking (usually the user or session id). Writes are remembered per
//     key in a Store: by default in memory, which works for one instance;
//     with several instances behind a load balancer, give a shared Store
//     (e.g. Redis), see Store.
//
// Window should be longer than your worst normal replica lag.
package kilnrw

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhdaslam790/kiln/orm"
)

// Store remembers, per key, until when reads must go to the primary.
// Implementations must be safe for concurrent use. Errors are not
// returned: a store that cannot answer should report a zero time (read
// from a replica) and log on its own.
type Store interface {
	// Mark records that reads for key go to the primary until the given time.
	Mark(ctx context.Context, key string, until time.Time)
	// Until returns the time recorded for key, or the zero time.
	Until(ctx context.Context, key string) time.Time
}

// Option configures New.
type Option func(*RW)

// Window is how long after a write reads go to the primary (default 2s).
func Window(d time.Duration) Option { return func(r *RW) { r.window = d } }

// Key returns who is asking, e.g. the user id from the context; "" means
// unknown (only the per-request tracker applies).
func Key(fn func(ctx context.Context) string) Option { return func(r *RW) { r.key = fn } }

// WithStore replaces the in-memory store (see Store).
func WithStore(s Store) Option { return func(r *RW) { r.store = s } }

// RW is the read-your-writes plugin: pass it to orm.Use.
type RW struct {
	window time.Duration
	key    func(context.Context) string
	store  Store
	now    func() time.Time
}

// New returns the plugin.
func New(opts ...Option) *RW {
	r := &RW{window: 2 * time.Second, now: time.Now}
	for _, o := range opts {
		o(r)
	}
	if r.key != nil && r.store == nil {
		r.store = NewMemoryStore()
	}
	return r
}

// Name implements orm.Plugin.
func (r *RW) Name() string { return "kilnrw" }

// Wrote implements orm.RoutePlugin.
func (r *RW) Wrote(ctx context.Context) {
	until := r.now().Add(r.window)
	if t, ok := ctx.Value(trackerKey{}).(*tracker); ok {
		t.until.Store(until.UnixNano())
	}
	if r.key != nil {
		if k := r.key(ctx); k != "" {
			r.store.Mark(ctx, k, until)
		}
	}
}

// ReadPrimary implements orm.RoutePlugin.
func (r *RW) ReadPrimary(ctx context.Context) bool {
	now := r.now()
	if t, ok := ctx.Value(trackerKey{}).(*tracker); ok && now.UnixNano() < t.until.Load() {
		return true
	}
	if r.key != nil {
		if k := r.key(ctx); k != "" {
			return now.Before(r.store.Until(ctx, k))
		}
	}
	return false
}

type trackerKey struct{}

type tracker struct{ until atomic.Int64 } // unix nanos

// Track returns a context that remembers writes made with it (or contexts
// derived from it), so that later reads with it go to the primary. Use it
// once per request or job; Middleware does it for HTTP handlers.
func Track(ctx context.Context) context.Context {
	if _, ok := ctx.Value(trackerKey{}).(*tracker); ok {
		return ctx
	}
	return context.WithValue(ctx, trackerKey{}, &tracker{})
}

// Middleware calls Track for every request.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		next.ServeHTTP(w, req.WithContext(Track(req.Context())))
	})
}

// MemoryStore is the default Store: a map with expiry, for one instance.
// Expired entries are removed as new ones are added, so it holds at most
// the keys that wrote within the window. It starts no goroutines.
type MemoryStore struct {
	mu      sync.Mutex
	m       map[string]time.Time
	now     func() time.Time
	nextGC  time.Time
	gcEvery time.Duration
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: map[string]time.Time{}, now: time.Now, gcEvery: time.Second}
}

// Mark implements Store.
func (s *MemoryStore) Mark(_ context.Context, key string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.After(s.nextGC) {
		for k, t := range s.m {
			if !now.Before(t) {
				delete(s.m, k)
			}
		}
		s.nextGC = now.Add(s.gcEvery)
	}
	if until.After(s.m[key]) {
		s.m[key] = until
	}
}

// Until implements Store.
func (s *MemoryStore) Until(_ context.Context, key string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key]
}

// Len returns the number of keys held (expired ones may not be removed yet).
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

var _ orm.RoutePlugin = (*RW)(nil)
