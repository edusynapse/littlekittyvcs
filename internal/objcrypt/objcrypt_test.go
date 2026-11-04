package objcrypt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
)

const testChunk = 4096

func testMaster() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = 0x11
	}
	return k
}

func testBinding() Binding {
	var b Binding
	for i := range b.RepoID {
		b.RepoID[i] = byte(i + 1)
		b.ObjectID[i] = byte(0x80 + i)
	}
	b.Kind = KindPack
	return b
}

func testRand(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}

func randomPlain(rng *rand.Rand, n int) []byte {
	buf := make([]byte, n)
	if n > 0 {
		rng.Read(buf)
	}
	return buf
}

func specCipherBytes(plainLen, chunkSize int) int64 {
	if plainLen == 0 {
		return HeaderSize + ChunkOverhead
	}
	full := plainLen / chunkSize
	rem := plainLen % chunkSize
	total := int64(HeaderSize)
	total += int64(full) * int64(chunkSize+ChunkOverhead)
	if rem != 0 {
		total += int64(rem + ChunkOverhead)
	}
	return total
}

func encrypt(t *testing.T, plain []byte, chunk int, kr *Keyring, b Binding) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, kr, b, WriterOptions{ChunkSize: chunk})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	n, err := w.Write(plain)
	if err != nil || n != len(plain) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if w.CiphertextSize() != int64(buf.Len()) {
		t.Fatalf("CiphertextSize %d, buffer %d", w.CiphertextSize(), buf.Len())
	}
	if w.CiphertextSize() != specCipherBytes(len(plain), chunk) {
		t.Fatalf("CiphertextSize %d, formula %d", w.CiphertextSize(), specCipherBytes(len(plain), chunk))
	}
	return buf.Bytes()
}

func store(t *testing.T, raw []byte) *blob.Mem {
	t.Helper()
	m := blob.NewMem()
	if err := m.Put(context.Background(), "obj", bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	return m
}

func openObj(t *testing.T, m *blob.Mem, kr *Keyring, b Binding) *Reader {
	t.Helper()
	rawLen := int64(len(m.Raw("obj")))
	rd, err := Open(context.Background(), m, "obj", rawLen, kr, b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return rd
}

func readAll(t *testing.T, rd *Reader) []byte {
	t.Helper()
	if rd.Size() == 0 {
		n, err := rd.ReadAt([]byte{0}, 0)
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("empty ReadAt n=%d err=%v", n, err)
		}
		return []byte{}
	}
	got := make([]byte, rd.Size())
	n, err := rd.ReadAt(got, 0)
	if err != nil || int64(n) != rd.Size() {
		t.Fatalf("ReadAt n=%d size=%d err=%v", n, rd.Size(), err)
	}
	return got
}

func TestRoundTripEdges(t *testing.T) {
	lengths := []int{0, 1, 4095, 4096, 4097, 3 * 4096, 3*4096 + 5}
	rng := testRand(20261103)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	for _, n := range lengths {
		plain := randomPlain(rng, n)
		raw := encrypt(t, plain, testChunk, kr, b)
		m := store(t, raw)
		rd := openObj(t, m, kr, b)
		if rd.Size() != int64(len(plain)) {
			t.Fatalf("len %d: Size %d", n, rd.Size())
		}
		got := readAll(t, rd)
		if !bytes.Equal(got, plain) {
			t.Fatalf("len %d: plaintext mismatch", n)
		}
	}
}

func TestReadChunk2Only(t *testing.T) {
	const chunks = 5
	plain := randomPlain(testRand(2), chunks*testChunk)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	raw := encrypt(t, plain, testChunk, kr, b)
	m := store(t, raw)
	m.ResetCalls()
	rd := openObj(t, m, kr, b)
	off := int64(2*testChunk + testChunk/2)
	buf := make([]byte, 100)
	n, err := rd.ReadAt(buf, off)
	if err != nil || n != len(buf) {
		t.Fatalf("ReadAt n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf, plain[off:off+100]) {
		t.Fatal("chunk 2 bytes mismatch")
	}
	calls := m.RangeCalls()
	wantChunk := blob.RangeCall{
		Key: "obj",
		Off: HeaderSize + 2*int64(testChunk+ChunkOverhead),
		N:   int64(testChunk + ChunkOverhead),
	}
	want := []blob.RangeCall{{Key: "obj", Off: 0, N: HeaderSize}, wantChunk}
	if len(calls) != len(want) {
		t.Fatalf("range calls %#v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d got %#v want %#v", i, calls[i], want[i])
		}
	}
}

func TestSequentialReadCache(t *testing.T) {
	const chunks = 5
	plain := randomPlain(testRand(3), chunks*testChunk)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	m := store(t, encrypt(t, plain, testChunk, kr, b))
	m.ResetCalls()
	rd := openObj(t, m, kr, b)
	var got []byte
	for off := 0; off < len(plain); {
		n := 37
		if n > len(plain)-off {
			n = len(plain) - off
		}
		buf := make([]byte, n)
		rn, err := rd.ReadAt(buf, int64(off))
		if err != nil || rn != n {
			t.Fatalf("off %d n=%d err=%v", off, rn, err)
		}
		got = append(got, buf...)
		off += n
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("sequential plaintext mismatch")
	}
	if calls := m.RangeCalls(); len(calls) != chunks+1 {
		t.Fatalf("range calls %d, want %d: %#v", len(calls), chunks+1, calls)
	}
}

func TestReorderedChunks(t *testing.T) {
	const chunks = 5
	plain := randomPlain(testRand(4), chunks*testChunk)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	raw := encrypt(t, plain, testChunk, kr, b)
	stride := testChunk + ChunkOverhead
	c1 := HeaderSize + 1*stride
	c2 := HeaderSize + 2*stride
	swapped := append([]byte(nil), raw...)
	tmp := append([]byte(nil), swapped[c1:c1+stride]...)
	copy(swapped[c1:c1+stride], swapped[c2:c2+stride])
	copy(swapped[c2:c2+stride], tmp)
	m := store(t, swapped)
	rd := openObj(t, m, kr, b)
	_, err = rd.ReadAt(make([]byte, 16), testChunk)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("reordered chunk: %v", err)
	}
}

func TestTruncatedAtChunkBoundary(t *testing.T) {
	const chunks = 5
	plain := randomPlain(testRand(5), chunks*testChunk)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	raw := encrypt(t, plain, testChunk, kr, b)
	stride := testChunk + ChunkOverhead
	cut := raw[:HeaderSize+4*stride]
	m := store(t, cut)
	rd := openObj(t, m, kr, b)
	if rd.Size() != int64(4*testChunk) {
		t.Fatalf("Size %d", rd.Size())
	}
	_, err = rd.ReadAt(make([]byte, 16), int64(3*testChunk))
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("truncated at boundary: %v", err)
	}
}

func TestTruncatedMidChunk(t *testing.T) {
	plain := randomPlain(testRand(6), 2*testChunk)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	raw := encrypt(t, plain, testChunk, kr, b)
	m := store(t, raw[:len(raw)-10])
	rd := openObj(t, m, kr, b)
	_, err = rd.ReadAt(make([]byte, rd.Size()), 0)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("truncated mid-chunk: %v", err)
	}

	short := store(t, raw[:HeaderSize+20])
	_, err = Open(context.Background(), short, "obj", HeaderSize+20, kr, b)
	if !errors.Is(err, ErrBadLength) {
		t.Fatalf("72+20: %v", err)
	}
}

func TestTruncatedToHeaderPlus28(t *testing.T) {
	const chunks = 3
	plain := randomPlain(testRand(9), chunks*testChunk)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	raw := encrypt(t, plain, testChunk, kr, b)
	if len(raw) <= HeaderSize+28 {
		t.Fatalf("ciphertext %d is not a 3-chunk object", len(raw))
	}
	cut := raw[:HeaderSize+28]
	m := store(t, cut)
	_, err = Open(context.Background(), m, "obj", int64(len(cut)), kr, b)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("Open of header+28: %v", err)
	}
}

func TestWrongBinding(t *testing.T) {
	plain := randomPlain(testRand(7), testChunk+3)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	m := store(t, encrypt(t, plain, testChunk, kr, b))
	cases := []Binding{b, b, b}
	cases[0].RepoID[0] ^= 0xff
	cases[1].ObjectID[0] ^= 0xff
	cases[2].Kind = KindIdx
	for i, other := range cases {
		_, err := Open(context.Background(), m, "obj", int64(len(m.Raw("obj"))), kr, other)
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("binding %d: %v", i, err)
		}
	}
}

func TestHeaderFieldChanged(t *testing.T) {
	plain := randomPlain(testRand(8), testChunk+8)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	raw := encrypt(t, plain, testChunk, kr, b)

	openTampered := func(mutate func([]byte)) error {
		t.Helper()
		cp := append([]byte(nil), raw...)
		mutate(cp)
		m := store(t, cp)
		_, err := Open(context.Background(), m, "obj", int64(len(cp)), kr, b)
		return err
	}

	err = openTampered(func(b []byte) { b[6] ^= 0x01 })
	if !errors.Is(err, ErrAuth) && !errors.Is(err, ErrBadHeader) {
		t.Fatalf("chunk_size: %v", err)
	}
	err = openTampered(func(b []byte) { b[10] ^= 0x01 })
	if !errors.Is(err, ErrAuth) && !errors.Is(err, ErrBadHeader) {
		t.Fatalf("dek_nonce: %v", err)
	}
	err = openTampered(func(b []byte) { b[5] ^= 0x01 })
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("key_id: %v", err)
	}
	err = openTampered(func(b []byte) { b[0] ^= 0x01 })
	if !errors.Is(err, ErrBadHeader) {
		t.Fatalf("magic: %v", err)
	}
	err = openTampered(func(b []byte) { b[4] = 2 })
	if !errors.Is(err, ErrVersion) {
		t.Fatalf("version: %v", err)
	}
}

func TestWrongMasterKey(t *testing.T) {
	plain := randomPlain(testRand(9), 100)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	m := store(t, encrypt(t, plain, testChunk, kr, b))
	var other [32]byte
	for i := range other {
		other[i] = 0x22
	}
	wrong, err := NewKeyring(other)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), m, "obj", int64(len(m.Raw("obj"))), wrong, b)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestCiphertextHidesPlaintext(t *testing.T) {
	plain := randomPlain(testRand(10), 64<<10)
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	raw := encrypt(t, plain, testChunk, kr, testBinding())
	if bytes.Contains(raw, plain[:64]) {
		t.Fatal("plaintext prefix found in ciphertext")
	}
}

func TestWriterMisuse(t *testing.T) {
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, kr, testBinding(), WriterOptions{ChunkSize: testChunk})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("Write after Close returned nil")
	}
	if err := w.Close(); err == nil {
		t.Fatal("second Close returned nil")
	}
	for _, n := range []int{100, 32 << 20} {
		if _, err := NewWriter(&buf, kr, testBinding(), WriterOptions{ChunkSize: n}); err == nil {
			t.Fatalf("ChunkSize %d was accepted", n)
		}
	}
}

func TestParseMasterKey(t *testing.T) {
	for _, s := range []string{
		"",
		base64.StdEncoding.EncodeToString(make([]byte, 31)),
		base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"not-base64",
	} {
		if _, err := ParseMasterKey(s); !errors.Is(err, ErrBadMasterKey) {
			t.Fatalf("%q: %v", s, err)
		}
	}
	want := testMaster()
	got, err := ParseMasterKey(base64.StdEncoding.EncodeToString(want[:]) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("parsed key mismatch")
	}
}
