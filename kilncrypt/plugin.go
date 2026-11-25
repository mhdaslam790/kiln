package kilncrypt

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/mhdaslam790/kiln/orm"
	"github.com/mhdaslam790/kiln/q"
)

var stringType = reflect.TypeOf(String{})

// Plugin returns the option that encrypts String fields on every insert
// and update (and q.Set values in UpdateAny), re-encrypts values sealed
// with an old key or in the v0.1 format, binds loaded values to their
// column so Open can check them, and keeps the given blind indexes up to
// date. Without it, writing a NewString value
// fails with ErrNotSealed rather than storing plaintext.
func Plugin(keys *Keys, indexes ...*Index) orm.Option {
	return orm.Use(&plugin{keys: keys, indexes: indexes})
}

type plugin struct {
	keys    *Keys
	indexes []*Index
	models  sync.Map // *orm.ModelInfo -> *model; bounded by the number of models
}

// model lists what the plugin does for one model.
type model struct {
	columns []string // String columns
	labels  []string // label(table, column) for each of columns
	indexes []*Index // indexes whose two columns the model has
}

// label returns the label of an encrypted column.
func (md *model) label(column string) (string, bool) {
	for i, c := range md.columns {
		if c == column {
			return md.labels[i], true
		}
	}
	return "", false
}

func (p *plugin) Name() string { return "kilncrypt" }

func (p *plugin) model(m *orm.ModelInfo) *model {
	if v, ok := p.models.Load(m); ok {
		return v.(*model)
	}
	md := &model{}
	for _, f := range m.Fields {
		if f.Type == stringType {
			md.columns = append(md.columns, f.Column)
			md.labels = append(md.labels, label(m.Table, f.Column))
		}
	}
	for _, idx := range p.indexes {
		src, ok1 := m.Column(idx.column)
		_, ok2 := m.Column(idx.indexColumn)
		if ok1 && ok2 && src.Type == stringType {
			md.indexes = append(md.indexes, idx)
		}
	}
	v, _ := p.models.LoadOrStore(m, md)
	return v.(*model)
}

// prepare returns s as it must be stored in the column label: new values
// sealed, values in an old format, with an old key or from another column
// re-sealed. changed is false if s is stored as is.
func (p *plugin) prepare(s String, label string) (out String, changed bool, err error) {
	switch {
	case s.hasPlain && s.sealed == "": // new value
		if s.sealed, err = p.keys.seal(s.plain, label); err != nil {
			return s, false, err
		}
	case s.sealed != "":
		if s.current(p.keys, label) {
			return s, false, nil
		}
		plain := s.plain
		if !s.hasPlain {
			if plain, err = p.keys.open(s.sealed, s.bound); err != nil {
				return s, false, err
			}
		}
		if s.sealed, err = p.keys.seal(plain, label); err != nil {
			return s, false, err
		}
	default:
		return s, false, nil // NULL
	}
	s.bound = label
	return s, true, nil
}

func (p *plugin) BeforeWrite(_ context.Context, w *orm.WriteInfo) error {
	if w.Model == nil {
		return nil
	}
	md := p.model(w.Model)
	for c, col := range md.columns {
		if !writes(w, col) {
			continue
		}
		v, _ := w.Get(col)
		s, changed, err := p.prepare(v.(String), md.labels[c])
		if err != nil {
			return fmt.Errorf("kilncrypt: %s.%s: %w", w.Table, col, err)
		}
		if !changed {
			continue
		}
		if err := w.Set(col, s); err != nil {
			return err
		}
	}
	for _, idx := range md.indexes {
		if !writes(w, idx.column) {
			continue
		}
		v, _ := w.Get(idx.column)
		s := v.(String)
		if !s.hasPlain && !s.IsNull() {
			continue // value not changed in this write (loaded ciphertext): index stays valid
		}
		if !writes(w, idx.indexColumn) {
			return fmt.Errorf("kilncrypt: updating %s.%s must also write its index %s", w.Table, idx.column, idx.indexColumn)
		}
		var val any // NULL for a NULL value
		if s.hasPlain {
			val = idx.Of(s.plain)
		}
		if err := w.Set(idx.indexColumn, val); err != nil {
			return err
		}
	}
	return nil
}

// writes reports whether the write stores column (inserts and full updates
// store every column).
func writes(w *orm.WriteInfo, column string) bool {
	if w.Fields == nil {
		return true
	}
	for _, f := range w.Fields {
		if f == column {
			return true
		}
	}
	return false
}

// BeforeQuery seals String values set by UpdateAny (q.Set).
func (p *plugin) BeforeQuery(_ context.Context, info *orm.QueryInfo) error {
	if len(info.Set) == 0 || info.Model == nil {
		return nil
	}
	md := p.model(info.Model)
	if len(md.columns) == 0 {
		return nil
	}
	copied := false
	for i, m := range info.Set {
		s, ok := m.Value.(String)
		if !ok {
			continue
		}
		col := strings.TrimPrefix(m.Field, info.Table+".")
		l, ok := md.label(col)
		if !ok {
			return fmt.Errorf("kilncrypt: %s.%s is not an encrypted column", info.Table, col)
		}
		s, changed, err := p.prepare(s, l)
		if err != nil {
			return fmt.Errorf("kilncrypt: %s.%s: %w", info.Table, col, err)
		}
		if !changed {
			continue
		}
		if !copied { // don't change the caller's slice
			info.Set = append([]q.Mutation(nil), info.Set...)
			copied = true
		}
		info.Set[i].Value = s
	}
	return nil
}

// AfterRead binds loaded values to their column, for Open.
func (p *plugin) AfterRead(_ context.Context, r *orm.ReadInfo) error {
	md := p.model(r.Model)
	for c, col := range md.columns {
		for i := 0; i < r.Len(); i++ {
			r.Field(i, col).(*String).bound = md.labels[c]
		}
	}
	return nil
}
