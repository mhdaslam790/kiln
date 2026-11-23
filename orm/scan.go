package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"
	"unsafe"

	"github.com/mhdaslam790/kiln/internal/meta"
)

var (
	scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
	bytesType   = reflect.TypeOf([]byte(nil))

	afterFinderType = reflect.TypeOf((*AfterFinder)(nil)).Elem()
)

// structScanner scans rows into structs of one model. The per-column plan
// and the NULL-safe holders are built once per result set and reused for
// every row, so scanning allocates little beyond the values themselves.
type structScanner struct {
	plan  *scanPlan
	cols  []colPlan
	dests []any
}

type scanKind uint8

const (
	skSkip   scanKind = iota // unknown column
	skDirect                 // pointer or sql.Scanner field: scan into the field
	skJSON                   // JSON-encoded field
	skBytes                  // named []byte type (json.RawMessage, ...)
	skString                 // NULL-safe holders for common kinds
	skInt
	skUint
	skFloat
	skBool
	skTime
	skGeneric // anything else: scan through **T
	skFast    // plain field written in place by a fastDest
)

type colPlan struct {
	f      *meta.Field
	kind   scanKind
	holder any           // reused destination for non-direct kinds
	ptr    reflect.Value // skGeneric: the **T holder
	off    uintptr       // skFast: field offset in the struct
	fast   fastDest      // skFast: the scanner's destination
}

var timeType = reflect.TypeOf(time.Time{})

// scanPlan is the column layout of one result shape for a model. It is
// computed once and cached; scanners built from it are pooled, so a query
// whose columns match the previous one allocates no scanner at all.
type scanPlan struct {
	names []string
	cols  []colPlan // holder and ptr unset: they belong to each scanner
	pool  sync.Pool
}

var scanPlans sync.Map // *meta.Model -> *scanPlan (most recent shape)

func newStructScanner(rows *sql.Rows, m *meta.Model) (*structScanner, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	plan, _ := scanPlans.Load(m)
	p, _ := plan.(*scanPlan)
	if p == nil || !equalNames(p.names, names) {
		p = buildPlan(m, names)
		scanPlans.Store(m, p)
	}
	if sc, ok := p.pool.Get().(*structScanner); ok {
		return sc, nil
	}
	return p.newScanner(), nil
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func buildPlan(m *meta.Model, names []string) *scanPlan {
	p := &scanPlan{names: append([]string(nil), names...), cols: make([]colPlan, len(names))}
	for i, n := range names {
		f := m.Field(n)
		c := colPlan{f: f}
		switch {
		case f == nil:
			c.kind = skSkip
		case f.JSON:
			c.kind = skJSON
		case newFastDest(f.Type) != nil && fastOK(m, f, &c):
			c.kind = skFast
		case f.Type.Kind() == reflect.Pointer || reflect.PointerTo(f.Type).Implements(scannerType):
			c.kind = skDirect
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Uint8 && f.Type != bytesType:
			c.kind = skBytes
		case f.Type == timeType:
			c.kind = skTime
		default:
			switch f.Type.Kind() {
			case reflect.String:
				c.kind = skString
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				c.kind = skInt
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				c.kind = skUint
			case reflect.Float32, reflect.Float64:
				c.kind = skFloat
			case reflect.Bool:
				c.kind = skBool
			default:
				c.kind = skGeneric
			}
		}
		p.cols[i] = c
	}
	return p
}

// newScanner allocates the per-scanner holders for the plan.
func (p *scanPlan) newScanner() *structScanner {
	s := &structScanner{plan: p, cols: make([]colPlan, len(p.cols)), dests: make([]any, len(p.cols))}
	copy(s.cols, p.cols)
	for i := range s.cols {
		c := &s.cols[i]
		switch c.kind {
		case skSkip:
			c.holder = new(any)
		case skJSON, skBytes:
			c.holder = new([]byte)
		case skTime:
			c.holder = new(sql.NullTime)
		case skString:
			c.holder = new(sql.NullString)
		case skInt, skUint:
			c.holder = new(sql.NullInt64)
		case skFloat:
			c.holder = new(sql.NullFloat64)
		case skBool:
			c.holder = new(sql.NullBool)
		case skGeneric:
			c.ptr = reflect.New(reflect.PointerTo(c.f.Type))
			c.holder = c.ptr.Interface()
		case skFast:
			c.fast = newFastDest(c.f.Type)
			c.holder = c.fast
		}
		s.dests[i] = c.holder
	}
	return s
}

// release clears the scanner (so pooled scanners keep no row data or
// pointers into the caller's structs alive) and returns it to the pool.
func (s *structScanner) release() {
	for i := range s.cols {
		c := &s.cols[i]
		switch h := c.holder.(type) {
		case *any:
			*h = nil
		case *[]byte:
			*h = nil
		case *sql.NullString:
			*h = sql.NullString{}
		case *sql.NullTime:
			*h = sql.NullTime{}
		}
		if c.kind == skDirect {
			s.dests[i] = nil
		}
		if c.kind == skFast {
			c.fast.set(nil)
		}
		if c.kind == skGeneric {
			c.ptr.Elem().SetZero()
		}
	}
	s.plan.pool.Put(s)
}

// scan reads the current row into dst (an addressable struct value).
func (s *structScanner) scan(rows *sql.Rows, dst reflect.Value) error {
	base := dst.Addr().UnsafePointer()
	for i := range s.cols {
		switch c := &s.cols[i]; c.kind {
		case skDirect:
			s.dests[i] = dst.FieldByIndex(c.f.Index).Addr().Interface()
		case skFast:
			c.fast.set(unsafe.Add(base, c.off))
		}
	}
	if err := rows.Scan(s.dests...); err != nil {
		return err
	}
	for i := range s.cols {
		c := &s.cols[i]
		if c.kind == skSkip || c.kind == skDirect || c.kind == skFast {
			continue
		}
		target := dst.FieldByIndex(c.f.Index)
		switch c.kind {
		case skJSON:
			raw := c.holder.(*[]byte)
			if len(*raw) == 0 || string(*raw) == "null" {
				target.SetZero()
			} else if err := json.Unmarshal(*raw, target.Addr().Interface()); err != nil {
				return fmt.Errorf("orm: column %s: %w", c.f.Column, err)
			}
		case skBytes:
			if raw := c.holder.(*[]byte); *raw == nil {
				target.SetZero()
			} else {
				target.Set(reflect.ValueOf(*raw).Convert(c.f.Type))
			}
		case skString:
			target.SetString(c.holder.(*sql.NullString).String)
		case skInt:
			v := c.holder.(*sql.NullInt64).Int64
			if target.OverflowInt(v) {
				return fmt.Errorf("orm: column %s: value %d overflows %s", c.f.Column, v, c.f.Type)
			}
			target.SetInt(v)
		case skUint:
			v := c.holder.(*sql.NullInt64).Int64
			if v < 0 || target.OverflowUint(uint64(v)) {
				return fmt.Errorf("orm: column %s: value %d overflows %s", c.f.Column, v, c.f.Type)
			}
			target.SetUint(uint64(v))
		case skFloat:
			target.SetFloat(c.holder.(*sql.NullFloat64).Float64)
		case skBool:
			target.SetBool(c.holder.(*sql.NullBool).Bool)
		case skTime:
			// Assign through a typed pointer: reflect.ValueOf(time) would
			// allocate for every row.
			*(target.Addr().Interface().(*time.Time)) = c.holder.(*sql.NullTime).Time
		case skGeneric:
			if p := c.ptr.Elem(); !p.IsNil() {
				target.Set(p.Elem())
			} else {
				target.SetZero()
			}
		}
	}
	return nil
}

// scanAll reads every row into a new slice of the model's struct type and
// runs AfterFind hooks. The returned slice's elements are addressable.
func scanAll(ctx context.Context, rows *sql.Rows, m *meta.Model) (reflect.Value, error) {
	defer rows.Close()
	out := reflect.MakeSlice(reflect.SliceOf(m.Type), 0, 8)
	sc, err := newStructScanner(rows, m)
	if err != nil {
		return out, err
	}
	defer sc.release()
	for rows.Next() {
		out = reflect.Append(out, reflect.Zero(m.Type))
		if err := sc.scan(rows, out.Index(out.Len()-1)); err != nil {
			return out, err
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if reflect.PointerTo(m.Type).Implements(afterFinderType) {
		for i := 0; i < out.Len(); i++ {
			if err := callHook(ctx, out.Index(i), hookAfterFind); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// scanInto scans raw query results into dst, which may be a pointer to a
// struct, a slice of structs (or struct pointers), a scalar, or a slice of
// scalars.
func scanInto(ctx context.Context, rows *sql.Rows, dst any) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		rows.Close()
		return fmt.Errorf("orm: scan destination must be a non-nil pointer, got %T", dst)
	}
	ev := rv.Elem()
	et := ev.Type()

	if et.Kind() == reflect.Slice && et != reflect.TypeOf([]byte(nil)) {
		elem := et.Elem()
		base := elem
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if base.Kind() == reflect.Struct && !meta.IsColumnType(base) {
			m, err := meta.Of(base)
			if err != nil {
				rows.Close()
				return err
			}
			items, err := scanAll(ctx, rows, m)
			if err != nil {
				return err
			}
			return assignSlice(ev, items)
		}
		defer rows.Close()
		out := reflect.MakeSlice(et, 0, 8)
		for rows.Next() {
			p := reflect.New(elem)
			if err := rows.Scan(p.Interface()); err != nil {
				return err
			}
			out = reflect.Append(out, p.Elem())
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ev.Set(out)
		return nil
	}

	if et.Kind() == reflect.Struct && !meta.IsColumnType(et) {
		m, err := meta.Of(et)
		if err != nil {
			rows.Close()
			return err
		}
		items, err := scanAll(ctx, rows, m)
		if err != nil {
			return err
		}
		if items.Len() == 0 {
			return ErrNotFound
		}
		ev.Set(items.Index(0))
		return nil
	}

	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrNotFound
	}
	if err := rows.Scan(dst); err != nil {
		return err
	}
	return rows.Err()
}

// assignSlice stores items ([]T) into dst ([]T or []*T).
func assignSlice(dst reflect.Value, items reflect.Value) error {
	if dst.Type().Elem().Kind() != reflect.Pointer {
		dst.Set(items)
		return nil
	}
	out := reflect.MakeSlice(dst.Type(), items.Len(), items.Len())
	for i := 0; i < items.Len(); i++ {
		out.Index(i).Set(items.Index(i).Addr())
	}
	dst.Set(out)
	return nil
}

// fastOK records the field offset for a fast destination; fields reached
// through an embedded pointer use the regular path.
func fastOK(m *meta.Model, f *meta.Field, c *colPlan) bool {
	off, ok := fieldOffset(m.Type, f.Index)
	c.off = off
	return ok
}
