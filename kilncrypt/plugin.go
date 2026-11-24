package kilncrypt

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/mhdaslam790/kiln/orm"
)

var stringType = reflect.TypeOf(String{})

// Plugin returns the option that encrypts String fields on every insert
// and update, re-encrypts values sealed with an old key, and keeps the
// given blind indexes up to date. Without it, writing a NewString value
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
	indexes []*Index // indexes whose two columns the model has
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

func (p *plugin) BeforeWrite(_ context.Context, w *orm.WriteInfo) error {
	if w.Model == nil {
		return nil
	}
	md := p.model(w.Model)
	for _, col := range md.columns {
		if !writes(w, col) {
			continue
		}
		v, _ := w.Get(col)
		s := v.(String)
		switch {
		case s.hasPlain && s.sealed == "": // new value
			sealed, err := p.keys.seal(s.plain)
			if err != nil {
				return fmt.Errorf("kilncrypt: %s.%s: %w", w.Table, col, err)
			}
			s.sealed = sealed
		case s.sealed != "": // loaded: re-encrypt only if sealed with an old key
			id, ok := s.keyID()
			if ok && id == p.keys.current {
				continue
			}
			plain, err := p.keys.open(s.sealed)
			if err != nil {
				return fmt.Errorf("kilncrypt: %s.%s: %w", w.Table, col, err)
			}
			if s.sealed, err = p.keys.seal(plain); err != nil {
				return fmt.Errorf("kilncrypt: %s.%s: %w", w.Table, col, err)
			}
		default:
			continue // NULL
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
