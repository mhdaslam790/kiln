package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/kilnshard"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

func TestShards(t *testing.T) {
	ctx := context.Background()
	shards := map[string]*orm.DB{}
	for _, name := range []string{"lite-1", "lite-2"} {
		sqldb, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), name+".db")+"?_pragma=busy_timeout(5000)")
		must(t, err)
		sqldb.SetMaxOpenConns(1)
		t.Cleanup(func() { sqldb.Close() })
		shards[name] = orm.Open(sqldb, dialect.SQLite())
	}
	for _, tg := range targets[1:] { // servers that are available locally / in CI
		if dsn := os.Getenv(tg.dsnEnv); dsn != "" {
			sqldb, err := sql.Open(tg.driver, dsn)
			must(t, err)
			t.Cleanup(func() { sqldb.Close() })
			db := orm.Open(sqldb, tg.d)
			_, _ = migrate.New(db, migrations()).Down(ctx, 10) // start clean
			t.Cleanup(func() { _, _ = migrate.New(db, migrations()).Down(ctx, 10) })
			shards[tg.name] = db
		}
	}
	router, err := kilnshard.New(shards)
	must(t, err)

	applied, err := router.Migrate(ctx, 2, migrations())
	must(t, err)
	for name, n := range applied {
		if n == 0 {
			t.Fatalf("no migrations ran on %s", name)
		}
	}

	// Each customer's rows land on its shard only.
	const customers = 40
	home := map[string]string{}
	for i := 0; i < customers; i++ {
		key := fmt.Sprintf("cust-%d", i)
		cctx := kilnshard.With(ctx, key)
		users, err := kilnshard.For[User](cctx, router)
		must(t, err)
		must(t, users.Insert(cctx, &User{TenantID: int64(i), Name: key, Email: key + "@x.io"}))
		home[key], _, _ = router.Shard(ctx, key)
	}
	var mu sync.Mutex
	total := 0
	must(t, router.Each(ctx, 0, func(ctx context.Context, name string, db *orm.DB) error {
		rows, err := orm.For[User](db).FindAll(ctx)
		if err != nil {
			return err
		}
		for _, u := range rows {
			if home[u.Name] != name {
				return fmt.Errorf("%s is on %s, belongs on %s", u.Name, name, home[u.Name])
			}
		}
		mu.Lock()
		total += len(rows)
		mu.Unlock()
		return nil
	}))
	if total != customers {
		t.Fatalf("%d rows across shards, want %d", total, customers)
	}
	used := map[string]bool{}
	for _, s := range home {
		used[s] = true
	}
	if len(used) != len(shards) {
		t.Fatalf("only %d of %d shards used", len(used), len(shards))
	}

	// Reads find a customer through its key; without a key nothing runs.
	cctx := kilnshard.With(ctx, "cust-7")
	users, err := kilnshard.For[User](cctx, router)
	must(t, err)
	if n, err := users.Count(cctx, q.Eq("name", "cust-7")); err != nil || n != 1 {
		t.Fatalf("lookup: %d %v", n, err)
	}
	if _, err := kilnshard.For[User](ctx, router); !errors.Is(err, kilnshard.ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	// Transactions work within a shard.
	db, err := router.DB(cctx)
	must(t, err)
	must(t, db.Transaction(cctx, func(ctx context.Context) error {
		_, err := users.UpdateAny(ctx, q.Eq("name", "cust-7"), q.Set("age", 7))
		return err
	}))
	t.Logf("%d shards: %v", len(shards), router.Names())
}
