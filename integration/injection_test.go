package integration

import (
	"context"
	"testing"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// TestIdentifierInjection: sort fields and SET targets often come from
// request parameters (?sort=, PATCH bodies); SQL in them must be refused.
func TestIdentifierInjection(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		must(t, users.Insert(ctx, &User{TenantID: 1, Name: "victim", Email: "v@x.io"}))
		evil := []string{
			"(SELECT 1)",
			"-(SELECT email FROM users LIMIT 1)",
			"name; DROP TABLE posts --",
			"name) OR 1=1 --",
			`name" ; DROP TABLE posts; --`,
			"name` ; DROP TABLE posts; --",
			"name = (SELECT email FROM users LIMIT 1), age",
		}
		for _, e := range evil {
			if _, err := users.FindAll(ctx, q.Sort(e)); err == nil {
				t.Errorf("q.Sort(%q) ran", e)
			}
			if _, err := users.Cursor(ctx, orm.CursorOpts{Sort: e}); err == nil {
				t.Errorf("Cursor sort %q ran", e)
			}
			if _, err := users.UpdateAny(ctx, q.Eq("name", "victim"), q.Set(e, "x")); err == nil {
				t.Errorf("q.Set(%q) ran", e)
			}
		}
		var n int64
		must(t, db.Query(ctx, &n, "SELECT COUNT(*) FROM posts"))
		got, err := users.Find(ctx, q.Eq("name", "victim"))
		must(t, err)
		if got.Email != "v@x.io" {
			t.Fatalf("row changed: %+v", got)
		}
		// Plain names, qualified names and descending still work.
		if _, err := users.FindAll(ctx, q.Sort("-users.name", "id")); err != nil {
			t.Fatal(err)
		}
	})
}
