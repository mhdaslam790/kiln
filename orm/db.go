// Package orm is a repository-style ORM on top of database/sql.
//
//	sqldb, _ := sql.Open("pgx", dsn)
//	db := orm.Open(sqldb, dialect.Postgres())
//
//	users := orm.For[User](db)
//	u, err := users.Find(ctx, q.Eq("email", "a@b.c"))
//	err = db.Transaction(ctx, func(ctx context.Context) error {
//		return users.Insert(ctx, &User{Name: "Ann"})
//	})
//
// Transactions travel in the context: every call made with the ctx passed
// to the Transaction callback runs inside that transaction.
package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/schema"
)

// ErrNotFound is returned by Find-style methods when no row matches.
var ErrNotFound = errors.New("orm: record not found")

// ErrStale is returned by Update and Delete on a model with a lock field
// (“Version int `db:"version,lock"` “) when the row's version is no longer
// the one the record was read with: someone else changed or deleted it in
// the meantime. Reload the record and apply the change again, or report a
// conflict to the user. Nothing was written.
var ErrStale = errors.New("orm: record was changed or deleted since it was read")

// ConstraintError wraps a driver error caused by a constraint violation.
type ConstraintError struct {
	Kind dialect.ErrorKind
	Err  error
}

func (e *ConstraintError) Error() string { return fmt.Sprintf("orm: %s constraint: %v", e.Kind, e.Err) }
func (e *ConstraintError) Unwrap() error { return e.Err }

// IsUniqueViolation reports whether err is a unique/primary key violation.
func IsUniqueViolation(err error) bool { return isKind(err, dialect.ErrUnique) }

// IsForeignKeyViolation reports whether err is a foreign key violation.
func IsForeignKeyViolation(err error) bool { return isKind(err, dialect.ErrForeignKey) }

func isKind(err error, k dialect.ErrorKind) bool {
	var ce *ConstraintError
	return errors.As(err, &ce) && ce.Kind == k
}

// Event describes one executed statement, for logging and metrics.
type Event struct {
	SQL      string
	Args     []any
	Duration time.Duration
	Err      error
	InTx     bool
	// Cached is true when the result came from the query cache and the
	// database was not contacted.
	Cached bool
}

// Option configures a DB.
type Option func(*DB)

// WithHook registers a function called after every statement (and every
// cache hit). It is called from whichever goroutine ran the statement, so it
// must be safe for concurrent use.
func WithHook(fn func(ctx context.Context, e Event)) Option {
	return Use(hookFunc(fn))
}

// WithLogger logs every statement at debug level, and statements slower
// than slow (if > 0) or failing at warn level.
func WithLogger(l *slog.Logger, slow time.Duration) Option {
	return WithHook(func(ctx context.Context, e Event) {
		attrs := []any{"sql", e.SQL, "duration", e.Duration}
		if e.Cached {
			attrs = append(attrs, "cached", true)
		}
		switch {
		case e.Err != nil && !errors.Is(e.Err, sql.ErrNoRows):
			l.WarnContext(ctx, "query failed", append(attrs, "error", e.Err)...)
		case slow > 0 && e.Duration >= slow:
			l.WarnContext(ctx, "slow query", attrs...)
		default:
			l.DebugContext(ctx, "query", attrs...)
		}
	})
}

// WithReplicas routes reads (Find, FindAll, Count, Select, Query, Preload)
// to the given read replicas, round-robin. Reads inside a transaction, and
// reads made with a context from UsePrimary, always go to the primary.
func WithReplicas(replicas ...*sql.DB) Option {
	return func(db *DB) { db.replicas = append(db.replicas, replicas...) }
}

type primaryKey struct{}

// UsePrimary returns a context whose reads go to the primary database, e.g.
// right after a write when replica lag would matter.
func UsePrimary(ctx context.Context) context.Context {
	return context.WithValue(ctx, primaryKey{}, true)
}

// WithClock overrides time.Now for timestamps (useful in tests).
func WithClock(now func() time.Time) Option { return func(db *DB) { db.now = now } }

// DB is the entry point of the ORM. It is safe for concurrent use.
type DB struct {
	sql          *sql.DB
	dialect      dialect.Dialect
	hooks        []func(context.Context, Event) // ObservePlugin.AfterQuery, incl. WithHook
	queryPlugins []QueryPlugin
	writePlugins []WritePlugin
	now          func() time.Time
	spSeq        atomic.Int64
	replicas     []*sql.DB
	rr           atomic.Uint64
	cache        *resultCache // nil unless WithCache
	stmts        *stmtCache   // nil unless WithStmtCache
}

// Open wraps an existing *sql.DB.
func Open(db *sql.DB, d dialect.Dialect, opts ...Option) *DB {
	o := &DB{sql: db, dialect: d, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Connect opens a database with the given driver name and DSN and pings it.
func Connect(ctx context.Context, driver, dsn string, d dialect.Dialect, opts ...Option) (*DB, error) {
	s, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if err := s.PingContext(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return Open(s, d, opts...), nil
}

// SQL returns the underlying *sql.DB.
func (db *DB) SQL() *sql.DB { return db.sql }

// Dialect returns the dialect in use.
func (db *DB) Dialect() dialect.Dialect { return db.dialect }

// Close closes the underlying *sql.DB and any replicas.
func (db *DB) Close() error {
	db.stmts.closeAll()
	errs := []error{db.sql.Close()}
	for _, r := range db.replicas {
		errs = append(errs, r.Close())
	}
	return errors.Join(errs...)
}

// Ping verifies the connection.
func (db *DB) Ping(ctx context.Context) error { return db.sql.PingContext(ctx) }

type conn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txKey struct{ db *DB }

type txState struct {
	tx      *sql.Tx
	mu      sync.Mutex
	touched map[string]bool // tables written, invalidated in the cache on commit
}

func (db *DB) txFrom(ctx context.Context) *txState {
	st, _ := ctx.Value(txKey{db}).(*txState)
	return st
}

type connKey struct{ db *DB }

func (db *DB) pinned(ctx context.Context) *sql.Conn {
	c, _ := ctx.Value(connKey{db}).(*sql.Conn)
	return c
}

func (db *DB) conn(ctx context.Context) conn {
	if st := db.txFrom(ctx); st != nil {
		return st.tx
	}
	if c := db.pinned(ctx); c != nil {
		return c
	}
	return db.sql
}

// WithConn pins one pooled connection for every call made with the ctx
// passed to fn (including transactions started inside it). Use it for
// session state: SET statements, temporary tables, PRAGMAs, locks.
func (db *DB) WithConn(ctx context.Context, fn func(ctx context.Context) error) error {
	if db.pinned(ctx) != nil || db.txFrom(ctx) != nil {
		return fn(ctx)
	}
	c, err := db.sql.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(context.WithValue(ctx, connKey{db}, c))
}

// InTransaction reports whether ctx carries a transaction of this DB.
func (db *DB) InTransaction(ctx context.Context) bool { return db.txFrom(ctx) != nil }

// Transaction runs fn in a transaction, committing if fn returns nil and
// rolling back on error or panic. Nested calls use savepoints, so an inner
// failure rolls back only the inner block.
func (db *DB) Transaction(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.TransactionOpts(ctx, nil, fn)
}

// TransactionOpts is Transaction with explicit isolation / read-only options
// (ignored for nested transactions).
func (db *DB) TransactionOpts(ctx context.Context, opts *sql.TxOptions, fn func(ctx context.Context) error) (err error) {
	if st := db.txFrom(ctx); st != nil {
		return db.savepoint(ctx, st, fn)
	}
	st, tctx, err := db.begin(ctx, opts)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = st.tx.Rollback()
			panic(p)
		}
	}()
	if err = fn(tctx); err != nil {
		if rbErr := st.tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, rbErr)
		}
		return err
	}
	return db.commit(ctx, st)
}

// begin starts a transaction and returns its state and the context that
// carries it. Transaction uses it directly (no Tx allocation).
func (db *DB) begin(ctx context.Context, opts *sql.TxOptions) (*txState, context.Context, error) {
	var (
		tx  *sql.Tx
		err error
	)
	if c := db.pinned(ctx); c != nil {
		tx, err = c.BeginTx(ctx, opts)
	} else {
		tx, err = db.sql.BeginTx(ctx, opts)
	}
	if err != nil {
		return nil, nil, err
	}
	st := &txState{tx: tx}
	return st, context.WithValue(ctx, txKey{db}, st), nil
}

// commit commits st, then invalidates cached results for the tables it
// wrote. Invalidating only after commit means concurrent readers never
// cache rows that might still be rolled back.
func (db *DB) commit(ctx context.Context, st *txState) error {
	err := st.tx.Commit()
	if err == nil && db.cache != nil {
		_ = db.cache.bump(ctx, st.touchedTables())
	}
	return err
}

// Tx is a transaction started with Begin.
type Tx struct {
	db     *DB
	st     *txState
	parent context.Context
	ctx    context.Context
}

// ErrNestedBegin is returned by Begin when ctx already carries a
// transaction of the same DB. Use Transaction for nested work: it creates
// a savepoint.
var ErrNestedBegin = errors.New("orm: Begin called inside a transaction; use Transaction for a savepoint")

// Begin starts a transaction and returns it without committing or rolling
// back. Most code should use Transaction, which does both for you; Begin
// is for lifetimes that do not fit a callback, such as test helpers that
// roll back when the test ends.
//
// Every call made with tx.Context() runs in the transaction, including
// Transaction (as a savepoint). Call Commit or Rollback exactly once.
func (db *DB) Begin(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	if db.txFrom(ctx) != nil {
		return nil, ErrNestedBegin
	}
	st, tctx, err := db.begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{db: db, st: st, parent: ctx, ctx: tctx}, nil
}

// Context returns the context that carries the transaction.
func (t *Tx) Context() context.Context { return t.ctx }

// Commit commits the transaction, then invalidates cached results for the
// tables it wrote.
func (t *Tx) Commit() error { return t.db.commit(t.parent, t.st) }

// Rollback discards the transaction. It returns sql.ErrTxDone if the
// transaction was already committed or rolled back.
func (t *Tx) Rollback() error { return t.st.tx.Rollback() }

func (db *DB) savepoint(ctx context.Context, st *txState, fn func(ctx context.Context) error) (err error) {
	name := fmt.Sprintf("orm_sp_%d", db.spSeq.Add(1))
	if _, err := st.tx.ExecContext(ctx, db.dialect.Savepoint(name)); err != nil {
		return err
	}
	rollback := func() error {
		_, e := st.tx.ExecContext(ctx, db.dialect.RollbackTo(name))
		return e
	}
	defer func() {
		if p := recover(); p != nil {
			_ = rollback()
			panic(p)
		}
	}()
	if err = fn(ctx); err != nil {
		if rbErr := rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	if rel := db.dialect.Release(name); rel != "" {
		_, err = st.tx.ExecContext(ctx, rel)
	}
	return err
}

func (db *DB) emit(ctx context.Context, s dialect.Stmt, start time.Time, err error) {
	if len(db.hooks) == 0 {
		return
	}
	e := Event{SQL: s.SQL, Args: s.Args, Duration: time.Since(start), Err: err, InTx: db.InTransaction(ctx)}
	for _, h := range db.hooks {
		h(ctx, e)
	}
}

func (db *DB) wrap(err error) error {
	if err == nil {
		return nil
	}
	if k := db.dialect.Classify(err); k != dialect.ErrOther {
		return &ConstraintError{Kind: k, Err: err}
	}
	return err
}

// reader picks the connection for a read-only statement.
func (db *DB) reader(ctx context.Context) conn {
	if st := db.txFrom(ctx); st != nil {
		return st.tx
	}
	if c := db.pinned(ctx); c != nil {
		return c
	}
	if len(db.replicas) == 0 || ctx.Value(primaryKey{}) != nil {
		return db.sql
	}
	return db.replicas[int(db.rr.Add(1)%uint64(len(db.replicas)))]
}

// queryRead runs a read-only statement built from qq (nil for statements
// that do not come from a q.Query), through the result cache when the
// context asks for it.
func (db *DB) queryRead(ctx context.Context, s dialect.Stmt, qq *q.Query) (*sql.Rows, error) {
	if o, tables, ok := db.cachedRead(ctx, qq); ok {
		return db.queryCached(ctx, s, o, tables)
	}
	return db.queryReadDirect(ctx, s)
}

func (db *DB) queryReadDirect(ctx context.Context, s dialect.Stmt) (*sql.Rows, error) {
	start := time.Now()
	rows, err := db.query(ctx, db.reader(ctx), s)
	db.emit(ctx, s, start, err)
	return rows, db.wrap(err)
}

func (db *DB) execStmt(ctx context.Context, s dialect.Stmt) (sql.Result, error) {
	start := time.Now()
	res, err := db.exec(ctx, db.conn(ctx), s)
	db.emit(ctx, s, start, err)
	return res, db.wrap(err)
}

func (db *DB) queryStmt(ctx context.Context, s dialect.Stmt) (*sql.Rows, error) {
	start := time.Now()
	rows, err := db.query(ctx, db.conn(ctx), s)
	db.emit(ctx, s, start, err)
	return rows, db.wrap(err)
}

// Exec runs a raw statement ('?' placeholders are rewritten for the dialect).
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s, err := db.dialect.Raw(query, args)
	if err != nil {
		return nil, err
	}
	return db.execStmt(ctx, s)
}

// Executor returns the active transaction or the pool, as a raw
// database/sql executor (useful for migrations and third-party code).
func (db *DB) Executor(ctx context.Context) schema.Executor {
	return db.conn(ctx)
}
