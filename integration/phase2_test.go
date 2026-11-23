package integration

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

func opsFunc(ops []schema.Op) migrate.Func {
	return func(s *schema.Schema) { s.Ops = append(s.Ops, ops...) }
}

func TestCompositeKey(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users, posts, tags := orm.For[User](db), orm.For[Post](db), orm.For[PostTag](db)
		u := User{Name: "a", Email: "a@x"}
		must(t, users.Insert(ctx, &u))
		p := Post{UserID: u.ID, Title: "p"}
		must(t, posts.Insert(ctx, &p))

		must(t, tags.InsertAll(ctx, []PostTag{{PostID: p.ID, Tag: "go", Weight: 1}, {PostID: p.ID, Tag: "sql", Weight: 2}}))
		got, err := tags.FindByPK(ctx, p.ID, "sql")
		must(t, err)
		if got.Weight != 2 {
			t.Fatalf("%+v", got)
		}
		got.Weight = 5
		must(t, tags.Update(ctx, &got))
		must(t, tags.Save(ctx, &PostTag{PostID: p.ID, Tag: "go", Weight: 9}))  // exists -> update
		must(t, tags.Save(ctx, &PostTag{PostID: p.ID, Tag: "new", Weight: 1})) // new -> insert
		g, _ := tags.FindByPK(ctx, p.ID, "go")
		s, _ := tags.FindByPK(ctx, p.ID, "sql")
		if g.Weight != 9 || s.Weight != 5 {
			t.Fatalf("go=%d sql=%d", g.Weight, s.Weight)
		}
		must(t, tags.Delete(ctx, &s))
		if n, _ := tags.Count(ctx); n != 2 {
			t.Fatalf("count %d", n)
		}
		if _, err := tags.FindByID(ctx, 1); err == nil || !strings.Contains(err.Error(), "composite") {
			t.Fatalf("FindByID on composite key: %v", err)
		}
	})
}

func TestChangeTracking(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		var stmts []string
		db2 := orm.Open(db.SQL(), db.Dialect(), orm.WithHook(func(_ context.Context, e orm.Event) { stmts = append(stmts, e.SQL) }))
		users2 := orm.For[User](db2)

		bio := "old"
		u := User{Name: "a", Email: "a@x", Bio: &bio, Prefs: map[string]any{"k": 1}}
		must(t, users.Insert(ctx, &u))

		cs := orm.Track(&u)
		if cs.HasChanges() {
			t.Fatal("fresh changeset has changes")
		}
		must(t, users2.UpdateChanged(ctx, &u, cs))
		if len(stmts) != 0 {
			t.Fatalf("no-op update ran SQL: %v", stmts)
		}

		*u.Bio = "new" // mutation through a pointer
		u.Prefs["k"] = 2
		u.Age = 33
		if got := strings.Join(cs.Changed(), ","); got != "Age,Bio,Prefs" {
			t.Fatalf("changed = %s", got)
		}
		must(t, users2.UpdateChanged(ctx, &u, cs))
		if len(stmts) != 1 || strings.Contains(stmts[0], "name") || !strings.Contains(stmts[0], "updated_at") {
			t.Fatalf("update sql: %v", stmts)
		}
		if cs.HasChanges() {
			t.Fatal("changeset not reset")
		}
		got, _ := users.FindByID(ctx, u.ID)
		if *got.Bio != "new" || got.Age != 33 || got.Name != "a" {
			t.Fatalf("%+v", got)
		}
		other := User{}
		if err := users.UpdateChanged(ctx, &other, cs); err == nil {
			t.Fatal("changeset for another record accepted")
		}
	})
}

func TestCascadeInsert(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users, posts := orm.For[User](db), orm.For[Post](db)

		u := User{
			Name: "Ann", Email: "ann@x",
			Profile: &Profile{Bio: "hi"},
			Posts: []Post{
				{Title: "one", Comments: []Comment{{Body: "c1"}, {Body: "c2"}}},
				{Title: "two"},
			},
		}
		must(t, users.Insert(ctx, &u, orm.Cascade()))
		if u.Profile.UserID != u.ID || u.Posts[0].UserID != u.ID || u.Posts[0].Comments[1].PostID != u.Posts[0].ID || u.Posts[0].Comments[1].ID == 0 {
			t.Fatalf("keys not linked: %+v", u)
		}
		back, _ := users.FindByID(ctx, u.ID)
		must(t, users.Preload(ctx, &back, "Posts.Comments", "Profile"))
		if len(back.Posts) != 2 || back.Profile == nil || len(back.Posts[0].Comments)+len(back.Posts[1].Comments) != 2 {
			t.Fatalf("reloaded: %+v", back)
		}

		// belongs_to: parent inserted first and its key copied.
		p := Post{Title: "orphan", User: &User{Name: "Bob", Email: "bob@x"}}
		must(t, posts.Insert(ctx, &p, orm.Cascade()))
		if p.User.ID == 0 || p.UserID != p.User.ID {
			t.Fatalf("belongs_to cascade: %+v", p)
		}

		// A failure anywhere rolls back everything.
		failing := Post{Title: "f", User: &User{Name: "y", Email: "ann@x"}}
		if err := posts.Insert(ctx, &failing, orm.Cascade()); !orm.IsUniqueViolation(err) {
			t.Fatalf("want unique violation, got %v", err)
		}
		if ok, _ := posts.Exists(ctx, q.Eq("title", "f")); ok {
			t.Fatal("cascade failure must roll back")
		}
	})
}

func TestCursorPagination(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		var batch []User
		for i := 0; i < 7; i++ {
			// Ages with ties to exercise the primary-key tie-breaker.
			batch = append(batch, User{Name: string(rune('a' + i)), Email: string(rune('a'+i)) + "@x", Age: i / 2})
		}
		must(t, users.InsertAll(ctx, batch))

		names := func(items []User) string {
			var s []string
			for _, u := range items {
				s = append(s, u.Name)
			}
			return strings.Join(s, "")
		}
		for _, tc := range []struct {
			sort  string
			pages []string
		}{
			{"", []string{"abc", "def", "g"}},
			{"-age", []string{"gfe", "dcb", "a"}},
			{"age", []string{"abc", "def", "g"}},
		} {
			o := orm.CursorOpts{Limit: 3, Sort: tc.sort}
			var pages []orm.CursorPage[User]
			for {
				p, err := users.Cursor(ctx, o)
				must(t, err)
				pages = append(pages, p)
				if !p.HasNext {
					break
				}
				o.After = p.Next
			}
			var got []string
			for _, p := range pages {
				got = append(got, names(p.Items))
			}
			if strings.Join(got, "|") != strings.Join(tc.pages, "|") {
				t.Fatalf("sort %q forward: %v", tc.sort, got)
			}
			if pages[0].HasPrev || !pages[len(pages)-1].HasPrev {
				t.Fatalf("sort %q: HasPrev flags wrong", tc.sort)
			}
			// Walk back from the last page using Prev.
			back := []string{names(pages[len(pages)-1].Items)}
			cur := pages[len(pages)-1]
			for cur.HasPrev {
				p, err := users.Cursor(ctx, orm.CursorOpts{Limit: 3, Sort: tc.sort, Before: cur.Prev})
				must(t, err)
				back = append([]string{names(p.Items)}, back...)
				cur = p
			}
			if strings.Join(back, "|") != strings.Join(tc.pages, "|") {
				t.Fatalf("sort %q backward: %v", tc.sort, back)
			}
		}
		if _, err := users.Cursor(ctx, orm.CursorOpts{After: "garbage!"}); !errors.Is(err, orm.ErrBadCursor) {
			t.Fatalf("bad cursor: %v", err)
		}
	})
}

func TestReadReplicas(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	openFile := func(name string) *sql.DB {
		s, err := sql.Open("sqlite", "file:"+filepath.Join(dir, name)+"?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		s.SetMaxOpenConns(1)
		t.Cleanup(func() { s.Close() })
		return s
	}
	primary, replica := openFile("primary.db"), openFile("replica.db")
	// Same schema on both, but the replica "lags" (never receives writes).
	for _, s := range []*sql.DB{primary, replica} {
		_, err := migrate.New(orm.Open(s, dialect.SQLite()), migrations()).Up(ctx)
		must(t, err)
	}
	db := orm.Open(primary, dialect.SQLite(), orm.WithReplicas(replica))
	users := orm.For[User](db)
	u := User{Name: "a", Email: "a@x"}
	must(t, users.Insert(ctx, &u))

	if _, err := users.FindByID(ctx, u.ID); !errors.Is(err, orm.ErrNotFound) {
		t.Fatalf("read should hit the (empty) replica, got %v", err)
	}
	if _, err := users.FindByID(orm.UsePrimary(ctx), u.ID); err != nil {
		t.Fatalf("UsePrimary: %v", err)
	}
	must(t, db.Transaction(ctx, func(ctx context.Context) error {
		_, err := users.FindByID(ctx, u.ID)
		return err
	}))
}

func TestInspectAndDiff(t *testing.T) {
	models := []any{User{}, Post{}, Comment{}, Profile{}, PostTag{}}
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()

		// The migration history matches the models...
		res, err := migrate.DiffMigrations(migrations(), models...)
		must(t, err)
		if !res.Empty() {
			t.Fatalf("history diff:\n%s", strings.Join(migrate.RenderGo(res.Up), "\n"))
		}

		// ...and so does the live database.
		st, err := migrate.Inspect(ctx, db)
		must(t, err)
		if st.Tables["users"] == nil || st.Tables["users"].Column("id").Type != "id" {
			t.Fatalf("inspect users: %+v", st.Tables["users"])
		}
		res, err = migrate.DiffFor(db.Dialect(), st, models...)
		must(t, err)
		if !res.Empty() {
			t.Fatalf("live diff:\n%s\nnotes: %v", strings.Join(migrate.RenderGo(res.Up), "\n"), res.Notes)
		}

		// A model change produces a migration that, once applied, leaves no diff.
		type Profile struct {
			ID       int64
			UserID   int64  `schema:"null,references:users"`
			Bio      string `schema:"null"`
			Website  string `schema:"limit:200"`
			Verified *time.Time
		}
		res, err = migrate.DiffFor(db.Dialect(), st, User{}, Post{}, Comment{}, Profile{}, PostTag{})
		must(t, err)
		if len(res.Up) != 2 {
			t.Fatalf("expected 2 ops, got:\n%s", strings.Join(migrate.RenderGo(res.Up), "\n"))
		}
		up := migrate.New(db, append(migrations(), migrate.Migration{Version: 20990101000000, Name: "auto", Up: opsFunc(res.Up), Down: opsFunc(res.Down)}))
		_, err = up.Up(ctx)
		must(t, err)
		st2, err := migrate.Inspect(ctx, db)
		must(t, err)
		again, err := migrate.DiffFor(db.Dialect(), st2, User{}, Post{}, Comment{}, Profile{}, PostTag{})
		must(t, err)
		if !again.Empty() {
			t.Fatalf("after applying: %s", strings.Join(migrate.RenderGo(again.Up), "\n"))
		}
		_, err = up.Down(ctx, 1)
		must(t, err)
		st3, _ := migrate.Inspect(ctx, db)
		if st3.Tables["profiles"].Column("website") != nil {
			t.Fatal("down did not drop website")
		}
	})
}

func TestManyToMany(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users, roles := orm.For[User](db), orm.For[Role](db)
		names := func(rs []Role) string {
			var out []string
			for _, r := range rs {
				out = append(out, r.Name)
			}
			return strings.Join(out, ",")
		}
		joinRows := func() int64 {
			var n int64
			must(t, db.Query(ctx, &n, "SELECT COUNT(*) FROM user_roles"))
			return n
		}

		// Cascade inserts the new roles and links them.
		ann := User{Name: "Ann", Email: "ann@m2m", Roles: []Role{{Name: "editor"}, {Name: "admin"}}}
		must(t, users.Insert(ctx, &ann, orm.Cascade()))
		if ann.Roles[0].ID == 0 || joinRows() != 2 {
			t.Fatalf("cascade: roles=%+v join rows=%d", ann.Roles, joinRows())
		}
		admin, editor := ann.Roles[1], ann.Roles[0]

		// Link is idempotent and accepts records or bare keys.
		bob := User{Name: "Bob", Email: "bob@m2m"}
		must(t, users.Insert(ctx, &bob))
		must(t, users.Link(ctx, &bob, "Roles", &admin))
		must(t, users.Link(ctx, &bob, "Roles", admin, editor.ID))
		if joinRows() != 4 {
			t.Fatalf("join rows = %d, want 4", joinRows())
		}
		cy := User{Name: "Cy", Email: "cy@m2m"}
		must(t, users.Insert(ctx, &cy))

		// Preload, with ordering applied to the targets.
		list := []User{ann, bob, cy}
		for i := range list {
			list[i].Roles = nil
		}
		must(t, db.Preload(ctx, &list, "Roles", q.Sort("name")))
		if got := names(list[0].Roles) + "|" + names(list[1].Roles) + "|" + names(list[2].Roles); got != "admin,editor|admin,editor|" {
			t.Fatalf("preload = %q", got)
		}
		if list[2].Roles == nil {
			t.Fatal("owners without links should get an empty slice")
		}

		// Unlink one, then all; the roles themselves remain.
		must(t, users.Unlink(ctx, &ann, "Roles", &admin))
		must(t, users.Unlink(ctx, &bob, "Roles"))
		must(t, users.Preload(ctx, &ann, "Roles"))
		must(t, users.Preload(ctx, &bob, "Roles"))
		if names(ann.Roles) != "editor" || len(bob.Roles) != 0 {
			t.Fatalf("after unlink: ann=%q bob=%q", names(ann.Roles), names(bob.Roles))
		}
		if n, _ := roles.Count(ctx); n != 2 {
			t.Fatalf("roles = %d", n)
		}

		// Errors.
		if err := users.Link(ctx, &ann, "Posts", &admin); err == nil {
			t.Fatal("Link on a has_many association should fail")
		}
		if err := users.Link(ctx, &User{}, "Roles", &admin); err == nil {
			t.Fatal("Link from an unsaved owner should fail")
		}
		if err := users.Link(ctx, &ann, "Roles", &Role{Name: "new"}); err == nil {
			t.Fatal("Link to an unsaved target should fail")
		}
	})
}
