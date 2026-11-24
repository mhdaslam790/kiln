package kilncrypt

import (
	"bytes"
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

func TestSealOpen(t *testing.T) {
	k := mustKeys(t, nil, key(1, 'a'))
	s, err := k.Seal("ann@x.io")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Value()
	stored := v.(string)
	if !strings.HasPrefix(stored, "kc1:") || strings.Contains(stored, "ann") {
		t.Fatalf("stored %q", stored)
	}
	again, _ := k.Seal("ann@x.io")
	if v2, _ := again.Value(); v2 == stored {
		t.Fatal("same value sealed twice gave the same ciphertext")
	}

	var loaded String
	if err := loaded.Scan(stored); err != nil {
		t.Fatal(err)
	}
	if got, err := loaded.Open(k); err != nil || got != "ann@x.io" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	// Tampering, a missing key and garbage are all ErrDecrypt.
	raw := []byte(stored)
	raw[len(raw)-3] ^= 1
	for name, in := range map[string]string{"tampered": string(raw), "garbage": "kc1:!!", "plain": "ann@x.io"} {
		var s String
		_ = s.Scan(in)
		if _, err := s.Open(k); !errors.Is(err, ErrDecrypt) {
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
	s2, _ := rotated.Seal("x")
	if id, _ := s2.keyID(); id != 2 {
		t.Fatalf("sealed with key %d", id)
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
