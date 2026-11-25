// Package kilnmask hides personal data in what leaves your service: API
// responses, logs, exports and support screens. Tag the fields once:
//
//	type User struct {
//		ID    int64
//		Name  string `mask:"first:1"` // "A****"
//		Email string `mask:"email"`   // "a****@acme.io"
//		Phone string `mask:"last:4"`  // "****4567"
//		Token string `mask:"full"`    // "****"
//	}
//
// and mask a copy where it leaves:
//
//	json.NewEncoder(w).Encode(kilnmask.Copy(u))   // the original is untouched
//	slog.Info("signup", "user", kilnmask.Log(u))  // masked only if the line is logged
//
// Masking replaces the hidden part with a fixed "****", so the length of
// the value is not revealed either. An empty value stays empty. Only
// string and *string fields are masked; nested structs, pointers to
// structs and slices of them are masked too. An unknown rule masks the
// whole value: kilnmask fails closed.
//
// The package has no dependencies, and it works on any struct, not only
// kiln models.
package kilnmask

import (
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

const hidden = "****"

// Rule masks one value. Use your own with WithRule.
type Rule func(string) string

// Built-in rules, by tag:
//
//	full      "****"
//	email     first letter of the local part and the domain: "a****@acme.io"
//	first:N   the first N characters: "Ann Lee" -> "Ann****" (first:3)
//	last:N    the last N characters:  "4111111111111111" -> "****1111"
func builtin(tag string) (Rule, bool) {
	switch {
	case tag == "full":
		return Full, true
	case tag == "email":
		return Email, true
	case strings.HasPrefix(tag, "first:"):
		if n, err := strconv.Atoi(tag[6:]); err == nil && n >= 0 {
			return func(s string) string { return First(s, n) }, true
		}
	case strings.HasPrefix(tag, "last:"):
		if n, err := strconv.Atoi(tag[5:]); err == nil && n >= 0 {
			return func(s string) string { return Last(s, n) }, true
		}
	}
	return nil, false
}

// Full hides the whole value.
func Full(s string) string {
	if s == "" {
		return ""
	}
	return hidden
}

// First keeps the first n characters (runes) and hides the rest. A value
// of n characters or fewer is hidden completely.
func First(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return Full(s)
	}
	return string(r[:n]) + hidden
}

// Last keeps the last n characters and hides the rest. A value of n
// characters or fewer is hidden completely.
func Last(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return Full(s)
	}
	return hidden + string(r[len(r)-n:])
}

// Email keeps the first character of the local part and the domain:
// "ann@acme.io" -> "a****@acme.io". Anything that is not an address is
// hidden completely.
func Email(s string) string {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return Full(s)
	}
	return First(s[:at], 1) + s[at:]
}

// Option configures Copy and Log.
type Option func(*options)

type options struct{ rules map[string]Rule }

// WithRule adds a rule for tags named name (or replaces a built-in one),
// e.g. WithRule("iban", func(s string) string { return kilnmask.Last(s, 4) }).
func WithRule(name string, r Rule) Option {
	return func(o *options) {
		if o.rules == nil {
			o.rules = map[string]Rule{}
		}
		o.rules[name] = r
	}
}

// Copy returns a copy of v with every tagged field masked. v itself, and
// anything it points to, is never changed. v is usually a struct, a
// pointer to one or a slice of them; other values are returned as they are.
func Copy[T any](v T, opts ...Option) T {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	rv := reflect.ValueOf(&v).Elem()
	if out, changed := mask(rv, &o, map[visit]reflect.Value{}); changed {
		return out.Interface().(T)
	}
	return v
}

// Log returns a slog.LogValuer that logs a masked copy of v. The copy is
// made only if the record is actually logged.
func Log(v any, opts ...Option) slog.LogValuer {
	return logValue{v: v, opts: opts}
}

type logValue struct {
	v    any
	opts []Option
}

func (l logValue) LogValue() slog.Value { return slog.AnyValue(Copy(l.v, l.opts...)) }

// plan is what to mask in one struct type; cached per type.
type plan struct {
	fields []fieldPlan
}

type fieldPlan struct {
	index int
	tag   string // mask tag, "" for a field to descend into
	ptr   bool   // *string
	zero  bool   // tagged, but not a string: cleared (fail closed)
}

var plans sync.Map // reflect.Type -> *plan; bounded by the number of struct types

func planFor(t reflect.Type) *plan {
	if p, ok := plans.Load(t); ok {
		return p.(*plan)
	}
	p := &plan{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, tagged := f.Tag.Lookup("mask")
		switch {
		case tagged && tag != "-" && f.Type.Kind() == reflect.String:
			p.fields = append(p.fields, fieldPlan{index: i, tag: tag})
		case tagged && tag != "-" && f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.String:
			p.fields = append(p.fields, fieldPlan{index: i, tag: tag, ptr: true})
		case tagged && tag != "-": // a number, []byte, struct, ...: no rule fits, so hide it all
			p.fields = append(p.fields, fieldPlan{index: i, zero: true})
		case tag != "-" && mayContain(f.Type, map[reflect.Type]bool{}):
			p.fields = append(p.fields, fieldPlan{index: i})
		}
	}
	v, _ := plans.LoadOrStore(t, p)
	return v.(*plan)
}

// mayContain reports whether t can hold a struct with mask tags.
func mayContain(t reflect.Type, seen map[reflect.Type]bool) bool {
	switch t.Kind() {
	case reflect.Interface:
		return true // decided by the value at run time
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return mayContain(t.Elem(), seen)
	case reflect.Struct:
		if seen[t] {
			return true // recursive type: descend at run time
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || f.Tag.Get("mask") == "-" {
				continue
			}
			if _, ok := f.Tag.Lookup("mask"); ok || mayContain(f.Type, seen) {
				return true
			}
		}
	}
	return false
}

// visit identifies a pointer already being copied, so cyclic data (a child
// pointing back to its parent) is copied once instead of recursing forever.
type visit struct {
	ptr uintptr
	typ reflect.Type
}

// mask returns a masked copy of v, or (v, false) if nothing in it needed
// masking, so untouched parts are shared rather than copied.
func mask(v reflect.Value, o *options, seen map[visit]reflect.Value) (reflect.Value, bool) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v, false
		}
		inner, changed := mask(v.Elem(), o, seen)
		if !changed {
			return v, false
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(inner)
		return out, true
	case reflect.Pointer:
		if v.IsNil() {
			return v, false
		}
		key := visit{v.Pointer(), v.Type()}
		if out, ok := seen[key]; ok {
			return out, true // a cycle: point to the copy being made
		}
		out := reflect.New(v.Type().Elem())
		seen[key] = out
		inner, changed := mask(v.Elem(), o, seen)
		if !changed { // nothing below referred to out (that would be a change)
			delete(seen, key)
			return v, false
		}
		out.Elem().Set(inner)
		return out, true
	case reflect.Map:
		if v.IsNil() {
			return v, false
		}
		var out reflect.Value
		iter := v.MapRange()
		for iter.Next() {
			item, changed := mask(iter.Value(), o, seen)
			if !changed {
				continue
			}
			if !out.IsValid() {
				out = reflect.MakeMapWithSize(v.Type(), v.Len())
				all := v.MapRange()
				for all.Next() {
					out.SetMapIndex(all.Key(), all.Value())
				}
			}
			out.SetMapIndex(iter.Key(), item)
		}
		if !out.IsValid() {
			return v, false
		}
		return out, true
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return v, false
		}
		var out reflect.Value
		for i := 0; i < v.Len(); i++ {
			item, changed := mask(v.Index(i), o, seen)
			if !changed {
				continue
			}
			if !out.IsValid() {
				if v.Kind() == reflect.Slice {
					out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
				} else {
					out = reflect.New(v.Type()).Elem()
				}
				reflect.Copy(out, v)
			}
			out.Index(i).Set(item)
		}
		if !out.IsValid() {
			return v, false
		}
		return out, true
	case reflect.Struct:
		p := planFor(v.Type())
		if len(p.fields) == 0 {
			return v, false
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		changed := false
		for _, f := range p.fields {
			fv := out.Field(f.index)
			switch {
			case f.zero:
				if !fv.IsZero() {
					fv.SetZero()
					changed = true
				}
			case f.tag == "":
				if m, ok := mask(fv, o, seen); ok {
					fv.Set(m)
					changed = true
				}
			case f.ptr:
				if !fv.IsNil() {
					s := reflect.New(fv.Type().Elem())
					s.Elem().SetString(o.apply(f.tag, fv.Elem().String()))
					fv.Set(s)
					changed = true
				}
			default:
				fv.SetString(o.apply(f.tag, fv.String()))
				changed = true
			}
		}
		if !changed {
			return v, false
		}
		return out, true
	}
	return v, false
}

func (o *options) apply(tag, s string) string {
	if r, ok := o.rules[tag]; ok {
		return r(s)
	}
	if r, ok := builtin(tag); ok {
		return r(s)
	}
	return Full(s) // unknown rule: fail closed
}
