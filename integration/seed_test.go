package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/seed"
)

func TestSeedRun(t *testing.T) {
	sqldb, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	db := orm.Open(sqldb, dialect.SQLite())
	var ran []string
	var s seed.Set
	for _, n := range []string{"a", "b"} {
		s.Add(n, func(context.Context, *orm.DB) error { ran = append(ran, n); return nil })
	}
	ctx := context.Background()
	if err := s.Run(ctx, db, "b", "typo"); err == nil || !strings.Contains(err.Error(), "typo") || len(ran) != 0 {
		t.Fatalf("typo: err %v, ran %v", err, ran)
	}
	if err := s.Run(ctx, db, "b"); err != nil || strings.Join(ran, ",") != "b" {
		t.Fatalf("named: %v %v", err, ran)
	}
	ran = nil
	if err := s.Run(ctx, db); err != nil || strings.Join(ran, ",") != "a,b" {
		t.Fatalf("all: %v %v", err, ran)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate name accepted")
		}
	}()
	s.Add("a", nil)
}
