package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/tenant"
)

// TestRelationEdges probes preload, many-to-many and composite keys with
// empty, duplicate, nested and filtered inputs.
func TestRelationEdges(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users, posts, roles, tags := orm.For[User](db), orm.For[Post](db), orm.For[Role](db), orm.For[PostTag](db)
		probe := func(name string, fn func() error) {
			t.Helper()
			if err := fn(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		mk := func(name string) User {
			u := User{TenantID: 1, Name: name, Email: name + "@x.io"}
			must(t, users.Insert(ctx, &u))
			return u
		}
		a, b, c := mk("a"), mk("b"), mk("c")
		for i, u := range []User{a, a, b} {
			must(t, posts.Insert(ctx, &Post{UserID: u.ID, Title: fmt.Sprint("p", i), Views: i}))
		}
		admin, editor := Role{Name: "admin"}, Role{Name: "editor"}
		must(t, roles.Insert(ctx, &admin))
		must(t, roles.Insert(ctx, &editor))

		probe("PreloadAll on an empty slice", func() error { return users.PreloadAll(ctx, []User{}, "Posts") })
		probe("PreloadAll: children land on the right parent, empty is non-nil-safe", func() error {
			list, err := users.FindAll(ctx, q.In("id", a.ID, b.ID, c.ID), q.Sort("id"))
			if err != nil {
				return err
			}
			if err := users.PreloadAll(ctx, list, "Posts"); err != nil {
				return err
			}
			if len(list[0].Posts) != 2 || len(list[1].Posts) != 1 || len(list[2].Posts) != 0 {
				return fmt.Errorf("%d %d %d", len(list[0].Posts), len(list[1].Posts), len(list[2].Posts))
			}
			for _, p := range list[0].Posts {
				if p.UserID != a.ID {
					return fmt.Errorf("post %d on wrong user", p.ID)
				}
			}
			return nil
		})
		probe("PreloadAll with the same parent twice", func() error {
			x, _ := users.FindByID(ctx, a.ID)
			list := []User{x, x}
			if err := users.PreloadAll(ctx, list, "Posts"); err != nil {
				return err
			}
			if len(list[0].Posts) != 2 || len(list[1].Posts) != 2 {
				return fmt.Errorf("%d %d", len(list[0].Posts), len(list[1].Posts))
			}
			return nil
		})
		probe("Preload twice does not duplicate children", func() error {
			x, _ := users.FindByID(ctx, a.ID)
			_ = users.Preload(ctx, &x, "Posts")
			if err := users.Preload(ctx, &x, "Posts"); err != nil {
				return err
			}
			if len(x.Posts) != 2 {
				return fmt.Errorf("%d posts", len(x.Posts))
			}
			return nil
		})
		probe("filtered and sorted preload", func() error {
			x, _ := users.FindByID(ctx, a.ID)
			if err := db.Preload(ctx, &x, "Posts", q.Gt("views", 0), q.Sort("-id")); err != nil {
				return err
			}
			if len(x.Posts) != 1 || x.Posts[0].Views != 1 {
				return fmt.Errorf("%+v", x.Posts)
			}
			return nil
		})
		probe("nested preload Posts.User", func() error {
			x, _ := users.FindByID(ctx, a.ID)
			if err := users.Preload(ctx, &x, "Posts.User"); err != nil {
				return err
			}
			for _, p := range x.Posts {
				if p.User == nil || p.User.ID != a.ID {
					return fmt.Errorf("post %d user %+v", p.ID, p.User)
				}
			}
			return nil
		})
		probe("unknown preload path is an error", func() error {
			x, _ := users.FindByID(ctx, a.ID)
			if err := users.Preload(ctx, &x, "Nope"); err == nil {
				return errors.New("no error")
			}
			return nil
		})
		probe("Link: duplicates and re-link are harmless", func() error {
			if err := users.Link(ctx, &a, "Roles", &admin, &admin); err != nil {
				return fmt.Errorf("dup in one call: %w", err)
			}
			if err := users.Link(ctx, &a, "Roles", &admin, &editor); err != nil {
				return fmt.Errorf("re-link: %w", err)
			}
			x, _ := users.FindByID(ctx, a.ID)
			if err := users.Preload(ctx, &x, "Roles"); err != nil {
				return err
			}
			if names(x.Roles) != "admin,editor" {
				return fmt.Errorf("roles %s", names(x.Roles))
			}
			return nil
		})
		probe("Link by id values and Unlink of a missing link", func() error {
			if err := users.Link(ctx, &b, "Roles", editor.ID); err != nil {
				return err
			}
			if err := users.Unlink(ctx, &b, "Roles", admin.ID); err != nil {
				return fmt.Errorf("unlink missing: %w", err)
			}
			if err := users.Unlink(ctx, &a, "Roles", &admin); err != nil {
				return err
			}
			list, _ := users.FindAll(ctx, q.In("id", a.ID, b.ID, c.ID), q.Sort("id"))
			if err := users.PreloadAll(ctx, list, "Roles"); err != nil {
				return err
			}
			if names(list[0].Roles) != "editor" || names(list[1].Roles) != "editor" || len(list[2].Roles) != 0 {
				return fmt.Errorf("%s / %s / %s", names(list[0].Roles), names(list[1].Roles), names(list[2].Roles))
			}
			return nil
		})
		probe("Link with no targets", func() error { return users.Link(ctx, &a, "Roles") })
		probe("Link on an unknown association is an error", func() error {
			if err := users.Link(ctx, &a, "Posts", &admin); err == nil {
				return errors.New("no error")
			}
			return nil
		})
		probe("composite key: FindByPK, Update, Save, Delete", func() error {
			p := Post{UserID: a.ID, Title: "tagged"}
			if err := posts.Insert(ctx, &p); err != nil {
				return err
			}
			t1 := PostTag{PostID: p.ID, Tag: "go", Weight: 1}
			if err := tags.Insert(ctx, &t1); err != nil {
				return err
			}
			if err := tags.Insert(ctx, &PostTag{PostID: p.ID, Tag: "db", Weight: 2}); err != nil {
				return err
			}
			got, err := tags.FindByPK(ctx, p.ID, "go")
			if err != nil || got.Weight != 1 {
				return fmt.Errorf("FindByPK %+v %v", got, err)
			}
			if _, err := tags.FindByPK(ctx, p.ID); err == nil {
				return errors.New("FindByPK with one of two keys accepted")
			}
			got.Weight = 5
			if err := tags.Update(ctx, &got); err != nil {
				return err
			}
			other, _ := tags.FindByPK(ctx, p.ID, "db")
			if other.Weight != 2 {
				return fmt.Errorf("update touched the other row: %+v", other)
			}
			s := PostTag{PostID: p.ID, Tag: "go", Weight: 7}
			if err := tags.Save(ctx, &s); err != nil {
				return fmt.Errorf("save existing: %w", err)
			}
			if err := tags.Save(ctx, &PostTag{PostID: p.ID, Tag: "new", Weight: 1}); err != nil {
				return fmt.Errorf("save new: %w", err)
			}
			n, _ := tags.Count(ctx, q.Eq("post_id", p.ID))
			if n != 3 {
				return fmt.Errorf("count %d", n)
			}
			if err := tags.Delete(ctx, &s); err != nil {
				return err
			}
			if _, err := tags.FindByPK(ctx, p.ID, "db"); err != nil {
				return fmt.Errorf("delete removed the sibling: %v", err)
			}
			return nil
		})
		probe("composite key: Each gives a clear error", func() error {
			err := tags.Each(ctx, 1, func(PostTag) error { return nil })
			if err == nil {
				return nil // supported
			}
			if !strings.Contains(err.Error(), "composite") {
				return err
			}
			return nil
		})
		probe("tenant: preload children of another tenant are hidden", func() error {
			tdb := orm.Open(db.SQL(), db.Dialect(), tenant.Plugin("tenant_id"))
			tu, tp := orm.For[User](tdb), orm.For[Post](tdb)
			t1, t2 := tenant.With(ctx, int64(91)), tenant.With(ctx, int64(92))
			owner := User{Name: "o", Email: "o@t.io"}
			if err := tu.Insert(t1, &owner); err != nil {
				return err
			}
			if err := tp.Insert(t1, &Post{UserID: owner.ID, Title: "x"}); err != nil {
				return err
			}
			p, _ := tp.FindAll(t1, q.Eq("user_id", owner.ID))
			if err := tp.Preload(t2, &p[0], "User"); err != nil {
				return err
			}
			if p[0].User != nil {
				return errors.New("tenant 92 preloaded tenant 91's user")
			}
			return nil
		})
	})
}

func names(rs []Role) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Name)
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}
