package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

func TestJSONFilters(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		for i, p := range []map[string]any{
			{"theme": "dark", "max": 10, "beta": true, "items": []any{map[string]any{"sku": "A-1"}}, "odd key": "x"},
			{"theme": "light", "max": 3, "beta": false},
			{"theme": "dark", "max": 25, "note": nil},
			nil,
		} {
			must(t, users.Insert(ctx, &User{Name: fmt.Sprint(i), Email: fmt.Sprintf("json%d@x.io", i), Prefs: p}))
		}
		names := func(fs ...q.Querier) string {
			t.Helper()
			list, err := users.FindAll(ctx, fs...)
			must(t, err)
			var n []string
			for _, u := range list {
				n = append(n, u.Name)
			}
			sort.Strings(n)
			return strings.Join(n, ",")
		}
		check := func(name, got, want string) {
			t.Helper()
			if got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		check("Eq text", names(q.JSON("prefs", "theme").Eq("dark")), "0,2")
		check("Gt num", names(q.JSON("prefs", "max").Gt(5)), "0,2")
		check("Lte num", names(q.JSON("prefs", "max").Lte(10)), "0,1")
		check("Eq bool", names(q.JSON("prefs", "beta").Eq(true)), "0")
		check("Eq false", names(q.JSON("prefs", "beta").Eq(false)), "1")
		check("In", names(q.JSON("prefs", "theme").In("light", "blue")), "1")
		check("array index", names(q.JSON("prefs", "items", "0", "sku").Eq("A-1")), "0")
		check("odd key", names(q.JSON("prefs", "odd key").Eq("x")), "0")
		check("IsNull", names(q.JSON("prefs", "note").IsNull()), "0,1,2,3")
		check("NotNull", names(q.JSON("prefs", "beta").NotNull()), "0,1")
		check("injection-proof key", names(q.JSON("prefs", `x') OR 1=1 --`).Eq("a")), "")
		if db.Dialect().Kind() == "postgres" || db.Dialect().Kind() == "mysql" {
			check("Contains", names(q.JSONContains("prefs", map[string]any{"theme": "dark", "max": 25})), "2")
		} else if _, err := users.FindAll(ctx, q.JSONContains("prefs", map[string]any{"a": 1})); err == nil {
			t.Error("JSONContains should be refused here")
		}
	})
}
