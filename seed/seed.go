// Package seed runs named data seeders, e.g. for development fixtures.
//
//	var Set seed.Set
//
//	func init() {
//		Set.Add("admin_user", func(ctx context.Context, db *orm.DB) error {
//			return orm.For[models.User](db).Insert(ctx, &models.User{Email: "admin@example.com"},
//				orm.OnConflictDoNothing("email"))
//		})
//	}
package seed

import (
	"context"
	"fmt"

	"github.com/mhdaslam790/kiln/orm"
)

// Func seeds data. It runs inside a transaction.
type Func func(ctx context.Context, db *orm.DB) error

type entry struct {
	name string
	fn   Func
}

// Set is an ordered collection of seeders.
type Set struct{ items []entry }

// Add registers a seeder. Seeders run in registration order (for generated
// files that is file-name, i.e. creation, order). It panics if name is
// already registered, as two seeders can't be told apart by name.
func (s *Set) Add(name string, fn Func) {
	for _, e := range s.items {
		if e.name == name {
			panic(fmt.Sprintf("seed: %q registered twice", name))
		}
	}
	s.items = append(s.items, entry{name, fn})
}

// Names lists the registered seeders.
func (s *Set) Names() []string {
	out := make([]string, len(s.items))
	for i, e := range s.items {
		out[i] = e.name
	}
	return out
}

// Run executes all seeders, or only the named ones, each in its own
// transaction. Seeders should be idempotent (e.g. use OnConflictDoNothing).
func (s *Set) Run(ctx context.Context, db *orm.DB, names ...string) error {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	// Check every name before running anything: a typo must not leave
	// the other seeders half applied.
	have := map[string]bool{}
	for _, e := range s.items {
		have[e.name] = true
	}
	for _, n := range names {
		if !have[n] {
			return fmt.Errorf("seed %q not found (have %v)", n, s.Names())
		}
	}
	for _, e := range s.items {
		if len(want) > 0 && !want[e.name] {
			continue
		}
		if err := db.Transaction(ctx, func(ctx context.Context) error { return e.fn(ctx, db) }); err != nil {
			return fmt.Errorf("seed %s: %w", e.name, err)
		}
	}
	return nil
}
