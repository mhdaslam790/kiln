package kilncrypt

import (
	"database/sql/driver"
	"fmt"
)

// String is an encrypted text column. The zero value is NULL.
//
// Set it with NewString; read it with Open. A loaded String holds only the
// ciphertext, so writing it back (a full Update) stores it unchanged
// without decrypting. Its column type is text.
type String struct {
	plain    string
	hasPlain bool
	sealed   string // stored form; "" if not sealed yet or NULL
	bound    string // label of the column it was loaded from or sealed for
}

// NewString returns a String holding v, encrypted when written.
func NewString(v string) String { return String{plain: v, hasPlain: true} }

// IsNull reports whether s is NULL.
func (s String) IsNull() bool { return !s.hasPlain && s.sealed == "" }

// Open returns the decrypted value ("" for NULL). It checks that the
// value belongs to the column it was loaded from. A value scanned with raw
// SQL returns ErrUnbound; use OpenColumn for it.
func (s String) Open(k *Keys) (string, error) {
	if s.hasPlain || s.sealed == "" {
		return s.plain, nil
	}
	return k.open(s.sealed, s.bound)
}

// OpenColumn decrypts a value read from table.column, e.g. scanned with
// raw SQL.
func (s String) OpenColumn(k *Keys, table, column string) (string, error) {
	if s.hasPlain || s.sealed == "" {
		return s.plain, nil
	}
	return k.open(s.sealed, label(table, column))
}

// current reports whether s is stored in the latest format, with the
// current key, for the column label.
func (s String) current(k *Keys, label string) bool {
	ver, id, _, ok := parse(s.sealed)
	return ok && ver == 2 && id == k.current && s.bound == label
}

// Value implements driver.Valuer: the ciphertext, or NULL.
func (s String) Value() (driver.Value, error) {
	switch {
	case s.sealed != "":
		return s.sealed, nil
	case s.hasPlain:
		return nil, ErrNotSealed
	default:
		return nil, nil
	}
}

// Scan implements sql.Scanner.
func (s *String) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = String{}
	case string:
		*s = String{sealed: v}
	case []byte:
		*s = String{sealed: string(v)}
	default:
		return fmt.Errorf("kilncrypt: cannot scan %T into String", src)
	}
	return nil
}

const redacted = "[encrypted]"

// String returns "[encrypted]" so values never end up in logs.
func (s String) String() string { return redacted }

// GoString returns "[encrypted]" (for %#v).
func (s String) GoString() string { return redacted }

// MarshalJSON writes "[encrypted]", or null for NULL, so a struct sent as
// an API response does not leak the value. Open it into a plain field to
// return it.
func (s String) MarshalJSON() ([]byte, error) {
	if s.IsNull() {
		return []byte("null"), nil
	}
	return []byte(`"` + redacted + `"`), nil
}
