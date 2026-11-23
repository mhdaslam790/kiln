// Package env loads environment variables from a .env file, so settings
// such as DATABASE_URL don't have to be exported in every terminal.
//
//	func main() {
//		env.Load() // reads .env if it exists
//		dsn := os.Getenv("DATABASE_URL")
//		...
//	}
//
// Variables already set in the environment always win, so production
// servers that set real environment variables are not affected by a stray
// .env file. A missing file is not an error.
//
// The format is the common one:
//
//	# comment
//	DATABASE_URL=postgres://user:pass@localhost:5432/app
//	export REDIS_ADDR=localhost:6379      # "export " is allowed
//	GREETING="hello world"                # double quotes: \n, \", \\ escapes
//	RAW='no $escapes here'                # single quotes: taken literally
package env

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Load reads the given files (".env" if none) and sets each variable that
// is not already set. Missing files are skipped.
func Load(files ...string) error {
	if len(files) == 0 {
		files = []string{".env"}
	}
	for _, f := range files {
		vars, err := Read(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for k, v := range vars {
			if _, set := os.LookupEnv(k); !set {
				if err := os.Setenv(k, v); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Read parses a .env file without changing the environment.
func Read(file string) (map[string]string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	vars, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return vars, nil
}

// Parse parses .env content.
func Parse(b []byte) (map[string]string, error) {
	vars := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // UTF-8 BOM
		}
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validKey(key) {
			return nil, fmt.Errorf("line %d: expected KEY=value", n)
		}
		v, err := value(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		vars[key] = v
	}
	return vars, sc.Err()
}

func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func value(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch q := v[0]; q {
	case '\'', '"':
		end := closingQuote(v, q)
		if end < 0 {
			return "", fmt.Errorf("missing closing %c", q)
		}
		rest := strings.TrimSpace(v[end+1:])
		if rest != "" && rest[0] != '#' {
			return "", fmt.Errorf("unexpected text after the closing quote")
		}
		if q == '\'' {
			return v[1:end], nil
		}
		return unescape(v[1:end]), nil
	}
	// Unquoted: an inline comment needs a space before '#', so URLs with
	// fragments (#) keep working.
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), nil
}

func closingQuote(v string, q byte) int {
	for i := 1; i < len(v); i++ {
		if q == '"' && v[i] == '\\' {
			i++
			continue
		}
		if v[i] == q {
			return i
		}
	}
	return -1
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default: // \" \\ and anything else: the character itself
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
