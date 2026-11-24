package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mhdaslam790/kiln/kilntest"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

func TestKilntest(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		users := orm.For[User](db)
		kilntest.Migrate(t, db, migrations()) // already applied: a no-op
		kilntest.Migrate(t, db, migrations()) // nothing pending: still fine

		factory := kilntest.NewFactory(db, func(n int) User {
			return User{Name: fmt.Sprintf("kt %d", n), Email: fmt.Sprintf("KT-%d@test.dev", n), Age: 30}
		})
		count := func(t *testing.T, ctx context.Context) int64 {
			t.Helper()
			n, err := users.Count(ctx, q.Like("email", "kt-%"))
			must(t, err)
			return n
		}

		t.Run("rolled back", func(t *testing.T) {
			ctx := kilntest.Tx(t, db)
			if !db.InTransaction(ctx) {
				t.Fatal("ctx carries no transaction")
			}

			u := factory.Create(t, ctx)
			if u.ID == 0 || u.Email != fmt.Sprintf("kt-%d@test.dev", 1) {
				t.Fatalf("created %+v", u) // id filled, BeforeInsert hook ran
			}
			admin := factory.Create(t, ctx, func(u *User) { u.Name = "admin" })
			if admin.Name != "admin" || admin.Email == u.Email {
				t.Fatalf("override: %+v", admin)
			}
			list := factory.CreateMany(t, ctx, 5, func(u *User) { u.Age = 7 })
			for _, x := range list {
				if x.ID == 0 || x.Age != 7 {
					t.Fatalf("CreateMany: %+v", x)
				}
			}
			if n := count(t, ctx); n != 7 {
				t.Fatalf("count in test tx = %d, want 7", n)
			}

			// Code under test using Transaction gets a savepoint.
			boom := errors.New("boom")
			err := db.Transaction(ctx, func(ctx context.Context) error {
				factory.Create(t, ctx)
				return boom
			})
			if !errors.Is(err, boom) {
				t.Fatalf("nested err = %v", err)
			}
			if n := count(t, ctx); n != 7 {
				t.Fatalf("failed nested block kept its row: count = %d", n)
			}
			must(t, db.Transaction(ctx, func(ctx context.Context) error {
				factory.Create(t, ctx)
				return nil
			}))
			if n := count(t, ctx); n != 8 {
				t.Fatalf("count after nested commit = %d, want 8", n)
			}

			if _, err := db.Begin(ctx, nil); !errors.Is(err, orm.ErrNestedBegin) {
				t.Fatalf("Begin in tx: %v", err)
			}
		})
		if n := count(t, context.Background()); n != 0 {
			t.Fatalf("rows leaked out of the test transaction: %d", n)
		}

		if b := factory.Build(); b.ID != 0 || b.Email != "KT-10@test.dev" {
			t.Fatalf("Build: %+v", b) // not inserted; sequence continues
		}
		if got := factory.CreateMany(t, context.Background(), 0); len(got) != 0 {
			t.Fatalf("CreateMany(0) = %v", got)
		}
	})
}

func TestBeginCommit(t *testing.T) {
	forEach(t, func(t *testing.T, db *orm.DB) {
		users := orm.For[User](db)
		tx, err := db.Begin(context.Background(), nil)
		must(t, err)
		u := User{Name: "begin", Email: "begin@test.dev"}
		must(t, users.Insert(tx.Context(), &u))
		must(t, tx.Commit())
		if err := tx.Rollback(); err == nil {
			t.Fatal("Rollback after Commit succeeded")
		}

		got, err := users.FindByID(context.Background(), u.ID)
		must(t, err)
		if got.Email != "begin@test.dev" {
			t.Fatalf("committed row: %+v", got)
		}
		must(t, users.HardDelete(context.Background(), &got))
	})
}
