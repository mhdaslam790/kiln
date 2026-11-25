package integration

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mhdaslam790/kiln/audit"
	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/kilnbreaker"
	"github.com/mhdaslam790/kiln/kilncrypt"
	"github.com/mhdaslam790/kiln/kilnrw"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
	"github.com/mhdaslam790/kiln/tenant"
)

var benchSeq atomic.Int64 // unique emails across benchmark rounds

// BenchmarkFeatures measures what each feature costs when it is ON, on a
// real (SQLite) database, so overhead shows next to the cost of a query.
// Compare each sub-benchmark with "none".
//
//	go test -run '^$' -bench Features -benchmem ./integration
func BenchmarkFeatures(b *testing.B) {
	ctx := context.Background()
	keys, _ := kilncrypt.NewKeys(bytes.Repeat([]byte{'i'}, 32), kilncrypt.Key{ID: 1, Secret: bytes.Repeat([]byte{'a'}, 32)})
	emailIdx, _ := keys.Index("email", "email_index", strings.ToLower)

	setups := []struct {
		name string
		opts func(sqldb *sql.DB) []orm.Option
		ctx  func(context.Context) context.Context
	}{
		{"none", nil, nil},
		{"tenant", func(*sql.DB) []orm.Option { return []orm.Option{tenant.Plugin("tenant_id")} },
			func(c context.Context) context.Context { return tenant.With(c, int64(1)) }},
		{"audit", func(*sql.DB) []orm.Option { return []orm.Option{audit.Plugin()} }, nil},
		{"kilncrypt", func(*sql.DB) []orm.Option { return []orm.Option{kilncrypt.Plugin(keys, emailIdx)} }, nil},
		{"kilnbreaker", func(*sql.DB) []orm.Option { return []orm.Option{kilnbreaker.Plugin()} }, nil},
		{"kilnrw", func(s *sql.DB) []orm.Option { return []orm.Option{orm.WithReplicas(s), orm.Use(kilnrw.New())} },
			kilnrw.Track},
		{"cache", func(*sql.DB) []orm.Option { return []orm.Option{orm.WithCache()} },
			func(c context.Context) context.Context { return orm.Cached(c, 0) }},
		{"stmtcache", func(*sql.DB) []orm.Option { return []orm.Option{orm.WithStmtCache(64)} }, nil},
	}
	for _, s := range setups {
		b.Run(s.name, func(b *testing.B) {
			sqldb, err := sql.Open("sqlite", "file:"+filepath.Join(b.TempDir(), "b.db")+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
			if err != nil {
				b.Fatal(err)
			}
			sqldb.SetMaxOpenConns(1)
			defer sqldb.Close()
			plain := orm.Open(sqldb, dialect.SQLite())
			if _, err := migrate.New(plain, append(migrations(), auditTable, patientsTable)).Up(ctx); err != nil {
				b.Fatal(err)
			}
			var opts []orm.Option
			if s.opts != nil {
				opts = s.opts(sqldb)
			}
			db := orm.Open(sqldb, dialect.SQLite(), opts...)
			c := ctx
			if s.ctx != nil {
				c = s.ctx(ctx)
			}

			if s.name == "none" { // baseline for kilncrypt: same table, value encrypted once up front
				sealed, _ := keys.SealColumn("x@x.io", "patients", "email")
				pats := orm.For[Patient](db)
				b.Run("PatientInsert", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						p := Patient{Name: "x", Email: sealed}
						if err := pats.Insert(c, &p); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
			if s.name == "kilncrypt" { // the encrypted model
				pats := orm.For[Patient](db)
				p := Patient{Name: "ann", Email: kilncrypt.NewString("ann@x.io")}
				if err := pats.Insert(c, &p); err != nil {
					b.Fatal(err)
				}
				b.Run("Find", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						got, err := pats.Find(c, emailIdx.Eq("ann@x.io"))
						if err != nil {
							b.Fatal(err)
						}
						if _, err := got.Email.Open(keys); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run("PatientInsert", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						p := Patient{Name: "x", Email: kilncrypt.NewString("x@x.io")}
						if err := pats.Insert(c, &p); err != nil {
							b.Fatal(err)
						}
					}
				})
				return
			}

			users := orm.For[User](db)
			u := User{TenantID: 1, Name: "ann", Email: "ann@x.io"}
			if err := users.Insert(c, &u); err != nil {
				b.Fatal(err)
			}
			b.Run("FindByID", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := users.FindByID(c, u.ID); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("FindAll", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := users.FindAll(c, q.Limit(20)); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Insert", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					v := User{TenantID: 1, Name: "x", Email: "x" + strconv.FormatInt(benchSeq.Add(1), 10) + "@x.io"}
					if err := users.Insert(c, &v); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Update", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					u.Age = i
					if err := users.Update(c, &u); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
