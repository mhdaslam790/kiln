// Package kilnotel traces kiln queries with OpenTelemetry: one span per
// statement, a child of the span in the query's context.
//
//	db := orm.Open(sqlDB, dialect.Postgres(), kilnotel.Plugin())
//
// Spans follow the OpenTelemetry database conventions: named
// "select users", with db.system.name, db.collection.name,
// db.operation.name and db.query.text. The query text is the SQL with
// placeholders; argument values are never recorded, so personal data does
// not reach your tracing backend. Cache hits are recorded too, with
// kiln.cached = true, so you can see which reads skipped the database.
//
// It is an orm.ObservePlugin: it adds nothing to queries and costs nothing
// when the tracer is not sampling.
package kilnotel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
)

const scope = "github.com/mhdaslam790/kiln/kilnotel"

// Option configures the plugin.
type Option func(*plugin)

// WithTracerProvider uses tp instead of the global provider
// (otel.GetTracerProvider()).
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(p *plugin) { p.tp = tp }
}

// WithArgs also records argument values (db.query.parameter.<n>). Off by
// default: values are often personal data.
func WithArgs() Option { return func(p *plugin) { p.args = true } }

// Plugin returns the option that traces every statement of a DB.
func Plugin(opts ...Option) orm.Option {
	p := &plugin{}
	for _, o := range opts {
		o(p)
	}
	return func(db *orm.DB) {
		tp := p.tp
		if tp == nil {
			tp = otel.GetTracerProvider()
		}
		// A copy per DB: each knows its own database system.
		q := &plugin{tp: tp, args: p.args, tracer: tp.Tracer(scope), system: system(db.Dialect().Kind())}
		orm.Use(q)(db)
	}
}

type plugin struct {
	tp     trace.TracerProvider
	tracer trace.Tracer
	system attribute.KeyValue
	args   bool
}

func (p *plugin) Name() string { return "kilnotel" }

func (p *plugin) AfterQuery(ctx context.Context, e orm.Event) {
	end := time.Now()
	_, span := p.tracer.Start(ctx, spanName(e),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(end.Add(-e.Duration)))
	if !span.IsRecording() { // not sampled: no attributes to build
		span.End(trace.WithTimestamp(end))
		return
	}
	attrs := make([]attribute.KeyValue, 0, 7+len(e.Args))
	attrs = append(attrs, p.system, attribute.String("db.query.text", e.SQL))
	if e.Table != "" {
		attrs = append(attrs, attribute.String("db.collection.name", e.Table))
	}
	if e.Action != 0 && e.Action != orm.ActionRaw {
		attrs = append(attrs, attribute.String("db.operation.name", e.Action.String()))
	}
	if e.Cached {
		attrs = append(attrs, attribute.Bool("kiln.cached", true))
	}
	if e.InTx {
		attrs = append(attrs, attribute.Bool("kiln.in_transaction", true))
	}
	if p.args {
		for i, a := range e.Args {
			attrs = append(attrs, attribute.String("db.query.parameter."+itoa(i), argString(a)))
		}
	}
	span.SetAttributes(attrs...)
	if e.Err != nil {
		span.RecordError(e.Err, trace.WithTimestamp(end))
		span.SetStatus(codes.Error, e.Err.Error())
	}
	span.End(trace.WithTimestamp(end))
}

// spanName is "<action> <table>" ("select users"), or the database system
// for raw SQL, as the conventions suggest when there is no low-cardinality
// summary.
func spanName(e orm.Event) string {
	if e.Table == "" || e.Action == 0 || e.Action == orm.ActionRaw {
		return "kiln raw query"
	}
	return e.Action.String() + " " + e.Table
}

func system(k dialect.Kind) attribute.KeyValue {
	name := "other_sql"
	switch k {
	case dialect.KindPostgres:
		name = "postgresql"
	case dialect.KindMySQL:
		name = "mysql"
	case dialect.KindSQLite:
		name = "sqlite"
	case dialect.KindMSSQL:
		name = "microsoft.sql_server"
	}
	return attribute.String("db.system.name", name)
}
