package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mhdaslam790/kiln/cache"
	"github.com/mhdaslam790/kiln/cache/rediscache"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// hits counts statements that reached the database vs. served from cache.
type hits struct{ db, cached atomic.Int64 }

func (h *hits) hook() orm.Option {
	return orm.WithHook(func(_ context.Context, e orm.Event) {
		if e.Cached {
			h.cached.Add(1)
		} else {
			h.db.Add(1)
		}
	})
}

// expect runs fn and checks how many statements hit the DB / the cache.
func (h *hits) expect(t *testing.T, what string, db, cached int64, fn func()) {
	t.Helper()
	d0, c0 := h.db.Load(), h.cached.Load()
	fn()
	if gd, gc := h.db.Load()-d0, h.cached.Load()-c0; gd != db || gc != cached {
		t.Fatalf("%s: got %d db / %d cached statements, want %d / %d", what, gd, gc, db, cached)
	}
}

type storeCase struct {
	name string
	opt  func(t *testing.T) orm.CacheOption // nil = default (memory)
}

func cacheStores() []storeCase {
	return []storeCase{
		{"memory", nil},
		{"redis", func(t *testing.T) orm.CacheOption {
			addr := os.Getenv("REDIS_ADDR")
			if addr == "" {
				t.Skip("set REDIS_ADDR to run Redis cache tests")
			}
			rdb := redis.NewClient(&redis.Options{Addr: addr})
			t.Cleanup(func() { rdb.Close() })
			if err := rdb.Ping(context.Background()).Err(); err != nil {
				t.Skipf("redis unavailable: %v", err)
			}
			return orm.CacheStore(rediscache.New(rdb))
		}},
	}
}

// withCache runs fn on every database × store with the result cache enabled.
func withCache(t *testing.T, ttl time.Duration, fn func(t *testing.T, db *orm.DB, h *hits)) {
	for _, sc := range cacheStores() {
		t.Run(sc.name, func(t *testing.T) {
			h := &hits{}
			forEachWith(t, func(t *testing.T) []orm.Option {
				// A unique prefix per run keeps shared Redis data from
				// earlier runs (whose tables were recreated) out of the way.
				copts := []orm.CacheOption{orm.CachePrefix(fmt.Sprintf("kiln-test:%s:%d:", t.Name(), time.Now().UnixNano())), orm.CacheTTL(ttl),
					orm.CacheOnError(func(_ context.Context, err error) { t.Errorf("cache store error: %v", err) })}
				if sc.opt != nil {
					copts = append(copts, sc.opt(t))
				}
				return []orm.Option{orm.WithCache(copts...), h.hook()}
			}, func(t *testing.T, db *orm.DB) { fn(t, db, h) })
		})
	}
}

func TestCacheOffByDefault(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		must(t, users.Insert(ctx, &User{Name: "a", Email: "a@x"}))
		if db.CacheEnabled() {
			t.Fatal("cache enabled without WithCache")
		}
		// Cached is a no-op without WithCache: both reads work normally.
		for range 2 {
			got, err := users.FindAll(orm.Cached(ctx, time.Minute))
			must(t, err)
			if len(got) != 1 {
				t.Fatalf("%d users", len(got))
			}
		}
	})
}

func TestResultCache(t *testing.T) {
	withCache(t, time.Minute, func(t *testing.T, db *orm.DB, h *hits) {
		ctx := context.Background()
		cctx := orm.Cached(ctx, 0) // 0 = the DB's default TTL
		users, posts := orm.For[User](db), orm.For[Post](db)
		bio := "hello"
		must(t, users.InsertAll(ctx, []User{
			{Name: "a", Email: "a@x", Age: 30, Bio: &bio, Prefs: map[string]any{"theme": "dark"}},
			{Name: "b", Email: "b@x", Age: 40},
		}))

		// Miss, then hit with identical results (times, pointers, JSON).
		var first, second []User
		h.expect(t, "first read", 1, 0, func() {
			var err error
			first, err = users.FindAll(cctx, q.Sort("name"))
			must(t, err)
		})
		h.expect(t, "second read", 0, 1, func() {
			var err error
			second, err = users.FindAll(cctx, q.Sort("name"))
			must(t, err)
		})
		if !reflect.DeepEqual(first, second) || len(second) != 2 || *second[0].Bio != "hello" || second[0].Prefs["theme"] != "dark" {
			t.Fatalf("cached result differs:\n%+v\n%+v", first, second)
		}
		// Callers may mutate what they get; the cache keeps its own copy.
		second[0].Name = "mutated"
		got, err := users.FindAll(cctx, q.Sort("name"))
		must(t, err)
		if got[0].Name != "a" {
			t.Fatal("cache entry was mutated through a returned value")
		}

		// Different arguments are different entries; plain ctx bypasses.
		h.expect(t, "other args", 1, 0, func() { _, err := users.Find(cctx, q.Eq("name", "b")); must(t, err) })
		h.expect(t, "other args again", 0, 1, func() { _, err := users.Find(cctx, q.Eq("name", "b")); must(t, err) })
		h.expect(t, "uncached ctx", 1, 0, func() { _, err := users.Find(ctx, q.Eq("name", "b")); must(t, err) })
		h.expect(t, "NoCache", 1, 0, func() { _, err := users.Find(orm.NoCache(cctx), q.Eq("name", "b")); must(t, err) })

		// Count / Exists / not-found are cached too.
		h.expect(t, "count", 1, 0, func() { _, err := users.Count(cctx); must(t, err) })
		h.expect(t, "count hit", 0, 1, func() { _, err := users.Count(cctx); must(t, err) })
		for range 2 {
			if _, err := users.Find(cctx, q.Eq("name", "nobody")); !errors.Is(err, orm.ErrNotFound) {
				t.Fatalf("want ErrNotFound, got %v", err)
			}
		}

		// Every kind of ORM write invalidates the table.
		countIs := func(what string, want int64) {
			t.Helper()
			n, err := users.Count(cctx)
			must(t, err)
			if n != want {
				t.Fatalf("%s: count %d, want %d", what, n, want)
			}
		}
		c := User{Name: "c", Email: "c@x"}
		must(t, users.Insert(ctx, &c))
		countIs("after insert", 3)
		c.Name = "c2"
		must(t, users.Update(ctx, &c))
		if u, err := users.Find(cctx, q.Eq("id", c.ID)); err != nil || u.Name != "c2" {
			t.Fatalf("after update: %+v %v", u, err)
		}
		must(t, users.Delete(ctx, &c)) // soft delete
		countIs("after delete", 2)
		must(t, users.Restore(ctx, &c))
		countIs("after restore", 3)
		_, err = users.UpdateAny(ctx, q.Eq("name", "c2"), q.Set("age", 99))
		must(t, err)
		if u, err := users.Find(cctx, q.Eq("id", c.ID)); err != nil || u.Age != 99 {
			t.Fatalf("after UpdateAny: %+v %v", u, err)
		}
		_, err = users.DeleteAny(ctx, q.Eq("name", "c2"))
		must(t, err)
		countIs("after DeleteAny", 2)

		// A write to another table does not invalidate users.
		a := first[0]
		must(t, posts.Insert(ctx, &Post{UserID: a.ID, Title: "p1"}))
		h.expect(t, "users after posts write", 0, 1, func() { countIs("unchanged", 2) })

		// Preload: both levels are cached, and a write to posts shows up.
		loadPosts := func() []Post {
			u, err := users.Find(cctx, q.Eq("id", a.ID))
			must(t, err)
			must(t, users.Preload(cctx, &u, "Posts"))
			return u.Posts
		}
		loadPosts()
		h.expect(t, "preload hit", 0, 2, func() { loadPosts() })
		must(t, posts.Insert(ctx, &Post{UserID: a.ID, Title: "p2"}))
		if ps := loadPosts(); len(ps) != 2 {
			t.Fatalf("preload after insert: %d posts", len(ps))
		}

		// Many-to-many: Link invalidates the join table.
		roles := orm.For[Role](db)
		admin := Role{Name: "admin"}
		must(t, roles.Insert(ctx, &admin))
		loadRoles := func() int {
			u, err := users.Find(cctx, q.Eq("id", a.ID))
			must(t, err)
			must(t, users.Preload(cctx, &u, "Roles"))
			return len(u.Roles)
		}
		if n := loadRoles(); n != 0 {
			t.Fatalf("roles before link: %d", n)
		}
		must(t, users.Link(ctx, &a, "Roles", &admin))
		if n := loadRoles(); n != 1 {
			t.Fatalf("roles after link: %d", n)
		}
		must(t, users.Unlink(ctx, &a, "Roles"))
		if n := loadRoles(); n != 0 {
			t.Fatalf("roles after unlink: %d", n)
		}
	})
}

func TestResultCacheTransactions(t *testing.T) {
	withCache(t, time.Minute, func(t *testing.T, db *orm.DB, h *hits) {
		ctx := context.Background()
		cctx := orm.Cached(ctx, time.Minute)
		users := orm.For[User](db)
		must(t, users.Insert(ctx, &User{Name: "a", Email: "a@x"}))
		count := func(ctx context.Context) int64 {
			t.Helper()
			n, err := users.Count(ctx)
			must(t, err)
			return n
		}
		count(cctx) // fill

		// Reads inside a transaction always hit the database, and the
		// write becomes visible to cached reads only after commit.
		must(t, db.Transaction(cctx, func(tctx context.Context) error {
			must(t, users.Insert(tctx, &User{Name: "b", Email: "b@x"}))
			h.expect(t, "read in tx", 1, 0, func() {
				if n := count(tctx); n != 2 {
					t.Errorf("in tx: %d", n)
				}
			})
			return nil
		}))
		if n := count(cctx); n != 2 {
			t.Fatalf("after commit: %d", n)
		}

		// A rolled back write leaves the cache valid.
		count(cctx)
		err := db.Transaction(ctx, func(tctx context.Context) error {
			must(t, users.Insert(tctx, &User{Name: "c", Email: "c@x"}))
			return errors.New("rollback")
		})
		if err == nil {
			t.Fatal("want rollback error")
		}
		h.expect(t, "after rollback", 0, 1, func() {
			if n := count(cctx); n != 2 {
				t.Fatalf("after rollback: %d", n)
			}
		})
	})
}

func TestResultCacheRawSQLAndTTL(t *testing.T) {
	withCache(t, 300*time.Millisecond, func(t *testing.T, db *orm.DB, h *hits) {
		ctx := context.Background()
		users := orm.For[User](db)
		must(t, users.Insert(ctx, &User{Name: "a", Email: "a@x"}))
		names := func(ctx context.Context) []string {
			t.Helper()
			var out []string
			must(t, db.Query(ctx, &out, "SELECT name FROM users ORDER BY name"))
			return out
		}

		// Raw SQL is only cached when its tables are listed.
		h.expect(t, "raw without tables", 2, 0, func() { names(orm.Cached(ctx, 0)); names(orm.Cached(ctx, 0)) })
		rctx := orm.Cached(ctx, 0, "users")
		h.expect(t, "raw with tables", 1, 1, func() { names(rctx); names(rctx) })

		// A raw write is invisible to the cache until InvalidateTables.
		_, err := db.Exec(ctx, "UPDATE users SET name = ?", "z")
		must(t, err)
		if got := names(rctx); got[0] != "a" {
			t.Fatalf("expected the stale cached value, got %v", got)
		}
		must(t, db.InvalidateTables(ctx, "users"))
		if got := names(rctx); got[0] != "z" {
			t.Fatalf("after InvalidateTables: %v", got)
		}

		// Entries expire after the TTL.
		names(rctx)
		time.Sleep(450 * time.Millisecond)
		h.expect(t, "after ttl", 1, 0, func() { names(rctx) })
	})
}

func TestResultCacheConcurrentMisses(t *testing.T) {
	withCache(t, time.Minute, func(t *testing.T, db *orm.DB, h *hits) {
		ctx := context.Background()
		users := orm.For[User](db)
		must(t, users.Insert(ctx, &User{Name: "a", Email: "a@x"}))
		cctx := orm.Cached(ctx, time.Minute)
		d0, c0 := h.db.Load(), h.cached.Load()
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := users.FindAll(cctx)
				if err != nil || len(got) != 1 {
					t.Errorf("%v %v", got, err)
				}
			}()
		}
		wg.Wait()
		// Merged misses: one database read; the rest share it or hit the cache.
		dbReads, cached := h.db.Load()-d0, h.cached.Load()-c0
		t.Logf("20 concurrent reads: %d from the database, %d from the cache", dbReads, cached)
		if dbReads > 3 {
			t.Errorf("%d database reads for 20 concurrent identical misses", dbReads)
		}
	})
}

func TestMemoryStoreDirect(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(0, 0)
	m := cache.NewMemory(cache.MaxEntries(2), cache.WithClock(func() time.Time { return now }))
	must(t, m.Set(ctx, "a", []byte("1"), time.Second))
	must(t, m.Set(ctx, "b", []byte("2"), 0))
	m.Get(ctx, "a") // a is now most recent
	must(t, m.Set(ctx, "c", []byte("3"), 0))
	if _, ok, _ := m.Get(ctx, "b"); ok {
		t.Fatal("b should have been evicted (least recently used)")
	}
	now = now.Add(2 * time.Second)
	if _, ok, _ := m.Get(ctx, "a"); ok {
		t.Fatal("a should have expired")
	}
	if v, ok, _ := m.Get(ctx, "c"); !ok || string(v) != "3" {
		t.Fatal("c missing")
	}
}
