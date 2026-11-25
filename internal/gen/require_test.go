package gen

import (
	"os"
	"strings"
	"testing"
)

func TestCheckImports(t *testing.T) {
	p := newProject(t)
	src := []byte(`package main

import (
	"context"

	_ "github.com/jackc/pgx/v5/stdlib"

	"example.com/app/database/migrations"
	"github.com/mhdaslam790/kiln/migrate"
	"github.com/mhdaslam790/kiln/orm"
)
`)
	cases := []struct{ gomod, want string }{
		{"", "go get github.com/jackc/pgx/v5/stdlib github.com/mhdaslam790/kiln@"},
		{"require github.com/mhdaslam790/kiln v1.10.1\n", "go get github.com/jackc/pgx/v5/stdlib\n"},
		{"require github.com/mhdaslam790/kiln/audit v0.1.1\nrequire github.com/jackc/pgx/v5 v5.7.0\n", "go get github.com/mhdaslam790/kiln@"},
		{"require (\n\tgithub.com/jackc/pgx/v5 v5.7.0 // indirect\n\tgithub.com/mhdaslam790/kiln v1.10.1\n)\n", ""},
	}
	for _, c := range cases {
		os.WriteFile(p.path("go.mod"), []byte("module example.com/app\n\ngo 1.23\n\n"+c.gomod), 0o644)
		err := p.checkImports(src)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%q: %v", c.gomod, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error()+"\n", c.want)):
			t.Errorf("%q: got %v, want %q", c.gomod, err, c.want)
		}
	}
}
