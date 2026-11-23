package orm

import (
	"database/sql"
	"fmt"
	"reflect"
	"time"
	"unsafe"
)

// Fast destinations: for plain string/int/int64/float64/bool/time.Time
// fields, the driver value is written straight into the struct field
// (located by its offset) instead of going through a sql.Null* holder and
// a reflection copy. NULL becomes the zero value, as on the slow path.

type fastDest interface {
	sql.Scanner
	set(p unsafe.Pointer)
}

type strDest struct{ p *string }

func (d *strDest) set(p unsafe.Pointer) { d.p = (*string)(p) }
func (d *strDest) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*d.p = ""
	case string:
		*d.p = x
	case []byte:
		*d.p = string(x)
	default:
		var n sql.NullString
		if err := n.Scan(v); err != nil {
			return err
		}
		*d.p = n.String
	}
	return nil
}

type int64Dest struct{ p *int64 }

func (d *int64Dest) set(p unsafe.Pointer) { d.p = (*int64)(p) }
func (d *int64Dest) Scan(v any) error {
	if x, ok := v.(int64); ok {
		*d.p = x
		return nil
	}
	var n sql.NullInt64
	if err := n.Scan(v); err != nil {
		return err
	}
	*d.p = n.Int64
	return nil
}

type intDest struct{ p *int }

func (d *intDest) set(p unsafe.Pointer) { d.p = (*int)(p) }
func (d *intDest) Scan(v any) error {
	x, ok := v.(int64)
	if !ok {
		var n sql.NullInt64
		if err := n.Scan(v); err != nil {
			return err
		}
		x = n.Int64
	}
	if int64(int(x)) != x {
		return errOverflow(x, "int")
	}
	*d.p = int(x)
	return nil
}

type float64Dest struct{ p *float64 }

func (d *float64Dest) set(p unsafe.Pointer) { d.p = (*float64)(p) }
func (d *float64Dest) Scan(v any) error {
	if x, ok := v.(float64); ok {
		*d.p = x
		return nil
	}
	var n sql.NullFloat64
	if err := n.Scan(v); err != nil {
		return err
	}
	*d.p = n.Float64
	return nil
}

type boolDest struct{ p *bool }

func (d *boolDest) set(p unsafe.Pointer) { d.p = (*bool)(p) }
func (d *boolDest) Scan(v any) error {
	if x, ok := v.(bool); ok {
		*d.p = x
		return nil
	}
	var n sql.NullBool
	if err := n.Scan(v); err != nil {
		return err
	}
	*d.p = n.Bool
	return nil
}

type timeDest struct{ p *time.Time }

func (d *timeDest) set(p unsafe.Pointer) { d.p = (*time.Time)(p) }
func (d *timeDest) Scan(v any) error {
	if x, ok := v.(time.Time); ok {
		*d.p = x
		return nil
	}
	var n sql.NullTime
	if err := n.Scan(v); err != nil {
		return err
	}
	*d.p = n.Time
	return nil
}

var (
	stringType  = reflect.TypeOf("")
	int64Type   = reflect.TypeOf(int64(0))
	intType     = reflect.TypeOf(0)
	float64Type = reflect.TypeOf(float64(0))
	boolType    = reflect.TypeOf(false)
)

// newFastDest returns a fast destination for exactly these types (named
// types such as `type Status string` take the slow path).
func newFastDest(t reflect.Type) fastDest {
	switch t {
	case stringType:
		return &strDest{}
	case int64Type:
		return &int64Dest{}
	case intType:
		return &intDest{}
	case float64Type:
		return &float64Dest{}
	case boolType:
		return &boolDest{}
	case timeType:
		return &timeDest{}
	}
	return nil
}

// fieldOffset returns the byte offset of the field at index within t, or
// false if reaching it goes through a pointer (embedded *Struct).
func fieldOffset(t reflect.Type, index []int) (uintptr, bool) {
	var off uintptr
	for i, x := range index {
		if t.Kind() != reflect.Struct {
			return 0, false
		}
		f := t.Field(x)
		off += f.Offset
		t = f.Type
		if i < len(index)-1 && t.Kind() == reflect.Pointer {
			return 0, false
		}
	}
	return off, true
}

func errOverflow(v int64, typ string) error {
	return fmt.Errorf("value %d overflows %s", v, typ)
}
