package objcrypt

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
)

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func threeChunkObject(t *testing.T) (plain []byte, m *blob.Mem, kr *Keyring, b Binding) {
	t.Helper()
	kr = testKeyring(t)
	b = testBinding()
	plain = randomPlain(testRand(1), 3*testChunk)
	m = store(t, encrypt(t, plain, testChunk, kr, b))
	return plain, m, kr, b
}

func openCached(t *testing.T, c *ChunkCache, m *blob.Mem, kr *Keyring, b Binding, key string, size int64, insert bool) *Reader {
	t.Helper()
	rd, err := c.Open(context.Background(), m, key, size, kr, b, insert)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return rd
}

func TestCacheHitSkipsRead(t *testing.T) {
	plain, m, kr, b := threeChunkObject(t)
	c := NewChunkCache(8 << 20)
	size := int64(len(m.Raw("obj")))
	first := openCached(t, c, m, kr, b, "obj", size, true)
	if got := readAll(t, first); !bytes.Equal(got, plain) {
		t.Fatal("first read mismatch")
	}
	m.ResetCalls()
	second := openCached(t, c, m, kr, b, "obj", size, true)
	if got := readAll(t, second); !bytes.Equal(got, plain) {
		t.Fatal("second read mismatch")
	}
	if calls := m.RangeCalls(); len(calls) != 1 {
		t.Fatalf("second read range calls %d, want the header only", len(calls))
	}
	if second.RangeReads() != 1 || second.CacheHits() != 3 {
		t.Fatalf("second reader reads %d hits %d", second.RangeReads(), second.CacheHits())
	}
}

func TestCacheKeyCoversBinding(t *testing.T) {
	_, m, kr, b := threeChunkObject(t)
	c := NewChunkCache(8 << 20)
	size := int64(len(m.Raw("obj")))
	if got := readAll(t, openCached(t, c, m, kr, b, "obj", size, true)); len(got) != 3*testChunk {
		t.Fatal("prime read")
	}
	before := c.indexes()
	raw := m.Raw("obj")
	if err := m.Put(context.Background(), "other", bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	otherRepo := b
	otherRepo.RepoID[0] ^= 0xff
	_, err := c.Open(context.Background(), m, "other", size, kr, otherRepo, true)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("other repository: %v", err)
	}
	otherKind := b
	otherKind.Kind = KindIdx
	_, err = c.Open(context.Background(), m, "other", size, kr, otherKind, true)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("other kind: %v", err)
	}
	if got := c.indexes(); len(got) != len(before) {
		t.Fatalf("cache indexes %v, before %v", got, before)
	}
}

func TestCacheKeyCoversSize(t *testing.T) {
	_, m, kr, b := threeChunkObject(t)
	c := NewChunkCache(8 << 20)
	full := int64(len(m.Raw("obj")))
	readAll(t, openCached(t, c, m, kr, b, "obj", full, true))
	short := full - chunkStride(testChunk)
	cached := openCached(t, c, m, kr, b, "obj", short, true)
	_, cachedErr := cached.ReadAt(make([]byte, cached.Size()), 0)
	plain, err := open(context.Background(), m, "obj", short, kr, b, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_, plainErr := plain.ReadAt(make([]byte, plain.Size()), 0)
	if !errors.Is(cachedErr, ErrAuth) || !errors.Is(plainErr, ErrAuth) {
		t.Fatalf("cached %v plain %v", cachedErr, plainErr)
	}
	if cached.RangeReads() != plain.RangeReads() {
		t.Fatalf("short read used the cache: cached reads %d plain reads %d", cached.RangeReads(), plain.RangeReads())
	}
	if cached.CacheHits() != 0 {
		t.Fatalf("short read cache hits %d", cached.CacheHits())
	}
}

func TestCacheSkipsFailedAuth(t *testing.T) {
	plain, m, kr, b := threeChunkObject(t)
	c := NewChunkCache(8 << 20)
	raw := m.Raw("obj")
	off := HeaderSize + int(chunkStride(testChunk))
	saved := raw[off]
	raw[off] ^= 0xff
	m.SetRaw("obj", raw)
	size := int64(len(raw))
	rd := openCached(t, c, m, kr, b, "obj", size, true)
	if _, err := rd.ReadAt(make([]byte, rd.Size()), 0); !errors.Is(err, ErrAuth) {
		t.Fatalf("tampered read: %v", err)
	}
	raw[off] = saved
	m.SetRaw("obj", raw)
	if got := c.indexes(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("cache indexes %v, want chunk 0 only", got)
	}
	if got := readAll(t, openCached(t, c, m, kr, b, "obj", size, true)); !bytes.Equal(got, plain) {
		t.Fatal("read after restore mismatch")
	}
}

func TestCacheByteLimit(t *testing.T) {
	kr := testKeyring(t)
	b := testBinding()
	plain := randomPlain(testRand(2), 5*testChunk)
	m := store(t, encrypt(t, plain, testChunk, kr, b))
	cost := int64(testChunk) + cacheEntryOverhead
	max := cost*2 + cost/2
	c := NewChunkCache(max)
	rd := openCached(t, c, m, kr, b, "obj", int64(len(m.Raw("obj"))), true)
	buf := make([]byte, testChunk)
	for i := 0; i < 5; i++ {
		n, err := rd.ReadAt(buf, int64(i)*int64(testChunk))
		if err != nil || n != testChunk {
			t.Fatalf("chunk %d n=%d err=%v", i, n, err)
		}
		_, accounted, _ := c.View()
		if accounted > max {
			t.Fatalf("after chunk %d accounted %d over %d", i, accounted, max)
		}
	}
	if got := c.indexes(); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("held %v, want the two most recent", got)
	}
	_, accounted, _ := c.View()
	if accounted != 2*cost {
		t.Fatalf("accounted %d, want %d", accounted, 2*cost)
	}

	tiny := NewChunkCache(cost - 1)
	one := store(t, encrypt(t, plain[:testChunk], testChunk, kr, b))
	readAll(t, openCached(t, tiny, one, kr, b, "obj", int64(len(one.Raw("obj"))), true))
	if n, bytes, _ := tiny.View(); n != 0 || bytes != 0 {
		t.Fatalf("oversized entry stored n=%d bytes=%d", n, bytes)
	}
}

func TestCacheInsertFalse(t *testing.T) {
	plain, m, kr, b := threeChunkObject(t)
	c := NewChunkCache(8 << 20)
	size := int64(len(m.Raw("obj")))
	first := openCached(t, c, m, kr, b, "obj", size, true)
	if _, err := first.ReadAt(make([]byte, testChunk), 0); err != nil {
		t.Fatal(err)
	}
	if got := c.indexes(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("primed indexes %v", got)
	}
	second := openCached(t, c, m, kr, b, "obj", size, false)
	if got := readAll(t, second); !bytes.Equal(got, plain) {
		t.Fatal("insert false read mismatch")
	}
	if second.CacheHits() < 1 {
		t.Fatalf("insert false hits %d", second.CacheHits())
	}
	if got := c.indexes(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("after insert false, indexes %v", got)
	}
}

func TestCacheConcurrent(t *testing.T) {
	plain, m, kr, b := threeChunkObject(t)
	c := NewChunkCache(8 << 20)
	size := int64(len(m.Raw("obj")))
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rd, err := c.Open(context.Background(), m, "obj", size, kr, b, true)
			if err != nil {
				errCh <- err
				return
			}
			got := make([]byte, rd.Size())
			n, err := rd.ReadAt(got, 0)
			if err != nil || int64(n) != rd.Size() || !bytes.Equal(got, plain) {
				errCh <- errors.New("concurrent read mismatch")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestNilCacheOpen(t *testing.T) {
	plain, m, kr, b := threeChunkObject(t)
	var c *ChunkCache
	size := int64(len(m.Raw("obj")))
	m.ResetCalls()
	rd := openCached(t, c, m, kr, b, "obj", size, true)
	if got := readAll(t, rd); !bytes.Equal(got, plain) {
		t.Fatal("nil cache read mismatch")
	}
	// Header plus one range read per chunk. Nothing is served from a cache.
	if calls := m.RangeCalls(); len(calls) != 4 {
		t.Fatalf("range calls %d, want header plus one per chunk", len(calls))
	}
	if rd.RangeReads() != 4 || rd.CacheHits() != 0 {
		t.Fatalf("reads %d hits %d", rd.RangeReads(), rd.CacheHits())
	}
}
