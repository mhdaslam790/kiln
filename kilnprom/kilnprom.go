// Package kilnprom exports kiln query metrics to Prometheus.
//
//	db := orm.Open(sqlDB, dialect.Postgres(), kilnprom.Plugin(prometheus.DefaultRegisterer))
//
// Metrics (labels in brackets):
//
//	kiln_query_duration_seconds  histogram [db, table, action, outcome]
//	                             outcome is "ok", "error" or "cached"; its
//	                             count is the query count, so error rate and
//	                             cache hit ratio come from the same series
//	go_sql_*                     connection pool [db_name]: open, in use,
//	                             idle, waits, wait time (the standard
//	                             database/sql collector, so existing
//	                             dashboards work)
//
// Labels stay low-cardinality: the table and action, never SQL or values.
// Raw SQL is counted with action "raw" and table "".
package kilnprom

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/mhdaslam790/kiln/orm"
)

// Option configures the metrics.
type Option func(*config)

type config struct {
	name    string
	buckets []float64
}

// WithDBName sets the "db" label, and db_name on the pool metrics
// (default "default"). Use it when one
// process has several databases, e.g. "primary" and "analytics".
func WithDBName(name string) Option { return func(c *config) { c.name = name } }

// WithBuckets sets the histogram buckets in seconds (default
// prometheus.DefBuckets, 5ms to 10s).
func WithBuckets(b []float64) Option { return func(c *config) { c.buckets = b } }

// Metrics is a DB's registered collectors.
type Metrics struct {
	reg      prometheus.Registerer
	duration *prometheus.HistogramVec
	pool     prometheus.Collector
	name     string
}

// New registers the metrics for db with reg. It returns an error if they
// conflict with metrics already registered (for example a second DB with
// the same WithDBName). Pass m.Plugin() to orm.Open, or use Plugin, which
// does both.
func New(db *orm.DB, reg prometheus.Registerer, opts ...Option) (*Metrics, error) {
	c := config{name: "default", buckets: prometheus.DefBuckets}
	for _, o := range opts {
		o(&c)
	}
	m := &Metrics{reg: reg, name: c.name}
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kiln_query_duration_seconds",
		Help:    "Duration of kiln statements, by table, action and outcome (ok, error, cached).",
		Buckets: c.buckets,
	}, []string{"db", "table", "action", "outcome"})
	if err := reg.Register(m.duration); err != nil {
		var are prometheus.AlreadyRegisteredError
		if !errors.As(err, &are) {
			return nil, err
		}
		// Several DBs share one histogram, told apart by the db label.
		m.duration = are.ExistingCollector.(*prometheus.HistogramVec)
	}
	m.pool = collectors.NewDBStatsCollector(db.SQL(), c.name)
	if err := reg.Register(m.pool); err != nil {
		return nil, err
	}
	return m, nil
}

// Plugin registers the metrics when the DB is opened and enables them. It
// panics if registration fails, like promauto; use New to handle the error.
func Plugin(reg prometheus.Registerer, opts ...Option) orm.Option {
	return func(db *orm.DB) {
		m, err := New(db, reg, opts...)
		if err != nil {
			panic("kilnprom: " + err.Error())
		}
		orm.Use(m)(db)
	}
}

// Unregister removes the pool collector, which holds the *sql.DB. Call it
// if you close a DB while the process keeps running.
func (m *Metrics) Unregister() { m.reg.Unregister(m.pool) }

// Name implements orm.Plugin.
func (m *Metrics) Name() string { return "kilnprom" }

// AfterQuery implements orm.ObservePlugin.
func (m *Metrics) AfterQuery(_ context.Context, e orm.Event) {
	outcome := "ok"
	switch {
	case e.Err != nil:
		outcome = "error"
	case e.Cached:
		outcome = "cached"
	}
	action := "raw"
	if e.Action != 0 {
		action = e.Action.String()
	}
	m.duration.WithLabelValues(m.name, e.Table, action, outcome).Observe(e.Duration.Seconds())
}
