package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "modernc.org/sqlite"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

// ---- models -----------------------------------------------------------------

type User struct {
	ID        int64
	TenantID  int64
	Name      string
	Email     string
	Age       int
	Bio       *string        `schema:"type:text"`
	Prefs     map[string]any `db:"prefs,json" schema:"type:text"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	Posts   []Post
	Profile *Profile
	Roles   []Role `m2m:"user_roles"`
}

// Role is linked to users through the user_roles join table.
type Role struct {
	ID   int64
	Name string `schema:"unique"`
}

func (u *User) BeforeInsert(ctx context.Context) error {
	u.Email = strings.ToLower(u.Email)
	return nil
}

func (u *User) AfterInsert(ctx context.Context) error {
	if u.Name == "fail-after" {
		return errors.New("after insert failed")
	}
	return nil
}

type Post struct {
	ID       int64
	UserID   int64 `schema:"null"`
	User     *User
	Title    string
	Views    int
	Comments []Comment
}

type Comment struct {
	ID     int64
	PostID int64  `schema:"null,references:posts,ondelete:CASCADE"`
	Body   string `schema:"type:text,null"`
}

type Profile struct {
	ID     int64
	UserID int64  `schema:"null,references:users"`
	Bio    string `schema:"null"`
}

// PostTag has a composite primary key.
type PostTag struct {
	PostID int64  `db:"post_id,primary" schema:"references:posts,ondelete:CASCADE"`
	Tag    string `db:"tag,primary" schema:"limit:50"`
	Weight int
}

// ---- migrations ---------------------------------------------------------------

func migrations() []migrate.Migration {
	var set migrate.Set
	set.Add(20260101000001, "create_users", func(s *schema.Schema) {
		s.CreateTable("users", func(t *schema.Table) {
			t.ID("id")
			t.BigInt("tenant_id", schema.Required(), schema.Default(0))
			t.String("name", schema.Required())
			t.String("email", schema.Required(), schema.Unique())
			t.Int("age", schema.Required(), schema.Default(0))
			t.Text("bio")
			t.Text("prefs")
			t.Timestamps()
			t.SoftDelete()
		})
	}, func(s *schema.Schema) { s.DropTable("users") })

	set.Add(20260101000002, "create_posts", func(s *schema.Schema) {
		s.CreateTable("posts", func(t *schema.Table) {
			t.ID("id")
			t.References("user_id", "users", schema.OnDelete("CASCADE"))
			t.String("title", schema.Required())
			t.Int("views", schema.Required(), schema.Default(0))
		})
		s.CreateTable("comments", func(t *schema.Table) {
			t.ID("id")
			t.References("post_id", "posts", schema.OnDelete("CASCADE"))
			t.Text("body")
		})
		s.CreateTable("profiles", func(t *schema.Table) {
			t.ID("id")
			t.References("user_id", "users")
			t.String("bio")
		})
		s.CreateTable("post_tags", func(t *schema.Table) {
			t.References("post_id", "posts", schema.Required(), schema.OnDelete("CASCADE"))
			t.String("tag", schema.Required(), schema.Limit(50))
			t.Int("weight", schema.Required(), schema.Default(0))
			t.PrimaryKey("post_id", "tag")
		})
	}, func(s *schema.Schema) {
		s.DropTable("post_tags")
		s.DropTable("profiles")
		s.DropTable("comments")
		s.DropTable("posts")
	})

	set.Add(20260101000003, "create_roles", func(s *schema.Schema) {
		s.CreateTable("roles", func(t *schema.Table) {
			t.ID("id")
			t.String("name", schema.Required(), schema.Unique())
		})
		s.CreateTable("user_roles", func(t *schema.Table) {
			t.References("user_id", "users", schema.Required(), schema.OnDelete("CASCADE"))
			t.References("role_id", "roles", schema.Required(), schema.OnDelete("CASCADE"))
			t.PrimaryKey("user_id", "role_id")
		})
	}, func(s *schema.Schema) {
		s.DropTable("user_roles")
		s.DropTable("roles")
	})
	return set.List()
}

// ---- harness ------------------------------------------------------------------

type target struct {
	name, driver, dsnEnv string
	d                    dialect.Dialect
}

var targets = []target{
	{"sqlite", "sqlite", "", dialect.SQLite()},
	{"postgres", "pgx", "PG_DSN", dialect.Postgres()},
	{"mysql", "mysql", "MYSQL_DSN", dialect.MySQL()},
	{"mssql", "sqlserver", "MSSQL_DSN", dialect.MSSQL()},
}

var clock = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func open(t *testing.T, tg target, extra ...orm.Option) *orm.DB {
	t.Helper()
	dsn := ""
	if tg.dsnEnv == "" {
		dsn = "file:" + filepath.Join(t.TempDir(), "test.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	} else if dsn = os.Getenv(tg.dsnEnv); dsn == "" {
		t.Skipf("set %s to run %s tests", tg.dsnEnv, tg.name)
	}
	sqldb, err := sql.Open(tg.driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if tg.name == "sqlite" {
		sqldb.SetMaxOpenConns(1)
	}
	t.Cleanup(func() { sqldb.Close() })
	var (
		logMu sync.Mutex // hooks run concurrently
		log   []string
	)
	opts := []orm.Option{
		orm.WithClock(func() time.Time { return clock }),
		orm.WithHook(func(ctx context.Context, e orm.Event) {
			logMu.Lock()
			log = append(log, e.SQL)
			logMu.Unlock()
		}),
	}
	if os.Getenv("KILN_TEST_STMT_CACHE") == "1" {
		opts = append(opts, orm.WithStmtCache(64))
	}
	db := orm.Open(sqldb, tg.d, append(opts, extra...)...)
	t.Cleanup(func() {
		logMu.Lock()
		defer logMu.Unlock()
		if t.Failed() && len(log) > 0 {
			n := min(len(log), 15)
			t.Logf("last statements:\n%s", strings.Join(log[len(log)-n:], "\n"))
		}
	})

	ctx := context.Background()
	// Start clean on shared servers.
	m := migrate.New(db, migrations())
	if _, err := m.Down(ctx, 10); err != nil {
		t.Logf("cleanup down: %v", err)
	}
	return db
}

func forEach(t *testing.T, fn func(t *testing.T, db *orm.DB)) {
	forEachWith(t, nil, fn)
}

// forEachWith is forEach with extra DB options per target.
func forEachWith(t *testing.T, opts func(t *testing.T) []orm.Option, fn func(t *testing.T, db *orm.DB)) {
	for _, tg := range targets {
		t.Run(tg.name, func(t *testing.T) {
			var extra []orm.Option
			if opts != nil {
				extra = opts(t)
			}
			db := open(t, tg, extra...)
			ctx := context.Background()
			if _, err := migrate.New(db, migrations()).Up(ctx); err != nil {
				t.Fatal(err)
			}
			fn(t, db)
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- tests --------------------------------------------------------------------

func TestMigrations(t *testing.T) {
	for _, tg := range targets {
		t.Run(tg.name, func(t *testing.T) {
			db := open(t, tg)
			ctx := context.Background()
			m := migrate.New(db, migrations())

			applied, err := m.Up(ctx)
			must(t, err)
			if len(applied) != 3 {
				t.Fatalf("applied %d", len(applied))
			}
			again, err := m.Up(ctx)
			must(t, err)
			if len(again) != 0 {
				t.Fatalf("re-applied %d", len(again))
			}
			st, err := m.Status(ctx)
			must(t, err)
			if len(st) != 3 || !st[0].Applied || !st[1].Applied || !st[2].Applied {
				t.Fatalf("status %+v", st)
			}
			for _, x := range st { // the right instant, in local time on every database
				if d := time.Since(x.AppliedAt); d < -time.Minute || d > time.Minute || x.AppliedAt.Location() != time.Local {
					t.Fatalf("AppliedAt %v (%v ago, %v)", x.AppliedAt, d, x.AppliedAt.Location())
				}
			}

			rolled, err := m.Down(ctx, 2)
			must(t, err)
			if len(rolled) != 2 || rolled[0].Name != "create_roles" || rolled[1].Name != "create_posts" {
				t.Fatalf("rolled %+v", rolled)
			}
			if _, err := db.Count(ctx, "posts"); err == nil {
				t.Fatal("posts should be gone")
			}
			pending, err := m.Pending(ctx)
			must(t, err)
			if len(pending) != 2 {
				t.Fatalf("pending %d", len(pending))
			}
			must(t, m.Redo(ctx)) // down users? no: redo rolls back the latest applied (users) and re-applies it
			_, err = m.Up(ctx)
			must(t, err)

			sqls, err := m.SQL(migrations()[0], true)
			must(t, err)
			if len(sqls) != 1 || !strings.Contains(sqls[0], "CREATE TABLE") {
				t.Fatalf("dry run: %v", sqls)
			}
		})
	}
}

func TestCRUD(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)

		bio := "hello"
		u := User{Name: "Ann", Email: "ANN@Example.com", Age: 30, Bio: &bio, Prefs: map[string]any{"theme": "dark"}}
		must(t, users.Insert(ctx, &u))
		if u.ID == 0 || !u.CreatedAt.Equal(clock) || u.Email != "ann@example.com" {
			t.Fatalf("after insert: %+v", u)
		}

		got, err := users.FindByID(ctx, u.ID)
		must(t, err)
		if got.Name != "Ann" || got.Bio == nil || *got.Bio != "hello" || got.Prefs["theme"] != "dark" || !got.CreatedAt.Equal(clock) {
			t.Fatalf("found %+v", got)
		}

		if _, err := users.FindByID(ctx, 999999); !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}

		// Untyped API.
		var u2 User
		must(t, db.Find(ctx, &u2, q.Eq("email", "ann@example.com")))
		if u2.ID != u.ID {
			t.Fatal("untyped find")
		}

		// Partial update.
		u.Name, u.Age = "Annie", 99
		must(t, users.Update(ctx, &u, "Name"))
		got, _ = users.FindByID(ctx, u.ID)
		if got.Name != "Annie" || got.Age != 30 {
			t.Fatalf("partial update: %+v", got)
		}

		// Bulk insert fills IDs.
		batch := []User{{Name: "B", Email: "b@x"}, {Name: "C", Email: "c@x", Age: 40}, {Name: "D", Email: "d@x", Age: 50}}
		must(t, users.InsertAll(ctx, batch))
		for _, b := range batch {
			if b.ID == 0 {
				t.Fatalf("bulk id missing: %+v", batch)
			}
		}
		back, err := users.FindByID(ctx, batch[2].ID)
		must(t, err)
		if back.Email != "d@x" {
			t.Fatalf("bulk ids out of order: %+v", back)
		}

		n, err := users.Count(ctx, q.Gte("age", 40))
		must(t, err)
		if n != 2 {
			t.Fatalf("count = %d", n)
		}
		ok, err := users.Exists(ctx, q.Eq("name", "C"))
		must(t, err)
		if !ok {
			t.Fatal("exists")
		}

		aff, err := users.UpdateAny(ctx, q.Gte("age", 40), q.Inc("age", 1))
		must(t, err)
		if aff != 2 {
			t.Fatalf("affected = %d", aff)
		}
		list, err := users.FindAll(ctx, q.Sort("-age"), q.Limit(2))
		must(t, err)
		if len(list) != 2 || list[0].Age != 51 {
			t.Fatalf("sorted: %+v", list)
		}

		page, err := users.Paginate(ctx, 2, 3, q.Sort("id"))
		must(t, err)
		if page.Total != 4 || len(page.Items) != 1 || page.TotalPages != 2 {
			t.Fatalf("page: %+v", page)
		}

		var seen int
		must(t, users.Each(ctx, 3, func(User) error { seen++; return nil }))
		if seen != 4 {
			t.Fatalf("each saw %d", seen)
		}

		// Save = upsert-by-pk convenience.
		s := User{Name: "S", Email: "s@x"}
		must(t, users.Save(ctx, &s))
		s.Age = 7
		must(t, users.Save(ctx, &s))
		got, _ = users.FindByID(ctx, s.ID)
		if got.Age != 7 {
			t.Fatal("save update")
		}

		// Unique violation is classified.
		err = users.Insert(ctx, &User{Name: "dup", Email: "b@x"})
		if !orm.IsUniqueViolation(err) {
			t.Fatalf("want unique violation, got %v", err)
		}

		// Raw scanning.
		var names []string
		must(t, db.Query(ctx, &names, "SELECT name FROM users WHERE age > ? ORDER BY name", 40))
		if strings.Join(names, ",") != "C,D" {
			t.Fatalf("names = %v", names)
		}
	})
}

func TestSoftDelete(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		u := User{Name: "X", Email: "x@x"}
		must(t, users.Insert(ctx, &u))
		must(t, users.Delete(ctx, &u))
		if u.DeletedAt == nil {
			t.Fatal("deleted_at not set")
		}
		if _, err := users.FindByID(ctx, u.ID); !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("soft-deleted row visible: %v", err)
		}
		if _, err := users.FindByID(ctx, u.ID, q.Unscoped()); err != nil {
			t.Fatalf("unscoped: %v", err)
		}
		must(t, users.Restore(ctx, &u))
		if _, err := users.FindByID(ctx, u.ID); err != nil {
			t.Fatal("restore")
		}
		n, err := users.DeleteAny(ctx, q.Eq("email", "x@x"))
		must(t, err)
		if n != 1 {
			t.Fatalf("deleteany %d", n)
		}
		c, _ := users.Count(ctx)
		cu, _ := users.Count(ctx, q.Unscoped())
		if c != 0 || cu != 1 {
			t.Fatalf("counts %d %d", c, cu)
		}
		must(t, users.HardDelete(ctx, &u))
		cu, _ = users.Count(ctx, q.Unscoped())
		if cu != 0 {
			t.Fatal("hard delete")
		}
	})
}

func TestTransactions(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)

		err := db.Transaction(ctx, func(ctx context.Context) error {
			must(t, users.Insert(ctx, &User{Name: "T1", Email: "t1@x"}))
			return errors.New("boom")
		})
		if err == nil {
			t.Fatal("want error")
		}
		if n, _ := users.Count(ctx); n != 0 {
			t.Fatal("rollback failed")
		}

		// Nested: inner failure only undoes inner work.
		must(t, db.Transaction(ctx, func(ctx context.Context) error {
			must(t, users.Insert(ctx, &User{Name: "outer", Email: "o@x"}))
			inner := db.Transaction(ctx, func(ctx context.Context) error {
				must(t, users.Insert(ctx, &User{Name: "inner", Email: "i@x"}))
				return errors.New("inner")
			})
			if inner == nil {
				t.Fatal("inner should fail")
			}
			return nil
		}))
		names := []string{}
		all, _ := users.FindAll(ctx)
		for _, u := range all {
			names = append(names, u.Name)
		}
		if strings.Join(names, ",") != "outer" {
			t.Fatalf("after savepoint: %v", names)
		}

		// A failing After hook undoes the insert automatically.
		err = users.Insert(ctx, &User{Name: "fail-after", Email: "f@x"})
		if err == nil {
			t.Fatal("want hook error")
		}
		if ok, _ := users.Exists(ctx, q.Eq("email", "f@x")); ok {
			t.Fatal("after-hook failure should roll back")
		}

		// Panics roll back and propagate.
		func() {
			defer func() { recover() }()
			_ = db.Transaction(ctx, func(ctx context.Context) error {
				must(t, users.Insert(ctx, &User{Name: "P", Email: "p@x"}))
				panic("x")
			})
		}()
		if ok, _ := users.Exists(ctx, q.Eq("email", "p@x")); ok {
			t.Fatal("panic should roll back")
		}
	})
}

func TestPreload(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users, posts := orm.For[User](db), orm.For[Post](db)
		ann := User{Name: "Ann", Email: "ann@x"}
		bob := User{Name: "Bob", Email: "bob@x"}
		must(t, users.Insert(ctx, &ann))
		must(t, users.Insert(ctx, &bob))
		ps := []Post{{UserID: ann.ID, Title: "a1"}, {UserID: ann.ID, Title: "a2"}, {UserID: bob.ID, Title: "b1"}}
		must(t, posts.InsertAll(ctx, ps))
		must(t, db.InsertAll(ctx, []Comment{{PostID: ps[0].ID, Body: "c1"}, {PostID: ps[0].ID, Body: "c2"}, {PostID: ps[2].ID, Body: "c3"}}))
		must(t, db.Insert(ctx, &Profile{UserID: bob.ID, Bio: "bob bio"}))

		list, err := users.FindAll(ctx, q.Sort("id"))
		must(t, err)
		must(t, users.PreloadAll(ctx, list, "Posts.Comments", "Profile"))
		if len(list[0].Posts) != 2 || len(list[1].Posts) != 1 {
			t.Fatalf("posts: %d %d", len(list[0].Posts), len(list[1].Posts))
		}
		var a1 Post
		for _, p := range list[0].Posts {
			if p.Title == "a1" {
				a1 = p
			}
		}
		if len(a1.Comments) != 2 || len(list[1].Posts[0].Comments) != 1 {
			t.Fatalf("nested comments: %+v", list)
		}
		if list[0].Profile != nil || list[1].Profile == nil || list[1].Profile.Bio != "bob bio" {
			t.Fatalf("has_one profile: %+v %+v", list[0].Profile, list[1].Profile)
		}

		// belongs_to + filtered preload.
		p, err := posts.FindByID(ctx, ps[2].ID)
		must(t, err)
		must(t, posts.Preload(ctx, &p, "User"))
		if p.User == nil || p.User.Name != "Bob" {
			t.Fatalf("belongs_to: %+v", p.User)
		}
		must(t, db.Preload(ctx, &ann, "Posts", q.Eq("title", "a2")))
		if len(ann.Posts) != 1 || ann.Posts[0].Title != "a2" {
			t.Fatalf("filtered preload: %+v", ann.Posts)
		}
	})
}

func TestScopesUpsertAndSelect(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		must(t, users.InsertAll(ctx, []User{
			{TenantID: 1, Name: "a", Email: "a@x", Age: 10},
			{TenantID: 1, Name: "b", Email: "b@x", Age: 20},
			{TenantID: 2, Name: "c", Email: "c@x", Age: 30},
		}))

		t1 := users.Scope(q.Eq("tenant_id", 1))
		n, err := t1.Count(ctx)
		must(t, err)
		if n != 2 {
			t.Fatalf("scoped count %d", n)
		}
		if _, err := t1.Find(ctx, q.Eq("name", "c")); !errors.Is(err, orm.ErrNotFound) {
			t.Fatal("scope leak")
		}
		aff, err := t1.UpdateAny(ctx, nil, q.Set("age", 0))
		must(t, err)
		if aff != 2 {
			t.Fatalf("scoped update %d", aff)
		}

		if db.Dialect().Kind() != dialect.KindMSSQL {
			// Upsert on the unique email column.
			up := User{Name: "a-new", Email: "a@x", Age: 77}
			must(t, users.Insert(ctx, &up, orm.OnConflictUpdate([]string{"email"}, "name", "age")))
			got, err := users.Find(ctx, q.Eq("email", "a@x"))
			must(t, err)
			if got.Name != "a-new" || got.Age != 77 {
				t.Fatalf("upsert: %+v", got)
			}
			must(t, users.Insert(ctx, &User{Name: "ignored", Email: "b@x"}, orm.OnConflictDoNothing("email")))
			if n, _ := users.Count(ctx); n != 3 {
				t.Fatalf("do nothing inserted: %d", n)
			}
		}

		// Aggregate into a custom struct.
		type row struct {
			TenantID int64 `db:"tenant_id"`
			N        int64 `db:"n"`
		}
		var rows []row
		must(t, db.Select(ctx, &rows, users.Query().
			Select("tenant_id", "COUNT(*) AS n").Group("tenant_id").Sort("tenant_id")))
		if fmt.Sprint(rows) != "[{1 2} {2 1}]" {
			t.Fatalf("grouped: %v", rows)
		}
	})
}
