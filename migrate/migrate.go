// Package migrate runs versioned schema migrations written with package schema.
//
//	var Migrations migrate.Set
//
//	func init() {
//		Migrations.Add(20261123120000, "create_users",
//			func(s *schema.Schema) {
//				s.CreateTable("users", func(t *schema.Table) {
//					t.ID("id")
//					t.String("email", schema.Required(), schema.Unique())
//					t.Timestamps()
//				})
//			},
//			func(s *schema.Schema) { s.DropTable("users") },
//		)
//	}
//
//	m := migrate.New(db, Migrations.List()...)
//	applied, err := m.Up(ctx)
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/schema"
)

// Func describes schema changes for one direction of a migration.
type Func func(s *schema.Schema)

// Migration is one versioned migration.
type Migration struct {
	Version int64
	Name    string
	Up      Func
	Down    Func
	// File is the base name of the source file that registered the
	// migration (set by Set.Add). It is recorded when the migration is
	// applied.
	File string
}

// Set collects migrations, typically from init() functions in generated files.
type Set struct{ items []Migration }

// Add registers a migration. down may be nil for irreversible migrations.
// The file calling Add is recorded as the migration's File.
func (s *Set) Add(version int64, name string, up, down Func) {
	mg := Migration{Version: version, Name: name, Up: up, Down: down}
	if _, file, _, ok := runtime.Caller(1); ok {
		mg.File = filepath.Base(file)
	}
	s.items = append(s.items, mg)
}

// List returns the migrations sorted by version.
func (s *Set) List() []Migration {
	out := append([]Migration(nil), s.items...)
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// local returns t in the local time zone: PostgreSQL returns it so, the
// other databases as UTC.
func local(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Local()
}

// Status describes one migration's state.
type Status struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time // in the local time zone, whatever the database returns
	// Missing is true for versions recorded in the database that are no
	// longer present in the code.
	Missing bool
	// File is the migration's source file: from the code, or from the
	// database for a missing migration.
	File string
	// Checksum is the SHA-256 of the SQL the migration's Up produces
	// (from the database for a missing migration).
	Checksum string
	// Changed is true when the migration was applied with a different
	// checksum: its Up was edited after it ran. Changes inside Do
	// functions are not detected.
	Changed bool
}

// Option configures a Migrator.
type Option func(*Migrator)

// WithTable changes the bookkeeping table (default "schema_migrations").
func WithTable(name string) Option { return func(m *Migrator) { m.table = name } }

// WithLog receives a line for every applied / rolled back migration.
func WithLog(fn func(format string, args ...any)) Option { return func(m *Migrator) { m.logf = fn } }

// WithoutLock disables the cross-process migration lock.
func WithoutLock() Option { return func(m *Migrator) { m.noLock = true } }

// Migrator applies and rolls back migrations.
type Migrator struct {
	db     *orm.DB
	list   []Migration
	table  string
	logf   func(string, ...any)
	noLock bool
	meta   bool // the table has the file and checksum columns

	sumMu sync.Mutex
	sums  map[int64]string // checksums of m.list, computed once
}

// New creates a Migrator. Duplicate versions cause a panic at Up/Down time.
func New(db *orm.DB, migrations []Migration, opts ...Option) *Migrator {
	m := &Migrator{db: db, table: "schema_migrations", logf: func(string, ...any) {}}
	m.list = append([]Migration(nil), migrations...)
	sort.Slice(m.list, func(i, j int) bool { return m.list[i].Version < m.list[j].Version })
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Migrator) validate() error {
	for i := 1; i < len(m.list); i++ {
		if m.list[i].Version == m.list[i-1].Version {
			return fmt.Errorf("migrate: duplicate version %d (%s, %s)", m.list[i].Version, m.list[i-1].Name, m.list[i].Name)
		}
	}
	for _, mg := range m.list {
		if mg.Up == nil {
			return fmt.Errorf("migrate: %d_%s has no Up function", mg.Version, mg.Name)
		}
	}
	return nil
}

func (m *Migrator) ensureTable(ctx context.Context) error {
	var s schema.Schema
	s.CreateTableIfNotExists(m.table, func(t *schema.Table) {
		t.BigInt("version", schema.Primary(), schema.Required())
		t.String("name", schema.Required())
		t.DateTime("applied_at", schema.Required())
		t.String("file")
		t.String("checksum", schema.Limit(64))
	})
	return m.exec(ctx, &s)
}

// hasMeta reports whether the bookkeeping table has the file and checksum
// columns (tables created by older versions of kiln don't).
func (m *Migrator) hasMeta(ctx context.Context) bool {
	if m.meta {
		return true
	}
	var rows []struct {
		File string `db:"file"`
	}
	d := m.db.Dialect()
	m.meta = m.db.Query(ctx, &rows, "SELECT "+d.Quote("file")+" FROM "+d.Quote(m.table)+" WHERE "+d.Quote("checksum")+" = 'x' AND 1 = 0") == nil
	return m.meta
}

// upgradeTable creates the bookkeeping table, or adds the file and checksum
// columns to an older one and fills them in for migrations applied before
// the upgrade, from the current code. It returns the applied migrations, so
// callers read the table once. Callers hold the migration lock.
func (m *Migrator) upgradeTable(ctx context.Context) (map[int64]appliedRow, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	if !m.hasMeta(ctx) {
		var s schema.Schema
		s.AddColumn(m.table, "file", schema.String)
		s.AddColumn(m.table, "checksum", schema.String, schema.Limit(64))
		if err := m.exec(ctx, &s); err != nil {
			return nil, fmt.Errorf("migrate: add file and checksum to %s: %w", m.table, err)
		}
		m.meta = true
	}
	done, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	d := m.db.Dialect()
	update := "UPDATE " + d.Quote(m.table) + " SET " + d.Quote("file") + " = ?, " + d.Quote("checksum") + " = ? WHERE version = ?"
	for _, mg := range m.list {
		r, ok := done[mg.Version]
		if !ok || r.Checksum != "" {
			continue
		}
		sum, err := m.checksum(mg)
		if err != nil {
			return nil, err
		}
		if _, err := m.db.Exec(ctx, update, mg.File, sum, mg.Version); err != nil {
			return nil, err
		}
		r.File, r.Checksum = mg.File, sum
		done[mg.Version] = r
	}
	return done, nil
}

type appliedRow struct {
	Version   int64     `db:"version"`
	Name      string    `db:"name"`
	AppliedAt time.Time `db:"applied_at"`
	File      string    `db:"file"`
	Checksum  string    `db:"checksum"`
}

func (m *Migrator) applied(ctx context.Context) (map[int64]appliedRow, error) {
	var rows []appliedRow
	cols := "version, name, applied_at"
	if m.hasMeta(ctx) {
		// FILE is reserved in SQL Server: always quote the new columns.
		d := m.db.Dialect()
		f, c := d.Quote("file"), d.Quote("checksum")
		cols += ", COALESCE(" + f + ", '') AS " + f + ", COALESCE(" + c + ", '') AS " + c
	}
	err := m.db.Query(ctx, &rows, "SELECT "+cols+" FROM "+m.db.Dialect().Quote(m.table))
	if err != nil {
		return nil, err
	}
	out := make(map[int64]appliedRow, len(rows))
	for _, r := range rows {
		out[r.Version] = r
	}
	return out, nil
}

// Status lists every known migration plus versions that exist only in the database.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	done, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	return m.status(done), nil
}

func (m *Migrator) status(done map[int64]appliedRow) []Status {
	var out []Status
	known := map[int64]bool{}
	for _, mg := range m.list {
		known[mg.Version] = true
		r, ok := done[mg.Version]
		sum, _ := m.checksum(mg) // a broken Up is reported by Up itself
		out = append(out, Status{
			Version: mg.Version, Name: mg.Name, Applied: ok, AppliedAt: local(r.AppliedAt),
			File: mg.File, Checksum: sum, Changed: ok && r.Checksum != "" && sum != "" && r.Checksum != sum,
		})
	}
	for v, r := range done {
		if !known[v] {
			out = append(out, Status{Version: v, Name: r.Name, Applied: true, AppliedAt: local(r.AppliedAt), Missing: true,
				File: r.File, Checksum: r.Checksum})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// Pending returns migrations not yet applied.
func (m *Migrator) Pending(ctx context.Context) ([]Migration, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	done, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	return m.pending(done), nil
}

func (m *Migrator) pending(done map[int64]appliedRow) []Migration {
	var out []Migration
	for _, mg := range m.list {
		if _, ok := done[mg.Version]; !ok {
			out = append(out, mg)
		}
	}
	return out
}

// Up applies all pending migrations in version order.
func (m *Migrator) Up(ctx context.Context) ([]Migration, error) { return m.UpTo(ctx, 0) }

// UpTo applies pending migrations with version <= target (0 means all).
func (m *Migrator) UpTo(ctx context.Context, target int64) (applied []Migration, err error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	err = m.locked(ctx, func() error {
		done, err := m.upgradeTable(ctx)
		if err != nil {
			return err
		}
		m.warnChanged(done)
		for _, mg := range m.pending(done) {
			if target > 0 && mg.Version > target {
				break
			}
			if err := m.run(ctx, mg, true); err != nil {
				return err
			}
			applied = append(applied, mg)
		}
		return nil
	})
	return applied, err
}

// Down rolls back the last `steps` applied migrations (newest first).
func (m *Migrator) Down(ctx context.Context, steps int) (rolled []Migration, err error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	if steps < 1 {
		steps = 1
	}
	err = m.locked(ctx, func() error {
		done, err := m.upgradeTable(ctx)
		if err != nil {
			return err
		}
		byVersion := map[int64]Migration{}
		for _, mg := range m.list {
			byVersion[mg.Version] = mg
		}
		versions := make([]int64, 0, len(done))
		for v := range done {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })
		for _, v := range versions {
			if len(rolled) == steps {
				break
			}
			mg, ok := byVersion[v]
			if !ok {
				return fmt.Errorf("migrate: version %d (%s) is applied but missing from code", v, done[v].Name)
			}
			if mg.Down == nil {
				return fmt.Errorf("migrate: %d_%s is irreversible (no Down)", mg.Version, mg.Name)
			}
			if err := m.run(ctx, mg, false); err != nil {
				return err
			}
			rolled = append(rolled, mg)
		}
		return nil
	})
	return rolled, err
}

// Redo rolls back the latest migration and applies it again.
func (m *Migrator) Redo(ctx context.Context) error {
	rolled, err := m.Down(ctx, 1)
	if err != nil || len(rolled) == 0 {
		return err
	}
	_, err = m.UpTo(ctx, rolled[0].Version)
	return err
}

// warnChanged logs every applied migration whose Up changed since it ran.
func (m *Migrator) warnChanged(done map[int64]appliedRow) {
	for _, s := range m.status(done) {
		if s.Changed {
			m.logf("warning: %d_%s was edited after it was applied (checksum differs); the database still has the old version", s.Version, s.Name)
		}
	}
}

// Checksum returns the SHA-256 (hex) of the SQL, with arguments, that the
// migration's Up produces for this Migrator's dialect. Do steps count only
// as a placeholder, so edits inside them don't change the checksum.
func (m *Migrator) Checksum(mg Migration) (string, error) {
	stmts, err := m.compile(mg, true, true)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(strings.Join(stmts, "\n")))
	return hex.EncodeToString(h[:]), nil
}

// checksum is Checksum, cached for the Migrator's own migrations (their
// versions are unique once validated).
func (m *Migrator) checksum(mg Migration) (string, error) {
	m.sumMu.Lock()
	sum, ok := m.sums[mg.Version]
	m.sumMu.Unlock()
	if ok {
		return sum, nil
	}
	sum, err := m.Checksum(mg)
	if err != nil {
		return "", err
	}
	m.sumMu.Lock()
	if m.sums == nil {
		m.sums = map[int64]string{}
	}
	m.sums[mg.Version] = sum
	m.sumMu.Unlock()
	return sum, nil
}

// SQL compiles a migration direction into SQL without running it (dry run).
// Do steps are rendered as comments.
func (m *Migrator) SQL(mg Migration, up bool) ([]string, error) {
	return m.compile(mg, up, false)
}

func (m *Migrator) compile(mg Migration, up, withArgs bool) ([]string, error) {
	fn := mg.Up
	if !up {
		fn = mg.Down
	}
	if fn == nil {
		return nil, fmt.Errorf("migrate: %d_%s has no %s function", mg.Version, mg.Name, map[bool]string{true: "Up", false: "Down"}[up])
	}
	var s schema.Schema
	fn(&s)
	var out []string
	for _, op := range s.Ops {
		if _, ok := op.(schema.Do); ok {
			out = append(out, "-- (Go function)")
			continue
		}
		if m.db.Dialect().Kind() == dialect.KindSQLite {
			switch o := op.(type) {
			case schema.ChangeColumn:
				out = append(out, "-- SQLite: rebuild table "+o.Table+" to change column "+o.Column.Name)
				continue
			case schema.AddForeignKey:
				out = append(out, "-- SQLite: rebuild table "+o.Table+" to add foreign key "+o.Key.Name)
				continue
			case schema.DropForeignKey:
				out = append(out, "-- SQLite: rebuild table "+o.Table+" to drop foreign key "+o.Name)
				continue
			}
		}
		stmts, err := m.db.Dialect().DDL(op)
		if err != nil {
			return nil, err
		}
		for _, st := range stmts {
			out = append(out, st.SQL)
			if withArgs && len(st.Args) > 0 {
				out = append(out, fmt.Sprintf("-- args: %#v", st.Args))
			}
		}
	}
	return out, nil
}

func (m *Migrator) run(ctx context.Context, mg Migration, up bool) error {
	start := time.Now()
	var s schema.Schema
	if up {
		mg.Up(&s)
	} else {
		mg.Down(&s)
	}
	apply := func(ctx context.Context) error {
		if err := m.exec(ctx, &s); err != nil {
			return err
		}
		d := m.db.Dialect()
		tbl := d.Quote(m.table)
		if up {
			sum, err := m.checksum(mg)
			if err != nil {
				return err
			}
			_, err = m.db.Exec(ctx, "INSERT INTO "+tbl+" (version, name, applied_at, "+d.Quote("file")+", "+d.Quote("checksum")+") VALUES (?, ?, ?, ?, ?)",
				mg.Version, mg.Name, time.Now().UTC(), mg.File, sum)
			return err
		}
		_, err := m.db.Exec(ctx, "DELETE FROM "+tbl+" WHERE version = ?", mg.Version)
		return err
	}
	err := m.withSQLiteFKsOff(ctx, func(ctx context.Context) error {
		if needsNoTx(m.db.Dialect().Kind(), s.Ops) {
			// CREATE INDEX CONCURRENTLY cannot run in a transaction.
			return apply(ctx)
		}
		return m.db.Transaction(ctx, apply)
	})
	dir := "up"
	if !up {
		dir = "down"
	}
	if err != nil {
		return fmt.Errorf("migrate %s %d_%s: %w", dir, mg.Version, mg.Name, err)
	}
	m.logf("migrated %s %d_%s (%s)", dir, mg.Version, mg.Name, time.Since(start).Round(time.Millisecond))
	return nil
}

func (m *Migrator) exec(ctx context.Context, s *schema.Schema) error {
	for _, op := range s.Ops {
		if do, ok := op.(schema.Do); ok {
			if err := do.Fn(ctx, m.db.Executor(ctx)); err != nil {
				return err
			}
			continue
		}
		if handled, err := m.sqliteAlter(ctx, op); handled || err != nil {
			if err != nil {
				return fmt.Errorf("%w (op %T)", err, op)
			}
			continue
		}
		stmts, err := m.db.Dialect().DDL(op)
		if err != nil {
			return err
		}
		for _, st := range stmts {
			if _, err := m.db.Executor(ctx).ExecContext(ctx, st.SQL, st.Args...); err != nil {
				return fmt.Errorf("%w\n  sql: %s", err, st.SQL)
			}
		}
	}
	return nil
}

const lockName = "kiln_migrate"

// locked holds a database-level lock so that concurrently starting
// instances do not run migrations twice.
func (m *Migrator) locked(ctx context.Context, fn func() error) error {
	k := m.db.Dialect().Kind()
	if m.noLock {
		return fn()
	}
	if k == dialect.KindSQLite {
		return m.sqliteLocked(ctx, fn)
	}
	conn, err := m.db.SQL().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var lock, unlock string
	switch k {
	case dialect.KindPostgres:
		lock, unlock = "SELECT pg_advisory_lock(7262017001)", "SELECT pg_advisory_unlock(7262017001)"
	case dialect.KindMySQL:
		lock, unlock = "SELECT GET_LOCK('"+lockName+"', 300)", "SELECT RELEASE_LOCK('"+lockName+"')"
	case dialect.KindMSSQL:
		lock = "EXEC sp_getapplock @Resource = '" + lockName + "', @LockMode = 'Exclusive', @LockOwner = 'Session', @LockTimeout = 300000"
		unlock = "EXEC sp_releaseapplock @Resource = '" + lockName + "', @LockOwner = 'Session'"
	}
	if k == dialect.KindMySQL {
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, lock).Scan(&got); err != nil {
			return err
		}
		if got.Int64 != 1 {
			return errors.New("migrate: timed out waiting for migration lock")
		}
	} else if _, err := conn.ExecContext(ctx, lock); err != nil {
		return fmt.Errorf("migrate: acquire lock: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), unlock) //nolint:errcheck
	return fn()
}

// SQLite has no lock functions: a row in a lock table is the lock. A lock
// older than sqliteLockStale (a crashed process) is taken over.
const (
	sqliteLockTable = "kiln_migrate_lock"
	sqliteLockStale = 10 * time.Minute
	sqliteLockWait  = 5 * time.Minute
)

func (m *Migrator) sqliteLocked(ctx context.Context, fn func() error) error {
	db := m.db.SQL()
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+sqliteLockTable+" (id INTEGER PRIMARY KEY, since INTEGER NOT NULL)"); err != nil && !busy(err) {
		return fmt.Errorf("migrate: lock table: %w", err)
	}
	deadline := time.Now().Add(sqliteLockWait)
	for {
		now := time.Now()
		_, err := db.ExecContext(ctx, "INSERT INTO "+sqliteLockTable+" (id, since) VALUES (1, ?)", now.UnixNano())
		if err == nil {
			break
		}
		// Held by another instance (or the database is busy): take over a
		// stale lock, otherwise wait.
		_, _ = db.ExecContext(ctx, "DELETE FROM "+sqliteLockTable+" WHERE id = 1 AND since < ?", now.Add(-sqliteLockStale).UnixNano())
		if now.After(deadline) {
			return fmt.Errorf("migrate: timed out waiting for migration lock (%s; delete its row if no migration is running): %w", sqliteLockTable, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer db.ExecContext(context.WithoutCancel(ctx), "DELETE FROM "+sqliteLockTable+" WHERE id = 1") //nolint:errcheck
	return fn()
}

// busy reports a SQLite "database is locked" error, retried by the caller.
func busy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY")
}

// needsNoTx reports whether ops must run outside a transaction.
func needsNoTx(k dialect.Kind, ops []schema.Op) bool {
	if k != dialect.KindPostgres {
		return false
	}
	for _, op := range ops {
		if ix, ok := op.(schema.CreateIndex); ok && ix.Concurrently {
			return true
		}
	}
	return false
}
