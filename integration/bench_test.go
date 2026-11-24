package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mhdaslam790/kiln/cache/rediscache"
	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// Benchmarks run on SQLite so they need no services:
//
//	cd integration && go test -run '^$' -bench . -benchmem
//
// BenchmarkRaw* use database/sql directly, as a baseline for ORM overhead.

func benchDB(b *testing.B, users, postsPerUser int, opts ...orm.Option) *orm.DB {
	b.Helper()
	sqldb, err := sql.Open("sqlite", "file:"+filepath.Join(b.TempDir(), "bench.db")+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)")
	if err != nil {
		b.Fatal(err)
	}
	sqldb.SetMaxOpenConns(1)
	b.Cleanup(func() { sqldb.Close() })
	db := orm.Open(sqldb, dialect.SQLite(), opts...)
	return seedBench(b, db, users, postsPerUser)
}

func seedBench(b *testing.B, db *orm.DB, users, postsPerUser int) *orm.DB {
	b.Helper()
	ctx := context.Background()
	m := migrate.New(db, migrations())
	if _, err := m.Down(ctx, 10); err != nil {
		b.Fatal(err)
	}
	if _, err := m.Up(ctx); err != nil {
		b.Fatal(err)
	}
	if users > 0 {
		list := make([]User, users)
		for i := range list {
			list[i] = User{Name: fmt.Sprintf("user%d", i), Email: fmt.Sprintf("u%d@x.io", i), Age: i % 90}
		}
		if err := orm.For[User](db).InsertAll(ctx, list); err != nil {
			b.Fatal(err)
		}
		var posts []Post
		for _, u := range list {
			for j := 0; j < postsPerUser; j++ {
				posts = append(posts, Post{UserID: u.ID, Title: fmt.Sprintf("post %d", j)})
			}
		}
		if len(posts) > 0 {
			if err := orm.For[Post](db).InsertAll(ctx, posts); err != nil {
				b.Fatal(err)
			}
		}
	}
	return db
}

func BenchmarkInsert(b *testing.B) {
	db := benchDB(b, 0, 0)
	users, ctx := orm.For[User](db), context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u := User{Name: "n", Email: fmt.Sprintf("e%d@x.io", i)}
		if err := users.Insert(ctx, &u); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertAll100(b *testing.B) {
	db := benchDB(b, 0, 0)
	users, ctx := orm.For[User](db), context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list := make([]User, 100)
		for j := range list {
			list[j] = User{Name: "n", Email: fmt.Sprintf("e%d_%d@x.io", i, j)}
		}
		if err := users.InsertAll(ctx, list); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertAll1000(b *testing.B) {
	db := benchDB(b, 0, 0)
	users, ctx := orm.For[User](db), context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list := make([]User, 1000)
		for j := range list {
			list[j] = User{Name: "n", Email: fmt.Sprintf("e%d_%d@x.io", i, j)}
		}
		if err := users.InsertAll(ctx, list); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertOneByOne1000(b *testing.B) {
	db := benchDB(b, 0, 0)
	users, ctx := orm.For[User](db), context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := db.Transaction(ctx, func(ctx context.Context) error {
			for j := 0; j < 1000; j++ {
				u := User{Name: "n", Email: fmt.Sprintf("e%d_%d@x.io", i, j)}
				if err := users.Insert(ctx, &u); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFindByID(b *testing.B) {
	db := benchDB(b, 1000, 0)
	users, ctx := orm.For[User](db), context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := users.FindByID(ctx, int64(i%1000+1)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFindAll1000(b *testing.B) {
	db := benchDB(b, 1000, 0)
	users, ctx := orm.For[User](db), context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list, err := users.FindAll(ctx, q.Sort("id"))
		if err != nil || len(list) != 1000 {
			b.Fatal(err, len(list))
		}
	}
}

func BenchmarkRawFindAll1000(b *testing.B) {
	db := benchDB(b, 1000, 0)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.SQL().QueryContext(ctx, `SELECT "id", "tenant_id", "name", "email", "age", "bio", "prefs", "created_at", "updated_at", "deleted_at" FROM "users" WHERE "deleted_at" IS NULL ORDER BY "id"`)
		if err != nil {
			b.Fatal(err)
		}
		var list []User
		for rows.Next() {
			var u User
			var prefs []byte
			if err := rows.Scan(&u.ID, &u.TenantID, &u.Name, &u.Email, &u.Age, &u.Bio, &prefs, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt); err != nil {
				b.Fatal(err)
			}
			list = append(list, u)
		}
		rows.Close()
		if len(list) != 1000 {
			b.Fatal(len(list))
		}
	}
}

func BenchmarkCount(b *testing.B) {
	db := benchDB(b, 100, 0)
	users, ctx := orm.For[User](db), context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := users.Count(ctx, q.Gt("age", 10)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPreloadAll(b *testing.B) {
	db := benchDB(b, 100, 10)
	users, ctx := orm.For[User](db), context.Background()
	list, err := users.FindAll(ctx)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := users.PreloadAll(ctx, list, "Posts"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpdate(b *testing.B) {
	db := benchDB(b, 10, 0)
	users, ctx := orm.For[User](db), context.Background()
	u, _ := users.FindByID(ctx, int64(1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u.Age = i
		u.UpdatedAt = time.Time{}
		if err := users.Update(ctx, &u, "Age"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCaching compares a primary-key lookup with no cache, the
// prepared statement cache and the result cache (memory and Redis) on every
// configured database:
//
//	PG_DSN=... MYSQL_DSN=... REDIS_ADDR=localhost:6379 \
//	  go test -run '^$' -bench Caching -benchmem
func BenchmarkCaching(b *testing.B) {
	modes := []struct {
		name string
		opts func(b *testing.B) []orm.Option
	}{
		{"none", func(*testing.B) []orm.Option { return nil }},
		{"stmt", func(*testing.B) []orm.Option { return []orm.Option{orm.WithStmtCache(64)} }},
		{"memory", func(*testing.B) []orm.Option { return []orm.Option{orm.WithCache()} }},
		{"redis", func(b *testing.B) []orm.Option {
			addr := os.Getenv("REDIS_ADDR")
			if addr == "" {
				b.Skip("set REDIS_ADDR")
			}
			rdb := redis.NewClient(&redis.Options{Addr: addr})
			b.Cleanup(func() { rdb.Close() })
			return []orm.Option{orm.WithCache(orm.CacheStore(rediscache.New(rdb)),
				orm.CachePrefix(fmt.Sprintf("kiln-bench:%d:", time.Now().UnixNano())))}
		}},
	}
	for _, tg := range targets[:3] {
		b.Run(tg.name, func(b *testing.B) {
			for _, md := range modes {
				b.Run(md.name, func(b *testing.B) {
					dsn := "file:" + filepath.Join(b.TempDir(), "bench.db") + "?_pragma=journal_mode(WAL)"
					if tg.dsnEnv != "" {
						if dsn = os.Getenv(tg.dsnEnv); dsn == "" {
							b.Skipf("set %s", tg.dsnEnv)
						}
					}
					sqldb, err := sql.Open(tg.driver, dsn)
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() { sqldb.Close() })
					db := seedBench(b, orm.Open(sqldb, tg.d, md.opts(b)...), 100, 0)
					users := orm.For[User](db)
					ctx := orm.Cached(context.Background(), time.Minute) // ignored unless WithCache
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, err := users.FindByID(ctx, int64(i%100)+1); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
