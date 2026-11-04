package packstore

import (
	"context"
	"io"
	"os"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"

	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
)

// Pack reads objects from one pack, spooled or stored, through the encrypted reader.
// It is not safe for concurrent use. One pack is opened per request.
type Pack struct {
	file *os.File
	pf   *packfile.Packfile
	rd   *objcrypt.Reader
}

// OpenStored opens a pack object in the bucket. idx is the decoded index.
// b.Kind is set to KindPack. size is the ciphertext length.
// chunks may be nil. objCache is shared by every pack a request opens.
// A nil objCache gives this pack its own go-git cache.
func OpenStored(ctx context.Context, src objcrypt.RangeSource, key string, size int64, kr *objcrypt.Keyring, b objcrypt.Binding, idx *idxfile.MemoryIndex, chunks *objcrypt.ChunkCache, objCache cache.Object) (*Pack, error) {
	b.Kind = objcrypt.KindPack
	rd, err := chunks.Open(ctx, src, key, size, kr, b, true)
	if err != nil {
		return nil, err
	}
	file := &readOnlyFile{name: key, r: io.NewSectionReader(rd, 0, rd.Size())}
	var pf *packfile.Packfile
	if objCache == nil {
		pf = packfile.NewPackfile(idx, nil, file, 0)
	} else {
		pf = packfile.NewPackfileWithCache(idx, nil, file, objCache, 0)
	}
	return &Pack{pf: pf, rd: rd}, nil
}

// RangeReads is the encrypted reader's header and chunk reads. A spooled pack is 0.
func (p *Pack) RangeReads() int64 {
	if p == nil || p.rd == nil {
		return 0
	}
	return p.rd.RangeReads()
}

// CacheHits is the encrypted reader's process-cache hits. A spooled pack is 0.
func (p *Pack) CacheHits() int64 {
	if p == nil || p.rd == nil {
		return 0
	}
	return p.rd.CacheHits()
}

// Get returns the object type and its content. An absent id is plumbing.ErrObjectNotFound.
func (p *Pack) Get(id [20]byte) (plumbing.ObjectType, []byte, error) {
	obj, err := p.pf.Get(plumbing.Hash(id))
	if err != nil {
		return 0, nil, err
	}
	r, err := obj.Reader()
	if err != nil {
		return 0, nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, nil, err
	}
	return obj.Type(), b, nil
}

// Close closes the spool file, when this pack has one.
func (p *Pack) Close() error {
	if p == nil || p.file == nil {
		return nil
	}
	err := p.file.Close()
	p.file = nil
	return err
}
