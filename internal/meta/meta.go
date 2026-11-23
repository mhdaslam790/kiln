// Package meta extracts and caches table metadata from Go structs.
//
// Tags:
//
//	db:"column_name,primary,auto,noauto,omitempty,json,readonly"
//	db:"-"                       skip the field
//	ref:"user_id" fk:"id"        association keys (owner column, target column)
package meta

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Field maps a struct field to a column.
type Field struct {
	Name      string // Go field name
	Column    string
	Index     []int
	Type      reflect.Type
	Primary   bool
	Auto      bool // database-generated (auto-increment)
	OmitEmpty bool // omit from INSERT when zero, so the DB default applies
	JSON      bool // marshal to/from JSON
	ReadOnly  bool // never written
	NoAuto    bool // integer primary key that is NOT auto-generated
	// Schema holds the raw `schema:"..."` tag used by migration diffs.
	Schema string
}

// AssocKind is the kind of association.
type AssocKind int

const (
	BelongsTo AssocKind = iota
	HasOne
	HasMany
	ManyToMany
)

func (k AssocKind) String() string {
	return [...]string{"belongs_to", "has_one", "has_many", "many_to_many"}[k]
}

// Assoc describes a relation to another model.
type Assoc struct {
	Name      string
	Index     []int
	Kind      AssocKind
	OwnerCol  string       // column on the owner (ref)
	TargetCol string       // column on the target (fk)
	Elem      reflect.Type // target struct type
	Slice     bool         // field is a slice
	Ptr       bool         // field (or slice element) is a pointer

	// ManyToMany only: OwnerCol / TargetCol are the keys on the two models
	// and the join table links them through JoinOwnerCol / JoinTargetCol.
	JoinTable     string
	JoinOwnerCol  string
	JoinTargetCol string

	refTag, fkTag, m2mTag string
}

// Model is the metadata for one struct type.
type Model struct {
	Type   reflect.Type
	Table  string
	Fields []*Field
	// PK is the single primary key; nil when the key is composite (see PKs).
	PK *Field
	// PKs lists every primary key field in declaration order.
	PKs       []*Field
	CreatedAt *Field
	UpdatedAt *Field
	DeletedAt *Field

	byCol  map[string]*Field
	byName map[string]*Field

	assocMu   sync.Mutex
	assocs    map[string]*Assoc // keyed by lower-case Go name and column-style name
	assocList []*Assoc
	resolved  map[*Assoc]bool
}

// Tabler lets a model choose its table name.
type Tabler interface{ TableName() string }

var (
	cache       sync.Map // reflect.Type -> *Model
	scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
	valuerType  = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
	timeType    = reflect.TypeOf(time.Time{})
	bytesType   = reflect.TypeOf([]byte(nil))
)

// Of returns the metadata for a struct type (or pointer to struct).
func Of(t reflect.Type) (*Model, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if v, ok := cache.Load(t); ok {
		return v.(*Model), nil
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("orm: %s is not a struct", t)
	}
	m := &Model{
		Type:     t,
		byCol:    map[string]*Field{},
		byName:   map[string]*Field{},
		assocs:   map[string]*Assoc{},
		resolved: map[*Assoc]bool{},
	}
	m.Table = tableName(t)
	if err := m.collect(t, nil); err != nil {
		return nil, err
	}
	if len(m.PKs) == 0 {
		if f := m.byCol["id"]; f != nil {
			f.Primary = true
			m.PKs = []*Field{f}
		}
	}
	if len(m.PKs) == 1 {
		m.PK = m.PKs[0]
	}
	if m.PK != nil && !m.PK.Auto && isInteger(m.PK.Type) && !m.PK.NoAuto {
		m.PK.Auto = true
	}
	m.CreatedAt = m.timeField("created_at")
	m.UpdatedAt = m.timeField("updated_at")
	if f := m.byCol["deleted_at"]; f != nil {
		if f.Type != reflect.PointerTo(timeType) && f.Type != reflect.TypeOf(sql.NullTime{}) {
			return nil, fmt.Errorf("orm: %s.%s: deleted_at must be *time.Time or sql.NullTime", t.Name(), f.Name)
		}
		m.DeletedAt = f
	}
	v, _ := cache.LoadOrStore(t, m)
	return v.(*Model), nil
}

// MustOf is Of that panics on error.
func MustOf(t reflect.Type) *Model {
	m, err := Of(t)
	if err != nil {
		panic(err)
	}
	return m
}

func (m *Model) timeField(col string) *Field {
	f := m.byCol[col]
	if f == nil {
		return nil
	}
	if f.Type == timeType || f.Type == reflect.PointerTo(timeType) {
		return f
	}
	return nil
}

func (m *Model) collect(t reflect.Type, parent []int) error {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		idx := append(append([]int(nil), parent...), i)
		tag, hasTag := sf.Tag.Lookup("db")
		if tag == "-" {
			continue
		}
		if !sf.IsExported() {
			continue
		}
		name, opts := parseTag(tag)

		// Embedded structs are flattened (pointer embeds are not supported).
		if sf.Anonymous && !hasTag && sf.Type.Kind() == reflect.Struct && !IsColumnType(sf.Type) {
			if err := m.collect(sf.Type, idx); err != nil {
				return err
			}
			continue
		}

		if !opts["json"] && !IsColumnType(sf.Type) {
			if a := assocFor(sf, idx); a != nil {
				m.assocList = append(m.assocList, a)
				m.assocs[strings.ToLower(sf.Name)] = a
				m.assocs[Snake(sf.Name)] = a
				continue
			}
		}

		if name == "" {
			name = Snake(sf.Name)
		}
		f := &Field{
			Name:      sf.Name,
			Column:    name,
			Index:     idx,
			Type:      sf.Type,
			Primary:   opts["primary"],
			Auto:      opts["auto"],
			OmitEmpty: opts["omitempty"],
			JSON:      opts["json"],
			ReadOnly:  opts["readonly"],
			NoAuto:    opts["noauto"],
			Schema:    sf.Tag.Get("schema"),
		}
		if prev, dup := m.byCol[name]; dup {
			return fmt.Errorf("orm: %s: fields %s and %s both map to column %q", t.Name(), prev.Name, f.Name, name)
		}
		if f.Primary {
			m.PKs = append(m.PKs, f)
		}
		m.Fields = append(m.Fields, f)
		m.byCol[name] = f
		m.byName[f.Name] = f
	}
	return nil
}

func assocFor(sf reflect.StructField, idx []int) *Assoc {
	t := sf.Type
	a := &Assoc{Name: sf.Name, Index: idx, refTag: sf.Tag.Get("ref"), fkTag: sf.Tag.Get("fk"), m2mTag: sf.Tag.Get("m2m")}
	if t.Kind() == reflect.Slice {
		a.Slice = true
		t = t.Elem()
	}
	if t.Kind() == reflect.Pointer {
		a.Ptr = true
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || IsColumnType(t) {
		return nil
	}
	a.Elem = t
	return a
}

// Field looks a field up by column name (case-insensitive) or Go name.
func (m *Model) Field(name string) *Field {
	if f := m.byCol[name]; f != nil {
		return f
	}
	if f := m.byName[name]; f != nil {
		return f
	}
	if f := m.byCol[strings.ToLower(name)]; f != nil {
		return f
	}
	return nil
}

// Assocs returns all associations, resolved.
func (m *Model) Assocs() ([]*Assoc, error) {
	for _, a := range m.assocList {
		if _, err := m.Assoc(a.Name); err != nil {
			return nil, err
		}
	}
	return m.assocList, nil
}

// Assoc returns a resolved association by Go name or snake_case name.
func (m *Model) Assoc(name string) (*Assoc, error) {
	a := m.assocs[name]
	if a == nil {
		a = m.assocs[strings.ToLower(name)]
	}
	if a == nil {
		return nil, fmt.Errorf("orm: %s has no association %q", m.Type.Name(), name)
	}
	m.assocMu.Lock()
	defer m.assocMu.Unlock()
	if m.resolved[a] {
		return a, nil
	}
	target, err := Of(a.Elem)
	if err != nil {
		return nil, err
	}
	if err := m.resolve(a, target); err != nil {
		return nil, err
	}
	m.resolved[a] = true
	return a, nil
}

func (m *Model) resolve(a *Assoc, target *Model) error {
	owner := m.Type.Name()
	switch {
	case a.m2mTag != "":
		// Roles []Role `m2m:"user_roles[,user_id,role_id]"`
		if !a.Slice {
			return fmt.Errorf("orm: %s.%s: m2m needs a slice field", owner, a.Name)
		}
		a.Kind = ManyToMany
		parts := strings.Split(a.m2mTag, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		a.JoinTable = parts[0]
		a.JoinOwnerCol = Snake(owner) + "_id"
		a.JoinTargetCol = Snake(target.Type.Name()) + "_id"
		switch len(parts) {
		case 1:
		case 3:
			a.JoinOwnerCol, a.JoinTargetCol = parts[1], parts[2]
		default:
			return fmt.Errorf("orm: %s.%s: m2m tag must be \"table\" or \"table,owner_col,target_col\"", owner, a.Name)
		}
		if a.JoinTable == "" || a.JoinOwnerCol == "" || a.JoinTargetCol == "" || a.JoinOwnerCol == a.JoinTargetCol {
			return fmt.Errorf("orm: %s.%s: invalid m2m tag %q (self-references need explicit join columns)", owner, a.Name, a.m2mTag)
		}
		a.OwnerCol, a.TargetCol = a.refTag, a.fkTag
		if a.OwnerCol == "" {
			if m.PK == nil {
				return fmt.Errorf("orm: %s.%s: %s has no single primary key; set ref tag", owner, a.Name, owner)
			}
			a.OwnerCol = m.PK.Column
		}
		if a.TargetCol == "" {
			if target.PK == nil {
				return fmt.Errorf("orm: %s.%s: %s has no single primary key; set fk tag", owner, a.Name, target.Type.Name())
			}
			a.TargetCol = target.PK.Column
		}
	case a.refTag != "" && a.fkTag != "":
		a.OwnerCol, a.TargetCol = a.refTag, a.fkTag
		switch {
		case a.Slice:
			a.Kind = HasMany
		case m.PK != nil && m.PK.Column == a.OwnerCol:
			a.Kind = HasOne
		default:
			a.Kind = BelongsTo
		}
	case !a.Slice && m.Field(a.refTagOr(Snake(a.Name)+"_id")) != nil:
		// e.g. Author User + AuthorID int64 -> belongs_to
		a.Kind = BelongsTo
		a.OwnerCol = m.Field(a.refTagOr(Snake(a.Name) + "_id")).Column
		if a.fkTag != "" {
			a.TargetCol = a.fkTag
		} else if target.PK != nil {
			a.TargetCol = target.PK.Column
		} else {
			return fmt.Errorf("orm: %s.%s: target %s has no primary key; set fk tag", owner, a.Name, target.Type.Name())
		}
	default:
		if a.Slice {
			a.Kind = HasMany
		} else {
			a.Kind = HasOne
		}
		if a.refTag != "" {
			a.OwnerCol = a.refTag
		} else if m.PK != nil {
			a.OwnerCol = m.PK.Column
		} else {
			return fmt.Errorf("orm: %s.%s: %s has no primary key; set ref tag", owner, a.Name, owner)
		}
		a.TargetCol = a.fkTag
		if a.TargetCol == "" {
			a.TargetCol = Snake(owner) + "_id"
		}
	}
	if m.Field(a.OwnerCol) == nil {
		return fmt.Errorf("orm: %s.%s: owner column %q not found on %s", owner, a.Name, a.OwnerCol, owner)
	}
	if target.Field(a.TargetCol) == nil {
		return fmt.Errorf("orm: %s.%s: column %q not found on %s", owner, a.Name, a.TargetCol, target.Type.Name())
	}
	a.OwnerCol = m.Field(a.OwnerCol).Column
	a.TargetCol = target.Field(a.TargetCol).Column
	return nil
}

func (a *Assoc) refTagOr(def string) string {
	if a.refTag != "" {
		return a.refTag
	}
	return def
}

// IsColumnType reports whether values of t are stored in a single column.
func IsColumnType(t reflect.Type) bool {
	if t == timeType || t == bytesType {
		return true
	}
	if t.Implements(valuerType) || reflect.PointerTo(t).Implements(scannerType) {
		return true
	}
	if t.Kind() == reflect.Pointer {
		return IsColumnType(t.Elem())
	}
	switch t.Kind() {
	case reflect.Struct:
		return false
	case reflect.Slice, reflect.Array:
		e := t.Elem()
		for e.Kind() == reflect.Pointer {
			e = e.Elem()
		}
		return e.Kind() != reflect.Struct || IsColumnType(e)
	case reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Interface:
		return false
	}
	return true
}

func isInteger(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func parseTag(tag string) (string, map[string]bool) {
	parts := strings.Split(tag, ",")
	opts := map[string]bool{}
	for _, p := range parts[1:] {
		opts[strings.TrimSpace(p)] = true
	}
	return strings.TrimSpace(parts[0]), opts
}

func tableName(t reflect.Type) string {
	v := reflect.New(t)
	if tb, ok := v.Interface().(Tabler); ok {
		return tb.TableName()
	}
	if tb, ok := v.Elem().Interface().(Tabler); ok {
		return tb.TableName()
	}
	return Plural(Snake(t.Name()))
}

// Snake converts CamelCase to snake_case, keeping acronyms together:
// UserID -> user_id, HTTPServer -> http_server.
func Snake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) {
			if i > 0 {
				prev := r[i-1]
				nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(c))
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Camel converts snake_case to CamelCase with common acronyms: user_id -> UserID.
func Camel(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		switch up := strings.ToUpper(p); up {
		case "ID", "URL", "URI", "API", "HTTP", "JSON", "UUID", "SQL", "IP", "HTML":
			parts[i] = up
		default:
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// Plural is a small English pluralizer good enough for table names.
func Plural(s string) string {
	if s == "" {
		return s
	}
	irregular := map[string]string{"person": "people", "child": "children", "man": "men", "woman": "women"}
	for sing, pl := range irregular {
		if s == sing || strings.HasSuffix(s, "_"+sing) {
			return strings.TrimSuffix(s, sing) + pl
		}
	}
	switch {
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"),
		strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	}
	return s + "s"
}

// Singular reverses Plural for common English table names.
func Singular(s string) string {
	irregular := map[string]string{"people": "person", "children": "child", "men": "man", "women": "woman"}
	for pl, sing := range irregular {
		if s == pl || strings.HasSuffix(s, "_"+pl) {
			return strings.TrimSuffix(s, pl) + sing
		}
	}
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zes"),
		strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "uses"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ss"):
		return s
	case strings.HasSuffix(s, "s"):
		return s[:len(s)-1]
	}
	return s
}

// ErrNotStruct is returned when a destination is not a struct pointer.
var ErrNotStruct = errors.New("orm: destination must be a pointer to a struct")
