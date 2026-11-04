package auth

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	a, ah, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, bh, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, Prefix) || len(a) != 4+43 {
		t.Fatalf("token %q len %d", a, len(a))
	}
	if a == b || ah == bh {
		t.Fatal("two tokens matched")
	}
	if ah != Hash(a) || bh != Hash(b) {
		t.Fatal("hash mismatch")
	}
}

func TestFromRequest(t *testing.T) {
	basic := func(user, password string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	}
	okCases := []struct {
		header string
		want   string
	}{
		{"Bearer t", "t"},
		{"bearer t", "t"},
		{basic("anything", "t"), "t"},
		{basic("", "t"), "t"},
	}
	for _, tc := range okCases {
		req := httptestRequest(tc.header)
		got, ok := FromRequest(req)
		if !ok || got != tc.want {
			t.Fatalf("%q -> %q ok=%v", tc.header, got, ok)
		}
	}
	bad := []string{
		"",
		basic("user", ""),
		"Basic !!!",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon")),
		"Digest x",
		"Bearer ",
	}
	for _, header := range bad {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if _, ok := FromRequest(req); ok {
			t.Fatalf("accepted %q", header)
		}
	}
}

func httptestRequest(header string) *http.Request {
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", header)
	return req
}

func TestAllows(t *testing.T) {
	var this, other [16]byte
	this[0] = 1
	other[0] = 2
	thisPtr := this
	cases := []struct {
		name  string
		tok   Token
		repo  [16]byte
		write bool
		want  bool
	}{
		{"ro all read", Token{Write: false}, this, false, true},
		{"ro all write", Token{Write: false}, this, true, false},
		{"ro this read", Token{Write: false, RepoID: &thisPtr}, this, false, true},
		{"ro this write", Token{Write: false, RepoID: &thisPtr}, this, true, false},
		{"ro other read", Token{Write: false, RepoID: &thisPtr}, other, false, false},
		{"ro other write", Token{Write: false, RepoID: &thisPtr}, other, true, false},
		{"rw all read", Token{Write: true}, this, false, true},
		{"rw all write", Token{Write: true}, this, true, true},
		{"rw this read", Token{Write: true, RepoID: &thisPtr}, this, false, true},
		{"rw this write", Token{Write: true, RepoID: &thisPtr}, this, true, true},
		{"rw other read", Token{Write: true, RepoID: &thisPtr}, other, false, false},
		{"rw other write", Token{Write: true, RepoID: &thisPtr}, other, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tok.Allows(tc.repo, tc.write); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}
