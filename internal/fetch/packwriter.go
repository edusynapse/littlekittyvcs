package fetch

import (
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/edusynapse/littlekittyvcs/internal/packstore"
)

// writeGenerated writes one pack of whole objects, zlib-compressed, with no
// deltas. An empty id list is a valid empty pack. Every Write to dst is at
// most sidebandDataMax bytes. One object's content is held at a time.
func writeGenerated(dst io.Writer, objs *packstore.Objects, ids [][20]byte) error {
	if len(ids) > math.MaxUint32 {
		return errors.New("fetch: object count does not fit a pack header")
	}
	limit := &limitWriter{dst: dst, max: sidebandDataMax}
	h := sha1.New()
	mw := io.MultiWriter(limit, h)
	var hdr [12]byte
	copy(hdr[:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(ids)))
	if _, err := mw.Write(hdr[:]); err != nil {
		return err
	}
	for _, id := range ids {
		typ, content, err := objs.Get(id)
		if err != nil {
			return err
		}
		n, err := packType(typ)
		if err != nil {
			return err
		}
		if err := writeObjectHeader(mw, n, len(content)); err != nil {
			return err
		}
		zw := zlib.NewWriter(mw)
		if _, err := zw.Write(content); err != nil {
			_ = zw.Close()
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
	}
	_, err := limit.Write(h.Sum(nil))
	return err
}

func packType(typ plumbing.ObjectType) (int, error) {
	switch typ {
	case plumbing.CommitObject:
		return 1, nil
	case plumbing.TreeObject:
		return 2, nil
	case plumbing.BlobObject:
		return 3, nil
	case plumbing.TagObject:
		return 4, nil
	default:
		return 0, errors.New("fetch: object type")
	}
}

// writeObjectHeader is git's pack type-and-size varint. The first byte holds
// the type and 4 size bits. Later bytes hold 7 size bits, with the high bit
// set on every byte but the last.
func writeObjectHeader(w io.Writer, typ, size int) error {
	header := make([]byte, 0, 8)
	c := (typ << 4) | (size & 15)
	size >>= 4
	for size > 0 {
		header = append(header, byte(c|0x80))
		c = size & 127
		size >>= 7
	}
	header = append(header, byte(c))
	_, err := w.Write(header)
	return err
}

// limitWriter splits every Write so dst never sees more than max bytes.
type limitWriter struct {
	dst io.Writer
	max int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > w.max {
			chunk = chunk[:w.max]
		}
		nw, err := w.dst.Write(chunk)
		n += nw
		if err != nil {
			return n, err
		}
		if nw == 0 {
			return n, io.ErrShortWrite
		}
		p = p[nw:]
	}
	return n, nil
}
