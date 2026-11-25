package integration

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhdaslam790/kiln/dialect"
	"github.com/mhdaslam790/kiln/kilnvalidate"
	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/tenant"
)

type VBase struct {
	Code      string `validate:"required,max=3"`
	CreatedAt time.Time
}

type VItem struct {
	ID       int64
	TenantID int64 `validate:"required"`
	VBase
	Name string `validate:"required"`
}

func (VItem) TableName() string { return "vitems" }

func TestValidateWithOtherPlugins(t *testing.T) {
	ctx := context.Background()
	sqldb, _ := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "v.db"))
	defer sqldb.Close()
	if _, err := sqldb.Exec(`CREATE TABLE vitems (id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id INTEGER, code TEXT, created_at DATETIME, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, validateFirst := range []bool{true, false} {
		opts := []orm.Option{tenant.Plugin("tenant_id"), kilnvalidate.Plugin()}
		if validateFirst { // the order must not matter
			opts = []orm.Option{kilnvalidate.Plugin(), tenant.Plugin("tenant_id")}
		}
		r := orm.For[VItem](orm.Open(sqldb, dialect.SQLite(), opts...))
		tctx := tenant.With(ctx, int64(1))
		it := VItem{VBase: VBase{Code: "ab"}, Name: "n"}
		must(t, r.Insert(tctx, &it)) // tenant_id (required) is filled by the tenant plugin first
		it.Code = "toolong"
		if err := r.Update(tctx, &it, "Code"); !errors.Is(err, kilnvalidate.ErrInvalid) {
			t.Fatalf("validateFirst=%v: partial update of an embedded field not checked: %v", validateFirst, err)
		}
		it.Code, it.Name = "ok", ""
		must(t, r.Update(tctx, &it, "Code")) // Name is not written, so not checked
	}
}
