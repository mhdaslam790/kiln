package orm

import (
	"container/list"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/mhdaslam790/kiln/dialect"
)

// WithStmtCache keeps up to size prepared statements per connection pool
// (primary and each replica) and reuses them, saving the database the work
// of parsing and planning the same SQL again. On MySQL/MariaDB it also turns
// each query from three round trips (prepare, execute, close) into one.
//
// Results are never cached, so data is always fresh. Statements are only
// reused on the pool, not inside transactions or WithConn blocks, and raw
// SQL containing ';' is never prepared. The database keeps each statement
// prepared on every pooled connection that used it, so keep size modest
// (MySQL limits prepared statements server-wide: max_prepared_stmt_count).
// After a schema change the database may reject a stale statement once;
// it is then dropped and prepared again on the next call.
func WithStmtCache(size int) Option {
	return func(db *DB) {
		if size > 0 {
			db.stmts = &stmtCache{size: size, pools: map[*sql.DB]*stmtLRU{}}
		}
	}
}

type stmtCache struct {
	mu    sync.Mutex
	size  int
	pools map[*sql.DB]*stmtLRU
}

type stmtLRU struct {
	ll    *list.List
	items map[string]*list.Element
}

type stmtEntry struct {
	sql  string
	stmt *sql.Stmt
}

// get returns a prepared statement for query on pool, preparing it on a miss.
func (c *stmtCache) get(ctx context.Context, pool *sql.DB, query string) (*sql.Stmt, error) {
	c.mu.Lock()
	l := c.pools[pool]
	if l == nil {
		l = &stmtLRU{ll: list.New(), items: map[string]*list.Element{}}
		c.pools[pool] = l
	}
	if el, ok := l.items[query]; ok {
		l.ll.MoveToFront(el)
		st := el.Value.(*stmtEntry).stmt
		c.mu.Unlock()
		return st, nil
	}
	c.mu.Unlock()

	// Prepare outside the lock; a concurrent miss may prepare the same SQL,
	// in which case the loser is closed.
	st, err := pool.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := l.items[query]; ok {
		_ = st.Close()
		l.ll.MoveToFront(el)
		return el.Value.(*stmtEntry).stmt, nil
	}
	l.items[query] = l.ll.PushFront(&stmtEntry{sql: query, stmt: st})
	for l.ll.Len() > c.size {
		e := l.ll.Remove(l.ll.Back()).(*stmtEntry)
		delete(l.items, e.sql)
		_ = e.stmt.Close() // safe: database/sql waits for open rows
	}
	return st, nil
}

// evict drops a statement that failed, so the next call prepares it again.
func (c *stmtCache) evict(pool *sql.DB, query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.pools[pool]; l != nil {
		if el, ok := l.items[query]; ok {
			l.ll.Remove(el)
			delete(l.items, query)
			_ = el.Value.(*stmtEntry).stmt.Close()
		}
	}
}

func (c *stmtCache) closeAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.pools {
		for el := l.ll.Front(); el != nil; el = el.Next() {
			_ = el.Value.(*stmtEntry).stmt.Close()
		}
	}
	c.pools = map[*sql.DB]*stmtLRU{}
}

// cachedPool returns the pool to prepare on, or nil when the statement
// should run unprepared.
func (db *DB) cachedPool(c conn, query string) *sql.DB {
	if db.stmts == nil || strings.Contains(query, ";") {
		return nil
	}
	pool, _ := c.(*sql.DB)
	return pool
}

func (db *DB) shouldEvict(err error) bool {
	return err != nil && db.dialect.Classify(err) == dialect.ErrOther
}

func (db *DB) query(ctx context.Context, c conn, s dialect.Stmt) (*sql.Rows, error) {
	if len(db.guards) != 0 {
		return db.guardedQuery(ctx, c, s)
	}
	return db.rawQuery(ctx, c, s)
}

func (db *DB) exec(ctx context.Context, c conn, s dialect.Stmt) (sql.Result, error) {
	if len(db.guards) != 0 {
		return db.guardedExec(ctx, c, s)
	}
	return db.rawExec(ctx, c, s)
}

func (db *DB) guardedQuery(ctx context.Context, c conn, s dialect.Stmt) (*sql.Rows, error) {
	if err := db.allow(ctx); err != nil {
		return nil, err
	}
	rows, err := db.rawQuery(ctx, c, s)
	db.done(ctx, err)
	return rows, err
}

func (db *DB) guardedExec(ctx context.Context, c conn, s dialect.Stmt) (sql.Result, error) {
	if err := db.allow(ctx); err != nil {
		return nil, err
	}
	res, err := db.rawExec(ctx, c, s)
	db.done(ctx, err)
	return res, err
}

// allow asks every guard; if one refuses, the guards that allowed are told
// the round trip did not happen (with the refusal), so they stay balanced.
func (db *DB) allow(ctx context.Context) error {
	for i, g := range db.guards {
		if err := g.Allow(ctx); err != nil {
			err = fmt.Errorf("orm: plugin %s: %w", g.Name(), err)
			for _, prev := range db.guards[:i] {
				prev.Done(ctx, err)
			}
			return err
		}
	}
	return nil
}

func (db *DB) done(ctx context.Context, err error) {
	for _, g := range db.guards {
		g.Done(ctx, err)
	}
}

func (db *DB) rawQuery(ctx context.Context, c conn, s dialect.Stmt) (*sql.Rows, error) {
	if pool := db.cachedPool(c, s.SQL); pool != nil {
		if st, err := db.stmts.get(ctx, pool, s.SQL); err == nil {
			rows, err := st.QueryContext(ctx, s.Args...)
			if db.shouldEvict(err) && ctx.Err() == nil {
				db.stmts.evict(pool, s.SQL)
			}
			return rows, err
		}
		// Could not prepare (e.g. unsupported statement): run it directly.
	}
	return c.QueryContext(ctx, s.SQL, s.Args...)
}

func (db *DB) rawExec(ctx context.Context, c conn, s dialect.Stmt) (sql.Result, error) {
	if pool := db.cachedPool(c, s.SQL); pool != nil {
		if st, err := db.stmts.get(ctx, pool, s.SQL); err == nil {
			res, err := st.ExecContext(ctx, s.Args...)
			if db.shouldEvict(err) && ctx.Err() == nil {
				db.stmts.evict(pool, s.SQL)
			}
			return res, err
		}
	}
	return c.ExecContext(ctx, s.SQL, s.Args...)
}
