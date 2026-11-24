package integration

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/kilnrw"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

type sessionKey struct{}

// TestReadYourWrites uses a "replica" that never catches up (a separate
// database), the worst case of replica lag: any read that wrongly goes to
// it misses the data.
func TestReadYourWrites(t *testing.T) {
	ctx := context.Background()
	openDB := func(name string) *sql.DB {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), name)+"?_pragma=busy_timeout(5000)")
		must(t, err)
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		_, err = migrate.New(orm.Open(db, dialect.SQLite()), migrations()).Up(ctx)
		must(t, err)
		return db
	}
	primary, replica := openDB("primary.db"), openDB("replica.db")
	rw := kilnrw.New(kilnrw.Key(func(ctx context.Context) string { s, _ := ctx.Value(sessionKey{}).(string); return s }))
	db := orm.Open(primary, dialect.SQLite(), orm.WithReplicas(replica), orm.Use(rw))
	users := orm.For[User](db)

	// Same request: the write and the read share a tracked context.
	req := kilnrw.Track(ctx)
	ann := User{TenantID: 1, Name: "ann", Email: "rw-ann@x.io"}
	must(t, users.Insert(req, &ann))
	if _, err := users.FindByID(req, ann.ID); err != nil {
		t.Fatalf("read after insert in the same request: %v", err)
	}
	if _, err := users.FindByID(ctx, ann.ID); !errors.Is(err, orm.ErrNotFound) {
		t.Fatalf("an unrelated read should use the (stale) replica: %v", err)
	}

	// Across requests: the session key remembers the write.
	sess := func() context.Context { return context.WithValue(kilnrw.Track(ctx), sessionKey{}, "s1") }
	_, err := users.UpdateAny(sess(), q.Eq("id", ann.ID), q.Set("name", "ann b"))
	must(t, err)
	got, err := users.FindByID(sess(), ann.ID) // next request, new context
	must(t, err)
	if got.Name != "ann b" {
		t.Fatalf("stale read: %q", got.Name)
	}
	other := context.WithValue(ctx, sessionKey{}, "s2")
	if n, _ := users.Count(other); n != 0 {
		t.Fatalf("another session read the primary (%d rows)", n)
	}

	// Raw Exec and transactions count as writes too.
	raw := kilnrw.Track(ctx)
	_, err = db.Exec(raw, "UPDATE users SET age = ? WHERE id = ?", 40, ann.ID)
	must(t, err)
	if n, err := users.Count(raw, q.Eq("age", 40)); err != nil || n != 1 {
		t.Fatalf("after raw exec: %d %v", n, err)
	}
	txctx := kilnrw.Track(ctx)
	must(t, db.Transaction(txctx, func(ctx context.Context) error {
		return users.Insert(ctx, &User{TenantID: 1, Name: "tx", Email: "rw-tx@x.io"})
	}))
	if n, err := users.Count(txctx); err != nil || n != 2 {
		t.Fatalf("after transaction: %d %v", n, err)
	}
}
