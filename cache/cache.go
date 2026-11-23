// Package cache defines the storage used by the ORM's query result cache,
// plus an in-memory implementation.
//
// The cache is off unless enabled on the DB:
//
//	db := orm.Open(sqldb, dialect.Postgres(), orm.WithCache())             // in-memory
//	db := orm.Open(sqldb, dialect.Postgres(), orm.WithCache(
//		orm.CacheStore(rediscache.New(client))))                          // Redis
//
// and even then only queries made with an orm.Cached context are cached.
// Any backend (Redis, Memcached, ...) can be plugged in by implementing
// Store.
package cache

import (
	"context"
	"time"
)

// Store is a key/value store with per-entry expiry. Implementations must be
// safe for concurrent use. The ORM treats store errors as cache misses, so
// an unavailable store slows queries down but never breaks them.
type Store interface {
	// Get returns the value for key and whether it was found.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key. ttl <= 0 means no expiry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes keys; missing keys are not an error.
	Delete(ctx context.Context, keys ...string) error
}
