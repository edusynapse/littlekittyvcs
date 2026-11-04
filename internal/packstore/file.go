package packstore

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/go-git/go-billy/v5"
)

// fileSource is a RangeSource over one spool file. The key is ignored.
type fileSource struct {
	f *os.File
}

func (s fileSource) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if off < 0 || n < 0 {
		return nil, errors.New("packstore: invalid range")
	}
	buf := make([]byte, n)
	got, err := s.f.ReadAt(buf, off)
	if int64(got) != n {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

var errReadOnly = errors.New("packstore: read-only file")

// readOnlyFile is a billy.File over a SectionReader. Name returns the object key.
type readOnlyFile struct {
	name string
	r    *io.SectionReader
}

var _ billy.File = (*readOnlyFile)(nil)

func (f *readOnlyFile) Name() string                                 { return f.name }
func (f *readOnlyFile) Read(p []byte) (int, error)                   { return f.r.Read(p) }
func (f *readOnlyFile) ReadAt(p []byte, off int64) (int, error)      { return f.r.ReadAt(p, off) }
func (f *readOnlyFile) Seek(off int64, whence int) (int64, error)    { return f.r.Seek(off, whence) }
func (f *readOnlyFile) Close() error                                 { return nil }
func (f *readOnlyFile) Write(p []byte) (int, error)                  { return 0, errReadOnly }
func (f *readOnlyFile) Truncate(size int64) error                    { return errReadOnly }
func (f *readOnlyFile) Lock() error                                  { return errReadOnly }
func (f *readOnlyFile) Unlock() error                                { return errReadOnly }
