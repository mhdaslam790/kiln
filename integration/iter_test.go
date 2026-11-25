package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// TestIter walks every row once, in the requested order, in pages smaller
// than the table, with filters, sorts (NULLs included), limits and early
// breaks; the loop body may query.
func TestIter(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		names := []string{"e", "a", "d", "b", "c", "a", "f"}
		for i, n := range names {
			must(t, users.Insert(ctx, &User{Name: n, Age: i % 3, Email: fmt.Sprintf("iter%d@x.io", i)}))
		}
		collect := func(qs ...q.Querier) (got []string) {
			t.Helper()
			for u, err := range users.Iter(ctx, qs...) {
				must(t, err)
				got = append(got, u.Name)
			}
			return got
		}
		each := func(size int, qs ...q.Querier) (got []string) {
			t.Helper()
			must(t, users.Each(ctx, size, func(u User) error { got = append(got, u.Name); return nil }, qs...))
			return got
		}
		sorted := slices.Clone(names)
		slices.Sort(sorted)
		rev := slices.Clone(sorted)
		slices.Reverse(rev)

		if got := collect(); !slices.Equal(got, names) {
			t.Errorf("by primary key: %v", got)
		}
		if got := each(2, q.Sort("name")); !slices.Equal(got, sorted) {
			t.Errorf("Each sorted, pages of 2: %v want %v", got, sorted)
		}
		if got := each(2, q.Sort("-name")); !slices.Equal(got, rev) {
			t.Errorf("Each sorted desc: %v want %v", got, rev)
		}
		if got := each(3, q.Sort("name"), q.Limit(4)); !slices.Equal(got, sorted[:4]) {
			t.Errorf("Each limit 4: %v", got)
		}
		if got := each(2, q.Eq("age", 1), q.Sort("-name")); !slices.Equal(got, []string{"c", "a"}) {
			t.Errorf("Each filtered: %v", got)
		}
		if got := collect(q.Sort("users.name")); !slices.Equal(got, sorted) {
			t.Errorf("Iter sorted: %v", got)
		}

		// Early break stops reading; the body can run queries.
		n := 0
		for u, err := range users.Iter(ctx) {
			must(t, err)
			if _, err := users.FindByID(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			if n++; n == 2 {
				break
			}
		}
		if n != 2 {
			t.Errorf("break: %d", n)
		}

		// Unsupported combinations are errors, not wrong results.
		for _, qs := range [][]q.Querier{{q.Offset(2)}, {q.Sort("name", "age")}, {q.Sort("nope")}} {
			var got error
			for _, err := range users.Iter(ctx, qs...) {
				got = err
			}
			if got == nil {
				t.Errorf("Iter(%v) gave no error", qs)
			}
		}
		stop := errors.New("stop")
		if err := users.Each(ctx, 2, func(User) error { return stop }); !errors.Is(err, stop) {
			t.Errorf("Each error: %v", err)
		}
	})
}

// TestIterNullSort walks a nullable sort column across page boundaries.
func TestIterNullSort(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		ctx := context.Background()
		users := orm.For[User](db)
		bio := func(s string) *string { return &s }
		bios := []*string{bio("b"), nil, bio("a"), nil, bio("c"), nil, bio("a")}
		for i, b := range bios {
			must(t, users.Insert(ctx, &User{Name: fmt.Sprint(i), Bio: b, Email: fmt.Sprintf("itern%d@x.io", i)}))
		}
		for _, sort := range []string{"bio", "-bio"} {
			seen := map[int64]int{}
			var order []string
			must(t, users.Each(ctx, 2, func(u User) error {
				seen[u.ID]++
				if u.Bio == nil {
					order = append(order, "-")
				} else {
					order = append(order, *u.Bio)
				}
				return nil
			}, q.Sort(sort)))
			if len(seen) != 7 {
				t.Errorf("%s: saw %d of 7 rows: %v", sort, len(seen), order)
			}
			want := []string{"a", "a", "b", "c", "-", "-", "-"}
			if sort == "-bio" {
				want = []string{"-", "-", "-", "c", "b", "a", "a"}
			}
			if !slices.Equal(order, want) {
				t.Errorf("%s: order %v want %v", sort, order, want)
			}
		}
	})
}
