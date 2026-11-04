package objcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// WriterOptions controls encryption. The zero ChunkSize selects DefaultChunkSize.
// Any other value must sit in [MinChunkSize, MaxChunkSize]. Production callers
// pass the zero value. Tests pass a smaller size so a pack spans many chunks.
type WriterOptions struct {
	ChunkSize int
}

// Writer encrypts a plaintext stream into the S3G1 format.
// Close seals the final chunk and must be called. A Writer is not safe for
// concurrent use.
type Writer struct {
	dst        io.Writer
	binding    Binding
	aead       cipher.AEAD
	headerSum  [32]byte
	chunkSize  int
	buf        []byte
	n          int
	chunkIndex uint64
	written    int64
	closed     bool
}

var errWriterClosed = errors.New("objcrypt: writer is closed")

// NewWriter generates a data key, writes the 72-byte header to dst, and returns.
func NewWriter(dst io.Writer, kr *Keyring, b Binding, opt WriterOptions) (*Writer, error) {
	chunkSize, err := normalizeChunkSize(opt.ChunkSize)
	if err != nil {
		return nil, err
	}
	keyID, master, err := kr.currentID()
	if err != nil {
		return nil, err
	}

	var dek [32]byte
	if _, err := rand.Read(dek[:]); err != nil {
		return nil, err
	}
	var dekNonce [NonceSize]byte
	if _, err := rand.Read(dekNonce[:]); err != nil {
		return nil, err
	}

	header := make([]byte, HeaderSize)
	putHeader(header, keyID, uint32(chunkSize), dekNonce[:], make([]byte, WrappedDEKSize))
	wrapped := master.Seal(nil, dekNonce[:], dek[:], aadWrap(header[:24], b))
	if len(wrapped) != WrappedDEKSize {
		return nil, fmt.Errorf("objcrypt: wrapped dek length %d: %w", len(wrapped), ErrBadHeader)
	}
	copy(header[24:], wrapped)

	block, err := aes.NewCipher(dek[:])
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

	if err := writeFull(dst, header); err != nil {
		return nil, err
	}

	return &Writer{
		dst:       dst,
		binding:   b,
		aead:      aead,
		headerSum: headerSum(header),
		chunkSize: chunkSize,
		buf:       make([]byte, chunkSize),
		written:   HeaderSize,
	}, nil
}

func normalizeChunkSize(n int) (int, error) {
	if n == 0 {
		return DefaultChunkSize, nil
	}
	if n < MinChunkSize || n > MaxChunkSize {
		return 0, fmt.Errorf("objcrypt: chunk size %d: %w", n, ErrBadHeader)
	}
	return n, nil
}

// Write buffers plaintext. A full chunk is sealed with last=0 only when
// another byte arrives, so Close can seal the final chunk with last=1.
// An exact multiple of the chunk size does not grow an extra empty chunk.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errWriterClosed
	}
	written := 0
	for len(p) > 0 {
		if w.n == w.chunkSize {
			if err := w.seal(false); err != nil {
				return written, err
			}
		}
		take := w.chunkSize - w.n
		if take > len(p) {
			take = len(p)
		}
		copy(w.buf[w.n:w.n+take], p[:take])
		w.n += take
		p = p[take:]
		written += take
	}
	return written, nil
}

// Close seals whatever is buffered, including an empty buffer when nothing
// was written. A second Close returns an error.
func (w *Writer) Close() error {
	if w.closed {
		return errWriterClosed
	}
	w.closed = true
	return w.seal(true)
}

// CiphertextSize is the number of bytes written to dst so far.
// It is final after Close.
func (w *Writer) CiphertextSize() int64 {
	return w.written
}

func (w *Writer) seal(last bool) error {
	var nonce [NonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	sealed := w.aead.Seal(nil, nonce[:], w.buf[:w.n], aadChunk(w.headerSum, w.binding, w.chunkIndex, last))
	if err := writeFull(w.dst, nonce[:]); err != nil {
		return err
	}
	w.written += NonceSize
	if err := writeFull(w.dst, sealed); err != nil {
		return err
	}
	w.written += int64(len(sealed))
	w.chunkIndex++
	w.n = 0
	return nil
}

func writeFull(dst io.Writer, p []byte) error {
	n, err := dst.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}
