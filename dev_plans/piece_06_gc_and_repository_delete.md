# Piece 6: garbage collection and repository delete

After this piece:
- `DELETE /admin/repos/{name}` removes a repository at once. Its refs, packs and the tokens limited to it go with it in one transaction, and its bucket objects are left for `gc`.
- `littlekittyvcs gc [--dry-run]` deletes bucket objects nothing needs any more. Those are packs from pushes whose transaction failed, and every object of a deleted repository. It only deletes objects older than 24 hours.
- `gc` refuses to touch a repository whose database row is missing but whose snapshots say it was never deleted. That is a lost database, and the fix is `restore`, not deletion.
- A test-only hook in the push path between upload and commit proves build steps 8 and 9.

It builds:

1. **`internal/snapshot`**: a **delete marker**, a snapshot with `"deleted": true` and no refs. `restore` skips a repository whose newest readable snapshot is one. The key parser moves out of `restore.go` so `gc` uses the same one.
2. **`internal/store`**: `BumpPushSeq`, `DeleteRepo`, `PackIDs`.
3. **`internal/objindex`**: `Drop`.
4. **`internal/httpapi`**: the delete route, and a `PushHook` field passed through to the push path.
5. **`internal/push`**: calls `Deps.BeforeCommit`, when set, after the uploads and before `CommitPush`.
6. **`internal/gc`**: the collector.
7. **`cmd/littlekittyvcs`**: the `gc` subcommand.
8. **`README.md`** and the stale comment in `internal/objcrypt/format.go`.

Done means: build steps 8 and 9 of [git_s3_server_plan.md](git_s3_server_plan.md) pass, and a deleted repository's objects are gone from the bucket after a `gc` run whose clock is past the grace period.

The main plan's repository-delete and `gc` text was corrected on 4 Nov 2026 for this piece. Where this document and that one disagree, report it. Do not silently pick one.

## Decisions already made

These were decided while writing this plan. Do not re-decide them.

### Why the earlier delete rule was replaced

The main plan said repository delete removes the `.snap` objects first and then the database row, so `gc` could tell a deleted repository (no row, no snapshot) from a lost database (no row, snapshot present). **That rule has a race.** A push writes its snapshot after its transaction commits (`internal/push/push.go`: `writeSnapshot` runs after `CommitPush`), with up to 30 seconds to finish. If such a push commits just before the delete starts, its snapshot can land after the delete has removed the others. The repository is then gone from the database but has a normal snapshot, so `gc` refuses it forever and `restore` brings it back.

### The delete marker

A delete writes one more snapshot, a **delete marker**, numbered above every push, and only then deletes the row:

1. `BumpPushSeq` commits `push_seq + 1` and returns it as `N`. This uses the same row lock pushes use, so every push that committed before this has a lower number.
2. Write the marker `{RepoID, Name, PushSeq: N, Deleted: true}` with `snapshot.Write`. A failure is a 500, and the repository stays.
3. `DeleteRepo(id, N)`: `delete from repositories where id = $1 and push_seq = $2`. The schema's `on delete cascade` removes the ref, pack and token rows (`internal/store/migrations/0001_init.sql`). Zero rows means a push committed between steps 1 and 3. That returns `ErrStale`, and the route answers 409 `repository changed during delete; try again`.
4. **On `ErrStale`, the route deletes the marker it just wrote**, by its exact key (`snapshot.Write` returns it). The push that got in between has a higher number, so its snapshot normally outranks the marker anyway. But if that push's own snapshot write failed, the stale marker would be the newest readable snapshot of a live repository. Then, after a database loss, `restore` would skip it and `gc` would delete it. If removing the marker fails, log it with the key. That leaves the same small risk, and it is recorded.

**The delete removes no bucket objects.** `gc` removes them after the grace period. By then no late snapshot can still land, because a snapshot write gives up after 30 seconds. So nothing races the cleanup.

How the marker is read:
- **Restore:** if the highest-numbered readable snapshot is a marker, print `skipped <uuid> deleted`. When choosing a snapshot to restore, never choose a marker, so a stale marker below a later push is ignored.
- **`gc`:** a uuid with no row whose highest readable snapshot is a marker was deleted, so its objects may go. A uuid with no row whose highest readable snapshot is a normal one is a lost database, so nothing is deleted and `gc` says to run restore.

### Other decisions

- **One grace period, 24 hours, a named constant.** The tests move `gc`'s clock, never the constant.
- **`gc` deletes only keys that parse exactly** as a pack, index or snapshot of a uuid, using the same parser as `restore`, which checks that the parsed ids give back the same key. Any other key under `repos/`, and anything outside `repos/`, is counted and never deleted.
- **`gc` keeps every snapshot of a repository that has a row.** Pruning snapshots is not this piece.
- **The marker goes last.** Under a deleted uuid, `gc` deletes every other key first. It deletes the marker in the same run only if every other key of that uuid was deleted successfully in that run and the marker itself is past the grace period. Otherwise the marker stays for the next run.
- **`gc` exits 1 when it refused a repository** (lost database, or snapshots it cannot read) or when a delete failed. Both need an operator.
- **The push hook is a function field, nil in production.** It is not a setting and has no environment variable. Its comment says it exists for tests.
- **`objindex.Drop`** is called by the delete route in the process that served it. Other processes keep a stale entry until they restart. A deleted repository cannot be looked up through them, because `ReadRepo` returns 404 first. The cost is memory, and this is a known version 1 cost.

## Out of scope for this piece

- Pruning old snapshots of live repositories.
- Repacking. Its constraint on `restore` is already in the main plan.
- Repository rename.
- Deleting objects as part of the delete request.
- Running `gc` on a schedule. The operator runs it, for example from cron.

## Rules for whoever implements this

- **Leave every change unstaged.** No `git add`, `git stash`, `git checkout`, `git reset`, `git restore`, `git clean`, no commits.
- **No recursive or forced deletes on disk**, and never a variable in a recursive delete path. This piece adds no filesystem delete.
  - The bucket deletes in `gc` are one `blob.Store.Delete` per exact key, and only for a key the parser accepted.
  - Tests use `t.TempDir()`, `internal/testdb`, and `blob.Mem`. The optional rustfs test deletes only keys under the uuid it created, one key at a time, as the piece 5 rustfs test does.
- **No backward compatibility.** Nothing has shipped. The snapshot format gains `deleted`. Change every reader and writer in the same change.
- **No new environment variables.** The grace period and the dry-run choice are a constant and a flag.
- **Never print a secret or repository content.**
  - `gc`'s output carries keys, uuids, repository names and counts. Keys contain only uuids.
  - No ref names or object ids anywhere.
- **Do not change `internal/objcrypt` or `internal/blob`**, except the one comment named below.
- Wait for `docker exec lkv-piece02-postgres pg_isready -U lkv -d lkv -h 127.0.0.1` before running the suite.

## Setup

No new dependencies. Containers: `lkv-piece02-postgres` and `lkv-piece01-rustfs`, as in pieces 4 and 5. Start each, use it, and stop it again. Never remove a container or its volume.

## Files

```text
internal/snapshot/
  snapshot.go     Snapshot.Deleted; the format change
  keys.go         ParseKey (moved out of restore.go's classifyKey)
  restore.go      skips deleted repositories; never chooses a marker
internal/store/
  repos.go        BumpPushSeq, DeleteRepo, PackIDs
internal/objindex/
  objindex.go     Drop
internal/push/
  push.go         Deps.BeforeCommit
internal/httpapi/
  httpapi.go      Deps.PushHook; the delete route
  admin.go        deleteRepo
internal/gc/
  gc.go           Run
  gc_test.go
cmd/littlekittyvcs/
  main.go, run.go the gc subcommand
test/integration/
  gc_test.go
internal/objcrypt/format.go   comment only
README.md
```

## `internal/snapshot`

### The format

```go
type Snapshot struct {
    RepoID  [16]byte
    Name    string
    PushSeq int64
    Deleted bool
    Refs    []store.Ref // empty when Deleted
}
```

`Write` now returns the key it wrote: `func Write(...) (key string, err error)`. The push path ignores the key. The delete route needs it to remove a stale marker.

The JSON gains `"deleted"`. `marshal` always writes it. `unmarshal`:
- requires the field to be present: decode it into a `*bool` and refuse nil
- `deleted: true` requires an empty `refs` array. No `HEAD` is required.
- `deleted: false` keeps every rule from piece 5

### `ParseKey`

```go
type KeyKind int // KeyPack, KeyIdx, KeySnap

type ParsedKey struct {
    RepoID [16]byte
    ObjID  [16]byte
    Kind   KeyKind
}

// ParseKey returns ok only for a key that is exactly PackKey, IdxKey or Key of the ids it contains.
// hasRepo reports whether a repository uuid could be read at all.
func ParseKey(key string) (p ParsedKey, hasRepo, ok bool)
```

This is `classifyKey` from `restore.go`, moved and exported, with the same round-trip check. `restore` calls it.

### `restore`

Change only the snapshot choice:
1. After the readable snapshots are sorted by `push_seq` descending, if the first is a marker: print `skipped <uuid> deleted`, count nothing, and go on.
2. When choosing, skip markers.
3. The restored `push_seq` is still the highest readable number, marker included.
4. Two readable snapshots with the same `push_seq` still fail the repository, marker or not.

## `internal/store`

```go
// BumpPushSeq commits push_seq + 1 and returns it. ErrNotFound when the row is gone.
func (s *Store) BumpPushSeq(ctx context.Context, id [16]byte) (int64, error)

// DeleteRepo deletes the row only while push_seq still equals pushSeq.
// ErrNotFound when the row is gone; ErrStale when push_seq moved.
func (s *Store) DeleteRepo(ctx context.Context, id [16]byte, pushSeq int64) error

// PackIDs returns the pack ids that have a row for this repository.
func (s *Store) PackIDs(ctx context.Context, repoID [16]byte) (map[[16]byte]struct{}, error)
```

- `DeleteRepo`: one statement, `delete ... where id = $1 and push_seq = $2`. On zero rows, a second query tells `ErrNotFound` from `ErrStale`.
- All three use the store's error filter.

## `internal/objindex`

```go
// Drop forgets a repository. The next Refresh for that id starts empty.
func (x *Index) Drop(repoID [16]byte)
```

Remove the package comment's "Dropping a repository is a later piece."

## `internal/push`

```go
type Deps struct {
    // ... existing fields
    // BeforeCommit runs after the pack and index are uploaded and before
    // CommitPush. Tests use it to stop or hold a push there. Nil in production.
    BeforeCommit func(ctx context.Context) error
}
```

- It is called once, only on the path that reaches `CommitPush`. The already-applied retry path never calls it.
- An error from it reports every command `ng <ref> storage error` and returns without committing. That is the simulated crash: the objects are uploaded, and nothing points at them.

## `internal/httpapi`

```go
type Deps struct {
    Store    *store.Store
    Bucket   blob.Store
    Keyring  *objcrypt.Keyring
    Index    *objindex.Index
    PushHook func(ctx context.Context) error // tests only; passed to push.Deps.BeforeCommit
}
```

**`DELETE /admin/repos/{name}`**, admin only:

1. `requireAdmin`. A name that fails `ValidRepoName` is 404.
2. `RepoByName`, and 404 when it is absent.
3. `BumpPushSeq` gives `N`. `ErrNotFound` is 404.
4. Write the marker with `snapshot.Write`, with a context detached from the request (`context.WithoutCancel`) and a 30-second timeout, the same as the push path. Failure: log it, 500, and stop.
5. `DeleteRepo(id, N)`, with the same detached context as step 4, so a client that hangs up after the marker is written cannot stop the row delete.
   - `ErrNotFound` is 404. Another delete got there first. Keep the marker: the repository is gone.
   - Any other error, `ErrStale` included: look the row up with `RepoByID`. If it is still there, delete the marker's key; a failure is logged with the key. If it is gone, keep the marker. If the lookup itself fails, keep the marker and log the key. Then `ErrStale` answers 409 `{"error":"repository changed during delete; try again"}`, and every other error is logged and answers 500.
   - Added after review: the first version used the request context and cleaned up only on `ErrStale`. A client hang-up between steps 4 and 5 then gave a 500, left the row in place, and left the marker as that live repository's newest snapshot. After a database loss before its next push, `restore` would skip it and `gc` would delete it.
   - Between steps 4 and 5, call the handler's unexported `beforeDeleteRow func()` when it is set. Only tests inside `package httpapi` set it, to push a commit in at exactly that point. It is not part of `Deps`.
6. `Index.Drop(id)`.
7. 204, no body. Log one info line with the repository id and name.

`runServe` passes no hook.

## `internal/gc`

```go
// Grace is how old an object must be before gc deletes it. A push uploads its
// objects before its transaction inserts the pack row. Without this, gc running
// in that window deletes a pack a branch is about to point at.
const Grace = 24 * time.Hour

type Options struct {
    Now    func() time.Time // time.Now in production; tests move it
    DryRun bool             // print what would be deleted, delete nothing
    Out    io.Writer
}

type Result struct {
    Deleted  int
    Refused  int // repositories gc would not touch
    Failures int // deletes that returned an error
}

func Run(ctx context.Context, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring, o Options) (Result, error)
```

`err` is for what stops the whole run: the listing failed, a database error, or the context ended. Per-key and per-repository problems are counted.

1. **List `repos/` once**, and group by repository uuid with `snapshot.ParseKey`. Unparsed keys are counted, printed once at the end as `unexpected keys=<n>`, and never deleted.
2. **For each uuid**, in key order, with `old(key)` meaning `LastModified` is before `Now() - Grace`:
   - **A repository row exists** (`RepoByID`):
     - Read `PackIDs` **after** the listing. A pack committed after the listing is younger than the grace period anyway.
     - Delete each pack and index key whose pack id has no row and that is `old`.
     - Keep every snapshot.
   - **No row, and no `.snap` keys.** This is an auto-create push that lost its race, so the uuid was never committed. Delete each `old` pack and index key.
   - **No row, `.snap` keys present.** Read them all with `snapshot.Read`.
     - **None readable:** print `refused <uuid> snapshots cannot be read; check MASTER_KEY`, count it in `Refused`, and delete nothing.
     - **The highest-numbered readable snapshot is a normal one:** print `refused <uuid> <name> has snapshots and no database row; run littlekittyvcs restore`, count it in `Refused`, and delete nothing.
     - **The highest-numbered readable snapshot is a marker** (deleted):
       - Delete every `old` key of that uuid except that marker, unreadable snapshots included.
       - Then delete the marker only if every other key of the uuid was deleted in this run and the marker is `old`.
3. **Output:**
   - Each delete prints `deleted <key>`, or `would delete <key>` with `DryRun`.
   - A failed delete prints `failed <key>`, logs the error, and counts in `Failures`.
   - The last line is `deleted=<n> refused=<n> failures=<n>`.
4. **`DryRun`** does everything except `Delete`. Because nothing is deleted in a dry run, the marker counts as deletable when every other key of the uuid would have been.

## `cmd/littlekittyvcs`

`littlekittyvcs gc [--dry-run]`:
- Flags are parsed the way `restore` parses them. Extra arguments are refused.
- Load `config.LoadStorage`, open the store, `Migrate`, build the keyring and the S3 client, and run `gc.Run` with `time.Now` and stdout.
- Exit 1 when `Refused > 0`, `Failures > 0`, or `err != nil`.
- The log goes to stderr, like `restore`. Add it to `usage()`.

## The other two edits

- **`internal/objcrypt/format.go`**, the `Binding` comment: `ObjectID` is the pack uuid for a pack and its index, and the snapshot's own uuid for a snapshot. Comment only.
- **`README.md`:**
  - Replace the paragraph Grok added in piece 5 ("Repository delete removes that uuid's snapshots first…") with how the delete marker works.
  - Add an "Operations" note for `gc`: run it from cron, daily is enough, `--dry-run` first. A `refused` line after a database loss means run `restore` first.

## Tests

Run `go vet ./...` and `go test ./... -count=1` once at the end, with the database variable set (after `pg_isready`) and once without.

### `internal/snapshot`

1. A marker round-trips. `deleted: true` with a ref, and a missing `deleted` field, are both refused.
2. `ParseKey` accepts the three key forms and refuses an uppercase uuid, an extra path segment, and a pack key whose ids do not round-trip.
3. Restore (database, `blob.Mem`):
   - a repository whose highest snapshot is a marker prints `skipped <uuid> deleted` and inserts nothing
   - a marker below a normal snapshot is ignored, and the normal one is restored with `push_seq` equal to the highest number
   - added after review: snapshots at 3 (normal, naming an object in no pack), 2 (marker) and 1 (normal, complete). The one at 1 is restored with `push_seq` 3, not the marker with no refs. The existing case does not test "never choose a marker": with the choosing loop's marker skip removed, every test still passed, because the normal snapshot on top was always complete.

### `internal/store` (PostgreSQL)

4. `BumpPushSeq` returns 1, then 2. On a missing id it returns `ErrNotFound`.
5. `DeleteRepo` with the current `push_seq` deletes the row. Its refs, packs, and a token limited to it are gone, and an admin token and a token for another repository remain.
6. `DeleteRepo` after a `CommitPush` moved `push_seq` returns `ErrStale`, and the row remains. On a deleted id it returns `ErrNotFound`.
7. `PackIDs` returns exactly the pack rows of that repository.

### `internal/gc` (PostgreSQL, `blob.Mem`; packs from `git pack-objects`)

`Now` is `time.Now().Add(25 * time.Hour)` for "past the grace period" and `time.Now()` for "inside it".

8. **Live repository:** a committed pack, plus an orphan pack and index (uploaded, no row).
   - Inside the grace period nothing is deleted.
   - Past it, only the orphan's two keys are deleted, and every snapshot remains.
9. **Unrelated keys:** `repos/not-a-uuid/x` and `repos/<uuid>/packs/<uuid>.tmp` are counted as unexpected and remain.
10. **Lost database:** push to a repository, then run `gc` past the grace period against a new empty database with the same bucket. It prints `refused <uuid> site has snapshots and no database row; run littlekittyvcs restore`, deletes nothing (compare the key list before and after), and returns `Refused` 1.
11. **Unreadable snapshots:** the same as 10, run with a different keyring. The `refused … cannot be read` line appears and nothing is deleted.
12. **Deleted repository:** delete through the route, then:
    - Inside the grace period nothing is deleted.
    - Past it, every key under the uuid is deleted, the marker last. It is the final `deleted` line for that uuid.
    - The listing of `repos/<uuid>/` is empty afterwards.
13. **Marker kept when a delete fails:** the bucket is wrapped so that `Delete` fails for one pack key of a deleted repository. The run reports `failed <key>` and `Failures` 1. The marker is not deleted. A second run without the failure deletes the rest and then the marker.
14. **Dry run:** the case 12 setup with `DryRun` prints `would delete` for every key, the marker last, and the bucket is unchanged.
15. **Auto-create race loser:** a pack and index under a uuid with no row and no snapshot are deleted past the grace period and kept inside it.

### `test/integration/gc_test.go` (PostgreSQL and git)

16. **Build step 8, the simulated crash.**
    - The harness's `PushHook` returns an error.
    - `git push` exits non-zero, and `ls-remote` shows no branch.
    - The bucket holds the uploaded pack and index, and there is no pack row.
    - A fresh handler and index ("restart") still shows no branch.
    - `gc` inside the grace period keeps both keys. `gc` past it deletes exactly those two.
    - A push of the same commit without the hook then succeeds, and a clone passes `fsck`.
17. **Build step 9, the held push.**
    - The hook blocks on a channel. Start `git push` in a goroutine, and wait until the hook is entered (a second channel).
    - Run `gc` with the real clock. It deletes nothing.
    - Release the hook. The push exits 0, and a clone has the commit and passes `fsck`.
18. **Delete through the route.**
    - A repository with two branches and a limited read-write token.
    - `DELETE /admin/repos/site` with an admin token is 204.
    - `info/refs` is then 404. `GET /admin/tokens` no longer lists the limited token. The bucket still holds every object of the uuid, plus one more snapshot, the marker.
    - Restore into a new database prints `skipped <uuid> deleted`.
    - Creating `site` again gives a new uuid, and a push and clone work.
19. **Delete refused while a push commits.**
    - Hold a push at the hook. Call `BumpPushSeq` and write the marker directly, then release the push and let it commit.
    - `DeleteRepo` with the bumped number returns `ErrStale`.
    - The route itself, called afterwards, succeeds. Restore then skips the repository as deleted.
    - This is the stale-marker race, made deterministic.
20. **The route removes a stale marker** (`internal/httpapi`, PostgreSQL, `blob.Mem`).
    - Set `beforeDeleteRow` to run a `CommitPush` that creates a ref.
    - The route answers 409, and the repository and its new ref remain.
    - The bucket holds no marker: every snapshot of the uuid reads back with `Deleted` false.
    - With `Delete` failing for that key, the route still answers 409, and the log carries the marker key.
    - Added after review: send the request with a cancellable context and set `beforeDeleteRow` to cancel it. The route answers 204, the row is gone, and the marker is in the bucket. With the request context passed to `DeleteRepo`, this answered 500 with the row and the marker both still there.
21. **The delete route's access rules.** A non-admin token is 403, no token is 401, and a missing name is 404.

### `cmd/littlekittyvcs`

22. `gc extra` is refused before any configuration is read. `gc` with no environment names every missing variable.

### Against rustfs (optional, `LKV_TEST_S3_ENDPOINT` set)

23. Case 12 against rustfs, only on a uuid this test created. `List` there returns real `LastModified` values, so this also checks that the age comparison uses the bucket's clock correctly. Delete the test's own leftovers by exact key if the test fails part-way.

## Done means

The report back must contain this table, every row filled with evidence (command output or file path), or the piece is not done:

| Item | Evidence |
|---|---|
| `go vet ./...` clean | |
| `go test ./... -count=1` without the database: passes, list the skipped tests | |
| `go test ./... -count=1` with it, after `pg_isready`: every test passes | |
| Build step 8: the push stderr, the two orphan keys, and both `gc` outputs (inside and past the grace period) | |
| Build step 9: the `gc` output while held, and the push stderr after release | |
| Delete route: the 204, the 404 after it, and restore's `skipped … deleted` line | |
| Lost-database refusal: the `refused` line verbatim, and the key count before and after | |
| Deleted repository past the grace period: the `gc` output verbatim, marker last | |
| The rustfs run: the `gc` output, or "skipped" with the reason | |
| Every name in this document that turned out different in the code | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
