package packstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"

	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
)

var (
	ErrThinPack  = errors.New("thin pack")
	ErrTruncated = errors.New("truncated pack")
	ErrBadPack   = errors.New("bad pack")
	ErrTrailing  = errors.New("pack has bytes after the trailer")
)

// Parsed is a pack that passed the parser and ends where the body ends.
type Parsed struct {
	Index    *idxfile.MemoryIndex
	IdxBytes []byte
	Objects  map[[20]byte]plumbing.ObjectType
}

// Parse decrypts sp, builds its index, and returns a Pack over the same file.
// go-git v5.19.3 parses pack version 2 only. A version 3 header passes Spool
// and fails here as a bad pack. This piece does not add a version 3 parser.
func Parse(ctx context.Context, sp *Spooled, kr *objcrypt.Keyring, b objcrypt.Binding) (*Parsed, *Pack, error) {
	f, err := os.Open(sp.Path)
	if err != nil {
		return nil, nil, err
	}
	parsed, pack, err := parseOpen(ctx, f, sp, kr, b)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return parsed, pack, nil
}

func parseOpen(ctx context.Context, f *os.File, sp *Spooled, kr *objcrypt.Keyring, b objcrypt.Binding) (*Parsed, *Pack, error) {
	b.Kind = objcrypt.KindPack
	rd, err := objcrypt.Open(ctx, fileSource{f: f}, sp.Path, sp.Size, kr, b)
	if err != nil {
		return nil, nil, err
	}
	if rd.Size() != sp.PlainSize || rd.Size() < 32 {
		return nil, nil, ErrBadPack
	}
	section := io.NewSectionReader(rd, 0, rd.Size())
	scanner := packfile.NewScanner(section)
	var idxWriter idxfile.Writer
	types := &typeObserver{objects: map[[20]byte]plumbing.ObjectType{}}
	parser, err := packfile.NewParser(scanner, &idxWriter, types)
	if err != nil {
		return nil, nil, err
	}
	sum, err := parser.Parse()
	if err != nil {
		return nil, nil, classifyParse(err)
	}
	var tail [20]byte
	n, err := rd.ReadAt(tail[:], rd.Size()-20)
	if n != len(tail) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, nil, ErrTruncated
	}
	// Parse verified the trailer where it stopped. The body must end on that
	// same trailer, or bytes after a valid pack would be stored and copied
	// into every clone.
	if plumbing.Hash(tail) != sum {
		return nil, nil, ErrTrailing
	}
	index, err := idxWriter.Index()
	if err != nil {
		return nil, nil, ErrBadPack
	}
	var buf bytes.Buffer
	if _, err := idxfile.NewEncoder(&buf).Encode(index); err != nil {
		return nil, nil, ErrBadPack
	}
	file := &readOnlyFile{name: sp.Path, r: io.NewSectionReader(rd, 0, rd.Size())}
	pf := packfile.NewPackfile(index, nil, file, 0)
	return &Parsed{
		Index:    index,
		IdxBytes: buf.Bytes(),
		Objects:  types.objects,
	}, &Pack{file: f, pf: pf}, nil
}

func classifyParse(err error) error {
	if errors.Is(err, packfile.ErrReferenceDeltaNotFound) {
		return ErrThinPack
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return ErrBadPack
}

// typeObserver records the resolved type of each object.
// OnInflatedObjectContent is given a nil body while deltas are resolved, so
// the type is taken from the header callback that precedes it.
type typeObserver struct {
	current plumbing.ObjectType
	objects map[[20]byte]plumbing.ObjectType
}

func (o *typeObserver) OnHeader(uint32) error { return nil }

func (o *typeObserver) OnInflatedObjectHeader(t plumbing.ObjectType, objSize int64, pos int64) error {
	o.current = t
	return nil
}

func (o *typeObserver) OnInflatedObjectContent(h plumbing.Hash, pos int64, crc uint32, content []byte) error {
	o.objects[h] = o.current
	return nil
}

func (o *typeObserver) OnFooter(plumbing.Hash) error { return nil }
