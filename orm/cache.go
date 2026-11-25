package orm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhdaslam790/kiln/cache"
	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/q"
)

// ---- configuration -----------------------------------------------------------

// CacheOption configures the query result cache.
type CacheOption func(*resultCache)

// CacheStore sets the backend (default: cache.NewMemory()).
func CacheStore(s cache.Store) CacheOption { return func(c *resultCache) { c.store = s } }

// CacheTTL sets the expiry used when orm.Cached is given ttl <= 0
// (default 1 minute). d <= 0 keeps the default: entries must expire, as
// results made stale by a write are never deleted, only no longer read.
func CacheTTL(d time.Duration) CacheOption {
	return func(c *resultCache) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// CachePrefix namespaces every key (default "kiln:"), so several
// applications or databases can share one Redis.
func CachePrefix(p string) CacheOption { return func(c *resultCache) { c.prefix = p } }

// CacheOnError is called when the store fails. Store failures never fail a
// query: reads fall back to the database.
func CacheOnError(fn func(ctx context.Context, err error)) CacheOption {
	return func(c *resultCache) { c.onError = fn }
}

// WithCache enables the query result cache. Without it, orm.Cached has no
// effect. Only reads made with an orm.Cached context are cached:
//
//	db := orm.Open(sqldb, d, orm.WithCache())                       // in-memory
//	db := orm.Open(sqldb, d, orm.WithCache(orm.CacheStore(redis)))  // shared
//
//	users.FindAll(orm.Cached(ctx, 5*time.Minute), q.Eq("active", true))
//
// Entries are invalidated automatically when the ORM writes to any table the
// query reads (Insert, Update, Delete, UpdateAny, Link, ...; inside a
// transaction, after it commits). Writes the ORM cannot see — raw
// db.Exec, other services, manual SQL — need InvalidateTables, or rely on
// the TTL.
func WithCache(opts ...CacheOption) Option {
	return func(db *DB) {
		c := &resultCache{ttl: time.Minute, prefix: "kiln:"}
		for _, o := range opts {
			o(c)
		}
		if c.store == nil {
			c.store = cache.NewMemory()
		}
		db.cache = c
	}
}

type cachedKey struct{}

type cachedOpts struct {
	ttl    time.Duration
	tables []string
}

// Cached returns a context whose reads (Find, FindAll, Count, Exists,
// Paginate, Preload, Select, Query) are served from the cache when possible.
// ttl <= 0 uses the DB's default (CacheTTL). Tables the query reads are
// detected from the query (table, joins, sub-queries); list extra tables for
// raw SQL or q.Expr fragments that read other tables. A raw query with no
// tables listed is not cached.
//
// It has no effect unless the DB was opened with WithCache, and reads
// inside a transaction or a WithConn block always go to the database.
func Cached(ctx context.Context, ttl time.Duration, tables ...string) context.Context {
	return context.WithValue(ctx, cachedKey{}, &cachedOpts{ttl: ttl, tables: tables})
}

// NoCache returns a context that bypasses the cache, undoing Cached.
func NoCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, cachedKey{}, (*cachedOpts)(nil))
}

// InvalidateTables drops cached results that read any of tables. Use it
// after writes the ORM did not make (raw Exec, other services). Inside a
// transaction it takes effect when the transaction commits.
func (db *DB) InvalidateTables(ctx context.Context, tables ...string) error {
	if db.cache == nil {
		return nil
	}
	if st := db.txFrom(ctx); st != nil {
		st.touch(tables)
		return nil
	}
	return db.cache.bump(ctx, tables)
}

// CacheEnabled reports whether the DB was opened with WithCache.
func (db *DB) CacheEnabled() bool { return db.cache != nil }

// ---- write side: table versions ---------------------------------------------

// resultCache keys every entry by the SQL, its arguments and the tables it
// reads, and stores in the entry the version token each table had when the
// rows were read. A write gives the table a fresh token, so older entries
// no longer match and are refilled (or simply expire).
type resultCache struct {
	store   cache.Store
	ttl     time.Duration
	prefix  string
	onError func(context.Context, error)
	flight  flightGroup
}

var (
	tokenSeed = func() string {
		var b [6]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}()
	tokenSeq atomic.Uint64
)

// newToken is unique across processes (random seed) and calls (time + seq).
func newToken() string {
	return fmt.Sprintf("%s.%x.%x", tokenSeed, time.Now().UnixNano(), tokenSeq.Add(1))
}

func (c *resultCache) fail(ctx context.Context, err error) {
	if c.onError != nil && err != nil {
		c.onError(ctx, err)
	}
}

func (c *resultCache) versionKey(table string) string { return c.prefix + "v:" + table }

func (c *resultCache) bump(ctx context.Context, tables []string) error {
	var errs []error
	for _, t := range dedupe(tables) {
		if err := c.store.Set(ctx, c.versionKey(t), []byte(newToken()), 0); err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	c.fail(ctx, err)
	return err
}

// MultiGetter is an optional Store extension that fetches several keys in
// one round trip (e.g. Redis MGET). Missing keys are returned as nil.
type MultiGetter interface {
	GetMulti(ctx context.Context, keys []string) ([][]byte, error)
}

// fetch returns the current token of each table plus the value of entryKey
// (nil if absent), in one round trip when the store supports MultiGetter.
// Tables without a token (never cached, or evicted) get a fresh one: a
// missing token is never treated as a known version, so an evicted token
// cannot revive old entries.
func (c *resultCache) fetch(ctx context.Context, tables []string, entryKey string) ([]string, []byte, error) {
	keys := make([]string, len(tables), len(tables)+1)
	for i, t := range tables {
		keys[i] = c.versionKey(t)
	}
	keys = append(keys, entryKey)
	vals := make([][]byte, len(keys))
	if mg, ok := c.store.(MultiGetter); ok {
		got, err := mg.GetMulti(ctx, keys)
		if err != nil {
			return nil, nil, err
		}
		copy(vals, got)
	} else {
		for i, k := range keys {
			v, ok, err := c.store.Get(ctx, k)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				vals[i] = v
			}
		}
	}
	vers := make([]string, len(tables))
	for i, v := range vals[:len(tables)] {
		if v == nil {
			tok := newToken()
			if err := c.store.Set(ctx, keys[i], []byte(tok), 0); err != nil {
				return nil, nil, err
			}
			v = []byte(tok)
		}
		vers[i] = string(v)
	}
	return vers, vals[len(tables)], nil
}

// touched records writes to tables: immediately outside a transaction, at
// commit inside one.
func (db *DB) touched(ctx context.Context, tables ...string) {
	if db.cache == nil {
		return
	}
	if st := db.txFrom(ctx); st != nil {
		st.touch(tables)
		return
	}
	_ = db.cache.bump(ctx, tables)
}

func (st *txState) touch(tables []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.touched == nil {
		st.touched = map[string]bool{}
	}
	for _, t := range tables {
		st.touched[t] = true
	}
}

func (st *txState) touchedTables() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.touched))
	for t := range st.touched {
		out = append(out, t)
	}
	return out
}

// ---- read side ----------------------------------------------------------------

// cachedRead decides whether a read may use the cache and returns its
// options and the tables it depends on.
func (db *DB) cachedRead(ctx context.Context, qq *q.Query) (*cachedOpts, []string, bool) {
	if db.cache == nil {
		return nil, nil, false
	}
	o, _ := ctx.Value(cachedKey{}).(*cachedOpts)
	if o == nil || db.txFrom(ctx) != nil || db.pinned(ctx) != nil {
		return nil, nil, false
	}
	tables := append([]string(nil), o.tables...)
	if qq != nil {
		tables = append(tables, queryTables(*qq)...)
	}
	tables = dedupe(tables)
	if len(tables) == 0 {
		return nil, nil, false
	}
	return o, tables, true
}

// queryCached serves a read from the cache, or runs it and stores the rows.
func (db *DB) queryCached(ctx context.Context, s dialect.Stmt, co *cachedOpts, tables []string, o op) (*sql.Rows, error) {
	c := db.cache
	start := time.Now()
	key, err := c.key(db.dialect.Kind(), s, tables)
	if err != nil { // argument we cannot hash reliably
		return db.queryReadDirect(ctx, s, o)
	}
	// One round trip: the tables' current versions and the entry, which
	// records the versions it was built from. It is only valid if they
	// still match.
	vers, b, err := c.fetch(ctx, tables, key)
	if err != nil {
		c.fail(ctx, err)
		return db.queryReadDirect(ctx, s, o)
	}
	if b != nil {
		if got, res, err := decodeEntry(b); err == nil && equalStrings(got, vers) {
			db.emitCached(ctx, s, o, start)
			return replay(ctx, res)
		}
	}

	res, err := c.flight.do(key+"\x00"+strings.Join(vers, ","), func() (*cachedResult, error) {
		rows, err := db.queryReadDirect(ctx, s, o)
		if err != nil {
			return nil, err
		}
		res, err := record(rows)
		if err != nil {
			return nil, err
		}
		if b, err := encodeEntry(vers, res); err == nil {
			ttl := co.ttl
			if ttl <= 0 {
				ttl = c.ttl
			}
			c.fail(ctx, c.store.Set(ctx, key, b, ttl))
		}
		return res, nil
	})
	if err != nil {
		if errors.Is(err, errFlightShared) { // the leader failed; try ourselves
			return db.queryReadDirect(ctx, s, o)
		}
		return nil, err
	}
	return replay(ctx, res)
}

// key identifies a query: dialect, SQL, arguments and the tables it reads.
func (c *resultCache) key(kind dialect.Kind, s dialect.Stmt, tables []string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", kind, s.SQL)
	for _, a := range s.Args {
		if v, ok := a.(driver.Valuer); ok {
			x, err := v.Value()
			if err != nil {
				return "", err
			}
			a = x
		}
		switch a.(type) {
		case nil, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64, bool, string, []byte, time.Time:
		default:
			// Pointers and composite values would hash by address or
			// formatting quirks; play safe and skip the cache.
			if !isPlainArg(a) {
				return "", fmt.Errorf("uncacheable argument %T", a)
			}
		}
		fmt.Fprintf(h, "%T\x00%v\x01", a, a)
	}
	for _, t := range tables {
		fmt.Fprintf(h, "%s\x02", t)
	}
	return c.prefix + "q:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (db *DB) emitCached(ctx context.Context, s dialect.Stmt, o op, start time.Time) {
	if len(db.hooks) == 0 {
		return
	}
	e := Event{SQL: s.SQL, Args: s.Args, Duration: time.Since(start), Cached: true, Table: o.table, Action: o.action}
	for _, h := range db.hooks {
		h(ctx, e)
	}
}

// queryTables lists the tables a query reads: its table, joined tables and
// tables of sub-queries used as filter values.
func queryTables(qq q.Query) []string {
	var out []string
	add := func(t string) {
		if t = tableName(t); t != "" {
			out = append(out, t)
		}
	}
	if qq.RawSQL == "" {
		add(qq.Table)
	}
	for _, j := range qq.Joins {
		add(j.Table)
	}
	var walk func(f q.Filter)
	walk = func(f q.Filter) {
		switch x := f.(type) {
		case q.Cond:
			if sub, ok := x.Value.(q.Query); ok {
				out = append(out, queryTables(sub)...)
			}
			for _, v := range x.Values {
				if sub, ok := v.(q.Query); ok {
					out = append(out, queryTables(sub)...)
				}
			}
		case q.AndFilter:
			for _, i := range x.Inner {
				walk(i)
			}
		case q.OrFilter:
			for _, i := range x.Inner {
				walk(i)
			}
		case q.NotFilter:
			walk(x.Inner)
		}
	}
	walk(qq.WhereCond)
	walk(qq.HavingCond)
	return out
}

// tableName strips an alias: "posts p" / "posts AS p" -> "posts".
func tableName(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexAny(t, " \t\n"); i >= 0 {
		t = t[:i]
	}
	return t
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dedupe(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := ss[:0:0]
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---- record / replay ------------------------------------------------------------

// cachedResult is a fully read result set holding raw driver values. It is
// immutable once built, so it may be shared between concurrent replays.
type cachedResult struct {
	cols []string
	rows [][]any
}

// record drains rows into a cachedResult.
func record(rows *sql.Rows) (*cachedResult, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &cachedResult{cols: cols}
	ptrs := make([]any, len(cols))
	for rows.Next() {
		vals := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		res.rows = append(res.rows, vals)
	}
	return res, rows.Err()
}

// replay turns a cachedResult back into *sql.Rows through an in-process
// driver, so the regular scanning code (and database/sql's conversions)
// handle cached rows exactly like live ones.
func replay(ctx context.Context, res *cachedResult) (*sql.Rows, error) {
	return replayDB().QueryContext(ctx, "replay", res)
}

var replayDB = sync.OnceValue(func() *sql.DB {
	db := sql.OpenDB(replayConnector{})
	db.SetMaxIdleConns(64)
	return db
})

type replayConnector struct{}

func (replayConnector) Connect(context.Context) (driver.Conn, error) { return replayConn{}, nil }
func (replayConnector) Driver() driver.Driver                        { return replayDriver{} }

type replayDriver struct{}

func (replayDriver) Open(string) (driver.Conn, error) { return replayConn{}, nil }

type replayConn struct{}

var errReplayOnly = errors.New("orm: replay connection is read-only")

func (replayConn) Prepare(string) (driver.Stmt, error) { return nil, errReplayOnly }
func (replayConn) Close() error                        { return nil }
func (replayConn) Begin() (driver.Tx, error)           { return nil, errReplayOnly }

// CheckNamedValue lets the *cachedResult through unconverted.
func (replayConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (replayConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	if len(args) != 1 {
		return nil, errReplayOnly
	}
	res, ok := args[0].Value.(*cachedResult)
	if !ok {
		return nil, errReplayOnly
	}
	return &replayRows{res: res}, nil
}

type replayRows struct {
	res *cachedResult
	i   int
}

func (r *replayRows) Columns() []string { return r.res.cols }
func (r *replayRows) Close() error      { return nil }
func (r *replayRows) Next(dest []driver.Value) error {
	if r.i >= len(r.res.rows) {
		return io.EOF
	}
	row := r.res.rows[r.i]
	r.i++
	for i := range dest {
		v := row[i]
		if b, ok := v.([]byte); ok { // scanners may keep the slice; hand out a copy
			v = append([]byte(nil), b...)
		}
		dest[i] = v
	}
	return nil
}

// ---- single flight ----------------------------------------------------------------

var errFlightShared = errors.New("orm: shared cache fill failed")

// flightGroup makes concurrent misses for one key run a single query.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	res  *cachedResult
	err  error
}

func (g *flightGroup) do(key string, fn func() (*cachedResult, error)) (*cachedResult, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flightCall{}
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		if c.err != nil {
			return nil, errFlightShared
		}
		return c.res, nil
	}
	c := &flightCall{done: make(chan struct{}), err: errFlightShared} // kept if fn panics
	g.calls[key] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(c.done)
	}()
	c.res, c.err = fn()
	return c.res, c.err
}
