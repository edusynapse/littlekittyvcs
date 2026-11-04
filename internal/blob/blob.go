// Package blob stores ciphertext in an S3-compatible bucket.
// It does not read environment variables. The caller passes the configuration.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrNotFound is returned by GetRange, Head, and Delete when the key does not exist.
var ErrNotFound = errors.New("blob: not found")

var (
	_ Store = (*Mem)(nil)
	_ Store = (*S3)(nil)
)

// Info describes one stored object.
type Info struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is the bucket client. GetRange returns exactly n bytes or an error.
// A short body is an error. A missing key is ErrNotFound from GetRange, Head, and Delete.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	GetRange(ctx context.Context, key string, off, n int64) ([]byte, error)
	Head(ctx context.Context, key string) (Info, error)
	List(ctx context.Context, prefix string, fn func(Info) error) error
	Delete(ctx context.Context, key string) error
}

func readN(r io.Reader, n int64) ([]byte, error) {
	if n < 0 {
		return nil, errors.New("blob: negative size")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("blob: short body: %w", err)
	}
	return buf, nil
}

func ensureEOF(r io.Reader) error {
	var extra [1]byte
	m, err := r.Read(extra[:])
	if m > 0 {
		return errors.New("blob: body longer than size")
	}
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// readExactly reads n bytes and requires the reader to end there.
func readExactly(r io.Reader, n int64) ([]byte, error) {
	buf, err := readN(r, n)
	if err != nil {
		return nil, err
	}
	if err := ensureEOF(r); err != nil {
		return nil, err
	}
	return buf, nil
}
