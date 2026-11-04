package store

import (
	"encoding/hex"
	"errors"
)

// FormatID renders id as a lowercase UUID.
func FormatID(id [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], id[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], id[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], id[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], id[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], id[10:16])
	return string(b[:])
}

// ParseID accepts the UUID text form in either case.
func ParseID(s string) ([16]byte, error) {
	var id [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return id, errors.New("store: invalid id")
	}
	raw := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	buf, err := hex.DecodeString(raw)
	if err != nil || len(buf) != 16 {
		return id, errors.New("store: invalid id")
	}
	copy(id[:], buf)
	return id, nil
}
