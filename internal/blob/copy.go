package blob

import (
	"context"
	"fmt"
	"io"
)

// Copy streams key from src into dst as one object of size bytes.
// It reads src with GetRange in pieces of PartSize, so one piece is in memory
// at a time. The buckets are not asked to copy the object themselves: they may
// be different providers.
func Copy(ctx context.Context, src, dst Store, key string, size int64) error {
	if size <= 0 {
		return fmt.Errorf("blob: size %d must be positive", size)
	}
	return dst.Put(ctx, key, &partReader{ctx: ctx, src: src, key: key, size: size}, size)
}

// partReader yields size bytes, fetching the next PartSize range when the
// current piece is exhausted. buf is nil between pieces.
type partReader struct {
	ctx  context.Context
	src  Store
	key  string
	size int64
	off  int64
	buf  []byte
	pos  int
}

func (r *partReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= len(r.buf) {
		if r.off >= r.size {
			return 0, io.EOF
		}
		n := int64(PartSize)
		if remain := r.size - r.off; remain < n {
			n = remain
		}
		buf, err := r.src.GetRange(r.ctx, r.key, r.off, n)
		if err != nil {
			return 0, err
		}
		if int64(len(buf)) != n {
			return 0, fmt.Errorf("blob: short range")
		}
		r.buf = buf
		r.pos = 0
		r.off += n
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	if r.pos >= len(r.buf) {
		r.buf = nil
	}
	return n, nil
}
