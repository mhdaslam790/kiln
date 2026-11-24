package kilnvalidate_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-playground/validator/v10"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/kilnvalidate"
	"github.com/mhdaslam790/kiln/orm"
)

// fakeConn accepts every statement and counts them.
type fakeConn struct{ n *atomic.Int64 }

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }
func (c fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.n.Add(1)
	return driver.RowsAffected(1), nil
}

type connector struct{ n *atomic.Int64 }

func (c connector) Connect(context.Context) (driver.Conn, error) { return fakeConn(c), nil }
func (connector) Driver() driver.Driver                          { return nil }

type User struct {
	ID    int64  `db:"id,primary,noauto"`
	Name  string `validate:"required,max=10"`
	Email string `db:"email_address" validate:"required,email"`
	Age   int    `validate:"gte=0,lte=130"`
	Bio   string
}

func open(t *testing.T, opts ...kilnvalidate.Option) (*orm.DB, *atomic.Int64) {
	n := new(atomic.Int64)
	sqldb := sql.OpenDB(connector{n})
	t.Cleanup(func() { sqldb.Close() })
	return orm.Open(sqldb, dialect.Postgres(), kilnvalidate.Plugin(opts...)), n
}

func TestValidate(t *testing.T) {
	ctx := context.Background()
	db, stmts := open(t)
	users := orm.For[User](db)

	bad := User{ID: 1, Name: "far too long a name", Email: "nope", Age: 30}
	err := users.Insert(ctx, &bad)
	var verr *kilnvalidate.Error
	if !errors.As(err, &verr) || !errors.Is(err, kilnvalidate.ErrInvalid) {
		t.Fatalf("Insert: %v", err)
	}
	if len(verr.Fields) != 2 {
		t.Fatalf("fields: %+v", verr.Fields)
	}
	email := verr.Fields[1]
	if email.Field != "Email" || email.Column != "email_address" || email.Tag != "email" || email.Value != "nope" {
		t.Errorf("email error: %+v", email)
	}
	want := "kilnvalidate: insert users: name must be at most 10; email_address must be a valid email address"
	if err.Error() != want {
		t.Errorf("message:\n got %q\nwant %q", err.Error(), want)
	}
	if stmts.Load() != 0 {
		t.Fatalf("an invalid record reached the database (%d statements)", stmts.Load())
	}

	good := User{ID: 2, Name: "ann", Email: "ann@x.io", Age: 30}
	if err := users.Insert(ctx, &good); err != nil || stmts.Load() != 1 {
		t.Fatalf("valid insert: %v (%d statements)", err, stmts.Load())
	}

	// A full update checks everything; a partial one only what it writes.
	good.Email = ""
	if err := users.Update(ctx, &good); !errors.Is(err, kilnvalidate.ErrInvalid) {
		t.Fatalf("full update with empty email: %v", err)
	}
	if err := users.Update(ctx, &good, "Bio", "Age"); err != nil {
		t.Fatalf("partial update not writing Email must pass: %v", err)
	}
	if err := users.Update(ctx, &good, "Email"); !errors.Is(err, kilnvalidate.ErrInvalid) {
		t.Fatalf("partial update writing an empty Email: %v", err)
	}

	// Bulk inserts check every record.
	err = users.InsertAll(ctx, []User{{ID: 3, Name: "a", Email: "a@x.io"}, {ID: 4, Name: "b", Email: "b"}})
	if !errors.Is(err, kilnvalidate.ErrInvalid) {
		t.Fatalf("InsertAll: %v", err)
	}
	if stmts.Load() != 2 { // the two updates above that passed; the batch never ran
		t.Fatalf("statements = %d, want 2", stmts.Load())
	}
}

func TestWithValidator(t *testing.T) {
	v := validator.New()
	must(t, v.RegisterValidation("no_admin", func(fl validator.FieldLevel) bool {
		return !strings.EqualFold(fl.Field().String(), "admin")
	}))
	type Account struct {
		ID   int64  `db:"id,primary,noauto"`
		Name string `validate:"no_admin"`
	}
	db, _ := open(t, kilnvalidate.WithValidator(v))
	err := orm.For[Account](db).Insert(context.Background(), &Account{ID: 1, Name: "Admin"})
	var verr *kilnvalidate.Error
	if !errors.As(err, &verr) || verr.Fields[0].Tag != "no_admin" || !strings.Contains(err.Error(), "name failed no_admin") {
		t.Fatalf("custom rule: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
