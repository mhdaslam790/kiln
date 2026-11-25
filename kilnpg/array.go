// Package kilnpg adds PostgreSQL arrays and enums to kiln: an Array column
// type with its filters, and migration helpers for enum types.
//
//	type Post struct {
//		ID   int64
//		Tags kilnpg.Array[string] `schema:"sqltype:TEXT[]"`
//	}
//
//	posts.FindAll(ctx, kilnpg.Has("tags", "go"))
//	posts.FindAll(ctx, kilnpg.Overlaps("tags", "go", "sql"))
//
// It works with any PostgreSQL driver (pgx, lib/pq): arrays travel in
// PostgreSQL's text format. Nothing here runs unless you use it.
package kilnpg

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Elem is the element type of an Array.
type Elem interface {
	~string | ~bool |
		~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// Array is a one-dimensional PostgreSQL array (TEXT[], BIGINT[], ...)
// held in a Go slice. A nil Array is NULL; an empty one is '{}'. NULL
// elements can't be held and are an error on Scan.
type Array[T Elem] []T

// Value encodes the array in PostgreSQL's text format.
func (a Array[T]) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.String:
			b.WriteByte('"')
			b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(rv.String()))
			b.WriteByte('"')
		case reflect.Bool:
			b.WriteString(strconv.FormatBool(rv.Bool()))
		case reflect.Float32, reflect.Float64:
			b.WriteString(strconv.FormatFloat(rv.Float(), 'g', -1, rv.Type().Bits()))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			b.WriteString(strconv.FormatUint(rv.Uint(), 10))
		default:
			b.WriteString(strconv.FormatInt(rv.Int(), 10))
		}
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan reads an array in PostgreSQL's text format.
func (a *Array[T]) Scan(src any) error {
	var s string
	switch x := src.(type) {
	case nil:
		*a = nil
		return nil
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return fmt.Errorf("kilnpg: cannot scan %T into an Array", src)
	}
	items, err := parseArray(s)
	if err != nil {
		return err
	}
	out := make(Array[T], len(items))
	for i, it := range items {
		if it == nil {
			return errors.New("kilnpg: the array has a NULL element; Array can't hold one")
		}
		if err := setElem(reflect.ValueOf(&out[i]).Elem(), *it); err != nil {
			return err
		}
	}
	*a = out
	return nil
}

func setElem(v reflect.Value, s string) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		v.SetBool(s == "t" || s == "true")
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("kilnpg: array element %q: %w", s, err)
		}
		v.SetFloat(f)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("kilnpg: array element %q: %w", s, err)
		}
		v.SetUint(n)
	default:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("kilnpg: array element %q: %w", s, err)
		}
		v.SetInt(n)
	}
	return nil
}

// parseArray splits a one-dimensional array literal; nil items are NULL.
func parseArray(s string) ([]*string, error) {
	if i := strings.IndexByte(s, '='); i > 0 && s[0] == '[' { // "[0:2]={...}" lower bounds
		s = s[i+1:]
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, fmt.Errorf("kilnpg: not an array: %q", s)
	}
	body := s[1 : len(s)-1]
	var out []*string
	if body == "" {
		return out, nil
	}
	for i := 0; i <= len(body); {
		var item strings.Builder
		quoted := false
		if i < len(body) && body[i] == '{' {
			return nil, errors.New("kilnpg: multi-dimensional arrays aren't supported")
		}
		if i < len(body) && body[i] == '"' {
			quoted = true
			i++
			for ; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' && i+1 < len(body) {
					i++
				}
				item.WriteByte(body[i])
			}
			if i >= len(body) {
				return nil, fmt.Errorf("kilnpg: unterminated quote in %q", s)
			}
			i++ // closing quote
		} else {
			for ; i < len(body) && body[i] != ','; i++ {
				item.WriteByte(body[i])
			}
		}
		v := item.String()
		if !quoted && strings.EqualFold(v, "NULL") {
			out = append(out, nil)
		} else {
			if !quoted {
				v = strings.TrimSpace(v)
			}
			out = append(out, &v)
		}
		if i < len(body) && body[i] != ',' {
			return nil, fmt.Errorf("kilnpg: malformed array %q", s)
		}
		i++ // the comma (or past the end)
	}
	return out, nil
}
