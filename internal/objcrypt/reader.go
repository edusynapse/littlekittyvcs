package objcrypt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
)

// RangeSource returns exactly n bytes starting at off.
// *blob.Mem and *blob.S3 satisfy it directly.
type RangeSource interface {
	GetRange(ctx context.Context, key string, off, n int64) ([]byte, error)
}

// Reader decrypts an S3G1 object through ranged reads.
// A Reader is not safe for concurrent use.
//
// recent keeps the two most recently decrypted chunks. A forward scan in
// small reads, including a read that crosses a chunk boundary, then fetches
// each chunk once. cache is the process-wide cache. It is nil when the
// reader was opened with Open, or with a nil ChunkCache.
type Reader struct {
	ctx        context.Context
	src        RangeSource
	key        string
	binding    Binding
	aead       cipher.AEAD
	headerSum  [32]byte
	objectSize int64
	chunkSize  int
	chunks     int
	plainSize  int64
	recent     [2]cachedChunk
	cache      *ChunkCache
	insert     bool
	rangeReads int64
	cacheHits  int64
}

type cachedChunk struct {
	index int
	plain []byte
	ok    bool
}

// Open reads the 72-byte header, unwraps the data key, and computes the
// plaintext size from objectSize. objectSize is the ciphertext length.
// Open does not issue a HEAD request. It reads the header. When the computed
// plaintext size is 0, it also fetches and authenticates chunk 0. That chunk
// is 28 bytes. A real empty object sealed it as the last chunk. A longer
// object cut down to the header plus those 28 bytes sealed it as a non-final
// chunk, so authentication fails here instead of reporting an empty file.
func Open(ctx context.Context, src RangeSource, key string, objectSize int64, kr *Keyring, b Binding) (*Reader, error) {
	return open(ctx, src, key, objectSize, kr, b, nil, false)
}

func open(ctx context.Context, src RangeSource, key string, objectSize int64, kr *Keyring, b Binding, cache *ChunkCache, insert bool) (*Reader, error) {
	if objectSize < HeaderSize {
		return nil, fmt.Errorf("objcrypt: object size %d: %w", objectSize, ErrBadLength)
	}
	hdr, err := src.GetRange(ctx, key, 0, HeaderSize)
	if err != nil {
		return nil, err
	}
	if len(hdr) != HeaderSize {
		return nil, fmt.Errorf("objcrypt: header length %d: %w", len(hdr), ErrBadHeader)
	}
	view, err := inspectHeader(hdr)
	if err != nil {
		return nil, err
	}
	master, err := kr.aead(view.keyID)
	if err != nil {
		return nil, err
	}
	if view.chunkSize < MinChunkSize || view.chunkSize > MaxChunkSize {
		return nil, fmt.Errorf("objcrypt: chunk size: %w", ErrBadHeader)
	}
	if view.wrappedLen != WrappedDEKSize {
		return nil, fmt.Errorf("objcrypt: wrapped dek length: %w", ErrBadHeader)
	}

	dek, err := master.Open(nil, hdr[10:22], hdr[24:HeaderSize], aadWrap(hdr[:24], b))
	if err != nil || len(dek) != 32 {
		return nil, fmt.Errorf("objcrypt: unwrap dek: %w", ErrAuth)
	}
	block, err := aes.NewCipher(dek)
	for i := range dek {
		dek[i] = 0
	}
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	plain, chunks, err := deriveLayout(objectSize, int(view.chunkSize))
	if err != nil {
		return nil, err
	}

	rd := &Reader{
		ctx:        ctx,
		src:        src,
		key:        key,
		binding:    b,
		aead:       aead,
		headerSum:  headerSum(hdr),
		objectSize: objectSize,
		chunkSize:  int(view.chunkSize),
		chunks:     chunks,
		plainSize:  plain,
		cache:      cache,
		insert:     cache != nil && insert,
		rangeReads: 1,
	}
	if plain == 0 {
		if _, err := rd.fetch(0); err != nil {
			return nil, err
		}
	}
	return rd, nil
}

// RangeReads is the header read plus each chunk read that missed the cache.
func (r *Reader) RangeReads() int64 {
	if r == nil {
		return 0
	}
	return r.rangeReads
}

// CacheHits is the number of chunks served from the process-wide cache.
func (r *Reader) CacheHits() int64 {
	if r == nil {
		return 0
	}
	return r.cacheHits
}

// Size returns the plaintext size in bytes.
func (r *Reader) Size() int64 {
	return r.plainSize
}

// ReadAt implements io.ReaderAt. A negative offset is an error.
// A read that reaches past Size returns the bytes that exist and io.EOF.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("objcrypt: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.plainSize {
		return 0, io.EOF
	}
	wantEOF := false
	if off+int64(len(p)) > r.plainSize {
		p = p[:r.plainSize-off]
		wantEOF = true
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		idx := int(pos / int64(r.chunkSize))
		chunkOff := int(pos % int64(r.chunkSize))
		plain, err := r.chunk(idx)
		if err != nil {
			if n > 0 {
				return n, err
			}
			return 0, err
		}
		if chunkOff > len(plain) {
			return n, fmt.Errorf("objcrypt: chunk %d: %w", idx, ErrAuth)
		}
		copied := copy(p[n:], plain[chunkOff:])
		if copied == 0 {
			return n, fmt.Errorf("objcrypt: chunk %d: %w", idx, ErrAuth)
		}
		n += copied
	}
	if wantEOF {
		return n, io.EOF
	}
	return n, nil
}

func (r *Reader) chunk(i int) ([]byte, error) {
	if plain, ok := r.lookup(i); ok {
		return plain, nil
	}
	plain, err := r.fetch(i)
	if err != nil {
		return nil, err
	}
	r.remember(i, plain)
	return plain, nil
}

func (r *Reader) lookup(i int) ([]byte, bool) {
	if r.recent[0].ok && r.recent[0].index == i {
		return r.recent[0].plain, true
	}
	if r.recent[1].ok && r.recent[1].index == i {
		r.recent[0], r.recent[1] = r.recent[1], r.recent[0]
		return r.recent[0].plain, true
	}
	return nil, false
}

func (r *Reader) remember(i int, plain []byte) {
	r.recent[1] = r.recent[0]
	r.recent[0] = cachedChunk{index: i, plain: plain, ok: true}
}

func (r *Reader) chunkKey(i int) chunkKey {
	return chunkKey{
		repo:   r.binding.RepoID,
		object: r.binding.ObjectID,
		kind:   r.binding.Kind,
		header: r.headerSum,
		size:   r.objectSize,
		index:  i,
	}
}

func (r *Reader) fetch(i int) ([]byte, error) {
	if i < 0 || i >= r.chunks {
		return nil, fmt.Errorf("objcrypt: chunk %d: %w", i, ErrAuth)
	}
	if r.cache != nil {
		if plain, ok := r.cache.get(r.chunkKey(i)); ok {
			r.cacheHits++
			return plain, nil
		}
	}
	plainLen := chunkPlainLen(r.plainSize, r.chunkSize, r.chunks, i)
	n := plainLen + ChunkOverhead
	off := HeaderSize + int64(i)*chunkStride(r.chunkSize)
	raw, err := r.src.GetRange(r.ctx, r.key, off, n)
	if err != nil {
		return nil, err
	}
	r.rangeReads++
	if int64(len(raw)) != n || len(raw) < ChunkOverhead {
		return nil, fmt.Errorf("objcrypt: chunk %d length: %w", i, ErrAuth)
	}
	last := i == r.chunks-1
	plain, err := r.aead.Open(nil, raw[:NonceSize], raw[NonceSize:], aadChunk(r.headerSum, r.binding, uint64(i), last))
	if err != nil {
		return nil, fmt.Errorf("objcrypt: chunk %d: %w", i, ErrAuth)
	}
	if r.cache != nil && r.insert {
		r.cache.put(r.chunkKey(i), plain)
	}
	return plain, nil
}
