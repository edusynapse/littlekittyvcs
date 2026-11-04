# Piece 10: process-wide chunk cache, and one object cache per request

After this piece:
- **Chunk cache.** Decrypted 4 MiB chunks of stored packs are kept in one process-wide cache with a fixed byte limit. A second fetch of the same new push, or a push whose fast-forward check reads commits a fetch just read, gets those chunks from memory instead of from S3.
- **Bounded per-request memory.** Today each stored pack a request opens gets its own go-git object cache of up to 96 MiB. After this piece, one request has one object cache of 96 MiB shared by all its packs.
- **Measurement.** Every fetch logs one line with its S3 reads and cache hits. Build step 15 (the real-history clone on App Platform and Contabo) reads those lines to set the cache size.

Start after piece 9. Both edit `README.md`. Build steps 14 and 15 of the main plan come after this piece.

## What the code does today (checked at source)

The main plan's "Chunk cache" paragraph (`dev_plans/git_s3_server_plan.md`, line 106) says that without a cache, "a clone of real history would otherwise do one round trip per object". That is no longer true, and this piece corrects it:

- **A clone does not read per object.** A request with no haves takes the joined-pack path. It opens every stored pack (`internal/fetch/fetch.go` `openPacks`, line 350) and copies each one front to back (`writeJoined`, line 390). Each `objcrypt.Reader` keeps its two most recent chunks (`internal/objcrypt/reader.go`, the `recent` field, line 34). So a clone makes one header read per pack and one read per 4 MiB chunk (`DefaultChunkSize`, `internal/objcrypt/format.go` line 16). A cache does not make the first clone faster. It only helps a later clone of the same repository, if the chunks are still cached.
- **Per-object reads happen in three places:**
  - the generated pack for an incremental fetch (`internal/fetch/fetch.go` line 166, `packstore.NewObjects`)
  - push's fast-forward check and peeled-tag fill (`internal/push/push.go` line 151)
  - `restore`'s tag peeling (`internal/snapshot/restore.go` line 256)

  Each request opens each stored pack once (`internal/packstore/objects.go` line 58). It reads through go-git's `packfile.Packfile` over the decrypting reader (`internal/packstore/pack.go` line 31).
- **Within one request, a small pack is already cheap.** A pack under 4 MiB is one chunk, so it costs one header read and one chunk read per request. The cost is across requests: every fetch opens and decrypts again. Inside a large pack, the walk jumps between commits, trees and blobs, so the two-chunk window misses and a 4 MiB chunk is fetched again.
- **go-git keeps an object cache per pack.** `packfile.NewPackfile` builds `cache.NewObjectLRUDefault()` (go-git v5.19.3, `plumbing/format/packfile/packfile.go` lines 74-75). That has `DefaultMaxSize = 96 * MiByte` (`plumbing/cache/common.go` line 14). Every object read is put in it (`packfile.go` lines 377, 421, 465). `Objects` keeps every pack it opened until the request ends. So one fetch that reads a lot of data from k packs can hold up to k × 96 MiB. Adding a process-wide cache on top of an unbounded per-request figure would make any memory budget meaningless, so this piece bounds both.

## Decisions

### Where the chunk cache lives

In `internal/objcrypt`, as `ChunkCache`. The README's layout line puts "the decrypted-chunk cache" in `objindex`. That was never built, and this piece corrects the README. The cache must sit inside the reader: only the reader knows a chunk authenticated.

- `objcrypt.NewChunkCache(maxBytes int64) *ChunkCache`
- `(*ChunkCache).Open(ctx, src, key, objectSize, kr, b, insert bool) (*Reader, error)`. It is the same as `objcrypt.Open`, and the returned reader looks chunks up in the cache. It adds the chunks it decrypts only when `insert` is true. On a nil `*ChunkCache` it is plain `Open`. `objcrypt.Open` itself is unchanged, so tokens, snapshots, indexes and the push spool keep reading without the cache.
- The callers that use it:
  - `packstore.OpenStored` (through `NewObjects`)
  - `fetch.openPacks`

  `push` and `fetch` get the cache through their `Deps`, and `httpapi.Deps` gains `ChunkCache`. `runServe` builds one cache at startup. `restore` passes nil: it is a one-shot command.

### Cache key, and why a hit is safe

The key is the whole of what authenticating the chunk depended on:
- the binding (repository uuid, object uuid, kind)
- the header digest the reader already computes (`headerSum`)
- the object size the caller passed
- the chunk index

A chunk goes into the cache only after `aead.Open` succeeds on it (`reader.go` `fetch`, line 208). A hit therefore returns bytes that were authenticated under exactly the same key, header, layout and index. The size is in the key because it decides the chunk count, and so which chunk carries the "last" flag. A reader given a different size must not reuse a chunk authenticated under the other layout.

Bucket objects are never rewritten under the same key: every object has a new uuid (piece 8 relies on the same fact). So a cached chunk never goes stale.

Values are the plaintext slices `fetch` returns. `ReadAt` only copies out of them (`reader.go` line 153), so sharing one slice between readers is safe. A test locks that in with `-race`.

### Limit and eviction

- One LRU by bytes, under a mutex.
- An entry counts as `cap(plain)` plus a fixed 128 bytes for the key and list node.
- Inserting past the limit evicts least recently used entries until the cache fits.
- An entry larger than the whole limit is not inserted.

**Starting size: `chunkCacheBytes = 256 << 20`** (64 chunks of 4 MiB), a constant in `cmd/littlekittyvcs`. It is not an environment variable (main plan). It is provisional. Build step 15 replaces it. The arithmetic behind it, for the 1 GiB App Platform instance piece 9 chose:

| Item | Amount |
|---|---|
| Instance memory | 1024 MiB |
| Chunk cache | 256 MiB |
| One request's go-git object cache (below) | 96 MiB |
| Left for the Go runtime, the object index and other requests | 672 MiB |
| The rest, if six requests each fill their object cache at once | 96 MiB |

The object index entry size is not measured yet. Build step 15 records peak memory, and the constant moves then.

**The joined path inserts only for a small repository.** A clone reads every chunk of the repository once. With an LRU, a clone larger than the cache evicts everything and keeps nothing useful: the scan problem.
- So `openPacks` uses the cache for lookups always, and inserts only when the repository's total pack bytes (the sum of `t.Packs` `Bytes`) are at most a quarter of the cache limit. With 256 MiB, that is 64 MiB.
- A CI job that clones the same small repository over and over then serves it from memory. A clone of a large one does not push recent push and fetch chunks out.
- The quarter is a choice, recorded here so build step 15 can revise it.
- `openPacks` passes that decision as `insert` to `(*ChunkCache).Open`. `NewObjects` always passes true.

**Nothing removes an entry except eviction.** When `gc` deletes a deleted repository's packs, their cached chunks stay until they age out. Nothing can read them: a hit needs a reader opened with that repository's binding, and the repository has no row, so no request opens one. Uuids are never reused, so an old entry never matches a new object.

### One go-git object cache per request

`NewObjects` builds one `cache.NewObjectLRU(cache.DefaultMaxSize)`. It passes it to every `OpenStored` it does, which uses `packfile.NewPackfileWithCache` (`packfile.go` line 51) instead of `NewPackfile`.
- The size is go-git's own default, kept as it was. The change is that it is shared, so a request holds at most 96 MiB of decoded objects instead of 96 MiB per pack.
- Objects are keyed by hash, and a hash names the same content in every pack of the repository. So sharing across packs is correct.
- The fresh pack of a push (`internal/packstore/parse.go` line 91) keeps its own cache. A push has exactly one fresh pack, so it adds a fixed 96 MiB, not a multiple.

### The measurement line

- Each `Reader` counts its range reads (the header read and each chunk read) and its cache hits.
- `Objects` sums the counts of the packs it opened, and `openPacks` sums the joined path's.
- When a fetch response finishes, `fetch.Handle` logs one info line:
  ```
  fetch served repository=<uuid> mode=joined|generated packs=<n> range_reads=<n> cache_hits=<n> bytes=<response bytes> ms=<duration>
  ```
- No ref names and no object ids, only the repository uuid, the same as every other log line.
- `runServe`'s `serving` line gains `chunk_cache_bytes`.

Push does not get a line in this piece. Build step 15 measures clones and fetches.

## Files

```
internal/objcrypt/cache.go      new: ChunkCache, key, LRU
internal/objcrypt/reader.go     cache lookup before fetch, insert after a successful Open, counters
internal/packstore/objects.go   NewObjects takes the chunk cache, builds the shared object LRU, sums counters
internal/packstore/pack.go      OpenStored takes the chunk cache and the object LRU
internal/fetch/fetch.go         Deps.ChunkCache, openPacks insert rule, the fetch served line
internal/push/push.go           Deps.ChunkCache passed to NewObjects
internal/snapshot/restore.go    NewObjects with a nil chunk cache
internal/httpapi/*.go           Deps.ChunkCache passed through
cmd/littlekittyvcs/run.go       chunkCacheBytes, one cache, the serving line field
README.md, dev_plans/git_s3_server_plan.md
```

## Tests

### `internal/objcrypt`
1. **A hit skips the read and the decrypt.** Use two readers from one `ChunkCache` over one object of three chunks, through a counting `RangeSource`. After the first reader reads everything, the second reader's full read makes exactly one range read: its header.
2. **The key covers the binding.** Copy an object's bytes to a second key. A reader with a different repository uuid, through the same cache, misses the cache and then fails to decrypt with `ErrAuth`. The same holds for a different kind.
3. **The key covers the size.** A reader opened with a size one chunk shorter (that chunk count) misses the cache and fails to authenticate the last chunk, as it does without the cache.
4. **A chunk that fails authentication is never cached.** Flip one byte of chunk 1 in the source. The first reader fails on chunk 1. Restore the byte. The cache holds chunk 0 only.
5. **Byte limit.** A cache of 2.5 chunks, after reading five chunks, holds two, the most recent. Its accounted size is never above the limit. An entry larger than the limit is not inserted.
6. **No insert when told not to.** A reader opened with insert false still hits existing entries, and adds none.
7. **Concurrency.** Eight goroutines read the same object through their own readers and one cache, under `go test -race`. Every read returns the right bytes.
8. **Nil cache.** `(*ChunkCache)(nil).Open` behaves as `Open`: test 1's object, the full read, one range read per chunk.

### `internal/packstore`
9. **One object cache per request.** Over three stored packs, wrap the object cache in a counting `cache.Object`, and read objects from all three. The one wrapper sees every `Put`, and its size stays at most its limit.

### `internal/fetch` and `test/integration` (real git, PostgreSQL, `blob.Mem` wrapped to count range reads)
10. **A repeated incremental fetch hits.** Two clones at the same commit, one new push, then both run `git fetch`. The second fetch's `fetch served` line has `cache_hits` above zero and fewer `range_reads` than the first. Both clones pass `git fsck`.
11. **A repeated small clone hits.** A repository well under a quarter of a test cache limit, cloned twice: the second clone's line has `range_reads` equal to its pack count (headers only).
12. **A large clone does not insert.** With a small test cache, a repository over a quarter of the limit, cloned once: the cache stays empty. A fetch's earlier entries are still there afterwards.
13. **The line has no ids.** Every `fetch served` line in the test logs matches the field list above, and no line matches `[0-9a-f]{40}`.
14. **Push and restore still work.** The existing suite passes unchanged, including the fast-forward and restore tests, which now go through the shared object cache.

### Checks for the reviewer
- The cache size constant and the arithmetic table above match the code.
- `go test -race ./internal/objcrypt ./internal/packstore ./internal/fetch` passes.

## README

- **Repository layout:** `objcrypt/` becomes "the encrypted object format (S3G1): writer, ranged-read reader, and the process-wide decrypted-chunk cache". `objindex/` loses "and the decrypted-chunk cache".
- **Operations:** one sentence on the `fetch served` line: what each field means, and that it carries no ref names or object ids.

## Main plan

In `dev_plans/git_s3_server_plan.md`:
- **"Chunk cache"** (line 106) is rewritten from this document's "What the code does today" and "Decisions":
  - clones stream packs, and the cache helps repeated reads
  - the key, the byte limit, and the quarter rule for the joined path
  - one go-git object cache per request
  - the constant, set by build step 15
- **Build step 15:** the measurements come from the `fetch served` lines (range reads, cache hits, time) and the instance's memory graph. Set `chunkCacheBytes` and the quarter rule from them.

## Done

| Item | Evidence |
|---|---|
| `go vet ./...` clean | |
| `go test ./... -count=1` without the database, with the skipped tests listed | |
| `go test ./... -count=1` with it, after `pg_isready`: every test passes | |
| `go test -race` on the three packages | |
| Test 10: both `fetch served` lines | |
| Test 11: both lines | |
| Test 12: cache contents before and after | |
| Every name in this document that turned out different in the code | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
