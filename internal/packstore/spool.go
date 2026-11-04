package packstore

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"os"

	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
)

// MaxPushBytes is the largest pack accepted, in plaintext bytes. App Platform
// gives at most 2 GiB of ephemeral disk; 1.5 GiB leaves room for the index and
// the process.
const MaxPushBytes = 1536 << 20

// maxPushBytes is the limit Spool enforces. Tests set a smaller value.
var maxPushBytes int64 = MaxPushBytes

const spoolChunk = 32 << 10

var (
	ErrTooLarge   = errors.New("pack exceeds the size limit")
	ErrTooSmall   = errors.New("pack is too small")
	ErrBadHeader  = errors.New("bad pack header")
	ErrBadVersion = errors.New("unsupported pack version")
	ErrChecksum   = errors.New("bad pack checksum")
)

// Spooled is an encrypted pack on disk. The caller removes Path.
type Spooled struct {
	Path        string
	Size        int64 // ciphertext bytes
	PlainSize   int64
	ObjectCount uint32
}

// Spool copies body into an encrypted temp file and checks the pack framing.
// The file is ciphertext from the first byte. b.Kind is set to KindPack.
func Spool(ctx context.Context, body io.Reader, dir string, kr *objcrypt.Keyring, b objcrypt.Binding) (*Spooled, error) {
	f, err := os.CreateTemp(dir, "lkv-push-*.spool")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	ok := false
	defer func() {
		if ok {
			return
		}
		_ = f.Close()
		_ = os.Remove(path)
	}()

	b.Kind = objcrypt.KindPack
	w, err := objcrypt.NewWriter(f, kr, b, objcrypt.WriterOptions{})
	if err != nil {
		return nil, err
	}
	sum := newStreamSum()
	buf := make([]byte, spoolChunk)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, rerr := body.Read(buf)
		if n > 0 {
			if sum.plain+int64(n) > maxPushBytes {
				return nil, ErrTooLarge
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return nil, err
			}
			sum.add(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if sum.plain < 32 {
		return nil, ErrTooSmall
	}
	if sum.hdrN < 12 || string(sum.hdr[:4]) != "PACK" {
		return nil, ErrBadHeader
	}
	version := binary.BigEndian.Uint32(sum.hdr[4:8])
	if version != 2 && version != 3 {
		return nil, ErrBadVersion
	}
	if sum.n != 20 || !sum.matches() {
		return nil, ErrChecksum
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	size := w.CiphertextSize()
	if err := f.Close(); err != nil {
		return nil, err
	}
	ok = true
	return &Spooled{
		Path:        path,
		Size:        size,
		PlainSize:   sum.plain,
		ObjectCount: binary.BigEndian.Uint32(sum.hdr[8:12]),
	}, nil
}

// streamSum hashes every plaintext byte except a 20-byte tail.
type streamSum struct {
	h     hash.Hash
	tail  [20]byte
	n     int
	hdr   [12]byte
	hdrN  int
	plain int64
}

func newStreamSum() *streamSum {
	return &streamSum{h: sha1.New()}
}

func (s *streamSum) add(p []byte) {
	if s.hdrN < len(s.hdr) {
		c := copy(s.hdr[s.hdrN:], p)
		s.hdrN += c
	}
	s.plain += int64(len(p))
	if s.n < len(s.tail) {
		take := len(s.tail) - s.n
		if take > len(p) {
			take = len(p)
		}
		copy(s.tail[s.n:], p[:take])
		s.n += take
		p = p[take:]
	}
	if len(p) == 0 {
		return
	}
	if len(p) >= len(s.tail) {
		_, _ = s.h.Write(s.tail[:s.n])
		_, _ = s.h.Write(p[:len(p)-len(s.tail)])
		copy(s.tail[:], p[len(p)-len(s.tail):])
		s.n = len(s.tail)
		return
	}
	_, _ = s.h.Write(s.tail[:len(p)])
	copy(s.tail[:], s.tail[len(p):s.n])
	copy(s.tail[s.n-len(p):], p)
}

func (s *streamSum) matches() bool {
	sum := s.h.Sum(nil)
	if len(sum) != s.n {
		return false
	}
	for i := range sum {
		if sum[i] != s.tail[i] {
			return false
		}
	}
	return true
}
