package gen

import (
	"os"
	"strings"
	"testing"
)

func TestDotEnv(t *testing.T) {
	p := newProject(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(p.path(".gitignore"), []byte("bin/"), 0o644)) // no trailing newline
	cfg := DefaultConfig()
	cfg.Dialect = "sqlite"
	must(p.Init(cfg, false))

	envFile := read(t, p.path(".env"))
	if !strings.Contains(envFile, "DATABASE_URL="+drivers["sqlite"].DSNExample) {
		t.Errorf(".env:\n%s", envFile)
	}
	if fi, _ := os.Stat(p.path(".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf(".env permissions = %v, want 0600", fi.Mode().Perm())
	}
	if got := read(t, p.path(".gitignore")); got != "bin/\n.env\n" {
		t.Errorf(".gitignore = %q", got)
	}
	if !strings.Contains(read(t, p.path("cmd", "migrate", "main.go")), "env.Load()") {
		t.Error("runner does not load .env")
	}

	// Re-running init keeps the user's .env and does not duplicate the ignore line.
	must(os.WriteFile(p.path(".env"), []byte("DATABASE_URL=file:mine.db\n"), 0o600))
	must(p.Init(cfg, true))
	if got := read(t, p.path(".env")); got != "DATABASE_URL=file:mine.db\n" {
		t.Errorf(".env overwritten: %q", got)
	}
	if got := read(t, p.path(".gitignore")); strings.Count(got, ".env") != 1 {
		t.Errorf(".gitignore = %q", got)
	}

	// Loading the project reads .env; a real environment variable wins.
	t.Setenv("DATABASE_URL", "from-env")
	if _, err := Load(p.Root); err != nil || os.Getenv("DATABASE_URL") != "from-env" {
		t.Fatalf("environment should win: %q %v", os.Getenv("DATABASE_URL"), err)
	}
	os.Unsetenv("DATABASE_URL")
	if _, err := Load(p.Root); err != nil || os.Getenv("DATABASE_URL") != "file:mine.db" {
		t.Fatalf("DATABASE_URL from .env = %q, %v", os.Getenv("DATABASE_URL"), err)
	}

	// A broken .env is reported with the file and line.
	must(os.WriteFile(p.path(".env"), []byte("oops\n"), 0o600))
	if _, err := Load(p.Root); err == nil || !strings.Contains(err.Error(), ".env: line 1") {
		t.Fatalf("broken .env: %v", err)
	}
}
