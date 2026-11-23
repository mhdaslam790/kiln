package ormtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/ormtest"
	"github.com/mhdaslam790/kiln/q"
)

type User struct {
	ID    int64
	Email string
}

// Service under test: depends only on interfaces.
type Signup struct {
	Users orm.Repository[User]
	Tx    orm.Transactor
}

var errTaken = errors.New("email taken")

func (s Signup) Register(ctx context.Context, email string) (User, error) {
	var u User
	err := s.Tx.Transaction(ctx, func(ctx context.Context) error {
		taken, err := s.Users.Exists(ctx, q.Eq("email", email))
		if err != nil {
			return err
		}
		if taken {
			return errTaken
		}
		u = User{Email: email}
		return s.Users.Insert(ctx, &u)
	})
	return u, err
}

func TestMockHappyPath(t *testing.T) {
	users := ormtest.New[User](t)
	users.ExpectExists(q.Eq("email", "a@b.c")).Return(false)
	users.ExpectInsert().Match(func(u *User) bool { return u.Email == "a@b.c" }).Run(func(u *User) { u.ID = 42 })

	u, err := Signup{Users: users, Tx: ormtest.Tx{}}.Register(context.Background(), "a@b.c")
	if err != nil || u.ID != 42 {
		t.Fatalf("got %+v, %v", u, err)
	}
}

func TestMockReturnsAndErrors(t *testing.T) {
	users := ormtest.New[User](t)
	users.ExpectExists(ormtest.Any).Return(true)
	if _, err := (Signup{Users: users, Tx: ormtest.Tx{}}).Register(context.Background(), "x"); !errors.Is(err, errTaken) {
		t.Fatalf("err = %v", err)
	}

	users.ExpectFindByID(int64(7)).NotFound()
	if _, err := users.FindByID(context.Background(), int64(7)); !errors.Is(err, orm.ErrNotFound) {
		t.Fatal(err)
	}
	users.ExpectFindAll(q.Sort("-id"), q.Limit(2)).Return([]User{{ID: 2}, {ID: 1}})
	list, _ := users.FindAll(context.Background(), q.Sort("-id"), q.Limit(2))
	if len(list) != 2 {
		t.Fatal(list)
	}
	users.ExpectCount().Return(3) // int converts to int64
	if n, _ := users.Count(context.Background()); n != 3 {
		t.Fatal(n)
	}
	users.ExpectUpdate().Times(2)
	users.ExpectUpdate("Email")
	u := User{ID: 1}
	_ = users.Update(context.Background(), &u, "Email")
	_ = users.Update(context.Background(), &u)
	_ = users.Update(context.Background(), &u, "ID")
}

// fakeT records failures so we can test that the mock reports problems.
type fakeT struct {
	testing.TB
	errs []string
	cl   []func()
}

func (f *fakeT) Helper()                        {}
func (f *fakeT) Errorf(format string, a ...any) { f.errs = append(f.errs, format) }
func (f *fakeT) Cleanup(fn func())              { f.cl = append(f.cl, fn) }

func TestMockReportsProblems(t *testing.T) {
	ft := &fakeT{TB: t}
	users := ormtest.New[User](ft)
	users.ExpectFind(q.Eq("id", 1))
	users.ExpectDelete()
	if _, err := users.Find(context.Background(), q.Eq("id", 2)); !errors.Is(err, ormtest.ErrUnexpected) {
		t.Fatalf("want unexpected, got %v", err)
	}
	for _, fn := range ft.cl {
		fn()
	}
	if len(ft.errs) != 3 { // unexpected call + 2 unmet expectations
		t.Fatalf("errors = %d: %v", len(ft.errs), ft.errs)
	}
}

func TestMockLink(t *testing.T) {
	users := ormtest.New[User](t)
	users.ExpectLink("Roles").Run(func(u *User) { u.Email = "linked" })
	users.ExpectUnlink("Roles").Error(errTaken)
	u := User{ID: 1}
	if err := users.Link(context.Background(), &u, "Roles", 7); err != nil || u.Email != "linked" {
		t.Fatalf("link: %v %+v", err, u)
	}
	if err := users.Unlink(context.Background(), &u, "Roles"); !errors.Is(err, errTaken) {
		t.Fatalf("unlink: %v", err)
	}
}
