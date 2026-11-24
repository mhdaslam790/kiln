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
// Values are sealed with AES-256-GCM. Loaded values stay encrypted until
// you call Open, and print, log and marshal to JSON as "[encrypted]".
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
	ErrNotSealed = errors.New("kilncrypt: value is not encrypted; add kilncrypt.Plugin(keys) to orm.Open or use Keys.Seal")
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
	current uint32
	aeads   map[uint32]cipher.AEAD // read-only after NewKeys
	index   []byte
}

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

// Seal encrypts v with the current key. Plugin does this for you on
// insert and update; use Seal for values in UpdateAny (q.Set) or raw SQL.
func (k *Keys) Seal(v string) (String, error) {
	sealed, err := k.seal(v)
	if err != nil {
		return String{}, err
	}
	return String{plain: v, hasPlain: true, sealed: sealed}, nil
}

// Stored form: "kc1:" + base64url(key id (4 bytes) | nonce | ciphertext+tag).
// The prefix and key id are authenticated, so they cannot be swapped.
const prefix = "kc1:"

func (k *Keys) seal(v string) (string, error) {
	aead := k.aeads[k.current]
	buf := make([]byte, 4+aead.NonceSize(), 4+aead.NonceSize()+len(v)+aead.Overhead())
	binary.BigEndian.PutUint32(buf, k.current)
	if _, err := rand.Read(buf[4:]); err != nil {
		return "", err
	}
	out := aead.Seal(buf, buf[4:], []byte(v), aad(buf[:4]))
	return prefix + base64.RawURLEncoding.EncodeToString(out), nil
}

func (k *Keys) open(sealed string) (string, error) {
	id, raw, ok := parse(sealed)
	if !ok {
		return "", ErrDecrypt
	}
	aead, ok := k.aeads[id]
	if !ok {
		return "", fmt.Errorf("%w: unknown key id %d", ErrDecrypt, id)
	}
	n := aead.NonceSize()
	if len(raw) < 4+n+aead.Overhead() {
		return "", ErrDecrypt
	}
	plain, err := aead.Open(nil, raw[4:4+n], raw[4+n:], aad(raw[:4]))
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}

func aad(id []byte) []byte { return append([]byte(prefix), id...) }

// parse returns the key id and the decoded bytes of a stored value.
func parse(sealed string) (uint32, []byte, bool) {
	if len(sealed) < len(prefix) || sealed[:len(prefix)] != prefix {
		return 0, nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed[len(prefix):])
	if err != nil || len(raw) < 4 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint32(raw), raw, true
}

// mac returns HMAC-SHA256(key, msg).
func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
