package orm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"
)

// Binary format of a cached result (version 1):
//
//	'G' 1 | uvarint ncols | ncols × string | uvarint nrows | nrows × ncols × value
//
// string = uvarint len + bytes; value = tag byte + payload.

const codecVersion = 1

const (
	tagNil byte = iota
	tagInt
	tagFloat
	tagFalse
	tagTrue
	tagBytes
	tagString
	tagTime
)

var errCodec = errors.New("orm: corrupt cache entry")

// encodeEntry prefixes an encoded result with the table versions it was
// read under: uvarint n | n × string | result.
func encodeEntry(vers []string, r *cachedResult) ([]byte, error) {
	var b []byte
	b = binary.AppendUvarint(b, uint64(len(vers)))
	for _, v := range vers {
		b = appendString(b, v)
	}
	rb, err := encodeResult(r)
	if err != nil {
		return nil, err
	}
	return append(b, rb...), nil
}

func decodeEntry(b []byte) ([]string, *cachedResult, error) {
	d := &decoder{b: b}
	n := d.uvarint()
	if d.err != nil || n > uint64(len(d.b)) {
		return nil, nil, errCodec
	}
	vers := make([]string, n)
	for i := range vers {
		vers[i] = string(d.bytes())
	}
	if d.err != nil {
		return nil, nil, d.err
	}
	r, err := decodeResult(d.b)
	return vers, r, err
}

func encodeResult(r *cachedResult) ([]byte, error) {
	b := make([]byte, 0, 64+len(r.rows)*len(r.cols)*8)
	b = append(b, 'G', codecVersion)
	b = binary.AppendUvarint(b, uint64(len(r.cols)))
	for _, c := range r.cols {
		b = appendString(b, c)
	}
	b = binary.AppendUvarint(b, uint64(len(r.rows)))
	for _, row := range r.rows {
		for _, v := range row {
			var err error
			if b, err = appendValue(b, v); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendValue(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, tagNil), nil
	case int64:
		return binary.AppendVarint(append(b, tagInt), x), nil
	case float64:
		return binary.LittleEndian.AppendUint64(append(b, tagFloat), math.Float64bits(x)), nil
	case bool:
		if x {
			return append(b, tagTrue), nil
		}
		return append(b, tagFalse), nil
	case []byte:
		b = binary.AppendUvarint(append(b, tagBytes), uint64(len(x)))
		return append(b, x...), nil
	case string:
		return appendString(append(b, tagString), x), nil
	case time.Time:
		tb, err := x.MarshalBinary()
		if err != nil {
			return nil, err
		}
		b = binary.AppendUvarint(append(b, tagTime), uint64(len(tb)))
		return append(b, tb...), nil
	}
	return nil, fmt.Errorf("orm: cannot cache driver value of type %T", v)
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = errCodec
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) bytes() []byte {
	n := d.uvarint()
	if d.err != nil || n > uint64(len(d.b)) {
		d.err = errCodec
		return nil
	}
	out := d.b[:n:n]
	d.b = d.b[n:]
	return out
}

func decodeResult(b []byte) (*cachedResult, error) {
	if len(b) < 2 || b[0] != 'G' || b[1] != codecVersion {
		return nil, errCodec
	}
	d := &decoder{b: b[2:]}
	ncols := d.uvarint()
	if d.err != nil || ncols > uint64(len(d.b)) {
		return nil, errCodec
	}
	r := &cachedResult{cols: make([]string, ncols)}
	for i := range r.cols {
		r.cols[i] = string(d.bytes())
	}
	nrows := d.uvarint()
	if d.err != nil || (ncols > 0 && nrows > uint64(len(d.b))) {
		return nil, errCodec
	}
	r.rows = make([][]any, nrows)
	for i := range r.rows {
		row := make([]any, ncols)
		for j := range row {
			if len(d.b) == 0 {
				return nil, errCodec
			}
			tag := d.b[0]
			d.b = d.b[1:]
			switch tag {
			case tagNil:
			case tagInt:
				v, n := binary.Varint(d.b)
				if n <= 0 {
					return nil, errCodec
				}
				row[j], d.b = v, d.b[n:]
			case tagFloat:
				if len(d.b) < 8 {
					return nil, errCodec
				}
				row[j], d.b = math.Float64frombits(binary.LittleEndian.Uint64(d.b)), d.b[8:]
			case tagFalse:
				row[j] = false
			case tagTrue:
				row[j] = true
			case tagBytes:
				row[j] = d.bytes()
			case tagString:
				row[j] = string(d.bytes())
			case tagTime:
				var t time.Time
				if err := t.UnmarshalBinary(d.bytes()); err != nil {
					return nil, errCodec
				}
				row[j] = t
			default:
				return nil, errCodec
			}
			if d.err != nil {
				return nil, d.err
			}
		}
		r.rows[i] = row
	}
	if len(d.b) != 0 {
		return nil, errCodec
	}
	return r, nil
}

// isPlainArg reports whether a query argument is a value of a basic kind
// (e.g. a named string type) whose %v formatting is unambiguous.
func isPlainArg(a any) bool {
	switch reflect.ValueOf(a).Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
