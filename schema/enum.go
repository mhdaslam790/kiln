package schema

import (
	"regexp"
	"strings"
)

// PostgreSQL enum statements (as kilnpg writes them) are understood when a
// migration is replayed, so they don't count as opaque raw SQL.
var (
	sqlName      = `("(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*)`
	sqlLit       = `'(?:[^']|'')*'`
	reCreateEnum = regexp.MustCompile(`(?is)^\s*CREATE\s+TYPE\s+` + sqlName + `\s+AS\s+ENUM\s*\(\s*((?:` + sqlLit + `\s*,\s*)*(?:` + sqlLit + `)?)\s*\)\s*;?\s*$`)
	reAddValue   = regexp.MustCompile(`(?is)^\s*ALTER\s+TYPE\s+` + sqlName + `\s+ADD\s+VALUE\s+(IF\s+NOT\s+EXISTS\s+)?(` + sqlLit + `)\s*;?\s*$`)
	reDropType   = regexp.MustCompile(`(?is)^\s*DROP\s+TYPE\s+(IF\s+EXISTS\s+)?` + sqlName + `\s*;?\s*$`)
	reLit        = regexp.MustCompile(sqlLit)
)

// unquoteName folds a bare identifier to lower case, as PostgreSQL does,
// and unquotes a quoted one.
func unquoteName(s string) string {
	if strings.HasPrefix(s, `"`) {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
}

func unquoteLit(s string) string { return strings.ReplaceAll(s[1:len(s)-1], "''", "'") }

// applyEnumSQL updates the enums for a CREATE TYPE ... AS ENUM, ALTER TYPE
// ... ADD VALUE or DROP TYPE statement without arguments, and reports
// whether sql was one.
func (s *State) applyEnumSQL(sql string, args []any) bool {
	if len(args) > 0 {
		return false
	}
	if m := reCreateEnum.FindStringSubmatch(sql); m != nil {
		var vals []string
		for _, l := range reLit.FindAllString(m[2], -1) {
			vals = append(vals, unquoteLit(l))
		}
		if s.Enums == nil {
			s.Enums = map[string][]string{}
		}
		s.Enums[unquoteName(m[1])] = vals
		return true
	}
	if m := reAddValue.FindStringSubmatch(sql); m != nil {
		name, v := unquoteName(m[1]), unquoteLit(m[3])
		vals, ok := s.Enums[name]
		if !ok {
			return false // a type made some other way
		}
		for _, x := range vals {
			if x == v {
				return true
			}
		}
		s.Enums[name] = append(vals, v)
		return true
	}
	if m := reDropType.FindStringSubmatch(sql); m != nil {
		name := unquoteName(m[2])
		if _, ok := s.Enums[name]; !ok {
			return false
		}
		delete(s.Enums, name)
		return true
	}
	return false
}
