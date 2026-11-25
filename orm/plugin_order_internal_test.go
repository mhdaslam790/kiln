package orm

import (
	"context"
	"testing"
)

type seqPlugin struct {
	name string
	log  *[]string
}

func (p seqPlugin) Name() string { return p.name }
func (p seqPlugin) BeforeWrite(context.Context, *WriteInfo) error {
	*p.log = append(*p.log, p.name)
	return nil
}

type seqCheck struct{ seqPlugin }

func (seqCheck) ChecksOnly() {}

func TestCheckPluginsRunLast(t *testing.T) {
	var log []string
	db, _ := fakeDB(t, 1)
	Use(seqCheck{seqPlugin{"check1", &log}})(db)
	Use(seqPlugin{"fill1", &log})(db)
	Use(seqCheck{seqPlugin{"check2", &log}})(db)
	Use(seqPlugin{"fill2", &log})(db)
	must(t, For[benchUser](db).Insert(context.Background(), &benchUser{Name: "x"}))
	if got := fmtList(log); got != "fill1 fill2 check1 check2" {
		t.Fatalf("order: %s", got)
	}
}

func fmtList(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
