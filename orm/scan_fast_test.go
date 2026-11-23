package orm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
)

// rowsConnector answers every query with fixed columns and rows.
type rowsConnector struct {
	cols []string
	rows [][]driver.Value
}

func (c rowsConnector) Connect(context.Context) (driver.Conn, error) { return c, nil }
func (c rowsConnector) Driver() driver.Driver                        { return nil }
func (c rowsConnector) Prepare(string) (driver.Stmt, error)          { return nil, driver.ErrSkip }
func (c rowsConnector) Close() error                                 { return nil }
func (c rowsConnector) Begin() (driver.Tx, error)                    { return nil, driver.ErrSkip }
func (c rowsConnector) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &listRows{cols: c.cols, rows: c.rows}, nil
}

type listRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *listRows) Columns() []string { return r.cols }
func (r *listRows) Close() error      { return nil }
func (r *listRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

type Base struct {
	CreatedAt time.Time `db:"created_at"`
}

type Status string

type scanRec struct {
	ID     int64   `db:"id,primary"`
	Name   string  `db:"name"`
	Age    int     `db:"age"`
	Score  float64 `db:"score"`
	Active bool    `db:"active"`
	Status Status  `db:"status"` // named type: regular path
	Bio    *string `db:"bio"`
	Base           // embedded by value: flattened (embedded pointers are not supported)
}

func (scanRec) TableName() string { return "recs" }

func scanRows(t *testing.T, cols []string, rows ...[]driver.Value) ([]scanRec, error) {
	t.Helper()
	sqldb := sql.OpenDB(rowsConnector{cols: cols, rows: rows})
	defer sqldb.Close()
	return For[scanRec](Open(sqldb, dialect.Postgres())).FindAll(context.Background())
}

func TestFastScan(t *testing.T) {
	cols := []string{"id", "name", "age", "score", "active", "status", "bio", "created_at", "note"}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := scanRows(t, cols,
		[]driver.Value{int64(1), "Ada", int64(36), 9.5, true, "on", "hi", ts, "n1"},
		// "note" has no field and is skipped. NULLs become zero values; text values (as MySQL sends them) convert.
		[]driver.Value{int64(2), nil, nil, nil, nil, nil, nil, nil, nil},
		[]driver.Value{[]byte("3"), []byte("Bob"), []byte("41"), []byte("1.5"), []byte("1"), []byte("off"), nil, ts, nil},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	a, b, c := got[0], got[1], got[2]
	if a.ID != 1 || a.Name != "Ada" || a.Age != 36 || a.Score != 9.5 || !a.Active || a.Status != "on" ||
		a.Bio == nil || *a.Bio != "hi" || !a.CreatedAt.Equal(ts) {
		t.Errorf("row 1: %+v", a)
	}
	if b.ID != 2 || b.Name != "" || b.Age != 0 || b.Score != 0 || b.Active || b.Status != "" || b.Bio != nil || !b.CreatedAt.IsZero() {
		t.Errorf("row 2 (NULLs): %+v", b)
	}
	if c.ID != 3 || c.Name != "Bob" || c.Age != 41 || c.Score != 1.5 || !c.Active || c.Status != "off" || !c.CreatedAt.Equal(ts) {
		t.Errorf("row 3 (text values): %+v", c)
	}
	// Rows are independent: writing to one does not affect another.
	got[0].Name = "changed"
	if got[2].Name != "Bob" {
		t.Error("rows share memory")
	}

	// A second query reuses the pooled scanner and sees no stale values.
	again, err := scanRows(t, cols, []driver.Value{int64(9), nil, nil, nil, nil, nil, nil, nil, nil})
	if err != nil || again[0].Name != "" || again[0].Age != 0 {
		t.Fatalf("pooled scanner leaked values: %+v %v", again, err)
	}
}

func TestFastScanErrors(t *testing.T) {
	if math.MaxInt == math.MaxInt64 {
		t.Skip("int is 64-bit: no overflow possible")
	}
	_, err := scanRows(t, []string{"id", "age"}, []driver.Value{int64(1), int64(math.MaxInt64)})
	if err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("want overflow error, got %v", err)
	}
}

func TestFastScanBadValue(t *testing.T) {
	_, err := scanRows(t, []string{"id", "age"}, []driver.Value{int64(1), []byte("not a number")})
	if err == nil || !strings.Contains(err.Error(), "age") {
		t.Fatalf("want a conversion error naming the column, got %v", err)
	}
}
