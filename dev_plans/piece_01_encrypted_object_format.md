# Piece 1: the encrypted object format and the bucket client

This is the first code in the repository. It builds the two things every later piece sits on:

1. **`internal/objcrypt`**: writes and reads the `S3G1` encrypted object format. Every pack, pack index, and ref snapshot the server stores goes through it.
2. **`internal/blob`**: talks to the S3-compatible bucket (upload, ranged download, list, delete), plus an in-memory fake for tests.

It also proves the one assumption the whole design rests on: **go-git v5 can parse a real Git pack while reading it through our decrypting reader**, without the plaintext pack ever existing as a file.

The overall design is in [git_s3_server_plan.md](git_s3_server_plan.md). This document is self-contained for this piece. Where the two disagree, this one is newer for this piece. Report the disagreement; do not silently pick one.

## Out of scope for this piece

No `main.go`, no HTTP server, no PostgreSQL, no Dockerfile, no environment-variable parsing beyond the master-key parser below, no chunk cache shared across readers, no object index. Do not create files in any other `internal/` folder.

## Rules for whoever implements this

- **Leave every change unstaged.** No `git add`, `git stash`, `git checkout`, `git reset`, `git restore`, `git clean`, no commits.
- **No recursive or forced deletes** (`rm -rf`, `rm -r`, `rm -f`, `find -delete`), and never a variable in a delete path. Tests clean up only through Go's `t.TempDir()`.
- **No backward compatibility.** Nothing has shipped. There is one format version, `1`. No code reads any other shape, no "legacy" branches, no fallbacks.
- **No new settings.** The only environment variables this piece reads are the test-only `LKV_TEST_S3_*` ones listed under the MinIO test, and only from `_test.go` files.
- **The go-git check is the point of this piece.** If go-git's parser cannot read through an `io.ReadSeeker` built on our reader, stop and report exactly what it needs instead. Do not work around it by decrypting to a temporary file. A workaround hides the failure this test exists to find.
- **Check every go-git and AWS SDK name used below against the pinned module source** before writing code against it. The names here are from memory of the v5 and v2 APIs. If one differs, use the real one and list the difference in the report.

## Setup

Go is not installed on the development machine yet (checked 3 Nov 2026: `go: command not found`). Install the current stable Go release first. `git` 2.34.1 is installed and is enough for the tests.

```bash
cd /home/surajitray/Development/VSCodeProjects/littlekittyvcs
go mod init github.com/edusynapse/littlekittyvcs
go get github.com/go-git/go-git/v5@latest
go get github.com/aws/aws-sdk-go-v2/aws@latest github.com/aws/aws-sdk-go-v2/config@latest github.com/aws/aws-sdk-go-v2/credentials@latest github.com/aws/aws-sdk-go-v2/service/s3@latest
```

Use go-git **v5**, never v6. Write the exact versions `go.mod` ended up with into the report.

No other dependencies. No UUID library: ids are `[16]byte` in this piece.

## Files to create

```text
go.mod, go.sum
internal/objcrypt/
  keyring.go          master key parsing, Keyring
  format.go           constants, header encode/decode, sizes, AAD builders, errors
  writer.go           streaming encrypting Writer
  reader.go           ranged-read decrypting Reader (io.ReaderAt)
  objcrypt_test.go    format, tamper, and read-pattern tests
  gogit_test.go       the go-git check
internal/blob/
  blob.go             Store interface, Info, ErrNotFound
  s3.go               S3 implementation
  parts.go            multipart part plan
  mem.go              in-memory fake that records reads
  parts_test.go
  mem_test.go
  s3_test.go          MinIO/rustfs test, skipped without LKV_TEST_S3_ENDPOINT
```

`objcrypt` must not import `blob`. It reads through its own small interface (below), so the dependency points from callers down to both.

## `internal/objcrypt`

### Constants

```go
const (
    Magic            = "S3G1"
    Version   uint8  = 1
    KeyIDCurrent uint8 = 1
    DefaultChunkSize = 4 << 20      // 4 MiB of plaintext per chunk
    MinChunkSize     = 4 << 10      // reader accepts header values in [MinChunkSize, MaxChunkSize]
    MaxChunkSize     = 16 << 20
    NonceSize        = 12
    TagSize          = 16
    ChunkOverhead    = NonceSize + TagSize       // 28
    WrappedDEKSize   = 32 + TagSize              // 48
    HeaderSize       = 4 + 1 + 1 + 4 + 12 + 2 + 48 // 72
)

type Kind uint8
const (
    KindPack Kind = 1
    KindIdx  Kind = 2
    KindSnap Kind = 3
)
```

### Binding

Every object is tied to where it belongs. Moving it anywhere else must make it fail to decrypt.

```go
type Binding struct {
    RepoID   [16]byte
    ObjectID [16]byte   // the pack uuid; the .idx and .snap of the same push use the same uuid with a different Kind
    Kind     Kind
}
```

### Header layout (all integers little-endian)

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `"S3G1"` |
| 4 | 1 | version, `1` |
| 5 | 1 | key_id |
| 6 | 4 | chunk_size (uint32) |
| 10 | 12 | dek_nonce |
| 22 | 2 | wrapped_dek_len (uint16), must be `48` |
| 24 | 48 | wrapped_dek |

`HeaderSize` is always 72 in version 1. The reader rejects any other `wrapped_dek_len` with `ErrBadHeader`.

### Data-encryption key

- A fresh 32-byte DEK per object from `crypto/rand`. A fresh 12-byte `dek_nonce` from `crypto/rand`.
- `wrapped_dek = AES-256-GCM(master key for key_id).Seal(dek_nonce, dek, aadWrap)` where

```text
aadWrap = header[0:24] || RepoID || ObjectID || Kind
```

`header[0:24]` is everything before `wrapped_dek`: magic, version, key_id, chunk_size, dek_nonce, wrapped_dek_len. Changing any header field, or opening the object under a different binding, fails the unwrap.

### Chunks

Plaintext is cut into chunks of `chunk_size` bytes. Every object has **at least one chunk**. Empty plaintext is one chunk of 0 bytes. If the plaintext length is an exact multiple of `chunk_size`, the last chunk is a full chunk. There is no extra empty chunk after it.

Each chunk on disk is `nonce(12) || ciphertext || tag(16)`, with a fresh random nonce per chunk and:

```text
aadChunk = sha256(header[0:72]) || RepoID || ObjectID || Kind || chunkIndex (uint64 LE) || last (uint8: 1 for the final chunk, else 0)
```

### Sizes

```text
object size  = 72 + sum over chunks of (28 + plaintext bytes in that chunk)
chunk i      starts at 72 + i * (chunk_size + 28)
```

From an object size `L` and the header's `C = chunk_size`, the reader computes the plaintext size:

```text
body = L - 72                      body < 28            -> ErrBadLength
full = body / (C + 28)
rem  = body % (C + 28)
rem == 0                           -> chunks = full, last chunk is full, plaintext = full * C
rem in [1, 27]                     -> ErrBadLength
rem == 28 and full > 0             -> ErrBadLength   (an empty final chunk only exists when the plaintext is empty)
otherwise                          -> chunks = full + 1, plaintext = full * C + (rem - 28)
```

A file cut at a chunk boundary still has a valid-looking length. It is caught because its final chunk was sealed with `last = 0`, and the reader opens the final chunk expecting `last = 1`.

### Errors

```go
var (
    ErrBadMasterKey = errors.New("objcrypt: master key must be base64 of exactly 32 bytes")
    ErrBadHeader    = errors.New("objcrypt: bad header")        // wrong magic, chunk size out of range, wrong wrapped_dek_len
    ErrVersion      = errors.New("objcrypt: unsupported version")
    ErrUnknownKey   = errors.New("objcrypt: unknown key id")
    ErrBadLength    = errors.New("objcrypt: object length does not match the format")
    ErrAuth         = errors.New("objcrypt: authentication failed") // any GCM open failure: wrong key, wrong binding, tampered or reordered chunk
)
```

Wrap them with context (`fmt.Errorf("...: %w", ErrAuth)`), so callers use `errors.Is`. Never put key bytes, DEKs, or plaintext into an error or a log line.

### `keyring.go`

```go
func ParseMasterKey(s string) ([32]byte, error)
```

Trims surrounding ASCII whitespace (a trailing newline from an env file is common), decodes **standard** base64 with padding, and requires exactly 32 bytes. Anything else returns `ErrBadMasterKey`. The config step later calls this. This piece does not read `MASTER_KEY` itself.

```go
type Keyring struct { /* key_id -> AES-256-GCM cipher.AEAD, current id */ }
func NewKeyring(master [32]byte) (*Keyring, error)   // registers it as key_id 1, the current key
```

Build the `cipher.AEAD` once in `NewKeyring`. The writer always wraps with the current key. The reader looks up the header's `key_id` and returns `ErrUnknownKey` if it is not registered.

### `writer.go`

```go
type WriterOptions struct {
    ChunkSize int // 0 means DefaultChunkSize; must be within [MinChunkSize, MaxChunkSize]
}

func NewWriter(dst io.Writer, kr *Keyring, b Binding, opt WriterOptions) (*Writer, error)
func (w *Writer) Write(p []byte) (int, error)
func (w *Writer) Close() error          // seals the final chunk with last = 1; required
func (w *Writer) CiphertextSize() int64 // bytes written to dst so far; final after Close
```

- `NewWriter` generates the DEK and nonces, writes the 72-byte header to `dst` immediately, and returns.
- `Write` fills a `ChunkSize` buffer. A full buffer is sealed with `last = 0` **only when another byte arrives**. That keeps the final chunk unsealed until `Close` knows it is the last one.
- `Close` seals whatever is buffered (possibly 0 bytes if nothing was ever written) with `last = 1`. A second `Close` returns an error. `Write` after `Close` returns an error.
- `ChunkSize` other than the default exists so tests can make a small pack span many chunks. Production callers pass the zero value.

`dst` will be a spool file on local disk during a push, and later the upload. The writer does not care which.

### `reader.go`

```go
type RangeSource interface {
    // GetRange returns exactly n bytes starting at off, or an error.
    GetRange(ctx context.Context, key string, off, n int64) ([]byte, error)
}

func Open(ctx context.Context, src RangeSource, key string, objectSize int64, kr *Keyring, b Binding) (*Reader, error)
func (r *Reader) Size() int64                               // plaintext size
func (r *Reader) ReadAt(p []byte, off int64) (int, error)   // io.ReaderAt
```

- `objectSize` is the ciphertext size, known to the caller from the `packs.bytes` row or from a HEAD request. `Open` does not issue a HEAD.
- `Open` does exactly one ranged read, `[0, 72)`, for the header. It validates magic, version, `key_id`, the chunk-size range, and `wrapped_dek_len`. It unwraps the DEK (failure is `ErrAuth`), computes the plaintext size with the rule above (failure is `ErrBadLength`), and keeps the `ctx` for later reads.
- `ReadAt` follows the `io.ReaderAt` contract: `off < 0` is an error, and a read reaching past `Size()` returns the bytes available and `io.EOF`. It may span several chunks.
- To read chunk `i`, fetch exactly that chunk's byte range, then open it with `aadChunk` for index `i` and `last = (i == chunks-1)`. A failure is `ErrAuth`.
- **Per-reader cache:** keep the **two most recently decrypted chunks**. go-git's scanner reads a pack front to back in many small reads, and a read often straddles a chunk boundary. Without this, every small read would refetch 4 MiB. The process-wide cache comes later in `internal/objindex`. Do not build it here.
- `Reader` is not required to be safe for concurrent use. Say so in its doc comment.

`*blob.Mem` and `*blob.S3` both satisfy `RangeSource` directly, because `blob.Store.GetRange` has the same name and signature. No adapter.

## `internal/blob`

### `blob.go`

```go
var ErrNotFound = errors.New("blob: not found")

type Info struct {
    Key          string
    Size         int64
    LastModified time.Time
}

type Store interface {
    Put(ctx context.Context, key string, r io.Reader, size int64) error
    GetRange(ctx context.Context, key string, off, n int64) ([]byte, error)
    Head(ctx context.Context, key string) (Info, error)
    List(ctx context.Context, prefix string, fn func(Info) error) error
    Delete(ctx context.Context, key string) error
}
```

`GetRange` returns exactly `n` bytes or an error. A short body is an error, not a short slice. A missing key is `ErrNotFound` from `GetRange`, `Head`, and `Delete`.

### `parts.go`

```go
const (
    SinglePutMax = 16 << 20   // objects up to this size use one PutObject
    PartSize     = 16 << 20   // multipart part size; S3 and Spaces reject parts under 5 MiB except the last
    MaxParts     = 10000      // S3 limit, so the largest object is 16 MiB x 10000 = 156.25 GiB
)

type part struct{ Number int32; Size int64 }
func planParts(size int64) ([]part, error)  // numbers start at 1; every part is PartSize except the last; size > PartSize*MaxParts is an error
```

### `s3.go`

```go
type S3Config struct {
    Endpoint, Region, Bucket, AccessKey, SecretKey string
    PathStyle bool
}
func NewS3(ctx context.Context, c S3Config) (*S3, error)
```

- The caller passes the config. This package does not read environment variables.
- Build the client with `s3.NewFromConfig`, `BaseEndpoint` set to `Endpoint`, `UsePathStyle` from `PathStyle`, and static credentials. Do not use the default credential chain. It would pick up a developer's `~/.aws` and send their AWS keys to Spaces.
- **Checksums:** recent `aws-sdk-go-v2/service/s3` releases add CRC checksums to every upload by default, and some S3-compatible services have rejected them. Set `RequestChecksumCalculation` and `ResponseChecksumValidation` to their `WhenRequired` values on the client config. Confirm the field names in the pinned SDK. Confirm against MinIO or rustfs in the test below. The Spaces check happens in a later piece.
- `Put`: if `size <= SinglePutMax`, read the whole body into memory and call one `PutObject` with `ContentLength`. Otherwise use `planParts`: `CreateMultipartUpload`, then for each part read exactly `part.Size` bytes into a buffer, `UploadPart`, and keep its ETag. Then `CompleteMultipartUpload` with every part number and ETag in order. If the reader yields fewer or more bytes than `size`, that is an error. On any error after `CreateMultipartUpload`, call `AbortMultipartUpload` with a fresh context (`context.WithTimeout(context.Background(), 30*time.Second)`) and return the original error. Upload parts one at a time. Parallel parts can come later if a measurement asks for them.
- `GetRange`: `GetObject` with `Range: bytes=<off>-<off+n-1>`. Read the body fully and check the length.
- `Head`: `HeadObject`. `List`: the `ListObjectsV2` paginator, calling `fn` per object and stopping on its first error. `Delete`: `DeleteObject`.
- Map the SDK's not-found errors (`NoSuchKey`, `NotFound`, and HTTP 404) to `ErrNotFound`.

### `mem.go`

```go
type Mem struct { /* mutex, map[key]{data []byte; modified time.Time}, recorded reads */ }
func NewMem() *Mem
type RangeCall struct{ Key string; Off, N int64 }
func (m *Mem) RangeCalls() []RangeCall   // copy of every GetRange call so far, in order
func (m *Mem) ResetCalls()
func (m *Mem) Raw(key string) []byte     // test helper: the stored bytes
func (m *Mem) SetRaw(key string, b []byte) // test helper: overwrite stored bytes (tamper tests)
```

`Mem` implements `Store` with the same semantics as `S3`, including `ErrNotFound` and exact-length ranges. A `GetRange` past the end of an object is an error.

## Tests

Run with `go test ./...` and `go vet ./...` from the repository root. Every test below must pass. In the report, quote the output, with `-v` for the go-git check so its logged numbers are visible.

### `internal/objcrypt/objcrypt_test.go`

Use `ChunkSize: 4096` unless a test says otherwise. Use a fixed `Binding` and a fixed test master key (32 bytes of a constant pattern; never a real key).

1. **Round trip at the edges.** Plaintext lengths `0, 1, 4095, 4096, 4097, 3*4096, 3*4096+5`, with random bytes from `math/rand` seeded with a constant. Write through `Writer`, store in `blob.Mem`, `Open`, and read everything with `ReadAt`. Assert: bytes equal; `Size()` equals the length; `CiphertextSize()` equals the stored length and the size formula.
2. **Chunk 2 alone.** A 5-chunk object. `ResetCalls`, `Open`, then `ReadAt` 100 bytes from the middle of chunk 2. Assert `RangeCalls()` is exactly two calls: `[0,72)` and chunk 2's exact range. Chunk 0 was never fetched.
3. **Sequential read.** A 5-chunk object read front to back in 37-byte `ReadAt` calls. Assert exactly 6 range calls (the header plus each chunk once). This proves the two-chunk cache works for the scanner's read pattern.
4. **Reordered chunks.** Swap the bytes of chunk 1 and chunk 2 in the stored object (`SetRaw`). Reading chunk 1 fails with `ErrAuth`.
5. **Truncated at a chunk boundary.** Drop the last chunk of a 5-chunk object whose last chunk is full. `Open` succeeds (the length is valid). Reading the new final chunk fails with `ErrAuth`.
6. **Truncated mid-chunk.** Cut 10 bytes off the end. Reading the final chunk fails with `ErrAuth`. Cut the object to `72 + 20` bytes: `Open` fails with `ErrBadLength`.
7. **Wrong binding.** Open the same stored bytes with a different `RepoID`, then a different `ObjectID`, then a different `Kind`. Each `Open` fails with `ErrAuth`.
8. **Header field changed.** Flip one bit in each of chunk_size, dek_nonce, and key_id (key_id changed to an unregistered id gives `ErrUnknownKey`; the others give `ErrAuth` or `ErrBadHeader`). Wrong magic gives `ErrBadHeader`, and version `2` gives `ErrVersion`.
9. **Wrong master key.** A keyring built from a different 32-byte key: `Open` fails with `ErrAuth`.
10. **Ciphertext is not plaintext.** For a 64 KiB plaintext, assert the plaintext's first 64 bytes do not appear anywhere in the stored object.
11. **Writer misuse.** `Write` after `Close` and a second `Close` both error. `ChunkSize` of `100` or `32 << 20` is rejected by `NewWriter`.
12. **`ParseMasterKey`.** Empty string, base64 of 31 bytes, base64 of 33 bytes, and non-base64 text all give `ErrBadMasterKey`. Base64 of 32 bytes with a trailing `"\n"` succeeds.

### `internal/objcrypt/gogit_test.go`: the go-git check

This test runs the real `git` binary as a separate program to build a test pack. It fails (it does not skip) if `git` is not on `PATH`.

1. In `t.TempDir()`: `git init`, set a local `user.name` and `user.email`, and turn off `commit.gpgsign`.
2. Make 60 commits. Each commit rewrites 5 text files of about 20 KiB, generated from `math/rand` with a constant seed, changing a few lines each time so Git stores deltas. Add one 200 KiB random binary file in the first commit.
3. `git repack -a -d -f --depth=50 --window=50`. There must now be exactly one `.pack` file under `.git/objects/pack/`.
4. Run `git verify-pack -v <that .idx>`. Count the object lines that have a delta depth and base (the rows with 7 fields). **Assert at least one delta exists**, so the test really exercises delta resolution.
5. Encrypt the `.pack` bytes with `Writer` (`ChunkSize: 16 << 10`, so the pack spans many chunks) into `blob.Mem`. `Open` it.
6. `sr := io.NewSectionReader(reader, 0, reader.Size())`. Parse with go-git v5: `packfile.NewScanner(sr)`, an `idxfile.Writer` as the observer, `packfile.NewParser(scanner, idxWriter)`, then `parser.Parse()`, then `idxWriter.Index()`.
7. Assert: the checksum `Parse` returns equals the last 20 bytes of the original `.pack` file. The set of object ids in the go-git index equals the output of `git cat-file --batch-all-objects --batch-check='%(objectname)'`. The total object count matches.
8. Read the whole plaintext back through `ReadAt` and assert it equals the original `.pack` bytes.
9. `t.Logf` the pack size, the chunk count, the number of `RangeCalls()` during parsing, and how many of them were repeated ranges. These numbers size the process-wide chunk cache in a later piece. Do not assert on them.

If step 6 cannot work through a `ReadSeeker` (for example, the parser needs a `billy.File` or a path on disk), stop. Report the exact function signatures involved and what they require. See the rules at the top.

### `internal/blob/parts_test.go`

`planParts` for sizes `16 MiB + 1` (two parts: 16 MiB, 1 byte), `20 MiB` (16 MiB, 4 MiB), `48 MiB` (three parts of 16 MiB), and `PartSize * MaxParts + 1` (error). Every part except the last equals `PartSize`. Part numbers start at 1.

### `internal/blob/mem_test.go`

Put, GetRange inside the object, GetRange past the end (error), Head (size), List by prefix (only matching keys), Delete, then GetRange, Head, and Delete on the deleted key all return `ErrNotFound`.

### `internal/blob/s3_test.go`

Skipped (via `t.Skip` with a message) unless `LKV_TEST_S3_ENDPOINT` is set. Also reads `LKV_TEST_S3_REGION`, `LKV_TEST_S3_BUCKET`, `LKV_TEST_S3_ACCESS_KEY`, `LKV_TEST_S3_SECRET_KEY`, and `LKV_TEST_S3_PATH_STYLE`. Each run uses a unique key prefix, `lkv-test/<random hex>/`.

Run it against a local MinIO or rustfs container. Whichever image and tag you use, write the exact `docker run` command into the report. MinIO and rustfs both enforce the 5 MiB minimum part size, which is what this test is for.

1. Put a 40 MiB random object (three multipart parts). Head reports 40 MiB. GetRange of 1 MiB starting at 20 MiB matches the source bytes.
2. Put a 1 KiB object (single put) and read it back.
3. List the prefix: exactly those two keys.
4. Encrypt a 20 MiB plaintext with `objcrypt.Writer` (default chunk size) into a buffer. `Put` it, `Open` it through the S3 store, and read chunk 3 alone.
5. Delete both keys. Head on each gives `ErrNotFound`.

The test deletes only keys it created, under its own prefix, by exact key.

## Done means

The report back must contain this table, every row filled with evidence (command output or file path), or the piece is not done:

| Item | Evidence |
|---|---|
| `go.mod` module path and pinned versions of go-git v5 and the AWS SDK modules | |
| `go vet ./...` clean | |
| `go test ./...` passes (objcrypt tests 1-12, blob parts and mem tests) | |
| go-git check passes, with its logged pack size, chunk count, and range-call numbers | |
| S3 test passed against MinIO or rustfs, with the exact `docker run` command | |
| Every go-git and AWS SDK name in this document that turned out different from the real API | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
