package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mhdaslam790/kiln/cache/rediscache"
)

func TestRedisStoreDeleteMany(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run Redis tests")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	s := rediscache.New(rdb)
	prefix := fmt.Sprintf("kiln-test:del:%d:", time.Now().UnixNano())
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}
	for _, k := range keys {
		must(t, s.Set(ctx, k, []byte("v"), time.Minute))
	}
	must(t, s.Delete(ctx, keys[0]))
	must(t, s.Delete(ctx, keys[1:]...))
	must(t, s.Delete(ctx))
	got, err := s.GetMulti(ctx, keys)
	must(t, err)
	for i, v := range got {
		if v != nil {
			t.Fatalf("%s survived Delete", keys[i])
		}
	}
}
