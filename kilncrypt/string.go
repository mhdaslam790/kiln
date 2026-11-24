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
}

// NewString returns a String holding v, encrypted when written.
func NewString(v string) String { return String{plain: v, hasPlain: true} }

// IsNull reports whether s is NULL.
func (s String) IsNull() bool { return !s.hasPlain && s.sealed == "" }

// Open returns the decrypted value ("" for NULL).
func (s String) Open(k *Keys) (string, error) {
	if s.hasPlain || s.sealed == "" {
		return s.plain, nil
	}
	return k.open(s.sealed)
}

// keyID returns the id of the key s is sealed with.
func (s String) keyID() (uint32, bool) {
	id, _, ok := parse(s.sealed)
	return id, ok
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
