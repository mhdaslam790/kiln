package kilnmask

import (
	"testing"
)

type mUser struct {
	Email string `mask:"email"`
	Card  int64  `mask:"full"`
	Raw   []byte `mask:"full"`
}

type mNode struct {
	Email string `mask:"email"`
	Peer  *mNode
}

type mHolder struct {
	ByID map[int]mUser
	Any  map[string]any
	Data any
}

func TestMapsAndInterfaces(t *testing.T) {
	h := mHolder{
		ByID: map[int]mUser{1: {Email: "ann@x.io"}},
		Any:  map[string]any{"u": mUser{Email: "bob@x.io"}, "n": 5},
		Data: &mUser{Email: "cy@x.io"},
	}
	out := Copy(h)
	if out.ByID[1].Email != "a****@x.io" || out.Any["u"].(mUser).Email != "b****@x.io" || out.Any["n"] != 5 || out.Data.(*mUser).Email != "c****@x.io" {
		t.Fatalf("not masked: %+v", out)
	}
	if h.ByID[1].Email != "ann@x.io" || h.Any["u"].(mUser).Email != "bob@x.io" || h.Data.(*mUser).Email != "cy@x.io" {
		t.Fatal("original changed")
	}
}

func TestNonStringTagsCleared(t *testing.T) {
	u := mUser{Email: "c@x.io", Card: 4111, Raw: []byte("secret")}
	out := Copy(u)
	if out.Card != 0 || out.Raw != nil {
		t.Fatalf("tagged non-string fields kept: %+v", out)
	}
	if u.Card != 4111 || string(u.Raw) != "secret" {
		t.Fatal("original changed")
	}
}

func TestCycles(t *testing.T) {
	a, b := &mNode{Email: "a@x.io"}, &mNode{Email: "b@x.io"}
	a.Peer, b.Peer = b, a
	ca := Copy(a)
	if ca == a || ca.Email != "****@x.io" || ca.Peer.Email != "****@x.io" || ca.Peer.Peer != ca {
		t.Fatalf("cycle copy: %+v %+v", ca, ca.Peer)
	}
	if a.Email != "a@x.io" || b.Email != "b@x.io" || a.Peer != b {
		t.Fatal("original changed")
	}
	self := &mNode{Email: "s@x.io"}
	self.Peer = self
	if c := Copy(self); c.Peer != c || c.Email != "****@x.io" {
		t.Fatalf("self cycle: %+v", c)
	}
}
