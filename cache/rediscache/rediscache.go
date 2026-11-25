// Package rediscache is a Redis backend for the kiln ORM query cache.
// It lives in its own module so the core library stays free of the Redis
// client dependency.
//
//	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
//	db, err := orm.Connect(ctx, "pgx", dsn, dialect.Postgres(),
//		orm.WithCache(orm.CacheStore(rediscache.New(rdb))))
//
// Every application instance sharing the Redis sees the same entries and the
// same invalidations, unlike the default in-memory store.
package rediscache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mhdaslam790/kiln/cache"
)

// Store implements cache.Store on a go-redis client (single node, sentinel
// or cluster).
type Store struct {
	c redis.UniversalClient
}

var _ cache.Store = (*Store)(nil)

// New wraps a go-redis client. The client is not closed by the store.
func New(c redis.UniversalClient) *Store { return &Store{c: c} }

// Get implements cache.Store.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	b, err := s.c.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// Set implements cache.Store. ttl <= 0 stores the key without expiry.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		ttl = 0
	}
	return s.c.Set(ctx, key, value, ttl).Err()
}

// Delete implements cache.Store. Several keys are deleted with pipelined
// DELs, not one multi-key DEL, which Redis Cluster refuses (CROSSSLOT) when
// the keys live in different slots.
func (s *Store) Delete(ctx context.Context, keys ...string) error {
	switch len(keys) {
	case 0:
		return nil
	case 1:
		return s.c.Del(ctx, keys[0]).Err()
	}
	_, err := s.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, k := range keys {
			p.Del(ctx, k)
		}
		return nil
	})
	return err
}

// GetMulti fetches several keys in one round trip (missing keys are nil).
// It pipelines GETs rather than using MGET so it also works on Redis
// Cluster when the keys live in different slots.
func (s *Store) GetMulti(ctx context.Context, keys []string) ([][]byte, error) {
	cmds := make([]*redis.StringCmd, len(keys))
	_, err := s.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, k := range keys {
			cmds[i] = p.Get(ctx, k)
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make([][]byte, len(keys))
	for i, c := range cmds {
		b, err := c.Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}
