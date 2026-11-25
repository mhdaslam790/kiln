// Package kilnshard splits data across several databases (shards) by a
// key, usually the tenant or customer id. Each shard is an ordinary
// *orm.DB; kilnshard only picks which one to use.
//
//	shards, err := kilnshard.New(map[string]*orm.DB{"a": dbA, "b": dbB})
//
//	ctx = kilnshard.With(ctx, customerID)        // in your middleware
//	users, err := kilnshard.For[User](ctx, shards) // the repo on that customer's shard
//	u, err := users.FindByID(ctx, id)
//
//	err = shards.Each(ctx, 4, func(ctx context.Context, name string, db *orm.DB) error {
//		... // reports, cleanups: run on every shard, 4 at a time
//	})
//
// By default a key's shard comes from rendezvous hashing over the shard
// names: stable, evenly spread, and adding a shard moves only about 1/N
// of the keys. To place keys yourself (a directory table, "EU customers on
// eu-1"), use Lookup.
//
// What sharding cannot do: a transaction or a join across shards, or a
// query over all shards in one statement (use Each and combine the
// results). Moving keys between shards (resharding) is up to you: copy the
// rows, then change Lookup.
package kilnshard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
)

var (
	// ErrNoKey is returned when the context carries no shard key, or an
	// empty one (e.g. an anonymous request), rather than sending all such
	// data to one shard.
	ErrNoKey = errors.New("kilnshard: no shard key in context")
	// ErrUnknownShard is returned when Lookup names a shard that doesn't exist.
	ErrUnknownShard = errors.New("kilnshard: unknown shard")
)

type ctxKey struct{}

// With returns a context whose queries go to the shard of key.
func With(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, ctxKey{}, key)
}

// KeyFrom returns the shard key carried by ctx; an empty key is no key.
func KeyFrom(ctx context.Context) (string, bool) {
	k, ok := ctx.Value(ctxKey{}).(string)
	return k, ok && k != ""
}

// Option configures New.
type Option func(*Router)

// Lookup places keys with your own function instead of hashing, e.g. a
// directory table or a region rule. It returns a shard name.
func Lookup(fn func(ctx context.Context, key string) (string, error)) Option {
	return func(r *Router) { r.lookup = fn }
}

// Key replaces where the shard key comes from (default: With). For
// example, to shard by tenant:
//
//	kilnshard.Key(func(ctx context.Context) (string, bool) {
//		id, ok := tenant.From(ctx)
//		return fmt.Sprint(id), ok
//	})
func Key(fn func(ctx context.Context) (string, bool)) Option {
	return func(r *Router) { r.key = fn }
}

// Router picks a shard. It is safe for concurrent use.
type Router struct {
	shards map[string]*orm.DB
	names  []string // sorted
	lookup func(context.Context, string) (string, error)
	key    func(context.Context) (string, bool)
}

// New returns a Router over the named shards. Names are part of the
// hashing: keep them stable (e.g. "shard-1"), and don't rename a shard
// that holds data.
func New(shards map[string]*orm.DB, opts ...Option) (*Router, error) {
	if len(shards) == 0 {
		return nil, errors.New("kilnshard: no shards")
	}
	r := &Router{shards: make(map[string]*orm.DB, len(shards)), key: KeyFrom}
	for name, db := range shards {
		if name == "" || db == nil {
			return nil, fmt.Errorf("kilnshard: shard %q: empty name or nil DB", name)
		}
		r.shards[name] = db
		r.names = append(r.names, name)
	}
	sort.Strings(r.names)
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Names returns the shard names, sorted.
func (r *Router) Names() []string { return append([]string(nil), r.names...) }

// Shard returns the name and DB of the shard holding key.
func (r *Router) Shard(ctx context.Context, key string) (string, *orm.DB, error) {
	name := ""
	if r.lookup != nil {
		var err error
		if name, err = r.lookup(ctx, key); err != nil {
			return "", nil, err
		}
	} else {
		name = r.hash(key)
	}
	db, ok := r.shards[name]
	if !ok {
		return "", nil, fmt.Errorf("%w %q for key %q", ErrUnknownShard, name, key)
	}
	return name, db, nil
}

// DB returns the DB of the shard for the context's key.
func (r *Router) DB(ctx context.Context) (*orm.DB, error) {
	key, ok := r.key(ctx)
	if !ok || key == "" {
		return nil, ErrNoKey
	}
	_, db, err := r.Shard(ctx, key)
	return db, err
}

// For returns the repository for T on the shard for the context's key.
func For[T any](ctx context.Context, r *Router) (*orm.Repo[T], error) {
	db, err := r.DB(ctx)
	if err != nil {
		return nil, err
	}
	return orm.For[T](db), nil
}

// Each calls fn for every shard, at most limit at a time (0 means all at
// once), and returns every error, each prefixed with its shard name. All
// shards run even if one fails; return early from fn by checking ctx.
func (r *Router) Each(ctx context.Context, limit int, fn func(ctx context.Context, name string, db *orm.DB) error) error {
	if limit <= 0 || limit > len(r.names) {
		limit = len(r.names)
	}
	sem := make(chan struct{}, limit)
	errs := make([]error, len(r.names))
	var wg sync.WaitGroup
	for i, name := range r.names {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, name string) {
			defer func() { <-sem; wg.Done() }()
			if err := fn(ctx, name, r.shards[name]); err != nil {
				errs[i] = fmt.Errorf("shard %s: %w", name, err)
			}
		}(i, name)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Migrate applies all pending migrations on every shard, limit at a time,
// and reports per shard how many ran. Call it from your migration runner.
func (r *Router) Migrate(ctx context.Context, limit int, migrations []migrate.Migration) (map[string]int, error) {
	var mu sync.Mutex
	applied := map[string]int{}
	err := r.Each(ctx, limit, func(ctx context.Context, name string, db *orm.DB) error {
		n, err := migrate.New(db, migrations).Up(ctx)
		mu.Lock()
		applied[name] = len(n)
		mu.Unlock()
		return err
	})
	return applied, err
}

// hash picks a shard by rendezvous (highest random weight) hashing.
func (r *Router) hash(key string) string {
	best, bestScore := "", uint64(0)
	for _, name := range r.names {
		if s := score(name, key); best == "" || s > bestScore {
			best, bestScore = name, s
		}
	}
	return best
}

// score is FNV-1a over name, a separator and key, finished with a mixer
// so close inputs spread well. No allocations.
func score(name, key string) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	for i := 0; i < len(name); i++ {
		h = (h ^ uint64(name[i])) * prime
	}
	h = (h ^ 0xff) * prime
	for i := 0; i < len(key); i++ {
		h = (h ^ uint64(key[i])) * prime
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
