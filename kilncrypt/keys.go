// Package kilncrypt encrypts columns in the application, so the database,
// its backups, replicas and caches only ever see ciphertext.
//
//	keys, err := kilncrypt.NewKeys(indexKey, kilncrypt.Key{ID: 1, Secret: secret1})
//	email, err := keys.Index("email", "email_index", strings.ToLower)
//	db := orm.Open(sqlDB, dialect.Postgres(), kilncrypt.Plugin(keys, email))
//
//	type User struct {
//		ID         int64
//		Email      kilncrypt.String // stored encrypted (a text column)
//		EmailIndex *string          // blind index, for exact-match lookups
//	}
//
//	u := User{Email: kilncrypt.NewString("ann@x.io")}
//	users.Insert(ctx, &u)                                     // writes ciphertext
//	got, err := users.Find(ctx, email.Eq("Ann@x.io"))     // search without decrypting
//	plain, err := got.Email.Open(keys)                        // decrypt where you need it
//
// Values are sealed with AES-256-GCM and bound to their table and column:
// a value copied into another column or table (by someone with write
// access to the database) does not decrypt. Loaded values stay encrypted
// until you call Open, and print, log and marshal to JSON as "[encrypted]".
// A value is not bound to its row: swapping values between two rows of the
// same column is not detected.
//
// Searching works through a blind index: an HMAC of the (normalized) value
// in a second column, compared with Index.Eq. It supports exact matches
// only, no LIKE, ranges or sorting, and equal values have equal indexes:
// anyone who can read the table sees which rows share a value. Don't index
// columns with few possible values (booleans, countries).
package kilncrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// KeySize is the length of every key: 32 bytes (AES-256, HMAC-SHA256).
// Generate one with: openssl rand -base64 32
const KeySize = 32

var (
	// ErrDecrypt is returned by Open when a value cannot be decrypted: it
	// was changed, or sealed with a key the Keys don't have.
	ErrDecrypt = errors.New("kilncrypt: cannot decrypt value")
	// ErrNotSealed is returned when a String created by NewString is
	// written without kilncrypt.Plugin, which would store it unencrypted.
	ErrNotSealed = errors.New("kilncrypt: value is not encrypted; add kilncrypt.Plugin(keys) to orm.Open, or use Keys.SealColumn for raw SQL")
	// ErrUnbound is returned by Open for a value that was not loaded
	// through kiln (e.g. scanned by raw SQL), so its column is unknown.
	ErrUnbound = errors.New("kilncrypt: value was not loaded through kiln; use OpenColumn with its table and column")
)

// Key is one encryption key. ID is stored with every value it seals, so
// keep IDs unique and never reuse one for a different secret.
type Key struct {
	ID     uint32
	Secret []byte // KeySize bytes
}

// DecodeKey builds a Key from a base64 secret, e.g. read from an
// environment variable or a secrets manager.
func DecodeKey(id uint32, base64Secret string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(base64Secret)
	if err != nil {
		return Key{}, fmt.Errorf("kilncrypt: key %d: %w", id, err)
	}
	return Key{ID: id, Secret: b}, nil
}

// Keys holds the encryption keys and the blind index key. It is safe for
// concurrent use.
type Keys struct {
	current   uint32
	aeads     map[uint32]cipher.AEAD // read-only after NewKeys
	index     []byte
	rejectKC1 bool
}

// RejectUnbound makes Open refuse values written by kilncrypt v0.1, which
// are not bound to a column (someone with write access to the database
// could copy one into another column). Call it after Rotate has rewritten
// every model's values, before the Keys are used.
func (k *Keys) RejectUnbound() { k.rejectKC1 = true }

// NewKeys returns Keys that seal with current and open values sealed with
// current or any of old (keys being rotated out, see Rotate). indexKey is
// the blind index key; pass nil if you don't use indexes. It must differ
// from the encryption keys.
func NewKeys(indexKey []byte, current Key, old ...Key) (*Keys, error) {
	k := &Keys{current: current.ID, aeads: make(map[uint32]cipher.AEAD, 1+len(old))}
	for _, key := range append([]Key{current}, old...) {
		if len(key.Secret) != KeySize {
			return nil, fmt.Errorf("kilncrypt: key %d is %d bytes, want %d", key.ID, len(key.Secret), KeySize)
		}
		if _, dup := k.aeads[key.ID]; dup {
			return nil, fmt.Errorf("kilncrypt: duplicate key id %d", key.ID)
		}
		if indexKey != nil && hmac.Equal(indexKey, key.Secret) {
			return nil, errors.New("kilncrypt: the index key must not be an encryption key")
		}
		block, err := aes.NewCipher(key.Secret)
		if err != nil {
			return nil, err
		}
		if k.aeads[key.ID], err = cipher.NewGCM(block); err != nil {
			return nil, err
		}
	}
	if indexKey != nil {
		if len(indexKey) != KeySize {
			return nil, fmt.Errorf("kilncrypt: index key is %d bytes, want %d", len(indexKey), KeySize)
		}
		k.index = append([]byte(nil), indexKey...)
	}
	return k, nil
}

// Seal returns a String holding v.
//
// Deprecated: values are now bound to their column, which Seal doesn't
// know. Use NewString (Plugin seals it on Insert, Update and UpdateAny's
// q.Set), or SealColumn for raw SQL.
func (k *Keys) Seal(v string) (String, error) { return NewString(v), nil }

// SealColumn encrypts v for the given table and column, for writing with
// raw SQL. Plugin does this for you on Insert, Update and UpdateAny.
func (k *Keys) SealColumn(v, table, column string) (String, error) {
	l := label(table, column)
	sealed, err := k.seal(v, l)
	if err != nil {
		return String{}, err
	}
	return String{plain: v, hasPlain: true, sealed: sealed, bound: l}, nil
}

// Stored form: prefix + base64url(key id (4 bytes) | nonce | ciphertext+tag).
// "kc2:" values authenticate the prefix, key id and the value's table and
// column; "kc1:" values (v0.1) only the prefix and key id. New values are
// always kc2; kc1 values are read, and rewritten as kc2 on their next
// write or by Rotate.
const (
	prefixV1 = "kc1:"
	prefixV2 = "kc2:"
)

// label identifies a column: table NUL column (unambiguous, as names
// can't contain NUL).
func label(table, column string) string { return table + "\x00" + column }

func (k *Keys) seal(v, label string) (string, error) {
	aead := k.aeads[k.current]
	buf := make([]byte, 4+aead.NonceSize(), 4+aead.NonceSize()+len(v)+aead.Overhead())
	binary.BigEndian.PutUint32(buf, k.current)
	if _, err := rand.Read(buf[4:]); err != nil {
		return "", err
	}
	out := aead.Seal(buf, buf[4:], []byte(v), aad(prefixV2, buf[:4], label))
	return prefixV2 + base64.RawURLEncoding.EncodeToString(out), nil
}

// open decrypts a stored value that belongs to the column label ("" if
// unknown, which only kc1 values accept).
func (k *Keys) open(sealed, label string) (string, error) {
	ver, id, raw, ok := parse(sealed)
	if !ok {
		return "", ErrDecrypt
	}
	pfx := prefixV2
	switch {
	case ver == 1 && k.rejectKC1:
		return "", fmt.Errorf("%w: v0.1 value not bound to a column (RejectUnbound is set; run Rotate)", ErrDecrypt)
	case ver == 1:
		pfx, label = prefixV1, ""
	case label == "":
		return "", ErrUnbound
	}
	aead, ok := k.aeads[id]
	if !ok {
		return "", fmt.Errorf("%w: unknown key id %d", ErrDecrypt, id)
	}
	n := aead.NonceSize()
	if len(raw) < 4+n+aead.Overhead() {
		return "", ErrDecrypt
	}
	plain, err := aead.Open(nil, raw[4:4+n], raw[4+n:], aad(pfx, raw[:4], label))
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}

func aad(prefix string, id []byte, label string) []byte {
	b := make([]byte, 0, len(prefix)+len(id)+len(label))
	return append(append(append(b, prefix...), id...), label...)
}

// parse returns the format version (1 or 2), key id and decoded bytes of
// a stored value.
func parse(sealed string) (ver int, id uint32, raw []byte, ok bool) {
	switch {
	case len(sealed) >= 4 && sealed[:4] == prefixV2:
		ver = 2
	case len(sealed) >= 4 && sealed[:4] == prefixV1:
		ver = 1
	default:
		return 0, 0, nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed[4:])
	if err != nil || len(raw) < 4 {
		return 0, 0, nil, false
	}
	return ver, binary.BigEndian.Uint32(raw), raw, true
}

// mac returns HMAC-SHA256(key, msg).
func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
