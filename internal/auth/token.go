// Package auth creates tokens and reads them from HTTP requests.
// It does not look tokens up. The caller hashes the plaintext and asks the store.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// Prefix is the start of every generated token.
const Prefix = "lkv_"

// Challenge is the WWW-Authenticate value every 401 must carry, so git prompts.
const Challenge = `Basic realm="littlekittyvcs"`

// Token is the authorization decision for one request.
type Token struct {
	ID     [16]byte
	Write  bool
	Admin  bool
	RepoID *[16]byte
}

// Generate returns a new token and the SHA-256 of that exact string.
// The plaintext is shown once and is not stored.
func Generate() (plaintext string, hash [32]byte, err error) {
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	plaintext = Prefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return plaintext, Hash(plaintext), nil
}

// Hash is SHA-256 of the exact token string.
func Hash(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// FromRequest reads a token from Authorization.
// Bearer uses the whole credential. Basic uses the password and ignores the username.
// The scheme name is case-insensitive. A missing or malformed header returns ok false.
func FromRequest(r *http.Request) (token string, ok bool) {
	header := r.Header.Get("Authorization")
	scheme, rest, found := strings.Cut(header, " ")
	if !found {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(scheme) {
	case "bearer":
		if rest == "" {
			return "", false
		}
		return rest, true
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return "", false
		}
		_, password, found := strings.Cut(string(raw), ":")
		if !found || password == "" {
			return "", false
		}
		return password, true
	default:
		return "", false
	}
}

// Allows reports whether the token may read or write repoID.
// write is the operation: false is a read, true is a write.
func (t Token) Allows(repoID [16]byte, write bool) bool {
	if write && !t.Write {
		return false
	}
	if t.RepoID != nil && *t.RepoID != repoID {
		return false
	}
	return true
}
