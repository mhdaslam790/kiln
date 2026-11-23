package orm

// Benchmarks against an in-memory driver that answers instantly, so the
// numbers show kiln's own overhead (query building, scanning, hooks) rather
// than the database's. Each kiln benchmark has a Raw twin doing the same
// work with hand-written database/sql code.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"

	"testing"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/q"
)

type benchUser struct {
	ID        int64     `db:"id,primary"`
	Name      string    `db:"name"`
	Email     string    `db:"email"`
	Age       int       `db:"age"`
	Bio       *string   `db:"bio"`
	Active    bool      `db:"active"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

var benchCols = []string{"id", "name", "email", "age", "bio", "active", "created_at", "updated_at"}

type fakeDriver struct{ rows int }

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{d: d}, nil }

type fakeConn struct{ d *fakeDriver }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) { return &fakeStmt{c: c, q: query}, nil }
func (c *fakeConn) Close() error                              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)                 { return fakeTx{}, nil }
func (c *fakeConn) CheckNamedValue(*driver.NamedValue) error  { return nil }

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "RETURNING") || strings.Contains(query, "OUTPUT") {
		return &fakeRows{cols: []string{"id"}, n: 1, id: true}, nil
	}
	n := c.d.rows
	if strings.Contains(query, "LIMIT 1") || strings.Contains(query, "TOP 1") {
		n = 1
	}
	return &fakeRows{cols: benchCols, n: n}, nil
}

func (c *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeStmt struct {
	c *fakeConn
	q string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.q, nil)
}

var benchTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type fakeRows struct {
	cols []string
	n, i int
	id   bool
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= r.n {
		return io.EOF
	}
	r.i++
	if r.id {
		dest[0] = int64(r.i)
		return nil
	}
	dest[0], dest[1], dest[2], dest[3] = int64(r.i), "Ada Lovelace", "ada@example.com", int64(36)
	dest[4], dest[5], dest[6], dest[7] = nil, true, benchTime, benchTime
	return nil
}

type fakeConnector struct{ d *fakeDriver }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{d: c.d}, nil }
func (c fakeConnector) Driver() driver.Driver                        { return c.d }

func fakeDB(b *testing.B, rows int) (*DB, *sql.DB) {
	b.Helper()
	sqldb := sql.OpenDB(fakeConnector{&fakeDriver{rows: rows}})
	b.Cleanup(func() { sqldb.Close() })
	return Open(sqldb, dialect.Postgres(), WithClock(func() time.Time { return benchTime })), sqldb
}

const rawSelect = `SELECT "id", "name", "email", "age", "bio", "active", "created_at", "updated_at" FROM "bench_users"`

func scanRaw(rows *sql.Rows, u *benchUser) error {
	return rows.Scan(&u.ID, &u.Name, &u.Email, &u.Age, &u.Bio, &u.Active, &u.CreatedAt, &u.UpdatedAt)
}

func BenchmarkFindByID(b *testing.B) {
	ctx := context.Background()
	b.Run("kiln", func(b *testing.B) {
		db, _ := fakeDB(b, 1)
		users := For[benchUser](db)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := users.FindByID(ctx, int64(1)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("raw", func(b *testing.B) {
		_, sqldb := fakeDB(b, 1)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rows, err := sqldb.QueryContext(ctx, rawSelect+` WHERE "id" = $1 LIMIT 1`, int64(1))
			if err != nil {
				b.Fatal(err)
			}
			var u benchUser
			if !rows.Next() {
				b.Fatal("no row")
			}
			if err := scanRaw(rows, &u); err != nil {
				b.Fatal(err)
			}
			rows.Close()
		}
	})
}

func BenchmarkFindAll100(b *testing.B) {
	ctx := context.Background()
	b.Run("kiln", func(b *testing.B) {
		db, _ := fakeDB(b, 100)
		users := For[benchUser](db)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			list, err := users.FindAll(ctx, q.Eq("active", true), q.Sort("-id"))
			if err != nil || len(list) != 100 {
				b.Fatal(err, len(list))
			}
		}
	})
	b.Run("raw", func(b *testing.B) {
		_, sqldb := fakeDB(b, 100)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rows, err := sqldb.QueryContext(ctx, rawSelect+` WHERE "active" = $1 ORDER BY "id" DESC`, true)
			if err != nil {
				b.Fatal(err)
			}
			var list []benchUser
			for rows.Next() {
				var u benchUser
				if err := scanRaw(rows, &u); err != nil {
					b.Fatal(err)
				}
				list = append(list, u)
			}
			rows.Close()
			if len(list) != 100 {
				b.Fatal(len(list))
			}
		}
	})
}

func BenchmarkInsert(b *testing.B) {
	ctx := context.Background()
	b.Run("kiln", func(b *testing.B) {
		db, _ := fakeDB(b, 1)
		users := For[benchUser](db)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			u := benchUser{Name: "Ada", Email: "ada@example.com", Age: 36, Active: true}
			if err := users.Insert(ctx, &u); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("raw", func(b *testing.B) {
		_, sqldb := fakeDB(b, 1)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			u := benchUser{Name: "Ada", Email: "ada@example.com", Age: 36, Active: true, CreatedAt: benchTime, UpdatedAt: benchTime}
			err := sqldb.QueryRowContext(ctx, `INSERT INTO "bench_users" ("name", "email", "age", "bio", "active", "created_at", "updated_at") VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING "id"`,
				u.Name, u.Email, u.Age, u.Bio, u.Active, u.CreatedAt, u.UpdatedAt).Scan(&u.ID)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkUpdate(b *testing.B) {
	ctx := context.Background()
	b.Run("kiln", func(b *testing.B) {
		db, _ := fakeDB(b, 1)
		users := For[benchUser](db)
		u := benchUser{ID: 1, Name: "Ada", Email: "ada@example.com", Age: 36, Active: true}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			u.Age++
			if err := users.Update(ctx, &u); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("raw", func(b *testing.B) {
		_, sqldb := fakeDB(b, 1)
		u := benchUser{ID: 1, Name: "Ada", Email: "ada@example.com", Age: 36, Active: true}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			u.Age++
			_, err := sqldb.ExecContext(ctx, `UPDATE "bench_users" SET "name" = $1, "email" = $2, "age" = $3, "bio" = $4, "active" = $5, "updated_at" = $6 WHERE "id" = $7`,
				u.Name, u.Email, u.Age, u.Bio, u.Active, benchTime, u.ID)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
