// Package objindex maps a Git object id to the stored pack that holds it.
// The map is rebuilt from encrypted index files. It is not a database table.
//
// Memory is about 60 bytes per object plus the MemoryIndex. A repository of
// millions of objects is a known cost for version 1.
package objindex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/format/idxfile"

	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// Index is the process-local object map, one entry per repository.
// Drop removes one repository's entry in this process. Another process
// keeps its copy until it restarts.
type Index struct {
	mu    sync.Mutex
	repos map[[16]byte]*repoIndex
}

type repoIndex struct {
	mu     sync.Mutex
	byObj  map[[20]byte][16]byte
	packs  map[[16]byte]loaded
	loaded map[[16]byte]struct{}
}

type loaded struct {
	pack store.Pack
	idx  *idxfile.MemoryIndex
}

// New returns an empty index.
func New() *Index {
	return &Index{repos: map[[16]byte]*repoIndex{}}
}

// Refresh loads every pack this process has not loaded yet.
// Two requests for one repository do not decrypt the same index twice.
// A load in one repository does not block a lookup in another: the process
// lock is held only while finding the repository entry.
func (x *Index) Refresh(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, repoID [16]byte, packs []store.Pack) error {
	ri := x.repo(repoID)
	ri.mu.Lock()
	defer ri.mu.Unlock()
	for _, p := range packs {
		if _, ok := ri.loaded[p.ID]; ok {
			continue
		}
		raw, err := src.GetRange(ctx, p.IdxKey, 0, p.IdxBytes)
		if err != nil {
			return err
		}
		rd, err := objcrypt.Open(ctx, byteSource{b: raw}, p.IdxKey, p.IdxBytes, kr, objcrypt.Binding{
			RepoID:   repoID,
			ObjectID: p.ID,
			Kind:     objcrypt.KindIdx,
		})
		if err != nil {
			return err
		}
		plain := make([]byte, rd.Size())
		n, err := rd.ReadAt(plain, 0)
		if int(n) != len(plain) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		idx := &idxfile.MemoryIndex{}
		// bytes.Reader has no Stat, so the decoder skips the v2 size check and still decodes.
		if err := idxfile.NewDecoder(bytes.NewReader(plain)).Decode(idx); err != nil {
			return err
		}
		if err := ri.add(p, idx); err != nil {
			return err
		}
	}
	return nil
}

// Add records a pack this process just committed, without reading it back.
// A failure leaves the pack unloaded, so the next Refresh reads the index.
func (x *Index) Add(repoID [16]byte, p store.Pack, idx *idxfile.MemoryIndex) {
	ri := x.repo(repoID)
	ri.mu.Lock()
	defer ri.mu.Unlock()
	_ = ri.add(p, idx)
}

// Drop forgets a repository. The next Refresh for that id starts empty.
func (x *Index) Drop(repoID [16]byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.repos, repoID)
}

// Lookup returns the pack that holds id and that pack's index.
// An id stored in two packs maps to whichever pack was loaded first.
func (x *Index) Lookup(repoID [16]byte, id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) {
	x.mu.Lock()
	ri := x.repos[repoID]
	x.mu.Unlock()
	if ri == nil {
		return store.Pack{}, nil, false
	}
	ri.mu.Lock()
	defer ri.mu.Unlock()
	packID, ok := ri.byObj[id]
	if !ok {
		return store.Pack{}, nil, false
	}
	got := ri.packs[packID]
	return got.pack, got.idx, true
}

func (x *Index) repo(id [16]byte) *repoIndex {
	x.mu.Lock()
	defer x.mu.Unlock()
	ri := x.repos[id]
	if ri == nil {
		ri = &repoIndex{
			byObj:  map[[20]byte][16]byte{},
			packs:  map[[16]byte]loaded{},
			loaded: map[[16]byte]struct{}{},
		}
		x.repos[id] = ri
	}
	return ri
}

func (ri *repoIndex) add(p store.Pack, idx *idxfile.MemoryIndex) error {
	if _, ok := ri.loaded[p.ID]; ok {
		return nil
	}
	iter, err := idx.Entries()
	if err != nil {
		return err
	}
	defer iter.Close()
	for {
		entry, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		id := [20]byte(entry.Hash)
		if _, exists := ri.byObj[id]; !exists {
			ri.byObj[id] = p.ID
		}
	}
	ri.packs[p.ID] = loaded{pack: p, idx: idx}
	ri.loaded[p.ID] = struct{}{}
	return nil
}

// byteSource serves one downloaded index object.
type byteSource struct {
	b []byte
}

func (s byteSource) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if off < 0 || n < 0 || off+n > int64(len(s.b)) {
		return nil, errors.New("objindex: range outside index object")
	}
	out := make([]byte, n)
	copy(out, s.b[off:off+n])
	return out, nil
}
