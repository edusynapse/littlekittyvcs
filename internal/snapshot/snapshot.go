// Package snapshot stores one encrypted copy of a repository's refs after
// each committed push, and rebuilds a database from those copies.
package snapshot

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// Snapshot is the plaintext of one ref snapshot.
// Deleted is a delete marker: Refs is empty and no HEAD is stored.
type Snapshot struct {
	RepoID  [16]byte
	Name    string
	PushSeq int64
	Deleted bool
	Refs    []store.Ref
}

// Key is the bucket key of one snapshot. snapID is its own id, not a pack id.
func Key(repoID, snapID [16]byte) string {
	return "repos/" + store.FormatID(repoID) + "/refs/" + store.FormatID(snapID) + ".snap"
}

type snapDoc struct {
	Format     int      `json:"format"`
	Repository string   `json:"repository"`
	RepoID     string   `json:"repo_id"`
	PushSeq    int64    `json:"push_seq"`
	Deleted    *bool    `json:"deleted"`
	Refs       []refDoc `json:"refs"`
}

// refDoc keeps symbolic and object_id as pointers so a missing field is
// distinct from an empty one. Neither and both are refused.
type refDoc struct {
	Name     string  `json:"name"`
	Symbolic *string `json:"symbolic,omitempty"`
	ObjectID *string `json:"object_id,omitempty"`
}

// Write stores s under a new random snapshot id and returns that object's key.
func Write(ctx context.Context, bucket blob.Store, kr *objcrypt.Keyring, s Snapshot) (string, error) {
	snapID, err := store.NewID()
	if err != nil {
		return "", err
	}
	plain, err := marshal(s)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, err := objcrypt.NewWriter(&buf, kr, objcrypt.Binding{
		RepoID:   s.RepoID,
		ObjectID: snapID,
		Kind:     objcrypt.KindSnap,
	}, objcrypt.WriterOptions{})
	if err != nil {
		return "", err
	}
	if _, err := w.Write(plain); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	key := Key(s.RepoID, snapID)
	if err := bucket.Put(ctx, key, bytes.NewReader(buf.Bytes()), w.CiphertextSize()); err != nil {
		return "", err
	}
	return key, nil
}

// Read decrypts and checks one snapshot object. size is its ciphertext length.
func Read(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, repoID, snapID [16]byte, size int64) (Snapshot, error) {
	rd, err := objcrypt.Open(ctx, src, Key(repoID, snapID), size, kr, objcrypt.Binding{
		RepoID:   repoID,
		ObjectID: snapID,
		Kind:     objcrypt.KindSnap,
	})
	if err != nil {
		return Snapshot{}, err
	}
	plain := make([]byte, rd.Size())
	n, err := rd.ReadAt(plain, 0)
	if int(n) != len(plain) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return Snapshot{}, err
	}
	return unmarshal(plain, repoID)
}

func marshal(s Snapshot) ([]byte, error) {
	if s.Deleted && len(s.Refs) != 0 {
		return nil, errors.New("snapshot: invalid")
	}
	refs := make([]refDoc, 0, len(s.Refs))
	if !s.Deleted {
		for _, ref := range s.Refs {
			doc := refDoc{Name: ref.Name}
			switch {
			case ref.ObjectID != nil && ref.Symbolic == "":
				hexID := hex.EncodeToString(ref.ObjectID[:])
				doc.ObjectID = &hexID
			case ref.ObjectID == nil && ref.Symbolic != "":
				sym := ref.Symbolic
				doc.Symbolic = &sym
			default:
				return nil, errors.New("snapshot: invalid")
			}
			refs = append(refs, doc)
		}
	}
	deleted := s.Deleted
	return json.Marshal(snapDoc{
		Format:     1,
		Repository: s.Name,
		RepoID:     store.FormatID(s.RepoID),
		PushSeq:    s.PushSeq,
		Deleted:    &deleted,
		Refs:       refs,
	})
}

func unmarshal(plain []byte, repoID [16]byte) (Snapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	var doc snapDoc
	if err := dec.Decode(&doc); err != nil {
		return Snapshot{}, errors.New("snapshot: invalid")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Snapshot{}, errors.New("snapshot: invalid")
	}
	if doc.Format != 1 || doc.PushSeq < 1 || !store.ValidRepoName(doc.Repository) || doc.Deleted == nil {
		return Snapshot{}, errors.New("snapshot: invalid")
	}
	id, err := store.ParseID(doc.RepoID)
	if err != nil || id != repoID {
		return Snapshot{}, errors.New("snapshot: invalid")
	}
	if *doc.Deleted {
		if len(doc.Refs) != 0 {
			return Snapshot{}, errors.New("snapshot: invalid")
		}
		return Snapshot{RepoID: repoID, Name: doc.Repository, PushSeq: doc.PushSeq, Deleted: true}, nil
	}
	refs, err := refsFromDoc(doc.Refs)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{RepoID: repoID, Name: doc.Repository, PushSeq: doc.PushSeq, Refs: refs}, nil
}

func refsFromDoc(in []refDoc) ([]store.Ref, error) {
	seen := map[string]struct{}{}
	var head bool
	out := make([]store.Ref, 0, len(in))
	for _, doc := range in {
		if doc.Name == "" {
			return nil, errors.New("snapshot: invalid")
		}
		if _, ok := seen[doc.Name]; ok {
			return nil, errors.New("snapshot: invalid")
		}
		seen[doc.Name] = struct{}{}
		hasSymbolic := doc.Symbolic != nil
		hasObject := doc.ObjectID != nil
		if hasSymbolic == hasObject {
			return nil, errors.New("snapshot: invalid")
		}
		if doc.Name == "HEAD" {
			if !hasSymbolic {
				return nil, errors.New("snapshot: invalid")
			}
			head = true
			out = append(out, store.Ref{Name: "HEAD", Symbolic: *doc.Symbolic})
			continue
		}
		if hasSymbolic || !strings.HasPrefix(doc.Name, "refs/") || plumbing.ReferenceName(doc.Name).Validate() != nil {
			return nil, errors.New("snapshot: invalid")
		}
		id, err := parseObjectID(*doc.ObjectID)
		if err != nil {
			return nil, errors.New("snapshot: invalid")
		}
		out = append(out, store.Ref{Name: doc.Name, ObjectID: id})
	}
	if !head {
		return nil, errors.New("snapshot: invalid")
	}
	return out, nil
}

func parseObjectID(s string) (*[20]byte, error) {
	if len(s) != 40 {
		return nil, errors.New("snapshot: invalid")
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, errors.New("snapshot: invalid")
		}
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 20 {
		return nil, errors.New("snapshot: invalid")
	}
	var id [20]byte
	copy(id[:], raw)
	return &id, nil
}
