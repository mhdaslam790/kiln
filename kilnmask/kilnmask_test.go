package kilnmask

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

type Address struct {
	Street string `mask:"full"`
	City   string
}

type User struct {
	ID       int64
	Name     string  `mask:"first:1"`
	Email    string  `mask:"email"`
	Phone    *string `mask:"last:4"`
	IBAN     string  `mask:"iban"`
	Secret   string  `mask:"nonsense"`
	Public   string
	Skip     Address `mask:"-"`
	Home     Address
	Work     *Address
	Previous []Address
	Friend   *User
	secret   string
}

func TestRules(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"full", "x"}: "****", {"full", ""}: "",
		{"email", "ann@acme.io"}: "a****@acme.io", {"email", "nope"}: "****", {"email", "@x"}: "****", {"email", "a@"}: "****",
		{"first:2", "Zoë Q"}: "Zo****", {"first:5", "Ann"}: "****",
		{"last:4", "4111111111111111"}: "****1111", {"last:4", "123"}: "****",
		{"bogus", "x"}: "****", {"first:x", "abc"}: "****",
	} {
		var o options
		if got := o.apply(in[0], in[1]); got != want {
			t.Errorf("%s(%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestCopy(t *testing.T) {
	phone := "+1 555 123 4567"
	orig := User{
		ID: 1, Name: "Ann", Email: "ann@acme.io", Phone: &phone, IBAN: "DE89370400440532013000",
		Secret: "s3cret", Public: "hi", Skip: Address{Street: "kept"},
		Home: Address{Street: "1 Main St", City: "Oslo"}, Work: &Address{Street: "2 Work Rd"},
		Previous: []Address{{Street: "3 Old Ln"}}, Friend: &User{Email: "bob@x.io"}, secret: "x",
	}
	got := Copy(orig, WithRule("iban", func(s string) string { return Last(s, 4) }))

	want := map[string][2]string{
		"Name": {got.Name, "A****"}, "Email": {got.Email, "a****@acme.io"}, "Phone": {*got.Phone, "****4567"},
		"IBAN": {got.IBAN, "****3000"}, "Secret": {got.Secret, "****"}, "Public": {got.Public, "hi"},
		"Skip": {got.Skip.Street, "kept"}, "Home": {got.Home.Street, "****"}, "City": {got.Home.City, "Oslo"},
		"Work": {got.Work.Street, "****"}, "Previous": {got.Previous[0].Street, "****"}, "Friend": {got.Friend.Email, "b****@x.io"},
	}
	for name, w := range want {
		if w[0] != w[1] {
			t.Errorf("%s = %q, want %q", name, w[0], w[1])
		}
	}
	// The original and everything it points to are untouched.
	if orig.Name != "Ann" || phone != "+1 555 123 4567" || orig.Work.Street != "2 Work Rd" ||
		orig.Previous[0].Street != "3 Old Ln" || orig.Friend.Email != "bob@x.io" || orig.Home.Street != "1 Main St" {
		t.Fatalf("original changed: %+v", orig)
	}

	// Pointers, slices and interfaces of structs.
	if p := Copy(&orig); p == &orig || p.Email != "a****@acme.io" || orig.Email != "ann@acme.io" {
		t.Fatalf("pointer: %+v", p)
	}
	list := []User{orig, {Email: "c@d.io"}}
	if l := Copy(list); l[1].Email != "****@d.io" || list[1].Email != "c@d.io" {
		t.Fatalf("slice: %+v", l)
	}
	if a := Copy(any(orig)).(User); a.Email != "a****@acme.io" {
		t.Fatalf("interface: %+v", a)
	}
	// Nothing to mask: the same value comes back.
	if s := Copy([]Address(nil)); s != nil {
		t.Fatal("nil slice")
	}
	if n := Copy(42); n != 42 {
		t.Fatal("int")
	}

	js, _ := json.Marshal(Copy(orig))
	if strings.Contains(string(js), "ann@") || strings.Contains(string(js), "Main St") {
		t.Fatalf("json leaked: %s", js)
	}
}

func TestLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	u := User{Email: "ann@acme.io", Home: Address{Street: "1 Main St"}}
	log.Info("signup", "user", Log(u))
	if out := buf.String(); strings.Contains(out, "ann@") || strings.Contains(out, "Main St") || !strings.Contains(out, "a****@acme.io") {
		t.Fatalf("log: %s", out)
	}
	if u.Email != "ann@acme.io" {
		t.Fatal("Log changed the value")
	}
}

// Recursive types must not loop forever when building the plan.
type Node struct {
	Name     string `mask:"full"`
	Children []*Node
}

func TestRecursive(t *testing.T) {
	n := Copy(Node{Name: "root", Children: []*Node{{Name: "kid"}}})
	if n.Name != "****" || n.Children[0].Name != "****" {
		t.Fatalf("%+v %+v", n, n.Children[0])
	}
}

func BenchmarkCopy(b *testing.B) {
	phone := "+1 555 123 4567"
	u := User{Name: "Ann", Email: "ann@acme.io", Phone: &phone, Home: Address{Street: "x"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Copy(u)
	}
}
