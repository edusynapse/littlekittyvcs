# Piece 7: fetch negotiation and peeled tags

After this piece:
- `git fetch` and `git pull` receive only the objects the client does not have. Today every fetch receives every stored pack joined into one (`internal/fetch/fetch.go`: `Handle` answers `NAK` and `writeJoined` on every request).
- A `git clone` and a fetch from a client that has nothing in common with the server still get the joined pack. That path is cheap for the server (sequential reads of whole packs) and stays as it is.
- The upload-pack advertisement carries peeled tag lines (`<id> refs/tags/v1^{}`). Without them, `git fetch` does not pick up a new annotated tag. That was measured below.

It builds:

1. **`internal/store`**: a `peeled` column on `refs`, written by `CommitPush` and `RestoreRepo`, read by `readRefs`.
2. **`internal/packstore`**: the object reader that push already has (`opener` in `internal/push/push.go`) moves here so push, fetch and restore share it, plus a `Peel` function.
3. **`internal/push`**: computes the peeled id of every ref update whose new object is an annotated tag.
4. **`internal/snapshot`**: restore computes the peeled ids of the refs it restores.
5. **`internal/gitproto`**: `multi_ack_detailed` in the upload-pack capabilities, and peeled lines in the upload-pack advertisement.
6. **`internal/fetch`**: negotiation, the commit and tree walk, and a streaming pack writer.
7. **`README.md`** and the main plan.

Done means: the tests below pass, including a fetch that receives exactly the new objects and a fetch that picks up both a new tag on a new commit and a new tag on an old commit.

## Measured on 4 Nov 2026

All of this was measured with git 2.34.1 against git's own `git http-backend`, behind a proxy that replaced the upload-pack capability list and logged every request and response. The client was forced to protocol v0 (`protocol.version=0`). This server ignores the `Git-Protocol` header, so real clients speak v0 to it.

Fixture: a server repository with 100 commits on `main`. Two clients cloned it at commit 60. One fetched the 40 new commits (the fast-forward case). The other first made 40 local commits that the server does not have, then fetched (the diverged case). Every commit touches one file, so each one adds a commit, a tree and a blob: 120 new objects.

**Without `multi_ack`, negotiation over HTTP does not work.** The client sends haves in rounds. When the server acknowledges a common commit, the client stops and sends `done`. But in that `done` request it sends **no haves**, so the server cannot know what the client already has. Logged requests: round 1 had `want` plus 16 haves, then `done` had `want` and 0 haves. git's own server then sent all 300 objects, and the client failed with `protocol error: bad band #65`, from ACK lines left over from round 1. So this server must offer `multi_ack_detailed`. In that mode the client repeats in each later request the commits the server acknowledged.

**`multi_ack_detailed` exchange, exactly as git's server answered:**

A negotiation round. The request is the want lines, a flush, have lines, and a flush. The response:
```
ACK <id> common        one line per have the server holds, in request order
...
ACK <id> ready         optional: git sends it when it decides it has enough
NAK                    always ends the round
```
A round where no have is known is just `NAK`.

The `done` request is the want lines, a flush, the haves the client was told are common (1 in the fast-forward case, 2 in the diverged case), and `done`. The response:
```
ACK <id> common        once per have in this request the server holds
ACK <last such id>     plain ACK, no suffix
<pack in side-band>
```
With no have in common, it is `NAK` and then the pack.

Results:
- **Fast-forward fetch:** 2 POSTs. Round 1 had 16 haves and was answered with 16 `common` ACKs, `ready` and `NAK`. Then `done` with 1 have. The pack had 120 objects, not 300.
- **Diverged fetch:** 4 POSTs. The first two rounds (32 local-only haves) got `NAK`. The third had 32 haves, with 24 `common`, no `ready`, and `NAK`. Then `done` with 2 haves. Again 120 objects.
- `fsck` was clean in both clients.

**Tags.** The server repository also had annotated tag `tag70` on new commit 70 and `tag20` on old commit 20. Both tags were created after the clients cloned.

| advertisement | tags the fetch created |
|---|---|
| `multi_ack_detailed`, no peeled lines | none |
| `multi_ack_detailed`, peeled lines | `tag20` and `tag70`. A second, small fetch asked for the `tag20` object. |
| `multi_ack_detailed` plus `include-tag`, no peeled lines | `tag70` only |

So the peeled lines are required, and `include-tag` is not enough on its own. This piece adds peeled lines and does not add `include-tag`.

**go-git cannot do the walk for us.** The main plan said "go-git's revlist against the custom storer". In go-git v5.19.3, `revlist.Objects` first computes the full set of objects reachable from the haves (`plumbing/revlist/revlist.go`: `ObjectsWithStorageForIgnores` calls `objects(ignoreStore, ignore, nil, true)`). That set covers every commit and every tree in the client's history, read through S3 on every fetch. And `packfile.Encoder.Encode` loads every object before writing the first byte (`plumbing/format/packfile/delta_selector.go`: `objectsToPack` calls `EncodedObject` for each hash). Both are unusable here. This piece writes its own walk and its own pack writer.

**go-git drops one peeled line.** `packp.AdvRefs` has a `Peeled` map, but when there is no `HEAD` the first sorted ref goes on the first line, and that line never gets a peeled line (`plumbing/protocol/packp/advrefs_encode.go`: `encodeFirstLine` writes none, and `encodeRefs` skips `firstRefName`). A repository whose only refs are tags, or whose `HEAD` branch does not exist, would lose the peeled line of its first tag. `gitproto` therefore writes the advertisement lines itself (below).

## Decisions already made

These were decided while writing this plan. Do not re-decide them.

- **Only `multi_ack_detailed`.** Not `multi_ack`, not `no-done`, not `include-tag`, not `thin-pack`, not protocol v2.
- **Never send `ACK ready`.** It is optional. Without it, the client stops sending haves once everything it would send is already known to be common (the fast-forward case went straight to `done` after one round). A test records the request count.
- **Which answer a `done` request gets:**
  - The client did not ask for `multi_ack_detailed`: today's answer, `NAK` and the joined pack. A plain-mode client sends no haves with `done` (measured), so there is nothing to negotiate.
  - It asked, but no have in the `done` request is in the object index: `NAK` and the joined pack. That covers every clone.
  - It asked, and at least one have is in the object index: the ACK lines above, then a **generated pack**.
- **A have is common when its id is in the object index** (`objindex.Lookup`), whatever its type. The index holds only objects of committed packs (`objindex.Refresh` loads only pack rows). Every stored object's references are stored too (the push closure rule), so excluding history the client has is safe.
- **Sending too much is always safe; sending too little is fatal.** git accepts objects it already has, and duplicates (measured in piece 4). Every rule below may fall back to sending more. No rule may leave out an object unless it was proven reachable from a common have.
- **No delta compression in a generated pack.** Each object is written whole and zlib-compressed. A fetch after a small edit to a large file sends the whole file. Reusing stored deltas, or computing new ones, is out of scope.
- **No new setting or environment variable, and no limit on walk size.** Nothing measured supports a number yet. Build step 15 measures it.
- **The peeled id is stored on the ref row**, not computed on each `info/refs`. Computing it there would read one tag object from S3 per tag per request.
- **The peeled id comes from the tag objects themselves.** `restore` recomputes it, and the snapshot format does not change. The objects are the one durable source.

## Out of scope

`include-tag`, `ready`, `no-done`, thin packs, delta reuse, shallow and partial clone (still refused), protocol v2, a process-wide chunk cache, and any change to the clone path.

## Rules for this piece

- Nothing in this piece logs a ref name, an object id, or a have or want line. Logs carry the repository uuid, counts and errors, as today.
- No new environment variable.
- Tests that touch PostgreSQL skip without `LKV_TEST_DATABASE_URL`, as today.

## Files

```
internal/store/migrations/0002_ref_peeled.sql   new
internal/store/repos.go                         Ref.Peeled; readRefs; RestoreRepo inserts it
internal/store/push.go                          RefUpdate.Peeled; CommitPush writes it
internal/packstore/objects.go                   new: Objects (was push's opener), Peel
internal/push/push.go                           uses packstore.Objects; computes Peeled
internal/snapshot/restore.go                    computes Peeled before RestoreRepo
internal/gitproto/advertise.go                  multi_ack_detailed; peeled lines; writes lines itself
internal/httpapi/inforefs.go                    passes peeled ids for upload-pack only
internal/httpapi/gitroutes.go                   passes the object index to fetch
internal/fetch/fetch.go                         request parsing; chooses joined or generated
internal/fetch/walk.go                          new: commit walk and tree walk
internal/fetch/packwriter.go                    new: streaming pack writer
README.md
dev_plans/git_s3_server_plan.md
```

## `internal/store`

`0002_ref_peeled.sql`:
```sql
alter table refs add column peeled bytea;
alter table refs add constraint refs_peeled check (
    peeled is null or (object_id is not null and octet_length(peeled) = 20));
```

- `Ref` gains `Peeled *[20]byte`. It is set only when `ObjectID` names an annotated tag. It is the first object down the tag chain that is not a tag.
- `readRefs` selects `peeled` and checks its length the same way it checks `object_id`.
- `RefUpdate` gains `Peeled *[20]byte`. It must be nil when `New` is nil.
- `CommitPush`:
  - The insert writes `peeled`.
  - The update sets `object_id = $new, peeled = $peeled` together. A ref that moves from a tag to a commit gets `peeled` null.
  - The delete is unchanged.
- `RestoreRepo` inserts `peeled` with each direct ref.
- `CreateRepo`'s `HEAD` row and every symbolic row keep `peeled` null.

The column's whole lifecycle:

| verb | where |
|---|---|
| create | `CommitPush` insert, `RestoreRepo` insert |
| read | `readRefs`, which feeds `ReadRepo`, then `info/refs` |
| update | `CommitPush` update, in the same statement as `object_id` |
| rebuild | `restore`, from the tag objects |
| delete | the ref row's delete, and the repository delete's cascade |

## `internal/packstore/objects.go`

Move `opener` from `internal/push/push.go` here, unchanged in behaviour, as:
```go
// Lookup finds the stored pack that holds id, and that pack's index.
type Lookup func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool)

// Objects reads objects of one repository from an optional fresh pack and
// from stored packs found through lookup. Each stored pack is opened once.
// Not safe for concurrent use.
type Objects struct { ... }
func NewObjects(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, repoID [16]byte, lookup Lookup, fresh *Pack) *Objects
func (o *Objects) Get(id [20]byte) (plumbing.ObjectType, []byte, error)  // absent: plumbing.ErrObjectNotFound
func (o *Objects) Close()

// Peel returns nil when id is not an annotated tag. Otherwise it follows
// tag targets until an object that is not a tag, and returns that id.
func Peel(o *Objects, id [20]byte) (*[20]byte, error)
```
**`packstore` must not import `objindex`.** `internal/objindex/objindex_test.go` is in `package objindex` and imports `packstore`, so that import would make `go test ./internal/objindex` fail with an import cycle. Callers pass `func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) { return index.Lookup(repoID, id) }`. `packstore` already imports `store` in production code (`internal/packstore/keys.go`), and `store` imports no other internal package except `testdb` in tests. So `packstore` importing `store` in production code makes no cycle either. Confirm with `go vet ./...`, which builds the tests too.

Push's fast-forward walk and connectivity check use `Objects` in place of `opener`. Delete `opener`. Do not keep both.

## `internal/push`

After the connectivity check passes and before `BeforeCommit`: for every command with a non-zero new id, find its type. Use `parsed.Objects` when the object is in the new pack. Otherwise use `Objects.Get`. If it is a tag, set `RefUpdate.Peeled` from `Peel`. A read error is `storage error` for every command, like the other storage failures in `check`.

The `alreadyApplied` retry path writes nothing and is unchanged.

## `internal/snapshot` (restore)

After the snapshot is chosen and before `RestoreRepo`, build `packstore.NewObjects` with a lookup on the restore's own index `idx` (`idx.Lookup(repoID, id)`), with no fresh pack, and set `Peeled` on each direct ref with `Peel`. A failure prints `failed <uuid> storage error`, is logged with the uuid, and counts as a failed repository, the same as the existing index-refresh failure. The snapshot's JSON is unchanged. `marshal` writes only the name and target, so `Peeled` never reaches it. Confirm that in `marshal` and do not change the format.

## `internal/gitproto`

- The upload-pack capabilities become `multi_ack_detailed side-band-64k ofs-delta`, then `agent=` and `symref=` as today.
- `Ref` gains `Peeled *[20]byte`. `httpapi.directRefs` fills it for upload-pack only. receive-pack advertisements carry no peeled lines, the same as git's own receive-pack.
- **Write the advertisement lines with `pktline` directly**, not through `packp.AdvRefs`, for the reason measured above. Keep every byte of today's output the same for refs without a peeled id, except the new capability. The existing tests in `internal/gitproto/advertise_test.go` pin the capability string (lines 19, 51 and 93). The only edit allowed there is `multi_ack_detailed ` at the start of the upload-pack string. The format:
  - the service prefix and a flush
  - the first line: `HEAD`'s id and `HEAD` when `HEAD`'s branch exists, otherwise the first ref by name, then NUL and the capabilities
  - then every other ref in name order
  - immediately after any ref with a peeled id, `<peeled> <name>^{}`, including after the first line
  - a flush
  - an empty repository keeps `capabilities^{}`, as today

## `internal/fetch`

### Request parsing

`parseRequest` also returns the have ids in request order, with duplicates removed, and whether the first want line asked for `multi_ack_detailed`. Everything else it does today stays: the shallow refusal, `not our ref`, the `0000` probe, and gzip.

### The handler

1. **Only when the request has at least one have and asked for `multi_ack_detailed`:** refresh the object index for the repository's packs (`Index.Refresh` with `Target.Packs`), as push does. A clone sends no haves, so a clone reads no index and costs what it costs today. `ReadRepo` read the refs and packs in one repeatable-read transaction, so every advertised object is in those packs.
2. `common` = the haves whose ids `Index.Lookup` finds. It is empty when step 1 did not run.
3. **Not `done`:**
   - with `multi_ack_detailed`: `ACK <id> common` for each common have, then `NAK`
   - without it: `NAK`, as today
4. **`done`, generated pack** (`multi_ack_detailed` and `common` not empty):
   - Run the walk below **before writing any header**. A walk error is a 500 `internal error`, logged with the uuid.
   - Then write `ACK <id> common` for each common have, `ACK <last common id>`, and the generated pack in side-band when the client asked for it. Then the flush, as today.
5. **`done`, otherwise:** today's `NAK` and joined pack, unchanged.

### The walk (`walk.go`)

Inputs: the wants and `common`. Output: the list of object ids to send. Every object read goes through one `packstore.Objects`, closed when the request ends.

**Starting points:**
- **Each want:** while the object is an annotated tag that is not in `common`, add the tag to the output and move to its target. A commit that ends the chain is an *interesting* start. A tree or blob that ends it goes to the tree step as a root to send whole. If any object in the chain is in `common`, stop there.
- **Each common have:** peel tags with `Peel`. A commit is an *uninteresting* start. A common tree or blob goes into the `known` set.

**Commit walk.** Read each commit when it is first queued, to get its committer time, parents and tree. Keep what was read: a commit queued a second time, with the other flag, is not read again. Keep a priority queue ordered by committer time, newest first, ties broken by id. Each commit has two flags, `interesting` and `uninteresting`. Repeat:
- Stop when no queued commit lacks `uninteresting`.
- Pop the newest.
- If it is `uninteresting`, give each parent `uninteresting` and queue the parents that were not already queued with that flag.
- Otherwise mark it `interesting`, give each parent `interesting`, and queue the parents not already queued.

**The commits to send** are those marked `interesting` and not `uninteresting`.

Why this is safe, which the tests rely on:
- `uninteresting` is only ever set by walking parents from a common have. So every commit left out is reachable from a common have, and the client has it.
- Every parent of a sent commit was queued. The loop only stops when every queued commit is `uninteresting`. So each parent of a sent commit is either sent or `uninteresting`.
- Clock skew in committer times can only cause more commits to be sent, never fewer.

**Tree step.** For each commit to send, compare its root tree with the root trees of all its parents, by path:
- Add the tree to the output unless its id is in `known`, already in the output, or equal to a parent's tree at the same path.
- For each entry:
  - Skip it if any parent's tree has an entry with the same name and the same id.
  - Skip a submodule entry (mode `160000`), as the closure rule does.
  - Otherwise send a blob unless it is in `known` or already in the output.
  - For a subtree, recurse with the parents' subtrees of the same name that are trees.
- A commit with no parents sends its whole tree.

Why it is safe: an object equal to a parent's at the same path belongs to that parent. By the commit rule above, that parent is either sent (and its own tree step covers it) or reachable from a common have. Objects that moved paths, or came back from older history, are sent again. That is allowed.

Blobs are never read during the walk. Only trees, commits and tags are.

**Order of the output:** tags, commits, trees, blobs, each in the order found. git accepts any order in a pack with no deltas.

### The pack writer (`packwriter.go`)

`writeGenerated(dst io.Writer, objs *packstore.Objects, ids [][20]byte) error`:
- Write the header `PACK`, version 2, and the count `len(ids)`.
- For each id: `Get` it, then write the standard pack object header (the type number 1 to 4 and the size as git's varint), then the content through `compress/zlib` at the default level.
- Write the SHA-1 of everything written as the 20-byte trailer.
- Hold one object's content at a time.
- An empty list writes a valid empty pack.
- Every `Write` handed to `dst` is at most `sidebandDataMax` (65515) bytes, through a small splitting writer. `writeJoined` stays within that limit for the same reason (`fetch.go`, the comment above `buf`).

### Memory

go-git's `Packfile`, which `OpenStored` builds with `packfile.NewPackfile`, keeps every object it fully reads in an LRU cache of up to 96 MiB per opened pack (`plumbing/cache/common.go`: `DefaultMaxSize`; `plumbing/format/packfile/packfile.go`: `cachePut` in `fillRegularObjectContent` and after `ApplyDelta`). So a generated fetch keeps what it reads in memory until the request ends. That is up to 96 MiB for each pack it touched. **Do not change this in this piece**, and do not add a cache-size constant. Build step 15 now also records peak memory during a fetch (below), and a later piece sets the size from that.

## `README.md`

- The "How a fetch works" paragraph: a clone gets every stored pack joined; a fetch negotiates and gets only what the client lacks, written whole with no deltas.
- Add that annotated tags are advertised peeled, so `git fetch` follows them.

## Main plan

In `dev_plans/git_s3_server_plan.md`:
- Fetch step 3: replace "go-git's revlist against the custom storer" with this piece's walk, and record why revlist was rejected.
- Fetch step 2: upload-pack capabilities include `multi_ack_detailed`.
- The ref row: add the `peeled` column to the schema block and to the ref row's lifecycle.
- Build step 15: also record peak memory during a fetch.

Leave the "First version does not include" list as it is.

## Tests

### `internal/packstore`
1. **`Peel`:** nil for a commit, the target for a tag, the final commit for a tag of a tag, and `ErrObjectNotFound` for a missing target. Use `blob.Mem` and packs built in the test.

### `internal/gitproto`
2. Every existing advertisement test passes, with only `multi_ack_detailed ` added at the start of the upload-pack capability strings.
3. The upload-pack advertisement of a repository with branch `main` and annotated tag `v1` has `<peeled> refs/tags/v1^{}` right after the `v1` line, and `multi_ack_detailed` is the first capability.
4. **No `HEAD` branch, only tag `v1`:** the first line is `v1` with the capabilities, and the next line is its peeled line. Confirm this test fails with go-git's encoder, so it guards the reason for writing the lines directly.
5. A receive-pack advertisement of the same repository has no `^{}` line.

### `internal/store` (PostgreSQL)
6. `CommitPush` with a `Peeled` stores it. Updating that ref to a commit with `Peeled` nil clears it. `ReadRepo` returns it.
7. `RestoreRepo` stores `Peeled`. The database refuses `peeled` on a symbolic ref and a `peeled` that is not 20 bytes.

### `internal/fetch` (unit, `blob.Mem`, packs built in the test)
8. **Parsing:** haves come back in order without duplicates, and the `multi_ack_detailed` flag is read from the first want line only.
9. **Round answers:** a round with two known haves and one unknown answers exactly `ACK <a> common`, `ACK <b> common`, `NAK`. With no known have it answers `NAK`. Without `multi_ack_detailed` it answers `NAK` even with known haves.
10. **The commit walk on a fixed graph.** Exact sets in each case:
    - **linear:** a common commit at 60 of 100 sends 61 to 100
    - **merge of an old side branch:** the side branch started from commit 10, is common at its own tip, and is merged at 100. It sends only the commits after the common tips. It does not walk into commit 10's history.
    - **added after review, side branch forked below the common commit:** the same merge graph, with only main's commit 60 common, not the side tip. The side commits are sent, and nothing from commit 10's history is. The case above cannot catch a walk that stops spreading `uninteresting` to parents, because there the side tip itself is common. With that spreading removed, every test passed while this case sent 53 commits instead of 43, 10 of them from commit 10's history.
    - **want equals a common commit:** sends nothing, and the pack is valid and empty
    - **clock skew:** a sent commit has a committer time older than a common commit. The result is a superset of the exact answer and never misses an object, checked by test 12
11. **The tree step:** a commit that changes one file in a nested directory sends exactly the commit, the trees on that path, and the one blob. A file restored to an old version sends that blob again (allowed). A submodule entry is not sent.
12. **Completeness check, the strongest guard.** For each graph in test 10, build the history with the test's git in a work repository, then:
    - Make a client repository holding exactly what the client has: `git init`, then `git fetch <work repo> <each common commit>`.
    - Write the generated pack to a file and run `git index-pack --stdin` inside the client repository.
    - Point a ref at each want with `git update-ref`.
    - Run `git fsck --connectivity-only`. It must pass.
    This is what proves nothing was left out. Also check that a deliberately broken walk fails it: drop one blob from the list in a test-only call, and the same fsck must fail.
13. **Pack writer:** a pack of a commit, a tree and a blob passes `git index-pack --stdin` (or `--strict`) with the test's git. No `Write` to the side-band destination is longer than 65515 bytes, including for a 200 KiB blob.

### `test/integration` (real git client, PostgreSQL, `blob.Mem` unless noted)

Count the upload-pack POSTs with a wrapper around the test server's handler. Count received objects with `git count-objects -v` before and after: `in-pack` grows by the objects received.

14. **Fast-forward fetch:** the fixture above, with no tags: 60 commits, then 40 more pushed from a second clone. Each commit writes the commit number into one of 7 files, so every blob is new. The fetch makes 2 upload-pack POSTs (with tags present, git makes one more fetch for a tag on an old commit, measured, so this test has none), `in-pack` grows by exactly 120, `origin/main` matches, and `fsck` is clean.
15. **Diverged fetch:** the client makes 40 local commits first. `in-pack` grows by exactly 120, and `fsck` is clean. Record the POST count in the report. Do not assert it.
16. **Tags:** `tag70` on new commit 70 and `tag20` on old commit 20, both pushed after the clone. A plain `git fetch` creates both tags. `git cat-file -t tag20` is `tag`. Also a tag of a tag, pushed and fetched, `git rev-parse tagtag^{}` equals the commit.
17. **Clone is unchanged:** a fresh clone still gets `NAK` and the joined pack. Check that the clone's first `done` response begins with `NAK`.
18. **Merge from an old branch:** a side branch from an early commit, merged after the clone. The fetch is clean, and `in-pack` grows by fewer objects than a full clone has.
19. **Restore keeps peeled lines:** push a branch and an annotated tag, restore into an empty database (as in the existing restore tests), and the `info/refs` body has the tag's `^{}` line.
20. **The connectivity guard still holds after the opener move.** The existing missing-object, missing-parent and non-fast-forward tests pass unchanged.
21. **rustfs (optional, skips without `LKV_TEST_S3_*`):** test 14 against rustfs.

## Done

Fill in every row. A row with no evidence is not done.

| Item | Evidence |
|---|---|
| `go vet ./...` clean | |
| `go test ./... -count=1` without the database: passes, list the skipped tests | |
| `go test ./... -count=1` with it, after `pg_isready`: every test passes | |
| Test 4 fails against go-git's `AdvRefs` encoder (how it was checked) | |
| Test 12: each graph, the fsck command and its exit status | |
| Test 14: POST count, `in-pack` before and after | |
| Test 15: POST count, `in-pack` before and after | |
| Test 16: the fetch output lines for both tags | |
| The rustfs run, or "skipped" with the reason | |
| `opener` is gone from `internal/push` (the grep that shows it) | |
| Every name in this document that turned out different in the code | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
