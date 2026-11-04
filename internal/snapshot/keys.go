package snapshot

import (
	"strings"

	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// KeyKind is the object a parsed bucket key names.
type KeyKind int

const (
	KeyPack KeyKind = iota + 1
	KeyIdx
	KeySnap
)

// ParsedKey is a bucket key that is exactly one pack, index, or snapshot.
type ParsedKey struct {
	RepoID [16]byte
	ObjID  [16]byte
	Kind   KeyKind
}

// ParseKey returns ok only for a key that is exactly PackKey, IdxKey, or Key
// of the ids it contains. hasRepo reports whether a repository uuid could be
// read at all. An uppercase uuid parses and then fails the round trip, so
// hasRepo is true and ok is false.
func ParseKey(key string) (p ParsedKey, hasRepo, ok bool) {
	rest, found := strings.CutPrefix(key, "repos/")
	if !found {
		return
	}
	head, tail, found := strings.Cut(rest, "/")
	if !found {
		return
	}
	var err error
	p.RepoID, err = store.ParseID(head)
	if err != nil {
		return
	}
	hasRepo = true
	var raw string
	switch {
	case strings.HasPrefix(tail, "packs/") && strings.HasSuffix(tail, ".pack"):
		raw = strings.TrimSuffix(strings.TrimPrefix(tail, "packs/"), ".pack")
		p.Kind = KeyPack
	case strings.HasPrefix(tail, "packs/") && strings.HasSuffix(tail, ".idx"):
		raw = strings.TrimSuffix(strings.TrimPrefix(tail, "packs/"), ".idx")
		p.Kind = KeyIdx
	case strings.HasPrefix(tail, "refs/") && strings.HasSuffix(tail, ".snap"):
		raw = strings.TrimSuffix(strings.TrimPrefix(tail, "refs/"), ".snap")
		p.Kind = KeySnap
	default:
		return
	}
	if strings.Contains(raw, "/") {
		p.Kind = 0
		return
	}
	p.ObjID, err = store.ParseID(raw)
	if err != nil {
		p.Kind = 0
		return
	}
	var want string
	switch p.Kind {
	case KeyPack:
		want = packstore.PackKey(p.RepoID, p.ObjID)
	case KeyIdx:
		want = packstore.IdxKey(p.RepoID, p.ObjID)
	case KeySnap:
		want = Key(p.RepoID, p.ObjID)
	}
	if want != key {
		p.Kind = 0
		return
	}
	ok = true
	return
}
