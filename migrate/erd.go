package migrate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mhdaslam790/kiln/schema"
)

// Replay applies the migrations' operations in version order to an empty
// schema, giving the schema they build (no database needed). Raw SQL and Go
// steps can't be modelled; they are counted in State.Opaque.
func Replay(history []Migration) (*schema.State, error) {
	sorted := append([]Migration(nil), history...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	state := schema.NewState()
	for _, mg := range sorted {
		var s schema.Schema
		mg.Up(&s)
		for _, op := range s.Ops {
			if err := state.Apply(op); err != nil {
				return nil, fmt.Errorf("replaying %d_%s: %w", mg.Version, mg.Name, err)
			}
		}
	}
	return state, nil
}

// ERD renders the schema as a Mermaid entity-relationship diagram
// (https://mermaid.js.org/syntax/entityRelationshipDiagram.html), which
// GitHub, GitLab and most Markdown viewers draw. Each table lists its
// columns with PK/FK/UK markers ("null" for nullable ones); each foreign
// key is a relationship labelled with its column(s): one-to-many, or
// one-to-one when the key is unique, and optional when it is nullable.
// A PostgreSQL enum column lists its values.
//
// With tables, only those tables and the relationships between them are
// drawn.
func ERD(st *schema.State, tables ...string) string {
	only := map[string]bool{}
	for _, t := range tables {
		only[t] = true
	}
	var names []string
	for name := range st.Tables {
		if name == "schema_migrations" || name == sqliteLockTable || (len(only) > 0 && !only[name]) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	drawn := map[string]bool{}
	for _, n := range names {
		drawn[n] = true
	}

	var b strings.Builder
	b.WriteString("erDiagram\n")
	for _, name := range names {
		t := st.Tables[name]
		fks := map[string]bool{}
		for _, fk := range t.ForeignKeys {
			for _, c := range fk.Columns {
				fks[c] = true
			}
		}
		fmt.Fprintf(&b, "    %s {\n", erdName(name))
		for _, c := range t.Columns {
			var keys []string
			pk := c.Primary || contains(t.PrimaryKey, c.Name)
			if pk {
				keys = append(keys, "PK")
			}
			if fks[c.Name] {
				keys = append(keys, "FK")
			}
			if !pk && (c.Unique || uniqueOn(t, []string{c.Name})) {
				keys = append(keys, "UK")
			}
			line := "        " + erdType(c) + " " + erdName(c.Name)
			if len(keys) > 0 {
				line += " " + strings.Join(keys, ", ")
			}
			var notes []string
			if !c.Required && !c.Primary && c.Type != schema.ID {
				notes = append(notes, "null")
			}
			if vals, ok := enumOf(st, c.SQLType); ok {
				notes = append(notes, enumNote(vals))
			}
			if len(notes) > 0 {
				line += ` "` + strings.Join(notes, "; ") + `"`
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("    }\n")
	}
	for _, name := range names {
		t := st.Tables[name]
		for _, fk := range t.ForeignKeys {
			if !drawn[fk.RefTable] {
				continue
			}
			parent := "||" // every child row has a parent
			for _, c := range fk.Columns {
				if col := t.Column(c); col != nil && !col.Required {
					parent = "|o" // the key is nullable: the parent is optional
				}
			}
			child := "o{" // zero or more children
			if uniqueOn(t, fk.Columns) || (len(fk.Columns) == 1 && isUniqueCol(t, fk.Columns[0])) {
				child = "o|" // at most one
			}
			fmt.Fprintf(&b, "    %s %s--%s %s : %q\n", erdName(fk.RefTable), parent, child, erdName(name), strings.Join(fk.Columns, ", "))
		}
	}
	return b.String()
}

// erdType is the column's type as one Mermaid word.
func erdType(c schema.Column) string {
	switch {
	case c.SQLType != "":
		return erdSQLType(c.SQLType)
	case c.Type == schema.String && c.Limit > 0:
		return fmt.Sprintf("string_%d", c.Limit)
	case c.Type == schema.ID:
		return "bigint"
	}
	return erdName(string(c.Type))
}

// erdName makes a Mermaid-safe identifier: letters, digits, _ and -.
func erdName(s string) string {
	ok := func(r rune) bool {
		return r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}
	return strings.Map(func(r rune) rune {
		if ok(r) {
			return r
		}
		return '_'
	}, s)
}

func uniqueOn(t *schema.TableState, cols []string) bool {
	for _, ix := range t.Indexes {
		if ix.Unique && sameSet(ix.Columns, cols) {
			return true
		}
	}
	return len(t.PrimaryKey) > 0 && sameSet(t.PrimaryKey, cols)
}

func isUniqueCol(t *schema.TableState, col string) bool {
	c := t.Column(col)
	return c != nil && (c.Unique || c.Primary)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

// enumNote lists an enum's values for a column comment (at most 8; a
// Mermaid comment can't hold double quotes).
func enumNote(vals []string) string {
	shown := vals
	if len(shown) > 8 {
		shown = shown[:8]
	}
	out := strings.ReplaceAll(strings.Join(shown, " | "), `"`, "'")
	if len(vals) > len(shown) {
		out += fmt.Sprintf(" | ... (%d more)", len(vals)-len(shown))
	}
	return out
}

// erdSQLType shows a SQL type as Mermaid allows: identifier quotes
// dropped, [] and (n) kept, anything else (spaces, commas) as _.
func erdSQLType(t string) string {
	t = strings.ToLower(strings.NewReplacer(`"`, "", "`", "").Replace(strings.TrimSpace(t)))
	arr := strings.HasSuffix(t, "[]")
	t = strings.NewReplacer("[", "", "]", "", ", ", ",").Replace(strings.TrimSuffix(t, "[]"))
	t = strings.Map(func(r rune) rune {
		if r == '(' || r == ')' || r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, t)
	if arr {
		t += "[]"
	}
	return t
}

// enumOf returns the values of the enum type a column (or array column)
// has, if it is one of the schema's enums.
func enumOf(st *schema.State, sqlType string) ([]string, bool) {
	base := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sqlType), "[]"))
	if base == "" || len(st.Enums) == 0 {
		return nil, false
	}
	if v, ok := st.Enums[base]; ok {
		return v, true
	}
	if len(base) > 1 && base[0] == '"' && base[len(base)-1] == '"' {
		v, ok := st.Enums[strings.ReplaceAll(base[1:len(base)-1], `""`, `"`)]
		return v, ok
	}
	v, ok := st.Enums[strings.ToLower(base)]
	return v, ok
}
