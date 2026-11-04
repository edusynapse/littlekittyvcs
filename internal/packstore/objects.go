package packstore

import (
	"context"
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// Lookup finds the stored pack that holds id, and that pack's index.
type Lookup func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool)

// Objects reads objects of one repository from an optional fresh pack and
// from stored packs found through lookup. Each stored pack is opened once.
// Not safe for concurrent use. Close does not close fresh.
type Objects struct {
	ctx      context.Context
	src      objcrypt.RangeSource
	kr       *objcrypt.Keyring
	repoID   [16]byte
	lookup   Lookup
	fresh    *Pack
	open     map[[16]byte]*Pack
	chunks   *objcrypt.ChunkCache
	objCache cache.Object
}

// NewObjects reads repoID. fresh may be nil. lookup may be nil when every
// object the caller needs is in fresh. chunks may be nil.
// Every stored pack this request opens shares one go-git object cache of
// cache.DefaultMaxSize.
func NewObjects(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, repoID [16]byte, lookup Lookup, fresh *Pack, chunks *objcrypt.ChunkCache) *Objects {
	return newObjects(ctx, src, kr, repoID, lookup, fresh, chunks, cache.NewObjectLRU(cache.DefaultMaxSize))
}

func newObjects(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, repoID [16]byte, lookup Lookup, fresh *Pack, chunks *objcrypt.ChunkCache, objCache cache.Object) *Objects {
	return &Objects{
		ctx: ctx, src: src, kr: kr, repoID: repoID, lookup: lookup, fresh: fresh,
		open: map[[16]byte]*Pack{}, chunks: chunks, objCache: objCache,
	}
}

// Get returns the object type and its content. An absent id is plumbing.ErrObjectNotFound.
func (o *Objects) Get(id [20]byte) (plumbing.ObjectType, []byte, error) {
	if o.fresh != nil {
		typ, content, err := o.fresh.Get(id)
		if err == nil {
			return typ, content, nil
		}
		if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return 0, nil, err
		}
	}
	if o.lookup == nil {
		return 0, nil, plumbing.ErrObjectNotFound
	}
	row, idx, ok := o.lookup(id)
	if !ok {
		return 0, nil, plumbing.ErrObjectNotFound
	}
	pk := o.open[row.ID]
	if pk == nil {
		opened, err := OpenStored(o.ctx, o.src, row.PackKey, row.Bytes, o.kr, objcrypt.Binding{
			RepoID: o.repoID, ObjectID: row.ID, Kind: objcrypt.KindPack,
		}, idx, o.chunks, o.objCache)
		if err != nil {
			return 0, nil, err
		}
		o.open[row.ID] = opened
		pk = opened
	}
	return pk.Get(id)
}

// OpenedPacks is how many stored packs this request has opened.
func (o *Objects) OpenedPacks() int {
	if o == nil {
		return 0
	}
	return len(o.open)
}

// RangeReads sums the stored packs this request opened.
func (o *Objects) RangeReads() int64 {
	if o == nil {
		return 0
	}
	var n int64
	for _, pk := range o.open {
		n += pk.RangeReads()
	}
	return n
}

// CacheHits sums the stored packs this request opened.
func (o *Objects) CacheHits() int64 {
	if o == nil {
		return 0
	}
	var n int64
	for _, pk := range o.open {
		n += pk.CacheHits()
	}
	return n
}

// Close closes stored packs this reader opened. It does not close fresh.
func (o *Objects) Close() {
	if o == nil {
		return
	}
	for id, pk := range o.open {
		_ = pk.Close()
		delete(o.open, id)
	}
}

// Peel returns nil when id is not an annotated tag. Otherwise it follows
// tag targets until an object that is not a tag, and returns that id.
// A missing target is plumbing.ErrObjectNotFound. The type field on the tag
// is not trusted: the target is always read.
func Peel(o *Objects, id [20]byte) (*[20]byte, error) {
	typ, content, err := o.Get(id)
	if err != nil {
		return nil, err
	}
	if typ != plumbing.TagObject {
		return nil, nil
	}
	seen := map[[20]byte]struct{}{id: {}}
	for {
		target, err := tagTarget(content)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[target]; ok {
			return nil, errors.New("packstore: tag cycle")
		}
		seen[target] = struct{}{}
		typ, content, err = o.Get(target)
		if err != nil {
			return nil, err
		}
		if typ != plumbing.TagObject {
			out := target
			return &out, nil
		}
	}
}

func tagTarget(content []byte) ([20]byte, error) {
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.TagObject)
	if _, err := mo.Write(content); err != nil {
		return [20]byte{}, err
	}
	var tag object.Tag
	if err := tag.Decode(mo); err != nil {
		return [20]byte{}, err
	}
	return tag.Target, nil
}
