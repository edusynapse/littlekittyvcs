package objcrypt

import (
	"container/list"
	"context"
	"fmt"
	"sort"
	"sync"
)

// cacheEntryOverhead is the fixed cost of a key and a list node, added to
// cap(plaintext) when an entry is counted against the byte limit.
const cacheEntryOverhead = 128

// chunkKey is everything authenticating a chunk depended on.
// size is the ciphertext length passed to Open. It decides the chunk count,
// and so which chunk carries the last flag.
type chunkKey struct {
	repo   [16]byte
	object [16]byte
	kind   Kind
	header [32]byte
	size   int64
	index  int
}

type chunkEntry struct {
	key   chunkKey
	plain []byte
	cost  int64
}

// ChunkCache is the process-wide cache of decrypted chunks.
// A chunk is stored only after authentication succeeds.
// A nil *ChunkCache is a cache that stores nothing: Open then behaves as Open.
type ChunkCache struct {
	max   int64
	mu    sync.Mutex
	used  int64
	ll    *list.List
	items map[chunkKey]*list.Element
}

// NewChunkCache returns a cache that holds at most maxBytes.
// An entry larger than maxBytes is not stored.
func NewChunkCache(maxBytes int64) *ChunkCache {
	return &ChunkCache{
		max:   maxBytes,
		ll:    list.New(),
		items: make(map[chunkKey]*list.Element),
	}
}

// MaxBytes is the limit passed to NewChunkCache. A nil cache reports 0.
func (c *ChunkCache) MaxBytes() int64 {
	if c == nil {
		return 0
	}
	return c.max
}

// Open is objcrypt.Open plus this cache.
// Decrypted chunks are stored only when insert is true.
// A nil cache does not store anything and does not change Open.
func (c *ChunkCache) Open(ctx context.Context, src RangeSource, key string, objectSize int64, kr *Keyring, b Binding, insert bool) (*Reader, error) {
	return open(ctx, src, key, objectSize, kr, b, c, insert)
}

func (c *ChunkCache) get(k chunkKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*chunkEntry).plain, true
}

func (c *ChunkCache) put(k chunkKey, plain []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		return
	}
	cost := int64(cap(plain)) + cacheEntryOverhead
	if cost > c.max {
		return
	}
	for c.used+cost > c.max {
		back := c.ll.Back()
		if back == nil {
			return
		}
		old := back.Value.(*chunkEntry)
		c.ll.Remove(back)
		delete(c.items, old.key)
		c.used -= old.cost
	}
	el := c.ll.PushFront(&chunkEntry{key: k, plain: plain, cost: cost})
	c.items[k] = el
	c.used += cost
}

// View returns the number of entries, the accounted bytes, and a sorted
// encoding of each key. Accounted bytes are never above MaxBytes.
func (c *ChunkCache) View() (entries int, bytes int64, keys []string) {
	if c == nil {
		return 0, 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys = make([]string, 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*chunkEntry)
		keys = append(keys, fmt.Sprintf("%x/%x/%d/%x/%d/%d",
			e.key.repo, e.key.object, e.key.kind, e.key.header, e.key.size, e.key.index))
	}
	sort.Strings(keys)
	return len(c.items), c.used, keys
}

func (c *ChunkCache) indexes() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, 0, len(c.items))
	for k := range c.items {
		out = append(out, k.index)
	}
	sort.Ints(out)
	return out
}
