package kilncrypt

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func key(id uint32, b byte) Key { return Key{ID: id, Secret: bytes.Repeat([]byte{b}, KeySize)} }

func mustKeys(t *testing.T, idx []byte, cur Key, old ...Key) *Keys {
	t.Helper()
	k, err := NewKeys(idx, cur, old...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sealV1 writes a value as kilncrypt v0.1 did (not bound to a column).
func sealV1(t *testing.T, k *Keys, v string) string {
	t.Helper()
	aead := k.aeads[k.current]
	buf := make([]byte, 4+aead.NonceSize(), 4+aead.NonceSize()+len(v)+aead.Overhead())
	binary.BigEndian.PutUint32(buf, k.current)
	if _, err := rand.Read(buf[4:]); err != nil {
		t.Fatal(err)
	}
	out := aead.Seal(buf, buf[4:], []byte(v), aad(prefixV1, buf[:4], ""))
	return prefixV1 + base64.RawURLEncoding.EncodeToString(out)
}

func scanned(stored string) String {
	var s String
	_ = s.Scan(stored)
	return s
}

func TestSealOpen(t *testing.T) {
	k := mustKeys(t, nil, key(1, 'a'))
	s, err := k.SealColumn("ann@x.io", "users", "email")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Value()
	stored := v.(string)
	if !strings.HasPrefix(stored, "kc2:") || strings.Contains(stored, "ann") {
		t.Fatalf("stored %q", stored)
	}
	again, _ := k.SealColumn("ann@x.io", "users", "email")
	if v2, _ := again.Value(); v2 == stored {
		t.Fatal("same value sealed twice gave the same ciphertext")
	}

	loaded := scanned(stored)
	if got, err := loaded.OpenColumn(k, "users", "email"); err != nil || got != "ann@x.io" {
		t.Fatalf("OpenColumn = %q, %v", got, err)
	}
	loaded.bound = label("users", "email") // as AfterRead does
	if got, err := loaded.Open(k); err != nil || got != "ann@x.io" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	// Bound to its column: another column or table does not decrypt.
	for _, at := range [][2]string{{"users", "name"}, {"admins", "email"}, {"users\x00e", "mail"}, {"", ""}} {
		if _, err := loaded.OpenColumn(k, at[0], at[1]); !errors.Is(err, ErrDecrypt) {
			t.Errorf("opened as %q.%q: %v", at[0], at[1], err)
		}
	}
	// Scanned outside kiln: Open can't know the column.
	if _, err := scanned(stored).Open(k); !errors.Is(err, ErrUnbound) {
		t.Errorf("unbound Open: %v", err)
	}

	// Tampering, a missing key and garbage are all ErrDecrypt.
	raw := []byte(stored)
	raw[len(raw)-3] ^= 1
	for name, in := range map[string]string{"tampered": string(raw), "garbage": "kc2:!!", "plain": "ann@x.io"} {
		if _, err := scanned(in).OpenColumn(k, "users", "email"); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := mustKeys(t, nil, key(2, 'b'))
	if _, err := loaded.Open(other); !errors.Is(err, ErrDecrypt) {
		t.Errorf("unknown key: %v", err)
	}
	// An old key still opens; the current one seals.
	rotated := mustKeys(t, nil, key(2, 'b'), key(1, 'a'))
	if got, err := loaded.Open(rotated); err != nil || got != "ann@x.io" {
		t.Fatalf("Open with old key = %q, %v", got, err)
	}
	s2, _ := rotated.SealColumn("x", "t", "c")
	if _, id, _, _ := parse(s2.sealed); id != 2 {
		t.Fatalf("sealed with key %d", id)
	}
	// Deprecated Seal leaves sealing to the plugin.
	if d, _ := k.Seal("x"); !d.hasPlain || d.sealed != "" {
		t.Fatalf("Seal: %+v", d)
	}
}

func TestV1Values(t *testing.T) {
	k := mustKeys(t, nil, key(1, 'a'))
	old := scanned(sealV1(t, k, "legacy"))
	// v0.1 values open in any column (they aren't bound), raw or loaded.
	if got, err := old.Open(k); err != nil || got != "legacy" {
		t.Fatalf("Open v1 = %q, %v", got, err)
	}
	old.bound = label("users", "email")
	if got, err := old.Open(k); err != nil || got != "legacy" {
		t.Fatalf("Open loaded v1 = %q, %v", got, err)
	}
	if old.current(k, old.bound) {
		t.Fatal("v1 value reported current (Rotate would skip it)")
	}
	// A kc2 value relabelled kc1 does not open (the prefix is authenticated).
	v2, _ := k.SealColumn("new", "users", "email")
	if _, err := scanned("kc1:" + v2.sealed[4:]).Open(k); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("downgraded kc2: %v", err)
	}
	k.RejectUnbound()
	if _, err := old.Open(k); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("RejectUnbound: %v", err)
	}
}

func TestNull(t *testing.T) {
	var s String
	if v, err := s.Value(); v != nil || err != nil || !s.IsNull() {
		t.Fatalf("zero String: %v %v", v, err)
	}
	_ = s.Scan(nil)
	if !s.IsNull() {
		t.Fatal("scanned NULL")
	}
	if v, err := NewString("x").Value(); v != nil || !errors.Is(err, ErrNotSealed) {
		t.Fatalf("unsealed Value = %v, %v", v, err)
	}
	if NewString("").IsNull() {
		t.Fatal("empty string is not NULL")
	}
}

func TestRedacted(t *testing.T) {
	k := mustKeys(t, nil, key(1, 'a'))
	s, _ := k.Seal("secret")
	u := struct {
		Name  string
		Email String
		Note  String
	}{"ann", s, String{}}
	for _, out := range []string{fmt.Sprint(u), fmt.Sprintf("%+v", u), fmt.Sprintf("%#v", u), s.String()} {
		if strings.Contains(out, "secret") || strings.Contains(out, "kc1:") {
			t.Errorf("leaked: %s", out)
		}
	}
	js, _ := json.Marshal(u)
	if string(js) != `{"Name":"ann","Email":"[encrypted]","Note":null}` {
		t.Errorf("json: %s", js)
	}
}

func TestNewKeysErrors(t *testing.T) {
	for name, fn := range map[string]func() error{
		"short":     func() error { _, err := NewKeys(nil, Key{ID: 1, Secret: []byte("short")}); return err },
		"dup id":    func() error { _, err := NewKeys(nil, key(1, 'a'), key(1, 'b')); return err },
		"short idx": func() error { _, err := NewKeys([]byte("x"), key(1, 'a')); return err },
		"same key":  func() error { _, err := NewKeys(key(9, 'a').Secret, key(1, 'a')); return err },
		"no idx": func() error {
			k, _ := NewKeys(nil, key(1, 'a'))
			_, err := k.Index("email", "email_index", nil)
			return err
		},
	} {
		if fn() == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := DecodeKey(1, "not base64!"); err == nil {
		t.Error("DecodeKey accepted garbage")
	}
}

func TestIndex(t *testing.T) {
	k := mustKeys(t, key(9, 'i').Secret, key(1, 'a'))
	email, _ := k.Index("email", "email_index", strings.ToLower)
	other, _ := k.Index("backup_email", "backup_email_index", strings.ToLower)
	if email.Of("Ann@X.io") != email.Of("ann@x.io") || len(email.Of("a")) != 32 {
		t.Fatal("normalized values differ")
	}
	if email.Of("ann@x.io") == other.Of("ann@x.io") {
		t.Fatal("two index columns share a key")
	}
	k2 := mustKeys(t, key(8, 'j').Secret, key(1, 'a'))
	email2, _ := k2.Index("email", "email_index", strings.ToLower)
	if email.Of("ann@x.io") == email2.Of("ann@x.io") {
		t.Fatal("index ignores the key")
	}
}
