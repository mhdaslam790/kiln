package schema

import (
	"fmt"
	"sort"
	"strings"
)

// TableState is the known shape of one table.
type TableState struct {
	Name        string
	Columns     []Column
	PrimaryKey  []string
	Indexes     []CreateIndex // includes unique constraints, normalized to unique indexes
	ForeignKeys []ForeignKey
}

// Column returns a column by name, or nil.
func (t *TableState) Column(name string) *Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// HasIndexOn reports whether an index (optionally unique) covers exactly cols.
func (t *TableState) HasIndexOn(cols []string, unique bool) bool {
	if unique && len(cols) == 1 {
		if c := t.Column(cols[0]); c != nil && (c.Unique || (c.Primary && len(t.PrimaryKey) <= 1)) {
			return true
		}
	}
	for _, ix := range t.Indexes {
		if (!unique || ix.Unique) && equalStrings(ix.Columns, cols) {
			return true
		}
	}
	return false
}

// ForeignKey returns a foreign key by name, or nil.
func (t *TableState) ForeignKey(name string) *ForeignKey {
	for i := range t.ForeignKeys {
		if t.ForeignKeys[i].Name == name {
			return &t.ForeignKeys[i]
		}
	}
	return nil
}

// State is an in-memory model of a database schema, built by replaying
// operations (see Replay) or by inspecting a live database.
type State struct {
	Tables map[string]*TableState
	// Opaque counts operations that could not be modelled (raw SQL, Go code).
	Opaque int
	// Enums holds PostgreSQL enum types and their values, in order.
	Enums map[string][]string `json:",omitempty"`
}

// NewState returns an empty State.
func NewState() *State { return &State{Tables: map[string]*TableState{}} }

// TableNames returns the table names sorted.
func (s *State) TableNames() []string {
	names := make([]string, 0, len(s.Tables))
	for n := range s.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Replay builds the schema produced by running fns in order.
func Replay(fns ...func(*Schema)) (*State, error) {
	st := NewState()
	for _, fn := range fns {
		var s Schema
		fn(&s)
		for _, op := range s.Ops {
			if err := st.Apply(op); err != nil {
				return nil, err
			}
		}
	}
	return st, nil
}

// Apply updates the state with one operation.
func (s *State) Apply(op Op) error {
	tbl := func(name string) (*TableState, error) {
		t := s.Tables[name]
		if t == nil {
			return nil, fmt.Errorf("schema: table %q does not exist", name)
		}
		return t, nil
	}
	switch o := op.(type) {
	case CreateTable:
		if s.Tables[o.Name] != nil {
			if o.IfNotExists {
				return nil
			}
			return fmt.Errorf("schema: table %q already exists", o.Name)
		}
		t := &TableState{Name: o.Name, Columns: append([]Column(nil), o.Columns...), PrimaryKey: o.PrimaryKey}
		if len(t.PrimaryKey) == 0 {
			for _, c := range o.Columns {
				if c.Primary {
					t.PrimaryKey = append(t.PrimaryKey, c.Name)
				}
			}
		}
		for _, u := range o.Uniques {
			t.Indexes = append(t.Indexes, CreateIndex{Table: o.Name, Name: IndexName(o.Name, u, true), Columns: u, Unique: true})
		}
		t.ForeignKeys = append(t.ForeignKeys, o.ForeignKeys...)
		s.Tables[o.Name] = t
	case DropTable:
		if s.Tables[o.Name] == nil && !o.IfExists {
			return fmt.Errorf("schema: table %q does not exist", o.Name)
		}
		delete(s.Tables, o.Name)
	case RenameTable:
		t, err := tbl(o.Name)
		if err != nil {
			return err
		}
		delete(s.Tables, o.Name)
		t.Name = o.NewName
		for i := range t.Indexes {
			t.Indexes[i].Table = o.NewName
		}
		s.Tables[o.NewName] = t
		for _, other := range s.Tables {
			for i := range other.ForeignKeys {
				if other.ForeignKeys[i].RefTable == o.Name {
					other.ForeignKeys[i].RefTable = o.NewName
				}
			}
		}
	case AddColumn:
		t, err := tbl(o.Table)
		if err != nil {
			return err
		}
		if t.Column(o.Column.Name) != nil {
			return fmt.Errorf("schema: column %s.%s already exists", o.Table, o.Column.Name)
		}
		c := o.Column
		if c.Ref != nil {
			t.ForeignKeys = append(t.ForeignKeys, *c.Ref)
			c.Ref = nil
		}
		t.Columns = append(t.Columns, c)
	case DropColumn:
		t, err := tbl(o.Table)
		if err != nil {
			return err
		}
		cols := t.Columns[:0]
		for _, c := range t.Columns {
			if c.Name != o.Name {
				cols = append(cols, c)
			}
		}
		t.Columns = cols
		idx := t.Indexes[:0]
		for _, ix := range t.Indexes {
			if !contains(ix.Columns, o.Name) {
				idx = append(idx, ix)
			}
		}
		t.Indexes = idx
	case RenameColumn:
		t, err := tbl(o.Table)
		if err != nil {
			return err
		}
		c := t.Column(o.Name)
		if c == nil {
			return fmt.Errorf("schema: column %s.%s does not exist", o.Table, o.Name)
		}
		c.Name = o.NewName
		renameIn(t.PrimaryKey, o.Name, o.NewName)
		for i := range t.Indexes {
			renameIn(t.Indexes[i].Columns, o.Name, o.NewName)
		}
		for i := range t.ForeignKeys {
			renameIn(t.ForeignKeys[i].Columns, o.Name, o.NewName)
		}
	case ChangeColumn:
		t, err := tbl(o.Table)
		if err != nil {
			return err
		}
		c := t.Column(o.Column.Name)
		if c == nil {
			return fmt.Errorf("schema: column %s.%s does not exist", o.Table, o.Column.Name)
		}
		nc := o.Column
		nc.Primary = c.Primary
		*c = nc
	case AddForeignKey:
		t, err := tbl(o.Table)
		if err != nil {
			return err
		}
		t.ForeignKeys = append(t.ForeignKeys, o.Key)
	case DropForeignKey:
		t, err := tbl(o.Table)
		if err != nil {
			return err
		}
		fks := t.ForeignKeys[:0]
		for _, fk := range t.ForeignKeys {
			if fk.Name != o.Name {
				fks = append(fks, fk)
			}
		}
		t.ForeignKeys = fks
	case CreateIndex:
		t, err := tbl(o.Table)
		if err != nil {
			return err
		}
		for _, ix := range t.Indexes {
			if ix.Name == o.Name {
				if o.IfNotExists {
					return nil
				}
				return fmt.Errorf("schema: index %q already exists", o.Name)
			}
		}
		t.Indexes = append(t.Indexes, o)
	case DropIndex:
		t := s.Tables[o.Table]
		if t == nil {
			return nil
		}
		idx := t.Indexes[:0]
		for _, ix := range t.Indexes {
			if ix.Name != o.Name {
				idx = append(idx, ix)
			}
		}
		t.Indexes = idx
	case Exec:
		if !s.applyEnumSQL(o.SQL, o.Args) {
			s.Opaque++
		}
	case Do:
		s.Opaque++
	case AllowUnsafe:
		// lint marker only
	default:
		return fmt.Errorf("schema: cannot apply %T", op)
	}
	return nil
}

func renameIn(ss []string, from, to string) {
	for i := range ss {
		if ss[i] == from {
			ss[i] = to
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}
