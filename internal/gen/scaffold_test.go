package gen

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLowerFirst(t *testing.T) {
	for in, want := range map[string]string{"User": "user", "OrderItem": "orderItem", "URLRecord": "urlRecord", "ID": "id", "A": "a"} {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScaffoldEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go build")
	}
	p := linkedProject(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.Dialect = "sqlite"
	must(p.Init(cfg, false))
	must(p.Create("User", []string{"name:string", "email:string:unique"}, CreateOptions{}))
	must(p.Create("OrderItem", []string{"qty:int"}, CreateOptions{}))

	// The default layout is clean architecture under database/ and internal/.
	for _, f := range []string{"database/migrations", "database/seeds", "internal/domain/user.go", "cmd/migrate/main.go"} {
		if _, err := os.Stat(p.path(f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}

	must(p.Scaffold("User", ScaffoldOptions{}))
	must(p.Scaffold("order_item", ScaffoldOptions{})) // shared helpers are not duplicated
	for _, f := range []string{
		"internal/domain/errors.go", "internal/domain/user_repository.go", "internal/domain/order_item_repository.go",
		"internal/repository/errors.go", "internal/repository/user_repository.go", "internal/repository/order_item_repository.go",
		"internal/usecase/user_usecase.go", "internal/usecase/order_item_usecase.go",
	} {
		if _, err := os.Stat(p.path(f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if got := read(t, p.path("internal", "repository", "order_item_repository.go")); !strings.Contains(got, "type orderItemRepository struct") {
		t.Errorf("repository naming:\n%s", got)
	}

	// The layers compile, and the usecase works against a fake repository:
	// it depends on the domain interface only.
	must(os.WriteFile(p.path("internal", "usecase", "user_usecase_test.go"), []byte(`package usecase

import (
	"context"
	"errors"
	"testing"

	"example.com/app/internal/domain"
)

type fakeUsers struct{ domain.UserRepository; byID map[int64]domain.User }

func (f fakeUsers) FindByID(_ context.Context, id int64) (domain.User, error) {
	u, ok := f.byID[id]
	if !ok {
		return u, domain.ErrNotFound
	}
	return u, nil
}

func TestGet(t *testing.T) {
	uc := NewUserUsecase(fakeUsers{byID: map[int64]domain.User{1: {ID: 1, Name: "Ann"}}})
	if u, err := uc.Get(context.Background(), 1); err != nil || u.Name != "Ann" {
		t.Fatal(u, err)
	}
	if _, err := uc.Get(context.Background(), 2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
}
`), 0o644))
	for _, args := range [][]string{{"vet", "./internal/..."}, {"test", "./internal/usecase"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = p.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}

	// Edits are never overwritten without --force.
	if err := p.Scaffold("User", ScaffoldOptions{}); err == nil || !strings.Contains(err.Error(), "already scaffolded") {
		t.Fatalf("second scaffold: %v", err)
	}
	must(p.Scaffold("User", ScaffoldOptions{Force: true}))

	if err := p.Scaffold("Missing", ScaffoldOptions{}); err == nil {
		t.Fatal("scaffold of an unknown model should fail")
	}
}
