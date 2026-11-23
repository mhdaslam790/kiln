package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	src := "\ufeff# comment\n" +
		"\n" +
		"DATABASE_URL=postgres://u:p@localhost:5432/app?sslmode=disable\n" +
		"export REDIS_ADDR = localhost:6379  # inline comment\n" +
		"FRAGMENT=http://x/#top\n" +
		`DQ="a \"b\"\nc"  # comment` + "\n" +
		`SQ='raw \n $x'` + "\n" +
		"EMPTY=\n" +
		"_UNDER9=1\n"
	got, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/app?sslmode=disable",
		"REDIS_ADDR":   "localhost:6379",
		"FRAGMENT":     "http://x/#top",
		"DQ":           "a \"b\"\nc",
		"SQ":           `raw \n $x`,
		"EMPTY":        "",
		"_UNDER9":      "1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vars: %q", len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{"NOEQUALS", "1BAD=x", "A B=x", `Q="open`, `Q="x" trailing`} {
		if _, err := Parse([]byte("OK=1\n" + src)); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%q: want a line-2 error, got %v", src, err)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".env")
	if err := os.WriteFile(file, []byte("KILN_ENV_A=from-file\nKILN_ENV_B=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILN_ENV_A", "from-env") // real environment wins
	os.Unsetenv("KILN_ENV_B")
	t.Cleanup(func() { os.Unsetenv("KILN_ENV_B") })

	if err := Load(file, filepath.Join(dir, "missing.env")); err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("KILN_ENV_A"); v != "from-env" {
		t.Errorf("A = %q, the environment should win", v)
	}
	if v := os.Getenv("KILN_ENV_B"); v != "from-file" {
		t.Errorf("B = %q", v)
	}

	if err := os.WriteFile(file, []byte("broken line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(file); err == nil || !strings.Contains(err.Error(), ".env: line 1") {
		t.Errorf("want file and line in the error, got %v", err)
	}
}
