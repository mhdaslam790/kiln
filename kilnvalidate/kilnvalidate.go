// Package kilnvalidate checks `validate` struct tags before kiln inserts or
// updates a record, using github.com/go-playground/validator.
//
//	db := orm.Open(sqlDB, dialect.Postgres(), kilnvalidate.Plugin())
//
//	type User struct {
//		ID    int64
//		Email string `validate:"required,email"`
//		Age   int    `validate:"gte=0,lte=130"`
//	}
//
//	err := users.Insert(ctx, &User{Email: "nope"})
//	var verr *kilnvalidate.Error
//	if errors.As(err, &verr) {
//		for _, f := range verr.Fields { ... } // every invalid field, for a form or a 422
//	}
//
// Inserts and full updates check the whole record; a partial update
// (repo.Update(ctx, &u, "Email")) checks only the fields it writes. Bulk
// queries (UpdateAny) set values, not records, so they are not checked.
// Nothing is sent to the database when a record is invalid.
package kilnvalidate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/mhdaslam790/kiln/orm"
)

// ErrInvalid matches every validation error: errors.Is(err, kilnvalidate.ErrInvalid).
var ErrInvalid = errors.New("kilnvalidate: invalid record")

// Option configures the plugin.
type Option func(*plugin)

// WithValidator uses v, for example one with your custom rules or tag
// name settings, instead of a new validator.New(validator.WithRequiredStructEnabled()).
func WithValidator(v *validator.Validate) Option { return func(p *plugin) { p.v = v } }

// Plugin returns the option that validates records before every insert
// and update.
func Plugin(opts ...Option) orm.Option {
	p := &plugin{}
	for _, o := range opts {
		o(p)
	}
	if p.v == nil {
		p.v = validator.New(validator.WithRequiredStructEnabled())
	}
	return orm.Use(p)
}

type plugin struct{ v *validator.Validate }

func (p *plugin) Name() string { return "kilnvalidate" }

// ChecksOnly makes kiln run validation after the other plugins have filled
// in their values (a tenant id, ...), whatever the plugin order.
func (p *plugin) ChecksOnly() {}

func (p *plugin) BeforeWrite(ctx context.Context, w *orm.WriteInfo) error {
	var err error
	if w.Action == orm.ActionUpdate && w.Fields != nil {
		names := make([]string, 0, len(w.Fields))
		rt := reflect.TypeOf(w.Record).Elem()
		for _, col := range w.Fields {
			if f, ok := w.Model.Column(col); ok {
				names = append(names, path(rt, f.Name))
			}
		}
		err = p.v.StructPartialCtx(ctx, w.Record, names...)
	} else {
		err = p.v.StructCtx(ctx, w.Record)
	}
	if err == nil {
		return nil
	}
	var ves validator.ValidationErrors
	if !errors.As(err, &ves) {
		return err // e.g. InvalidValidationError: a bug, not bad input
	}
	out := &Error{Table: w.Table, Action: w.Action, Fields: make([]FieldError, 0, len(ves))}
	for _, fe := range ves {
		f := FieldError{Field: fe.StructField(), Tag: fe.Tag(), Param: fe.Param(), Value: fe.Value(), Err: fe}
		if fi, ok := fieldByName(w.Model, fe.StructField()); ok {
			f.Column = fi.Column
		}
		out.Fields = append(out.Fields, f)
	}
	return out
}

// path returns the field's name as the validator wants it in a partial
// check: through embedded structs ("Base.Code"). A plain "Code" would match
// nothing there, and the field would silently go unchecked.
func path(t reflect.Type, name string) string {
	sf, ok := t.FieldByName(name)
	if !ok || len(sf.Index) == 1 {
		return name
	}
	parts := make([]string, len(sf.Index))
	for i, idx := range sf.Index {
		f := t.Field(idx)
		parts[i] = f.Name
		t = f.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	return strings.Join(parts, ".")
}

func fieldByName(m *orm.ModelInfo, name string) (orm.FieldInfo, bool) {
	if m == nil {
		return orm.FieldInfo{}, false
	}
	for _, f := range m.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return orm.FieldInfo{}, false
}

// Error lists every invalid field of one record.
type Error struct {
	Table  string
	Action orm.Action
	Fields []FieldError
}

// FieldError is one failed rule.
type FieldError struct {
	Field  string // Go field name
	Column string // column name ("" for a field that is not a column)
	Tag    string // the failed rule, e.g. "required", "email", "max"
	Param  string // the rule's parameter, e.g. "100" for max=100
	Value  any    // the rejected value
	// Err is the validator's own error, for translations
	// (validator.FieldError.Translate).
	Err validator.FieldError
}

// Message is a short English description, e.g. "is required" or "must be at most 100".
func (f FieldError) Message() string {
	switch f.Tag {
	case "required", "required_if", "required_unless", "required_with", "required_without":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "url", "http_url":
		return "must be a valid URL"
	case "uuid", "uuid4":
		return "must be a valid UUID"
	case "min", "gte":
		return "must be at least " + f.Param
	case "max", "lte":
		return "must be at most " + f.Param
	case "gt":
		return "must be greater than " + f.Param
	case "lt":
		return "must be less than " + f.Param
	case "len":
		return "must have length " + f.Param
	case "oneof":
		return "must be one of: " + strings.ReplaceAll(f.Param, " ", ", ")
	}
	if f.Param != "" {
		return fmt.Sprintf("failed %s=%s", f.Tag, f.Param)
	}
	return "failed " + f.Tag
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "kilnvalidate: %s %s:", e.Action, e.Table)
	for i, f := range e.Fields {
		if i > 0 {
			b.WriteByte(';')
		}
		name := f.Column
		if name == "" {
			name = f.Field
		}
		b.WriteString(" " + name + " " + f.Message())
	}
	return b.String()
}

// Is makes errors.Is(err, ErrInvalid) true.
func (e *Error) Is(target error) bool { return target == ErrInvalid }
