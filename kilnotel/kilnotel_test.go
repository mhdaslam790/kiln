package kilnotel_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/kilnotel"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

// fakeConn accepts every statement; SQL containing "fail" returns an error.
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

func setup(t *testing.T, opts ...kilnotel.Option) (*tracetest.SpanRecorder, *orm.DB, trace.Tracer) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	sqldb := sql.OpenDB(connector{})
	t.Cleanup(func() { sqldb.Close() })
	db := orm.Open(sqldb, dialect.Postgres(), kilnotel.Plugin(append(opts, kilnotel.WithTracerProvider(tp))...))
	return rec, db, tp.Tracer("test")
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, a := range s.Attributes() {
		m[a.Key] = a.Value
	}
	return m
}

func TestSpans(t *testing.T) {
	rec, db, tracer := setup(t)
	ctx, parent := tracer.Start(context.Background(), "request")

	n, err := orm.For[Note](db).UpdateAny(ctx, q.Eq("id", 7), q.Set("body", "secret value"))
	if err != nil || n != 1 {
		t.Fatalf("UpdateAny = %d, %v", n, err)
	}
	if _, err := db.Exec(ctx, "DELETE FROM notes WHERE body = 'fail'"); err == nil {
		t.Fatal("want the driver error")
	}
	parent.End()

	spans := rec.Ended()
	if len(spans) != 3 {
		t.Fatalf("%d spans", len(spans))
	}
	upd, raw := spans[0], spans[1]

	if upd.Name() != "update notes" || upd.SpanKind() != trace.SpanKindClient {
		t.Errorf("span = %q %v", upd.Name(), upd.SpanKind())
	}
	if upd.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("span is not a child of the request span")
	}
	a := attrs(upd)
	if a["db.system.name"].AsString() != "postgresql" || a["db.collection.name"].AsString() != "notes" ||
		a["db.operation.name"].AsString() != "update" || !strings.Contains(a["db.query.text"].AsString(), "UPDATE") {
		t.Errorf("attributes: %v", a)
	}
	for k, v := range a {
		if strings.Contains(v.Emit(), "secret value") {
			t.Errorf("argument value leaked into %s", k)
		}
	}
	if upd.EndTime().Before(upd.StartTime()) {
		t.Error("end before start")
	}

	if raw.Name() != "kiln raw query" || raw.Status().Code != codes.Error {
		t.Errorf("raw span = %q %v", raw.Name(), raw.Status())
	}
	if _, ok := attrs(raw)["db.operation.name"]; ok {
		t.Error("raw SQL must not claim an operation")
	}
}

func TestWithArgs(t *testing.T) {
	rec, db, _ := setup(t, kilnotel.WithArgs())
	if _, err := db.Exec(context.Background(), "UPDATE notes SET body = ? WHERE id = ?", "hi", 3); err != nil {
		t.Fatal(err)
	}
	a := attrs(rec.Ended()[0])
	if a["db.query.parameter.0"].AsString() != "hi" || a["db.query.parameter.1"].AsString() != "3" {
		t.Errorf("args: %v", a)
	}
}

func TestNotSampled(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()), sdktrace.WithSpanProcessor(rec))
	sqldb := sql.OpenDB(connector{})
	defer sqldb.Close()
	db := orm.Open(sqldb, dialect.Postgres(), kilnotel.Plugin(kilnotel.WithTracerProvider(tp)))
	if _, err := db.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if len(rec.Ended()) != 0 {
		t.Fatal("unsampled spans were recorded")
	}
}
