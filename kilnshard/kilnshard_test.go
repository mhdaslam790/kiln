package kilnshard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
)

func dbs(names ...string) map[string]*orm.DB {
	m := map[string]*orm.DB{}
	for _, n := range names {
		m[n] = orm.Open(&sql.DB{}, dialect.Postgres())
	}
	return m
}

var ctx = context.Background()

func TestHashSpreadAndStability(t *testing.T) {
	three, _ := New(dbs("s1", "s2", "s3"))
	four, _ := New(dbs("s1", "s2", "s3", "s4"))
	const n = 30000
	count := map[string]int{}
	moved := 0
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("customer-%d", i)
		a, _, err := three.Shard(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		count[a]++
		if again, _, _ := three.Shard(ctx, key); again != a {
			t.Fatal("same key, different shard")
		}
		b, _, _ := four.Shard(ctx, key)
		if b != a {
			moved++
			if b != "s4" {
				t.Fatalf("key %s moved between old shards %s -> %s", key, a, b)
			}
		}
	}
	for name, c := range count {
		if share := float64(c) / n; share < 0.30 || share > 0.37 {
			t.Errorf("%s holds %.1f%%", name, share*100)
		}
	}
	if share := float64(moved) / n; share < 0.22 || share > 0.28 {
		t.Errorf("adding a 4th shard moved %.1f%% of keys, want ~25%%", share*100)
	}
}

func TestContextAndLookup(t *testing.T) {
	shards := dbs("eu", "us")
	r, _ := New(shards, Lookup(func(_ context.Context, key string) (string, error) {
		if strings.HasPrefix(key, "eu-") {
			return "eu", nil
		}
		if key == "mars" {
			return "mars", nil
		}
		return "us", nil
	}))
	if _, err := r.DB(ctx); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := r.DB(With(ctx, "")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty key: %v", err)
	}
	custom, _ := New(shards, Key(func(context.Context) (string, bool) { return "", true }))
	if _, err := custom.DB(ctx); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty key from Key: %v", err)
	}
	if db, err := r.DB(With(ctx, "eu-42")); err != nil || db != shards["eu"] {
		t.Fatalf("eu: %v", err)
	}
	if repo, err := For[struct{ ID int64 }](With(ctx, "acme"), r); err != nil || repo == nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := r.DB(With(ctx, "mars")); !errors.Is(err, ErrUnknownShard) {
		t.Fatalf("unknown shard: %v", err)
	}
	// A custom key source.
	type tenantKey struct{}
	r2, _ := New(shards, Key(func(ctx context.Context) (string, bool) { s, ok := ctx.Value(tenantKey{}).(string); return s, ok }))
	if _, err := r2.DB(context.WithValue(ctx, tenantKey{}, "t1")); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(r.Names()) != "[eu us]" {
		t.Fatal(r.Names())
	}
}

func TestNewErrors(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("no shards")
	}
	if _, err := New(map[string]*orm.DB{"a": nil}); err == nil {
		t.Error("nil DB")
	}
}

func TestEach(t *testing.T) {
	r, _ := New(dbs("a", "b", "c", "d", "e"))
	var running, peak atomic.Int32
	var seen sync32
	err := r.Each(ctx, 2, func(_ context.Context, name string, _ *orm.DB) error {
		if n := running.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(10 * time.Millisecond)
		running.Add(-1)
		seen.add(name)
		if name == "b" || name == "d" {
			return errors.New("boom")
		}
		return nil
	})
	if peak.Load() > 2 {
		t.Fatalf("%d ran at once, limit 2", peak.Load())
	}
	if seen.n.Load() != 5 {
		t.Fatalf("ran on %d shards", seen.n.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "shard b: boom") || !strings.Contains(err.Error(), "shard d: boom") {
		t.Fatalf("errors: %v", err)
	}
}

type sync32 struct{ n atomic.Int32 }

func (s *sync32) add(string) { s.n.Add(1) }

func BenchmarkShard(b *testing.B) {
	r, _ := New(dbs("s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = r.Shard(ctx, "customer-12345")
	}
}
