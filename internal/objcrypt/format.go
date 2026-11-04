// Package objcrypt reads and writes the S3G1 encrypted object format.
// One format version exists. A header with any other version is rejected.
package objcrypt

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Magic                  = "S3G1"
	Version          uint8 = 1
	KeyIDCurrent     uint8 = 1
	DefaultChunkSize       = 4 << 20 // 4 MiB of plaintext per chunk
	MinChunkSize           = 4 << 10
	MaxChunkSize           = 16 << 20
	NonceSize              = 12
	TagSize                = 16
	ChunkOverhead          = NonceSize + TagSize         // 28
	WrappedDEKSize         = 32 + TagSize                // 48
	HeaderSize             = 4 + 1 + 1 + 4 + 12 + 2 + 48 // 72
)

// Kind identifies what an object stores. It is part of the authenticated data,
// so a pack opened as an index fails to decrypt.
type Kind uint8

const (
	KindPack   Kind = 1
	KindIdx    Kind = 2
	KindSnap   Kind = 3
	KindTokens Kind = 4
)

// Binding ties an object to the repository and object it belongs to.
// ObjectID is the pack uuid for a pack and its index, the snapshot's own uuid
// for a snapshot, and the token backup's own uuid for a token backup.
// RepoID is zero for a token backup.
type Binding struct {
	RepoID   [16]byte
	ObjectID [16]byte
	Kind     Kind
}

var (
	ErrBadMasterKey = errors.New("objcrypt: master key must be base64 of exactly 32 bytes")
	ErrBadHeader    = errors.New("objcrypt: bad header")
	ErrVersion      = errors.New("objcrypt: unsupported version")
	ErrUnknownKey   = errors.New("objcrypt: unknown key id")
	ErrBadLength    = errors.New("objcrypt: object length does not match the format")
	ErrAuth         = errors.New("objcrypt: authentication failed")
)

type headerView struct {
	keyID      uint8
	chunkSize  uint32
	wrappedLen uint16
}

// inspectHeader checks magic and version. Chunk size, key id, and the wrapped
// DEK length are checked by the caller in that order.
func inspectHeader(b []byte) (headerView, error) {
	if len(b) != HeaderSize || string(b[:4]) != Magic {
		return headerView{}, fmt.Errorf("objcrypt: magic: %w", ErrBadHeader)
	}
	if b[4] != Version {
		return headerView{}, fmt.Errorf("objcrypt: version %d: %w", b[4], ErrVersion)
	}
	return headerView{
		keyID:      b[5],
		chunkSize:  binary.LittleEndian.Uint32(b[6:10]),
		wrappedLen: binary.LittleEndian.Uint16(b[22:24]),
	}, nil
}

func putHeader(dst []byte, keyID uint8, chunkSize uint32, dekNonce, wrapped []byte) {
	copy(dst[0:4], Magic)
	dst[4] = Version
	dst[5] = keyID
	binary.LittleEndian.PutUint32(dst[6:10], chunkSize)
	copy(dst[10:22], dekNonce)
	binary.LittleEndian.PutUint16(dst[22:24], uint16(WrappedDEKSize))
	copy(dst[24:HeaderSize], wrapped)
}

// aadWrap authenticates the header prefix together with the binding.
// prefix is header[0:24], everything before the wrapped DEK.
func aadWrap(prefix []byte, b Binding) []byte {
	aad := make([]byte, 24+16+16+1)
	copy(aad, prefix)
	copy(aad[24:], b.RepoID[:])
	copy(aad[40:], b.ObjectID[:])
	aad[56] = byte(b.Kind)
	return aad
}

// aadChunk authenticates one chunk. last is 1 only for the final chunk.
func aadChunk(headerSum [32]byte, b Binding, index uint64, last bool) []byte {
	aad := make([]byte, 32+16+16+1+8+1)
	copy(aad, headerSum[:])
	copy(aad[32:], b.RepoID[:])
	copy(aad[48:], b.ObjectID[:])
	aad[64] = byte(b.Kind)
	binary.LittleEndian.PutUint64(aad[65:73], index)
	if last {
		aad[73] = 1
	}
	return aad
}

func headerSum(header []byte) [32]byte {
	return sha256.Sum256(header)
}

// deriveLayout recovers the plaintext size from the ciphertext length.
// An empty final chunk is valid only when the plaintext itself is empty.
// A file cut on a chunk boundary still has a valid length; the last-chunk
// flag, checked when that chunk is opened, rejects it.
func deriveLayout(objectSize int64, chunkSize int) (plain int64, chunks int, err error) {
	body := objectSize - HeaderSize
	if body < ChunkOverhead {
		return 0, 0, fmt.Errorf("objcrypt: body %d: %w", body, ErrBadLength)
	}
	stride := int64(chunkSize) + ChunkOverhead
	full := body / stride
	rem := body % stride
	switch {
	case rem == 0:
		return full * int64(chunkSize), int(full), nil
	case rem < ChunkOverhead:
		return 0, 0, fmt.Errorf("objcrypt: trailing %d bytes: %w", rem, ErrBadLength)
	case rem == ChunkOverhead && full > 0:
		return 0, 0, fmt.Errorf("objcrypt: empty trailing chunk: %w", ErrBadLength)
	default:
		return full*int64(chunkSize) + (rem - ChunkOverhead), int(full + 1), nil
	}
}

func chunkStride(chunkSize int) int64 {
	return int64(chunkSize) + ChunkOverhead
}

func chunkPlainLen(plainSize int64, chunkSize, chunks, i int) int64 {
	if i == chunks-1 {
		return plainSize - int64(i)*int64(chunkSize)
	}
	return int64(chunkSize)
}
