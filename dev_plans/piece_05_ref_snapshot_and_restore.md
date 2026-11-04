# Piece 5: ref snapshot and restore

After this piece, losing the PostgreSQL database does not lose a repository:
- Every committed push writes one encrypted **ref snapshot** to the bucket. It holds the repository's name, its `push_seq` after that push, and the **full** ref set as that push committed it, including `HEAD`.
- A new subcommand, `littlekittyvcs restore`, rebuilds the repository rows, ref rows and pack rows of a fresh database from the bucket and the master key alone.

It builds:

1. **`internal/snapshot`**: the snapshot format, writing one, reading one back, and the restore logic.
2. **`internal/store`**: `CommitPush` returns the full ref set it committed. New `RepoByID` and `RestoreRepo`.
3. **`internal/push`**: writes the snapshot after the transaction commits.
4. **`internal/config`**: a `Storage` group (database, master key, bucket) that `Serve` embeds and `restore` loads alone.
5. **`cmd/littlekittyvcs`**: the `restore` subcommand.
6. **`README.md`**: a short "Recovering from a lost database" section.

Done means: build step 13 of [git_s3_server_plan.md](git_s3_server_plan.md) passes. Push to two branches in separate pushes, start over on an empty database with the same bucket and master key, run the restore, and a clone has both branches.

The main plan's "Ref snapshot for a lost database" section was corrected on 4 Nov 2026 for this piece. Where this document and that one disagree, report it. Do not silently pick one.

## Decisions already made

These were decided while writing this plan. Do not re-decide them.

- **The snapshot key uses its own random id**, `repos/<repo uuid>/refs/<snapshot uuid>.snap`, not the pack id. The main plan said `<pack-uuid>.snap`, which does not work: a delete-only push, and a push whose objects are all stored already, have no pack.
- **Restore must also rebuild the pack rows.** The `packs` table is lost with the database. Restore lists `repos/<uuid>/packs/` and inserts a row for every pack whose `.pack` and `.idx` objects both exist and decrypt. The main plan only mentioned refs.
- **Restoring packs that no ref reaches is safe.** That includes packs uploaded by a push whose transaction then failed. Every uploaded pack passed the connectivity check first (`internal/push/push.go`: `check` runs before `storePack`). The check only trusted packs that had a committed row at the time, because the object index is filled only from pack rows (`Refresh` with `t.Packs`) and from `Add` after a commit. Nothing deletes a pack yet. So every pack in the bucket is closed over the other packs in the bucket.
- **The newest snapshot is used only if every object it names is in a restored pack.** Otherwise restore tries the next newest. That keeps "do not invent a ref whose object is missing" without dropping single refs.
- **The restored `push_seq` is the highest `push_seq` of any readable snapshot, not the one chosen.** If restore fell back to an older snapshot and set that older number, the next push would reuse a number an existing broken snapshot already has. A later restore would then pick the broken one again and fall back past the new push.
- **Two readable snapshots with the same `push_seq` fail that repository.** Pushes get distinct numbers under the repository row lock, so equal numbers mean something is wrong. Do not guess which one wins.
- **No snapshot on `POST /admin/repos`.** A repository that was never pushed has nothing to lose but its name. The operator creates it again.
- **The snapshot is written after the commit and before the report**, with its own context: `context.WithoutCancel(r.Context())` and a 30-second timeout (a named constant). A client that hangs up after the commit would otherwise cancel the write. A write failure is logged and the push still reports `ok`.

## Out of scope for this piece

- **`littlekittyvcs gc`**, repository delete, and the crash and hold test hooks (build steps 8 and 9).
- **Pruning old snapshots.** There is one per push and they are all kept. Restore decrypts every snapshot of a repository it restores, so a repository with ten thousand pushes costs ten thousand small GETs once, during a disaster. That is a known cost for version 1.
- **Restoring tokens.** Tokens are not in the bucket. After a restore the operator runs `admin-token` and creates tokens again.
- **Restoring into a database that already has the repository.** Restore skips any repository whose id is already a row. It never merges.

## Rules for whoever implements this

- **Leave every change unstaged.** No `git add`, `git stash`, `git checkout`, `git reset`, `git restore`, `git clean`, no commits.
- **No recursive or forced deletes**, and never a variable in a recursive delete path. This piece adds no delete to production code. Tests delete single bucket keys from `blob.Mem` by exact key only, and use `t.TempDir()` and `internal/testdb`.
- **No backward compatibility.** Nothing has shipped. `CommitPush`'s return value and `config.Serve`'s layout change. Change every caller and delete the old form in the same change. No migration file is added: the schema from piece 2 has every column this needs.
- **No new environment variables.** The 30-second snapshot timeout is a constant in code with the reason in its comment.
- **Never print a secret or repository content.**
  - Token plaintext, `MASTER_KEY`, `DATABASE_URL`, S3 keys and data keys never reach a log line, an error, or stdout.
  - Ref names and object ids never reach a log line or restore's stdout. They are inside the ciphertext so a bucket holder cannot read them, and the output should not undo that. Print counts, repository names and uuids only. The repository name is already plaintext in the database.
- **Do not change `internal/objcrypt` or `internal/blob`.** Call them. If one cannot do what this piece needs, stop and report it.
- Wait for `docker exec lkv-piece02-postgres pg_isready -U lkv -d lkv -h 127.0.0.1` before running the suite.

## Setup

No new dependencies. Containers from earlier pieces: `lkv-piece02-postgres` and `lkv-piece01-rustfs` (`127.0.0.1:19000`, access key `lkvtest`, secret `lkvtestsecret`, bucket `lkv-test`, path style). Start each, use it, and stop it again. Never remove a container or its volume.

## Files

```text
internal/snapshot/
  snapshot.go     Snapshot, Key, Write, Read, the JSON format and its checks
  restore.go      Restore: bucket listing -> repository, ref and pack rows
  snapshot_test.go
  restore_test.go
internal/store/
  push.go         CommitPush returns Committed
  repos.go        RepoByID, RestoreRepo
  push_test.go, repos_test.go
internal/push/
  push.go         snapshot write after the commit
internal/config/
  config.go       Storage, LoadStorage; Serve embeds Storage
cmd/littlekittyvcs/
  main.go         the restore subcommand and its usage line
  run.go          runRestore
test/integration/
  restore_test.go
README.md         "Recovering from a lost database"
```

## `internal/store`

### `CommitPush` returns what it committed

```go
type Committed struct {
    PushSeq int64
    Refs    []Ref // every ref of the repository after this push, HEAD included, ordered by name
}

func (s *Store) CommitPush(ctx context.Context, p Push) (Committed, error)
```

- Read the refs with the same query and row checks as `ReadRepo`, inside the transaction, after `fixHead` and before the commit. Move that row loop into one helper that both call, so the two cannot drift.
- The set is exactly what this push committed. Every write to `refs` is either in `CommitPush`, after its `update repositories ... push_seq + 1` row lock, or in `CreateRepo` for a brand-new repository (`internal/store/push.go` and `internal/store/repos.go` are the only files that write `refs`). So no other push to the same repository can commit in between.

### `RepoByID`

```go
func (s *Store) RepoByID(ctx context.Context, id [16]byte) (Repo, error) // ErrNotFound when absent
```

### `RestoreRepo`

```go
type RestoredPack struct {
    ID       [16]byte
    PackKey  string
    IdxKey   string
    Bytes    int64
    IdxBytes int64
}

type Restore struct {
    ID      [16]byte
    Name    string
    PushSeq int64
    Refs    []Ref
    Packs   []RestoredPack
}

// RestoreRepo inserts the repository, its refs and its packs in one transaction.
func (s *Store) RestoreRepo(ctx context.Context, r Restore) error
```

- One transaction: insert the repository row with `push_seq`, then every ref (symbolic or direct, as the `Ref` says), then every pack row.
- A unique violation (the id or the name taken) returns `ErrExists`, and nothing is inserted.
- An invalid name, or a ref with neither or both targets, is an error before the transaction starts.
- Same error filter as the other request-path methods.

## `internal/snapshot`

### The format

```go
type Snapshot struct {
    RepoID  [16]byte
    Name    string
    PushSeq int64
    Refs    []store.Ref
}

func Key(repoID, snapID [16]byte) string // "repos/<repo uuid>/refs/<snapshot uuid>.snap", uuids via store.FormatID
```

The plaintext is JSON, encrypted with `objcrypt` under binding `{RepoID, snapID, KindSnap}`:

```json
{"format":1,"repository":"site","repo_id":"<uuid>","push_seq":7,
 "refs":[{"name":"HEAD","symbolic":"refs/heads/main"},
         {"name":"refs/heads/main","object_id":"<40 hex>"}]}
```

Reading refuses, each as an error:
- unknown fields (`DisallowUnknownFields`), and anything after the JSON value
- `format` other than 1
- `repo_id` different from the repository in the binding
- a name that fails `store.ValidRepoName`
- `push_seq` below 1. Every snapshot is written by a push, and the first push makes it 1.
- a ref with neither or both of `symbolic` and `object_id`, or an `object_id` that is not 40 lowercase hex
- a ref name other than `HEAD` that does not start with `refs/` or fails `plumbing.ReferenceName.Validate()`
- the same ref name twice
- no `HEAD`, a direct `HEAD`, or a symbolic ref other than `HEAD`

### `Write` and `Read`

```go
// Write stores s under a new random snapshot id.
func Write(ctx context.Context, bucket blob.Store, kr *objcrypt.Keyring, s Snapshot) error

// Read decrypts and checks one snapshot object. size is its ciphertext length from List.
func Read(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, repoID, snapID [16]byte, size int64) (Snapshot, error)
```

- `Write` encodes, encrypts into a buffer with `objcrypt.NewWriter`, and `Put`s it with `CiphertextSize()`.
- `Read` uses `objcrypt.Open` and reads the whole plaintext, the same way `objindex.Refresh` reads an index.

### `Restore`

```go
type Options struct {
    OnlyName string    // non-empty: restore only the repository with this name
    Out      io.Writer // one line per repository uuid; counts and names only
}

// Restore rebuilds every repository in the bucket whose id is not a row yet.
// failed counts repositories that could not be restored.
func Restore(ctx context.Context, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring, o Options) (failed int, err error)
```

`err` is for a failure that stops the whole run: the listing failed, or the context ended. A problem with one repository is a `failed` line, and the run goes on to the next.

Steps:

1. **List `repos/` once** and group keys by repository uuid. A key matches exactly one of:
   - `repos/<uuid>/packs/<uuid>.pack` or `.idx`
   - `repos/<uuid>/refs/<uuid>.snap`

   Parse both uuids with `store.ParseID`, and require `packstore.PackKey`, `packstore.IdxKey` or `snapshot.Key` of those ids to give back the same key. Any other key is counted as `unexpected` in that repository's line, or, when no uuid can be parsed from it, in one closing `unexpected keys=<n>` line. It is never an error.
2. **For each repository uuid**, in key order:
   1. `RepoByID` finds it: print `skipped <uuid> already in database` and go on.
   2. Read every `.snap` with `Read`. Count the ones that fail as `unreadable`.
   3. No `.snap` keys at all: print `skipped <uuid> no snapshot` and go on. An auto-create push that lost its race leaves packs like this, under an id that was never committed.
   4. `.snap` keys but none readable: `failed <uuid> no readable snapshot`.
   5. The readable snapshots disagree on the name: `failed <uuid> snapshots name different repositories`.
   6. `OnlyName` is set and is not that name: go on without printing. When `OnlyName` is set, the two failures above (4 and 5) are printed but not counted in `failed`, because they happen before the name is known. Otherwise one damaged unrelated repository would make every named restore exit 1.
   7. Two readable snapshots have the same `push_seq`: `failed <uuid> two snapshots with push_seq <n>`.
   8. **Packs.** A `.pack` without its `.idx`, or the reverse, is counted as `incomplete` and left out. For each complete pair:
      - `objcrypt.Open` the `.pack` with binding `{uuid, pack id, KindPack}`. That checks the header and the size.
      - `Refresh` a fresh `objindex.Index` with that one pack, so a bad index fails only its own pack.
      - Either failing counts the pack as `bad` and leaves it out.
   9. **Choose the snapshot.** In descending `push_seq`, take the first whose every direct ref's object is found by `Lookup`. None: `failed <uuid> no snapshot whose objects are all stored`.
   10. `RestoreRepo` with the chosen refs, every good pack, and `PushSeq` = the **highest** readable `push_seq`. `ErrExists`: `failed <uuid> name or id already taken`. Any other error: `failed <uuid> storage error`, and log it.
   11. Print `restored <name> <uuid> push_seq=<n> refs=<n> packs=<n>`, followed by `used_older_snapshot=true` when the newest snapshot was not chosen, and `unreadable=<n> incomplete=<n> bad=<n> unexpected=<n>` for any count above zero.
3. With `OnlyName` set and no repository of that name found, print `failed no repository named <name> in the bucket` and count it as one failure.

Restore can run while the server runs. It only inserts rows for ids the database does not have, and a name taken in the meantime fails that repository. The README still says to run it before serving, so clients never see a half-restored list.

## `internal/push`

After `CommitPush` succeeds:

```go
ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), snapshotTimeout)
err := snapshot.Write(ctx, d.Bucket, d.Keyring, snapshot.Snapshot{
    RepoID: repoID, Name: name, PushSeq: committed.PushSeq, Refs: committed.Refs,
})
```

- `name` is `t.Repo.Name`, or `t.Name` when the push created the repository.
- A failure logs `ref snapshot` with the error and the repository id, and nothing else. The push still reports `ok`.
- The existing `push` info line gains `snapshot` (true or false) and `push_seq`.
- The already-applied retry path writes no snapshot. It commits nothing.

## `internal/config` and the command

```go
// Storage is what restore needs: the database, the master key, and the bucket.
type Storage struct {
    Database
    MasterKey [32]byte
    S3        blob.S3Config
}

func LoadStorage(getenv Getenv) (Storage, error)

type Serve struct {
    Storage
    Port               int
    PublicURL          string
    AnonymousRead      bool
    DenyNonFastForward bool
    AutoCreate         bool
}
```

- `LoadServe` calls the same helpers for the storage part, so both report problems the same way: one error, sorted by variable name, naming variables, never values.
- `littlekittyvcs restore [--repo <name>]`:
  - Parse flags the way `runAdminToken` does. Refuse extra arguments, and a `--repo` value that fails `store.ValidRepoName`.
  - Load `Storage`, open the store, run `Migrate` (a replacement database is empty), build the keyring and the S3 client, and run `snapshot.Restore` with stdout as `Out`.
  - Exit 1 when `failed > 0` or `err != nil`, with the error on stderr. Like `admin-token`, its log goes to stderr, so stdout carries only the restore lines.
  - Add it to `usage()`.

## README

Add "Recovering from a lost database", after "Encryption and what it protects":
1. Start a new empty PostgreSQL and point `DATABASE_URL` at it. Keep the same bucket settings and the same `MASTER_KEY`.
2. Run `littlekittyvcs restore` before starting the server, for example `docker compose run --rm app restore`.
3. Read the output. A `failed` line names the repository uuid and why.
4. Run `admin-token` and create tokens again. Tokens are not in the bucket.
5. What can be lost: a push whose snapshot write failed comes back as the push before it. Without the master key, nothing can be restored.

## Tests

Run `go vet ./...` and `go test ./... -count=1` once at the end, with the database variable set (after `pg_isready`) and once without.

### `internal/snapshot` (`blob.Mem`; cases 1 to 3 need no database)

1. **Round trip.** `Write` then `List` then `Read` gives back the same snapshot. The stored bytes contain none of: a ref name, an object id as 40-char hex, the same id as 20 raw bytes, the repository name.
2. **Binding.** The same bytes copied (`SetRaw`) to another repository's key, or to another snapshot id in the same repository, fail `Read`. One flipped byte fails `Read`.
3. **Format checks.** One case per refusal listed under "The format", each built by encrypting hand-written JSON with the test keyring.
4. **Restore logic without git** (database, `blob.Mem`, packs from `git pack-objects` in `t.TempDir()`):
   - a pack without its `.idx` is `incomplete` and not restored
   - an unexpected key is counted and does not fail the repository
   - snapshot uuid `A` with no `.snap` prints `skipped <A> no snapshot`
   - two snapshots with the same `push_seq` fail the repository and insert nothing

### `internal/store` (PostgreSQL)

5. `CommitPush` on a repository with `main`, pushing only `dev`, returns `HEAD`, `refs/heads/dev` and `refs/heads/main` in that order, with the right ids, and `PushSeq` 2.
6. `RestoreRepo` inserts the repository, refs and packs. `ReadRepo` returns them, and `push_seq` is the given value. A second call with the same id returns `ErrExists`. So does a different id with the same name. Nothing extra was inserted either time.
7. `RepoByID` finds a created repository and returns `ErrNotFound` for a new id.

### `test/integration/restore_test.go` (PostgreSQL and git)

Each case pushes through the harness. "A new database" means a second `testdb.URL(t)` schema with its own `store.Store`, migrated, and the **same** bucket and keyring. After a restore, the server under test is a new `httpapi.New` over the new store, with a new `objindex.Index`.

8. **Build step 13.**
   - Push `main`, then push `dev` in a second push. The bucket now has two `.snap` objects. The second, read with `snapshot.Read`, names `HEAD`, `main` and `dev`.
   - Restore into a new database: one `restored repo ...` line, `push_seq=2 refs=3 packs=2`, `failed` 0.
   - A clone through the new server has `origin/main` and `origin/dev` at the pushed ids, files checked out, and a clean `fsck --full`.
   - Push a third commit through the new server. It succeeds, and its snapshot has `push_seq` 3.
9. **First branch `master`.** A repository whose only push was `master` restores with `HEAD` pointing at `refs/heads/master`, and the clone has its files checked out.
10. **Snapshot write failure.**
    - The bucket is wrapped so that `Put` fails for keys containing `/refs/`, for the second push only.
    - The second push still exits 0 with `[new branch]`. The log has one `ref snapshot` error line, and no log line contains a ref name or object id.
    - Restore into a new database gives `refs=2` (`HEAD` and `main`, no `dev`), `push_seq=1`, and a clone with only `main`.
11. **Fallback when the newest snapshot's objects are gone.**
    - Push three commits to `main` in three pushes. Delete the third push's `.pack` and `.idx` from `blob.Mem` by exact key.
    - Restore: `push_seq=3`, `used_older_snapshot=true`, `incomplete` or `bad` 0, and `main` at the second commit.
    - Push a new commit from the second commit through the restored server. Its snapshot has `push_seq` 4, above every older snapshot.
12. **Tampered newest snapshot.** Flip one byte of the newest `.snap` with `SetRaw`. Restore prints `unreadable=1` and uses the previous snapshot. `push_seq` is the highest readable one.
13. **Run twice.** A second restore into the same new database prints `skipped <uuid> already in database` for every repository, returns `failed` 0, and changes no row (compare `ReadRepo` before and after).
14. **`OnlyName`.** Two repositories in the bucket. Restore with `OnlyName` set to one of them inserts only that one. Then `OnlyName` set to a name not in the bucket gives `failed` 1.
15. **Name taken.** Create a repository with the same name but a new id in the new database first. Restore prints `failed <uuid> name or id already taken` and inserts nothing for that uuid.
16. **Output is clean.** In cases 8 to 15, restore's output and the logs contain no ref name and no object id.

### `cmd/littlekittyvcs`

17. `restore` with no environment reports every missing variable in one error, names only. `restore extra` and `restore --repo ../x` are refused before connecting to anything.

### Against rustfs (optional, `LKV_TEST_S3_ENDPOINT` set)

18. Case 8 against rustfs. The `lkv-test` bucket is shared with earlier pieces' tests and earlier runs of this one, so restore will see their keys too.
    - Name the repository `restore-<16 random hex>` and run restore with `OnlyName` set to that name.
    - Check only that repository's `restored` line, and the clone. Do not assert the run's `failed` count: leftovers from other runs can fail on their own.

## Done means

The report back must contain this table, every row filled with evidence (command output or file path), or the piece is not done:

| Item | Evidence |
|---|---|
| `go vet ./...` clean | |
| `go test ./... -count=1` without the database: passes, list the skipped tests | |
| `go test ./... -count=1` with it, after `pg_isready`: every test passes | |
| Build step 13: the restore output lines, and the clone's `git branch -r` and `fsck` output | |
| The snapshot-failure case: the push stderr, and the log's `ref snapshot` line verbatim | |
| The fallback case: the restore line verbatim, and the `push_seq` of the next push's snapshot | |
| The tampered case: the restore line verbatim | |
| The second-run case: the output verbatim | |
| The rustfs run: the restore line, or "skipped" with the reason | |
| Every name in this document that turned out different in the code or in go-git | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
