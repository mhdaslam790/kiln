package kilnrw

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type userKey struct{}

func withUser(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

func setup(opts ...Option) (*RW, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(opts...)
	r.now = c.now
	if m, ok := r.store.(*MemoryStore); ok {
		m.now = c.now
	}
	return r, c
}

func TestTracker(t *testing.T) {
	r, c := setup(Window(time.Second))
	bg := context.Background()
	if r.ReadPrimary(bg) {
		t.Fatal("no write yet")
	}
	r.Wrote(bg) // untracked, no key: no effect
	if r.ReadPrimary(bg) {
		t.Fatal("untracked context")
	}
	ctx := Track(bg)
	if Track(ctx) != ctx {
		t.Fatal("Track twice made a new tracker")
	}
	if r.ReadPrimary(ctx) {
		t.Fatal("tracked, no write")
	}
	r.Wrote(withUser(ctx, "x")) // a derived context shares the tracker
	if !r.ReadPrimary(ctx) {
		t.Fatal("read after write went to a replica")
	}
	if r.ReadPrimary(Track(bg)) {
		t.Fatal("another request was affected")
	}
	c.add(time.Second)
	if r.ReadPrimary(ctx) {
		t.Fatal("window did not end")
	}
}

func TestKey(t *testing.T) {
	r, c := setup(Window(2*time.Second), Key(func(ctx context.Context) string { s, _ := ctx.Value(userKey{}).(string); return s }))
	ann, bob := withUser(context.Background(), "ann"), withUser(context.Background(), "bob")
	r.Wrote(ann)                                               // request 1
	if !r.ReadPrimary(withUser(context.Background(), "ann")) { // request 2, new context
		t.Fatal("next request of the same user read a replica")
	}
	if r.ReadPrimary(bob) || r.ReadPrimary(context.Background()) {
		t.Fatal("other users affected")
	}
	c.add(2 * time.Second)
	if r.ReadPrimary(ann) {
		t.Fatal("window did not end")
	}
}

func TestMemoryStoreBounded(t *testing.T) {
	r, c := setup(Window(time.Second), Key(func(ctx context.Context) string { s, _ := ctx.Value(userKey{}).(string); return s }))
	s := r.store.(*MemoryStore)
	for i := 0; i < 1000; i++ {
		r.Wrote(withUser(context.Background(), fmt.Sprint(i)))
	}
	if s.Len() != 1000 {
		t.Fatalf("len %d", s.Len())
	}
	c.add(2 * time.Second)
	r.Wrote(withUser(context.Background(), "late"))
	if s.Len() != 1 {
		t.Fatalf("expired keys kept: %d", s.Len())
	}
	// An earlier Mark never shortens a later one.
	s.Mark(context.Background(), "late", time.Unix(0, 0))
	if !r.ReadPrimary(withUser(context.Background(), "late")) {
		t.Fatal("window shortened")
	}
}

func TestMiddleware(t *testing.T) {
	r, _ := setup()
	var before, after bool
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		before = r.ReadPrimary(req.Context())
		r.Wrote(req.Context())
		after = r.ReadPrimary(req.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	if before || !after {
		t.Fatalf("before %v after %v", before, after)
	}
}

func TestConcurrent(t *testing.T) {
	r := New(Key(func(ctx context.Context) string { s, _ := ctx.Value(userKey{}).(string); return s }))
	ctx := Track(context.Background())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			u := withUser(ctx, fmt.Sprint(g%3))
			for i := 0; i < 1000; i++ {
				r.Wrote(u)
				_ = r.ReadPrimary(u)
			}
		}(g)
	}
	wg.Wait()
}
