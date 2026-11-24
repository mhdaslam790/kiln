package kilnprom_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/kilnprom"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("no tx") }
func (fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "fail") {
		return nil, errors.New("boom")
	}
	return driver.RowsAffected(1), nil
}

type connector struct{}

func (connector) Connect(context.Context) (driver.Conn, error) { return fakeConn{}, nil }
func (connector) Driver() driver.Driver                        { return nil }

type Note struct {
	ID   int64
	Body string
}

func open(t *testing.T, opts ...orm.Option) *orm.DB {
	sqldb := sql.OpenDB(connector{})
	t.Cleanup(func() { sqldb.Close() })
	return orm.Open(sqldb, dialect.Postgres(), opts...)
}

func TestMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	db := open(t, kilnprom.Plugin(reg))
	ctx := context.Background()
	notes := orm.For[Note](db)
	for i := 0; i < 3; i++ {
		if _, err := notes.UpdateAny(ctx, q.Eq("id", i), q.Set("body", "x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, "DELETE FROM notes WHERE body = 'fail'"); err == nil {
		t.Fatal("want error")
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	pool := false
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "go_sql_") && mf.GetMetric()[0].GetLabel()[0].GetValue() == "default" {
			pool = true
		}
		if mf.GetName() != "kiln_query_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			counts[l["db"]+" "+l["table"]+" "+l["action"]+" "+l["outcome"]] = m.GetHistogram().GetSampleCount()
		}
	}
	if counts["default notes update ok"] != 3 || counts["default  raw error"] != 1 || len(counts) != 2 {
		t.Fatalf("series: %v", counts)
	}
	if !pool {
		t.Fatal("pool metrics missing")
	}
}

func TestTwoDBs(t *testing.T) {
	reg := prometheus.NewRegistry()
	a := open(t, kilnprom.Plugin(reg, kilnprom.WithDBName("primary")))
	b := open(t, kilnprom.Plugin(reg, kilnprom.WithDBName("analytics")))
	for _, db := range []*orm.DB{a, b} {
		if _, err := db.Exec(context.Background(), "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	if n := testutil.CollectAndCount(reg, "kiln_query_duration_seconds"); n != 2 {
		t.Fatalf("%d series, want one per db", n)
	}

	// The same name twice is a configuration error, reported by New.
	sqldb := sql.OpenDB(connector{})
	defer sqldb.Close()
	if _, err := kilnprom.New(orm.Open(sqldb, dialect.Postgres()), reg, kilnprom.WithDBName("primary")); err == nil {
		t.Fatal("duplicate db name must fail")
	}
}

func TestUnregister(t *testing.T) {
	reg := prometheus.NewRegistry()
	sqldb := sql.OpenDB(connector{})
	defer sqldb.Close()
	db := orm.Open(sqldb, dialect.Postgres())
	m, err := kilnprom.New(db, reg)
	if err != nil {
		t.Fatal(err)
	}
	m.Unregister()
	if _, err := kilnprom.New(db, reg); err != nil {
		t.Fatalf("re-register after Unregister: %v", err)
	}
}
