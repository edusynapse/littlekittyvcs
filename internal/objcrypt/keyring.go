package objcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
)

// ParseMasterKey decodes a standard padded base64 master key.
// Surrounding ASCII whitespace is removed so a trailing newline from an env
// file is accepted. The decoded value must be exactly 32 bytes.
func ParseMasterKey(s string) ([32]byte, error) {
	s = trimASCIISpace(s)
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return [32]byte{}, ErrBadMasterKey
	}
	var key [32]byte
	copy(key[:], raw)
	return key, nil
}

func trimASCIISpace(s string) string {
	i, j := 0, len(s)
	for i < j && isASCIISpace(s[i]) {
		i++
	}
	for j > i && isASCIISpace(s[j-1]) {
		j--
	}
	return s[i:j]
}

func isASCIISpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

// Keyring holds the master keys that wrap per-object data keys.
// The writer always wraps with the current key. The reader uses the key id
// stored in the header.
type Keyring struct {
	current uint8
	keys    map[uint8]cipher.AEAD
}

// NewKeyring registers master as key id 1, the current key.
func NewKeyring(master [32]byte) (*Keyring, error) {
	block, err := aes.NewCipher(master[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keyring{
		current: KeyIDCurrent,
		keys:    map[uint8]cipher.AEAD{KeyIDCurrent: aead},
	}, nil
}

func (k *Keyring) currentID() (uint8, cipher.AEAD, error) {
	if k == nil {
		return 0, nil, fmt.Errorf("objcrypt: nil keyring: %w", ErrUnknownKey)
	}
	aead, ok := k.keys[k.current]
	if !ok {
		return 0, nil, fmt.Errorf("objcrypt: key id %d: %w", k.current, ErrUnknownKey)
	}
	return k.current, aead, nil
}

func (k *Keyring) aead(id uint8) (cipher.AEAD, error) {
	if k == nil {
		return nil, fmt.Errorf("objcrypt: nil keyring: %w", ErrUnknownKey)
	}
	aead, ok := k.keys[id]
	if !ok {
		return nil, fmt.Errorf("objcrypt: key id %d: %w", id, ErrUnknownKey)
	}
	return aead, nil
}
