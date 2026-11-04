# Piece 4: push and clone

After this piece, littlekittyvcs works as a real Git remote:
- `git push` stores the pack encrypted in the bucket and moves branch tips in one PostgreSQL transaction.
- `git clone`, `git fetch`, and `git pull` get every stored pack back.
- Two pushes racing for the same branch cannot both win.
- A pack that refers to an object the repository does not have is refused.
- A second server process on the same database and bucket sees the first one's pushes.

It builds:

1. **`internal/packstore`**: spools a push body to local disk already encrypted, parses it, builds its index, checks it, and reads objects back out of stored packs.
2. **`internal/objindex`**: the in-memory map, per repository, from Git object id to the stored pack that holds it. It is rebuilt from the encrypted index files, never stored as a table.
3. **`internal/push`**: `POST /{repo}/git-receive-pack`, the receive path up to and including the report sent back to git.
4. **`internal/fetch`**: `POST /{repo}/git-upload-pack`, which answers clone and fetch with every stored pack joined into one.
5. **`internal/store`**: the pack list in `ReadRepo`, and the push transaction.
6. **`internal/httpapi`**: the two POST routes, sharing the access rules `info/refs` already uses.
7. **`test/integration`**: end-to-end tests with the real `git` client.

Done means: a push of one commit, then `git clone` into a second directory, gives byte-identical files and a clean `git fsck`, and no stored bucket object contains the pack in plain form. The tests also cover these build-order steps from [git_s3_server_plan.md](git_s3_server_plan.md):
- step 5: concurrent pushes to the same branch
- step 6: non-fast-forward refused
- step 7: branch delete
- step 10: a pack whose parent object is missing anywhere is refused
- step 11: two server processes
- step 12: a fresh process rebuilds the index

The main plan's "Push and fetch" and "Pack object and index object" sections were updated on 3 Nov 2026 for this piece. Where this document and that one disagree, report it. Do not silently pick one.

## What was measured before this plan

These were checked on 3 Nov 2026 with git 2.34.1 against a throwaway server in the scratchpad. Do not re-decide them. Re-prove them in the tests below.

- **A clone sends one POST:** `want <id> side-band-64k ofs-delta agent=git/2.34.1\n`, a flush, then `done\n`.
- **Joined packs are accepted.** A clone served as one pack made by joining two stored packs (summed object count, new trailer) worked. So did one where the second pack repeated every object of the first. `git fsck --full` was clean both times.
- **Fetch works with no negotiation.** A clone that had commit A fetched B in one POST: `want B`, `have A`, `done`. The server answered `NAK` and sent every stored pack joined. git printed `warning: no common commits`, updated `origin/main`, and `fsck` was clean. So the first version answers every fetch with every stored pack. It costs bandwidth, not correctness. Smarter answers are a later piece.
- **What a push sends:**
  - `<old> <new> refs/heads/main\0 report-status side-band-64k agent=git/2.34.1`. There is a **space after the NUL**.
  - Then a flush, then `PACK...` to the end of the body.
  - git requests only the capabilities it uses (no `atomic`, no `delete-refs` unless it deletes).
  - A delete-only push has nothing after the flush.
- **The push report through side-band is accepted.** git took `pkt("\x01" + pkt("unpack ok\n") + pkt("ok refs/heads/main\n") + "0000") + "0000"` and printed `* [new branch] main -> main`.
- **go-git v5.19.3 facts used below:**
  - `packfile.Parser` hands observers a nil body during delta resolution, so the connectivity check reads objects in a second pass, not inside the parse.
  - A reference delta whose base is not in the pack fails with `packfile.ErrReferenceDeltaNotFound` when no storage is given, so a thin pack is refused for free.
  - `Scanner.Checksum` verifies the trailer, but only at the position where the scanner thinks the pack ends.
  - `packfile.NewPackfile(index, nil, file, 0)` reads objects with delta resolution from a `billy.File`, returning in-memory objects when `fs` is nil.
  - `idxfile.Writer` is a parser observer that builds a `*idxfile.MemoryIndex`. `idxfile.NewEncoder` and `NewDecoder` write and read the version 2 format.
  - `sideband.NewMuxer(sideband.Sideband64k, w)` frames data on band 1.
  - `packp.ReferenceUpdateRequest.Decode` leaves the rest of the body, the pack, in `req.Packfile`.

## Out of scope for this piece

- **The ref snapshot (`.snap`) and its import** (build step 13). The push transaction does not read the full ref set yet.
- **`littlekittyvcs gc`** and the crash and hold hooks (build steps 8 and 9).
- **Repacking.**
- **Shallow clone.**
- **Smarter fetch answers** (sending only what the client lacks). Every fetch gets every pack.
- **A process-wide chunk cache.** `objcrypt.Reader` already keeps its two most recent chunks. The shared cache is sized from the clone measurement in build step 15.
- **Progress messages on side-band channel 2.**
- **Peeled tag lines in the advertisement.**
- **Repository delete.**

## Rules for whoever implements this

- **Leave every change unstaged.** No `git add`, `git stash`, `git checkout`, `git reset`, `git restore`, `git clean`, no commits.
- **No recursive or forced deletes**, and never a variable in a recursive delete path. The one delete in production code is the spool file. It uses `os.Remove` on the exact path `os.CreateTemp` returned, in a `defer`. Never `os.RemoveAll`. Tests use `t.TempDir()` for files and `internal/testdb` for schemas.
- **No backward compatibility.** Nothing has shipped. No migration file is added: the schema from piece 2 already has every column this needs. When a signature below replaces one from pieces 2 or 3 (`ReadRepo`, `httpapi.New`), change every caller and delete the old form in the same change.
- **No new settings.** Limits below are constants in code, named and commented with the reason.
- **Never print a secret or repository content.**
  - Token plaintext, `MASTER_KEY`, and data keys never reach a log line or an error.
  - Push logs carry counts and ids (repository id, pack id, object count, byte sizes, number of refs updated). They do not carry ref names or object ids: those are inside the ciphertext so a bucket holder cannot read them, and the logs should not undo that.
- **Plaintext never touches the disk.** The spool file is written in the final encrypted format from the first byte.
- **Check every go-git name below against v5.19.3 source.** List any difference in the report.
- **Do not change `internal/objcrypt` or `internal/blob`.** Call them. If one cannot do what this piece needs, stop and report it.
- Wait for `docker exec lkv-piece02-postgres pg_isready -U lkv -d lkv -h 127.0.0.1` before running the suite.

## Setup

No new direct dependencies. go-git v5.19.3 and go-billy (already indirect, through go-git) are enough. `go mod tidy` may move go-billy to a direct requirement because `packstore` implements `billy.File`. That is expected.

Containers from earlier pieces:
- `lkv-piece02-postgres` (PostgreSQL)
- `lkv-piece01-rustfs`: the S3 stand-in, at `127.0.0.1:19000`, access key `lkvtest`, secret `lkvtestsecret`, bucket `lkv-test`, path style.

Start each, use it, and stop it again. Never remove a container or its volume.

```text
LKV_TEST_DATABASE_URL=postgres://lkv:lkvtest@127.0.0.1:15432/lkv?sslmode=disable
LKV_TEST_S3_ENDPOINT=http://127.0.0.1:19000   (and the other LKV_TEST_S3_* values from piece 1)
```

## Files

```text
internal/packstore/
  spool.go        Spool: request body -> encrypted temp file, with the checks that need the whole body
  parse.go        Parse: pack -> MemoryIndex, object list, encoded .idx
  file.go         fileSource (RangeSource over *os.File) and readOnlyFile (billy.File over a ReaderAt)
  pack.go         Pack: open a stored or spooled pack and Get objects with delta resolution
  closure.go      Refs: every object id a commit, tree, or tag points at
  keys.go         PackKey, IdxKey
  *_test.go
internal/objindex/
  objindex.go     Index: per repository, object id -> pack id; Refresh; Add
  objindex_test.go
internal/push/
  push.go         Handle: the receive path
  report.go       report-status, plain or through side-band
  push_test.go
internal/fetch/
  fetch.go        Handle: parse the request, answer NAK, stream the joined pack
  fetch_test.go
internal/store/
  repos.go        ReadRepo gains packs
  push.go         CommitPush: the push transaction
  push_test.go
internal/httpapi/
  gitroutes.go    shared access decision for info/refs and both POST routes
test/integration/
  helpers_test.go server setup, git runner, repository builders
  push_clone_test.go
```

## `internal/store`

### `ReadRepo` gains the pack list

```go
type Pack struct {
    ID       [16]byte
    PackKey  string
    IdxKey   string
    Bytes    int64 // ciphertext size of the .pack object
    IdxBytes int64 // ciphertext size of the .idx object
}

func (s *Store) ReadRepo(ctx context.Context, name string) (Repo, []Ref, []Pack, error)
```

The packs are read in the same repeatable-read, read-only transaction as the refs, ordered by `created_at, id`. Then a ref is never seen without the pack that holds its objects. Update every caller (`info/refs` ignores the packs).

### `CommitPush`

```go
type RefUpdate struct {
    Name string
    Old  *[20]byte // nil: the ref must not exist (create)
    New  *[20]byte // nil: delete
}

type NewPack struct {
    ID       [16]byte
    PackKey  string
    IdxKey   string
    Bytes    int64
    IdxBytes int64
}

type Push struct {
    RepoID     [16]byte
    CreateName string    // non-empty: create the repository row first (AUTO_CREATE)
    Pack       *NewPack  // nil when the push stored no pack (delete-only, or every object already stored)
    Updates    []RefUpdate
}

var ErrStale = errors.New("store: a ref changed since it was read")

// CommitPush applies the whole push in one transaction or nothing.
func (s *Store) CommitPush(ctx context.Context, p Push) (pushSeq int64, err error)
```

Inside one read-committed transaction, in this order:

1. **Create, if asked.** When `CreateName` is set:
   - insert the repository row and its `HEAD` row (`refs/heads/main`), as `CreateRepo` does
   - a taken name returns `ErrExists`, and the push reports every ref failed
2. **Lock and count.** `update repositories set push_seq = push_seq + 1 where id = $1 returning push_seq`. This row lock serializes pushes to one repository for the length of this short transaction. Zero rows means the repository was deleted: return `ErrNotFound`.
3. **Each update, as a compare-and-swap.** Any statement affecting zero rows returns `ErrStale`. The transaction rolls back, including the pack row.
   - Create (`Old` nil, `New` set): `insert into refs (repo_id, name, object_id) values (...) on conflict do nothing`
   - Update: `update refs set object_id = $new where repo_id = $1 and name = $2 and object_id = $old`
   - Delete (`New` nil): `delete from refs where repo_id = $1 and name = $2 and object_id = $old`
4. **The pack row**, when `Pack` is set.
5. **Fix `HEAD`.** If `HEAD`'s symbolic target is not a ref after the updates, and this push created a ref under `refs/heads/`, point `HEAD` at the first such ref in command order.
   - This is what makes a repository whose first push was `master`, not `main`, clone with files checked out.
   - In every other case `HEAD` is left alone.
6. Commit, and return the new `push_seq`.

`CommitPush` uses the same error filter as the other request-path methods. `ErrStale`, `ErrExists`, and `ErrNotFound` pass through it unchanged.

## `internal/packstore`

### Keys

```go
func PackKey(repoID, packID [16]byte) string // "repos/<repo uuid>/packs/<pack uuid>.pack"
func IdxKey(repoID, packID [16]byte) string  // "repos/<repo uuid>/packs/<pack uuid>.idx"
```

They use `store.FormatID` for both uuids.

### `Spool`

```go
// MaxPushBytes is the largest pack accepted, in plaintext bytes. App Platform gives
// at most 2 GiB of ephemeral disk; 1.5 GiB leaves room for the index and the process.
const MaxPushBytes = 1536 << 20

type Spooled struct {
    Path       string // the temp file; the caller removes it with os.Remove
    Size       int64  // ciphertext bytes in the file
    PlainSize  int64  // pack bytes
    ObjectCount uint32
}

func Spool(ctx context.Context, body io.Reader, dir string, kr *objcrypt.Keyring, b objcrypt.Binding) (*Spooled, error)
```

- **Temp file:** `os.CreateTemp(dir, "lkv-push-*.spool")`, with `dir` = `os.TempDir()` in production and `t.TempDir()` in tests. On any error, close and `os.Remove` that exact path before returning.
- **Encrypt as it writes:** the body goes through `objcrypt.NewWriter(file, kr, b, WriterOptions{})` with `b.Kind = KindPack`, so the file *is* the object that will be uploaded. `Writer.CiphertextSize()` gives `Size`.
- **Count while writing:**
  - a running SHA-1 over every byte except the last 20 (keep a 20-byte tail buffer)
  - the plaintext byte count
- **Refuse** (each a distinct error, reported to git as the `unpack` reason):
  - more than `MaxPushBytes`, checked while streaming, not after
  - fewer than 32 bytes
  - a header that is not `PACK`, version 2 or 3, followed by the object count
  - a last 20 bytes that is not the SHA-1 of everything before it
- `ObjectCount` comes from the header.

### `Parse`

```go
type Parsed struct {
    Index   *idxfile.MemoryIndex
    IdxBytes []byte              // idxfile-encoded, plaintext; the caller encrypts it
    Objects map[[20]byte]plumbing.ObjectType
}

func Parse(ctx context.Context, sp *Spooled, kr *objcrypt.Keyring, b objcrypt.Binding) (*Parsed, *Pack, error)
```

1. **Open the spool.** Use `objcrypt.Open` over a `fileSource` on the spool file. `fileSource.GetRange` is `ReadAt` on the `*os.File`, returning exactly `n` bytes or an error. Wrap the reader in `io.NewSectionReader(rd, 0, rd.Size())`. It is an `io.ReadSeeker`, so `packfile.NewScanner` marks it seekable and the parser can go back for delta bases.
2. **Parse.** Run `packfile.NewParser(scanner, idxWriter)` with an `idxfile.Writer` and `Parse()`.
   - A missing delta base (`ErrReferenceDeltaNotFound`, a thin pack) is a refusal.
   - So is any other parse error.
3. **The pack must end where the body ends.** `Parse` returns the trailer it verified at the end of the last object. It must equal the last 20 bytes of the body.
   - Without this check, bytes appended after a valid pack would pass both checksums and then be copied into every clone by the joined-pack fetch.
   - With both checks together (this one and the spool's SHA-1 over the body), the parser's end is the body's end.
4. **Collect the results:**
   - `Index` from `idxWriter.Index()`
   - `IdxBytes` from `idxfile.NewEncoder(&buf).Encode(index)`
   - `Objects` from the observer's `OnInflatedObjectHeader` and `OnInflatedObjectContent` calls. Write a tiny observer that records type by hash.
5. **Return a `*Pack` over the spool** for the checks below.

### `Pack`

```go
// Pack reads objects from one pack, spooled or stored, through the encrypted reader.
// Not safe for concurrent use. One per request.
type Pack struct{ /* packfile.Packfile over readOnlyFile */ }

func OpenStored(ctx context.Context, src objcrypt.RangeSource, key string, size int64, kr *objcrypt.Keyring, b objcrypt.Binding, idx *idxfile.MemoryIndex) (*Pack, error)
func (p *Pack) Get(id [20]byte) (plumbing.ObjectType, []byte, error) // ErrObjectNotFound when absent
func (p *Pack) Close() error
```

- `readOnlyFile` implements `billy.File` over an `io.SectionReader`: `Read`, `ReadAt`, `Seek`, `Close`, and `Name()`, which returns the key. `Write`, `Truncate`, `Lock`, and `Unlock` return an error.
- Build with `packfile.NewPackfile(idx, nil, file, 0)`. With `fs` nil it returns in-memory objects, which is what we want: there is no filesystem.

### `Refs`

```go
// Refs returns every object id obj points at:
//   commit: tree and parents
//   tree:   entries, except mode 160000 (a submodule's commit lives in another repository)
//   tag:    target
//   blob:   nothing
func Refs(t plumbing.ObjectType, content []byte) ([][20]byte, error)
```

Decode with go-git's `object.Commit`, `object.Tree`, and `object.Tag` over a `plumbing.MemoryObject`. Skipping mode 160000 is required: a push of a repository with submodules is otherwise refused for objects that can never be here.

## `internal/objindex`

```go
type Index struct{ /* mutex; map repo id -> *repoIndex */ }

func New() *Index

// Refresh loads every pack in packs that this process has not loaded yet:
// GetRange the whole .idx object, decrypt, idxfile.Decode, add its ids.
// Called at the start of every POST, with the pack list ReadRepo returned.
func (x *Index) Refresh(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, repoID [16]byte, packs []store.Pack) error

// Add records a pack this process just committed, without re-reading it.
func (x *Index) Add(repoID [16]byte, p store.Pack, idx *idxfile.MemoryIndex)

// Lookup returns which pack holds id, and that pack's index.
func (x *Index) Lookup(repoID [16]byte, id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool)
```

- Per repository: object id to pack id, pack id to `(store.Pack, *MemoryIndex)`, and the set of pack ids loaded.
- An id found in two packs (duplicates are legal, as measured) maps to either. Both hold the same bytes, because Git ids are content hashes.
- Refresh holds a per-repository lock while it loads, so two requests do not decrypt the same index twice. Lookups never block on another repository's load.
- Packs are only added in this piece. Repository delete and repack, later, will need a way to drop a repository's entry. Do not build it now.
- Memory is about 60 bytes per object plus the `MemoryIndex`. Write that in the package comment, with a note that a repository of millions of objects is a known cost for version 1.

## `internal/push`

```go
type Deps struct {
    Store              *store.Store
    Bucket             blob.Store
    Keyring            *objcrypt.Keyring
    Index              *objindex.Index
    SpoolDir           string
    DenyNonFastForward bool
    Log                *slog.Logger
}

// Target is decided by httpapi before Handle runs: who may push where.
type Target struct {
    Name   string
    Repo   *store.Repo   // nil: the repository does not exist and AUTO_CREATE allows creating it
    Refs   []store.Ref
    Packs  []store.Pack
}

func Handle(d Deps, w http.ResponseWriter, r *http.Request, t Target)
```

In order:

1. **Request.**
   - `Content-Type` must be `application/x-git-receive-pack-request`: otherwise **415**.
   - The response content type is `application/x-git-receive-pack-result`, with `Cache-Control: no-cache`.
   - A `Content-Encoding` other than none is **415**. git does not compress pushes (measured: plain body).
2. **Commands.** Decode with `packp.NewReferenceUpdateRequest().Decode(r.Body)`.
   - Refuse with **400** and a one-line text body, before reading any pack:
     - shallow lines
     - zero commands
     - a name not starting with `refs/` or failing `plumbing.ReferenceName.Validate()`
     - the same name twice
     - old and new both zero
   - Remember whether `report-status` and `side-band-64k` were requested.
3. **Repository id.** Use `t.Repo.ID`, or `store.NewID()` when creating. It goes into every binding.
4. **Pack.**
   - If every command is a delete, the body must have nothing left: one read returning `io.EOF`. Otherwise refuse.
   - If any command is not a delete: `packID := store.NewID()`, then `Spool` then `Parse`, with binding `{RepoID, packID, KindPack}`. A failure here reports `unpack <reason>` and every command `ng <ref> unpacker error`.
5. **Refresh.** `Index.Refresh` with `t.Packs`.
6. **Checks.** Any failure reports `unpack ok` and every command `ng <ref> <reason>`. All or nothing: one bad command fails the whole push.
   1. **Connectivity, by closure.** For every object in `Parsed.Objects` of type commit, tree, or tag, every id from `Refs` must be in `Parsed.Objects` or found by `Index.Lookup`. Reason: `missing object`.
      - Why this is enough: every pack this server stores was held to the same rule against the packs before it. So by induction, every object in any stored pack has everything it points at stored too. A push can then only be corrupt through its own objects, and those are all checked here.
      - It reads no stored pack, only indexes.
      - It replaces the main plan's "walk from each new tip, stopping at objects reachable from current refs". That walk had to compute reachability from refs, and it trusted any object found in a stored pack even when no ref reached it.
   2. **Every new id exists:** in the new pack or the index. Reason: `missing object`.
   3. **A new id under `refs/heads/` must be a commit.** The type comes from `Parsed.Objects`, or from reading the object in its stored pack. Reason: `not a commit`.
   4. **Fast-forward.** For each update under `refs/heads/` with old and new both set, while `DenyNonFastForward` is true:
      - Walk breadth-first from new over commit parents, with a visited set, reading each commit from the new pack or the stored pack `Index.Lookup` names. Open each stored `Pack` once per request.
      - Finding old: fine.
      - Exhausting the walk: reason `non-fast-forward`.
      - Tags and other refs are not checked, matching git's `receive.denyNonFastForwards`.
      - This walk can read a lot of history from the bucket on a rejected push. That is a known cost for version 1.
7. **Already applied.** If every command already matches the current refs in `t.Refs` (each update's ref is at new, and each delete's ref is absent), report every command `ok` and stop. Do not upload anything. This is the retry of a push whose response was lost.
8. **Store the pack.** Skip this when `ObjectCount` is 0: nothing to store, `Push.Pack` stays nil.
   - Put the spool file as it is, with its `Size`.
   - Encrypt `IdxBytes` with binding `{RepoID, packID, KindIdx}` into a buffer, and Put that.
   - Both go up before the transaction. An upload error reports every command `ng <ref> storage error`. The uploaded objects stay for `gc`.
9. **`CommitPush`**, with `Old` from each command's old id (nil when zero).
   - `ErrStale` reports every command `ng <ref> failed to update ref`.
   - `ErrExists` (an auto-create race) reports the same.
   - Any other error reports `ng <ref> storage error` and is logged.
10. **After commit.** `Index.Add` the new pack, so the next request here sees it without a refresh. Log one info line with the counts and ids listed in the rules.
11. **Report.**
    - With `report-status`: `packp.ReportStatus{UnpackStatus: "ok" or reason, CommandStatuses: ...}` encoded. When `side-band-64k` was requested, wrap that whole encoding in band 1 with `sideband.NewMuxer` and write a final flush.
    - Without `report-status`: an empty 200.
    - Status is 200 whenever the request got as far as reading commands. git reads the result from the body.

The spool file is removed in a `defer` placed right after `Spool` returns, on every path.

## `internal/fetch`

```go
type Deps struct {
    Bucket  blob.Store
    Keyring *objcrypt.Keyring
    Log     *slog.Logger
}

type Target struct {
    Repo  store.Repo
    Refs  []store.Ref
    Packs []store.Pack
}

func Handle(d Deps, w http.ResponseWriter, r *http.Request, t Target)
```

1. **Request.**
   - `Content-Type` must be `application/x-git-upload-pack-request`.
   - `Content-Encoding: gzip` is decoded with `gzip.NewReader`. git compresses fetch requests over 1024 bytes, which a fetch with many local commits sends.
   - Any other encoding is **415**.
   - Limit the decoded body to 10 MiB, which is about 200,000 `have` lines, with an `io.LimitReader`. Past that is **400**.
2. **Parse the pkt-lines** with go-git's `pktline.Scanner`:
   - `want <id>[ <caps>]` lines, capabilities on the first only, then a flush
   - then any number of `have <id>` lines, optionally broken by flushes
   - then optionally `done`
   - `shallow`, `deepen`, `deepen-since`, `deepen-not`, and `filter` lines: **400** `shallow and partial clone are not supported`.
3. **Every want must be the current id of a direct ref in `t.Refs`.** Otherwise answer `ERR upload-pack: not our ref <id>` as a pkt-line, with status 200. We do not advertise `allow-tip-sha1-in-want`.
4. **The response** has content type `application/x-git-upload-pack-result` and `Cache-Control: no-cache`.
   - **No `done`:** write `pkt("NAK\n")` and return. That is one stateless round, the first of a multi-round negotiation.
   - **`done`:** write `pkt("NAK\n")`, then the joined pack (below), on band 1 when `side-band-64k` was requested, otherwise raw. With side-band, finish with a flush.
5. **The joined pack:**
   - Open every pack in `t.Packs` with `objcrypt.Open`.
   - Read each pack's 12-byte header for its object count, and sum them.
   - Write `PACK`, version 2, and the sum. Then, for each pack in order, copy bytes `[12, PlainSize-20)` through a `SectionReader` with a 1 MiB `io.CopyBuffer`. Write the SHA-1 of everything written as the trailer.
   - Offset deltas point backwards inside their own pack, and each pack's objects stay contiguous, so every offset still holds. Reference deltas do not occur, because thin packs are refused.
   - Duplicate objects across packs are accepted by git (measured).
   - A pack with version 3 in its header is copied the same way. The joined header says version 2, and the object encoding of the two versions is the same for SHA-1 repositories. Write that in a comment, with this plan's reference.
6. **A failure after the first byte** cannot change the status any more. With side-band, write `ERR` text on band 3, then stop. Without side-band, stop writing: the client fails on the bad trailer. Log the error either way.

## `internal/httpapi`

```go
type Deps struct {
    Store   *store.Store
    Bucket  blob.Store
    Keyring *objcrypt.Keyring
    Index   *objindex.Index
}

type Config struct {
    AnonymousRead      bool
    AutoCreate         bool
    DenyNonFastForward bool
    PublicURL          string
}

func New(log *slog.Logger, d Deps, cfg Config) http.Handler
```

- New routes: `POST /{repo}/git-upload-pack` and `POST /{repo}/git-receive-pack`.
- Move the access decision out of `infoRefs` into `gitroutes.go`, and call it from all three Git routes, so a POST can never be looser than its `info/refs`:
  - the token check
  - the anonymous rules
  - 404 for a missing repository or another repository's token
  - 403 for a read-only token on push
  - the `AUTO_CREATE` exception
- `ReadRepo` runs once per request. Its refs and packs become the `Target`.
- `runServe` builds one `objindex.New()` and passes the S3 bucket, the keyring, and `DenyNonFastForward`. `SpoolDir` is `os.TempDir()`.

## Tests

Run `go vet ./...` and `go test ./... -count=1` once at the end, with the database variable set (after `pg_isready`) and once without. Database tests skip without it, git tests skip without the `git` binary, and the rustfs test skips without `LKV_TEST_S3_ENDPOINT`.

Build test repositories with the `git` binary in `t.TempDir()`, in the scratch environment from piece 3:
- `HOME`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `PATH`
- fixed author and committer names, emails, and `GIT_AUTHOR_DATE` and `GIT_COMMITTER_DATE`, so ids are stable

Make packs with `git pack-objects --revs --stdout`, giving tips and `^exclusions` on stdin. The server under test is `httpapi.New` on a real store and `blob.Mem`, behind `httptest.NewServer`, unless a case says otherwise.

### `internal/packstore` (git binary, no database)

1. **Round trip.**
   - Spool and parse a pack of a repository with 50 commits, a tree three levels deep, and binary files.
   - `Objects` has every id `git rev-list --objects --all` prints, with the right type.
   - `Get` returns content identical to `git cat-file <type> <id>` for every object.
   - The spool file's bytes do not start with `PACK`, and do not contain a 40-byte sentinel string written into one of the files.
2. **Refusals**, each with its distinct error:
   - a thin pack (`git pack-objects --thin --revs` with `C` and `^P` on stdin, where C's blob is stored as a delta against a blob of P). Make the delta likely by changing one line of a large file.
   - a truncated body
   - one flipped byte in the middle
   - a wrong trailer
   - 20 valid extra bytes appended after a valid pack (the trailer of the pack repeated). This is the case where the parser's own checksum check passes.
   - a pack header with version 4
   - a body of 10 bytes
3. **`MaxPushBytes`.** Run the spool limit with a smaller value through an internal test hook, a package variable the test sets, so the test does not write 1.5 GiB. Bytes over the limit are refused while streaming.
4. **`Refs`:**
   - a commit gives tree and parents
   - a tree with a mode 160000 entry leaves that entry out
   - an annotated tag gives its target
5. **The spool file is gone** after a failed `Spool`: the temp directory is empty.

### `internal/objindex`

With `blob.Mem`:
- Two stored packs loaded by `Refresh`, then a third added with `Add`. Lookup finds ids from all three.
- A second `Refresh` with the same list reads no index again (count `RangeCalls`).
- A fresh `Index` rebuilds from `Refresh` alone.

### `internal/store/push_test.go` (PostgreSQL)

1. Create a branch, update it, delete it, each through `CommitPush`. `push_seq` goes 1, 2, 3.
2. **A stale old id** returns `ErrStale`, no ref moved, no pack row inserted, and `push_seq` unchanged.
3. **A create of an existing ref** returns `ErrStale`.
4. **Two goroutines** push different new ids onto the same branch from the same old id, both released by one channel close. Exactly one returns nil. The ref equals the winner's new id. Exactly one pack row exists.
5. **`CreateName`:** the repository and its `HEAD` row are created. A second push with the same `CreateName` returns `ErrExists`.
6. **`HEAD` fix-up:**
   - A new repository whose first push creates only `refs/heads/master` gets `HEAD` pointing at `refs/heads/master`.
   - A push creating `refs/heads/dev` into a repository whose `main` exists leaves `HEAD` alone.

### `test/integration/push_clone_test.go` (PostgreSQL and git)

Each case starts from a new repository on the server.

1. **Push one commit, clone it.** This is build step 4.
   - `git push` (Bearer header, read-write token) of one commit with a text file, a binary file, and a subdirectory exits 0 with `[new branch]`.
   - `git clone` into a second directory exits 0.
   - Every file is byte-identical to the source. `git -C clone fsck --full` is clean. `git -C clone log --format=%H` equals the source's.
   - Every object in `blob.Mem` under `repos/` is checked against the source repository's objects:
     - it does not start with `PACK`
     - it does not contain the text file's sentinel line
     - it does not contain any commit id as 40-char hex or as 20 raw bytes
2. **Second push, fetch, pull.**
   - Push two more commits (fast-forward). Two pack rows now exist.
   - A fresh clone has all three commits and a clean `fsck`.
   - In the first clone, `git pull` brings it to the new tip and `fsck` is clean.
3. **Fetch with a compressed request.**
   - Make 40 local commits in a clone without pushing them, then `git fetch` after another push from elsewhere.
   - The server sees `Content-Encoding: gzip` (a counting wrapper in the test) and the fetch succeeds.
   - This case also runs a negotiation of several rounds. git sends `have` lines in batches, and each answer without `done` is a bare `NAK`. That was **not** measured before this plan: the measured fetch had one `have` and sent `done` in its first request.
   - Report the number of POSTs and whether each had `done`. If git does not end with `done` after the bare `NAK` answers, stop and report it with the request bodies (`%q`, first 300 bytes each). Do not invent a different answer.
4. **Non-fast-forward.** This is build step 6.
   - With the default settings, push a commit not descended from the tip, using `--force`. git exits non-zero, stderr contains `non-fast-forward`, and the ref is unchanged (ls-remote).
   - With `DenyNonFastForward` false on a second handler over the same store and bucket, the same `--force` push succeeds.
5. **Delete a branch.** This is build step 7.
   - Push `dev`, then `git push origin :dev`. `ls-remote` no longer lists `dev`, and a fresh clone has no `origin/dev`.
   - No new pack row was written for the delete.
6. **Concurrent pushes.** This is build step 5, through raw HTTP.
   - Build two request bodies: the command line with the same old id, two different new commits, a flush, and each one's pack.
   - POST both at once.
   - Exactly one report says `ok refs/heads/main`, and the other says `ng refs/heads/main failed to update ref`.
   - `ls-remote` shows the winner, and a fresh clone passes `fsck`.
7. **Missing parent.** This is build step 10, through raw HTTP.
   - A pack of commit C without its parent P (`C` and `^P` on stdin), where P was never pushed.
   - The report says `ng refs/heads/main missing object`, and no ref, pack row, or bucket object was added.
8. **Thin pack through raw HTTP.**
   - P is pushed first. Then send a `--thin` pack of C against P.
   - The report's unpack line is not `ok`, and no ref moved.
   - This proves the refusal does not depend on the client honouring `no-thin`.
9. **Retry of an applied push.**
   - Build one body that creates `refs/heads/retry` (old id zero) with a full pack of a new commit, and POST it twice.
   - Both reports are `ok refs/heads/retry`.
   - The second POST uploaded nothing: the count of bucket objects and of pack rows is the same after it as after the first.
10. **Two server processes.** This is build step 11.
    - Handlers A and B share the store and bucket, with separate `objindex.Index` values.
    - Push through A, and clone through B immediately after. The clone has the commit.
    - Then push a child commit through B. B must refresh to find A's pack for the connectivity check. A fresh clone through A has it.
11. **Restart.** This is build step 12. A third handler with a new index, created after cases 1 and 2, clones and fetches successfully.
12. **Auto-create.**
    - With `AutoCreate` true, push to a name that does not exist. The repository row now exists, and a clone works.
    - The same push with `AutoCreate` false gets 404 at `info/refs`.
13. **First branch is not `main`.**
    - Push only `master` to a new repository.
    - The clone has its files checked out, and stderr has no `remote HEAD refers to nonexistent ref`.
14. **Submodule.**
    - Push a commit whose tree has a submodule entry, a gitlink to a commit that exists nowhere on the server. The push is accepted.
    - The clone's `git ls-tree HEAD` shows the `160000` entry.
15. **A delete-only push with a pack appended** (raw HTTP) is refused with 400, and nothing changes.
16. **Read-only token** gets 403 on `POST /git-receive-pack` directly (raw HTTP), not only at `info/refs`.
17. **A token for another repository** gets 404 on `POST /git-upload-pack`.

### `test/integration` against rustfs (optional, `LKV_TEST_S3_ENDPOINT` set)

- Push a commit containing a 20 MiB random file, which cannot compress. That makes the stored pack over 16 MiB, so the multipart path runs.
- Clone it back, and check the file's SHA-256 matches.
- Report the pack object's ciphertext size.

## Done means

The report back must contain this table, every row filled with evidence (command output or file path), or the piece is not done:

| Item | Evidence |
|---|---|
| `go vet ./...` clean | |
| `go test ./... -count=1` without the database: passes, list the skipped tests | |
| `go test ./... -count=1` with it, after `pg_isready`: every test passes | |
| Push-then-clone: the push stdout and stderr, the `fsck` output, and the file-compare result | |
| The bucket check: number of objects inspected and what each was checked for | |
| The concurrent-push reports, both of them, verbatim | |
| The missing-parent report and the thin-pack report, verbatim | |
| Non-fast-forward stderr | |
| Gzip fetch: the request count that carried `Content-Encoding: gzip` | |
| The rustfs run: pack ciphertext size and the clone's SHA-256 match, or "skipped" with the reason | |
| Every go-git name in this document that turned out different | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
