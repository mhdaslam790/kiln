package orm

import (
	"context"
	"testing"
)

type noopPlugin struct{}

func (noopPlugin) Name() string                                  { return "noop" }
func (noopPlugin) BeforeQuery(context.Context, *QueryInfo) error { return nil }
func (noopPlugin) BeforeWrite(context.Context, *WriteInfo) error { return nil }
func (noopPlugin) AfterQuery(context.Context, Event)             {}

// pluginDB is fakeDB with a plugin that implements every hook and does
// nothing, to measure what the hooks themselves cost.
func pluginDB(t testing.TB) *DB {
	base, sqldb := fakeDB(t, 1)
	return Open(sqldb, base.dialect, Use(noopPlugin{}))
}

func BenchmarkTransaction(b *testing.B) {
	db, _ := fakeDB(b, 1)
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = db.Transaction(ctx, func(ctx context.Context) error { return nil })
	}
}

func benchInsertAll100(b *testing.B, db *DB) {
	users, ctx := For[benchUser](db), context.Background()
	list := make([]benchUser, 100)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for j := range list {
			list[j] = benchUser{Name: "n", Email: "e", Age: j}
		}
		_ = users.InsertAll(ctx, list)
	}
}

func BenchmarkInsertAll100(b *testing.B) {
	db, _ := fakeDB(b, 1)
	benchInsertAll100(b, db)
}

// BenchmarkPlugin shows the cost of the hooks when a plugin is installed.
func BenchmarkPlugin(b *testing.B) {
	ctx := context.Background()
	b.Run("FindByID", func(b *testing.B) {
		users := For[benchUser](pluginDB(b))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = users.FindByID(ctx, 1)
		}
	})
	b.Run("Insert", func(b *testing.B) {
		users := For[benchUser](pluginDB(b))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			u := benchUser{Name: "n"}
			_ = users.Insert(ctx, &u)
		}
	})
	b.Run("Update", func(b *testing.B) {
		users := For[benchUser](pluginDB(b))
		u := benchUser{ID: 1, Name: "n"}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = users.Update(ctx, &u, "Age")
		}
	})
	b.Run("InsertAll100", func(b *testing.B) { benchInsertAll100(b, pluginDB(b)) })
}
