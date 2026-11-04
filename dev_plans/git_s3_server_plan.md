# Git server that stores the repository in S3

Name: `littlekittyvcs`. Repository: `github.com/edusynapse/littlekittyvcs`, which is also the Go module path. The binary and its subcommands use the same name (`littlekittyvcs serve`, `littlekittyvcs gc`). License of this project's own files: **MIT**.

This is a new project. It is not a feature of Know to Win, and it does not replace Forgejo, lakeFS, or a normal Git host that keeps a bare repository on a disk.

A stock Git client (`git clone`, `git fetch`, `git push`) talks to one HTTP server. The server runs from the same container image on a Contabo VM and on a DigitalOcean App Platform container. The bytes of the repository live in any S3-compatible bucket, including DigitalOcean Spaces. Branch tips live in PostgreSQL, because that is the compare-and-swap Spaces does not provide.

## What the server is

One static Go binary. It speaks Git smart HTTP and always answers in protocol version 0. It ignores the `Git-Protocol` header: a client asking for version 1 or 2 accepts a version 0 answer (checked with git 2.34.1). It does not shell out to the `git` binary. The container image does not contain Git.

Routes:

| Request | Who calls it |
|---|---|
| `GET /healthz` | The platform health check. It does not touch Postgres, S3, or a repository. |
| `GET /{name}.git/info/refs?service=git-upload-pack` | `git fetch` and `git clone`, to learn the branch tips. |
| `GET /{name}.git/info/refs?service=git-receive-pack` | `git push`, to learn the branch tips. |
| `POST /{name}.git/git-upload-pack` | `git fetch` and `git clone`, to download objects. |
| `POST /{name}.git/git-receive-pack` | `git push`, to upload a pack and move refs. |
| `POST /admin/repos` | An operator, to create a repository. |
| `POST /admin/tokens` | An operator, to mint a bearer token. The plaintext token is returned once. |
| `GET /admin/tokens` | An operator, to list tokens (id, label, rights, repository, created). Never the hash. |
| `DELETE /admin/tokens/{id}` | An operator, to revoke a token. |

A token limited to another repository gets 404, the same as a repository that does not exist, so it cannot probe for names. A read-only token asking for the push advertisement gets 403. A push advertisement with no token is always 401, even when anonymous read is on, so `git push` prompts. With `AUTO_CREATE` on, a write token with no repository limit gets an empty push advertisement for an unknown name. The repository row is created later, by the push transaction, never by the advertisement.

`{name}` is a single path segment, `[a-z0-9][a-z0-9._-]{0,100}`. The server strips a trailing `.git`. `..`, slashes, and empty names are rejected. Nested namespaces are not in the first version.

The listen address is `0.0.0.0` and the port is `$PORT`, default `8080`. App Platform sends traffic to that interface. Binding only to `127.0.0.1` makes the container look dead.

## License and what we will not import

Our files are MIT. A dependency is allowed only when its license lets an MIT program link it and ship a binary. Each of those libraries keeps its own license. The repository ships a `NOTICE` file with the Apache-2.0 notices.

| Use it | For | License |
|---|---|---|
| `github.com/go-git/go-git/v5` | Pack decoding, pkt-line and pack protocol types, commit and tree objects, the have/want object walk | Apache-2.0 |
| `github.com/aws/aws-sdk-go-v2/service/s3` plus `config` and `credentials` | Put, Get with `Range`, Delete, List on Spaces or any other S3 endpoint | Apache-2.0 |
| `github.com/jackc/pgx/v5` | PostgreSQL | MIT |
| Go standard library `crypto/aes`, `crypto/cipher`, `crypto/rand`, `crypto/sha256`, `net/http`, `log/slog` | Encryption, hashing, HTTP, logs | Go's own license |

Pin go-git to the latest **v5** release, not v6. v6 was still `v6.0.0-beta.1` on 4 Oct 2026, and its upload-pack code still says protocol v2 is a to-do. v5 already has a server session in `plumbing/transport/server` (`NewServer`, upload-pack, receive-pack) and the pack libraries we actually call.

Do not import these, even though they solve a nearby problem:

| Project | Why it stays out |
|---|---|
| `git-fs-s3` | AGPL-3.0. Importing it would force this server to be AGPL. |
| libgit2, JGit | libgit2 is C and needs cgo, which ends the single static binary. JGit needs a Java runtime in the image. |
| The `git` binary at runtime | Not a license problem. Running a GPL program as a separate process does not make this server GPL, which is also why tests may run it. It stays out because `git http-backend`, `index-pack`, and `pack-objects` need the repository on local disk, and App Platform gives no volume and at most 2 GiB of ephemeral disk. If every repository were known to stay well under 1 GiB, a disk cache plus `git http-backend` with S3 as the source of truth would be less code. Repository size is not bounded here, so the server stays pure Go. |
| walgit, Enroute, ogit, Trunks, git3 | Applications with their own storage format. Read them. Do not vendor them. walgit (MIT) publishes a push by compare-and-swap on one S3 object. Spaces `PutObject` does not list `If-Match` or `If-None-Match`. Copy and GET do. Two pushes can overwrite each other's manifest. Enroute (Apache-2.0) is a 0.x release, keeps a Raft log on a real disk, and does not encrypt. |
| An ORM, a web framework, a migration framework, an env-config framework | The surface is small enough for `net/http`, SQL files, and environment variables. |

## What is custom

These parts do not exist as a library we can take under MIT.

**The receive path's commit rule.** go-git's `ReceivePack` writes objects and then updates refs through the storer, one ref at a time (`plumbing/transport/server` in v5). That is the wrong commit point. A push is durable only when one PostgreSQL transaction has inserted the pack row and moved every ref. Decode the request with go-git's `packp` types. Parse the pack with go-git's packfile reader. Upload the encrypted pack first. Then run the transaction. Then write the report-status pkt-lines. Do not call `ReceivePack` and hope a storer wrapper makes that atomic.

**Advertise `no-thin`.** Git assumes a receive-pack server can thicken a thin pack unless the server advertises `no-thin`. The first version rejects a pack whose delta refers to an object that is not in that pack. Thickening can wait. Clients that honor `no-thin` send a self-contained pack.

**Fast-forward check.** The wire protocol does not have a force flag. The client sends the old object id it thinks the ref has. The transaction updates the ref only when that old id is still current. When `DENY_NON_FAST_FORWARD` is true (the default), an update whose new commit cannot reach the old commit is rejected inside that same transaction. A delete (new id all zeros) is allowed when the old id matches. Turning the flag off allows a force push that still names the current tip.

**Encrypted pack format.** Spaces can do SSE-C, and a person who holds the Spaces key would still see plaintext if we used it, because the server would send the plaintext and the key to Spaces on every write. This server encrypts before `PutObject`. Spaces stores ciphertext. The master key never leaves the process environment.

Format, version 1, little-endian:

```text
magic            4 bytes  "S3G1"
version          uint8    1
key_id           uint8    which master key wrapped the DEK. First version only uses 1.
chunk_size       uint32   4194304
dek_nonce        12 bytes
wrapped_dek_len  uint16
wrapped_dek      those bytes   AES-256-GCM(master key, dek_nonce, raw 32-byte DEK)
chunk 0..n       each is nonce(12) || ciphertext || gcm-tag(16)
```

The chunk plaintext is the next slice of the raw Git pack, or of the raw pack index. The last chunk may be short.

Every chunk is sealed with additional authenticated data:

```text
aad = sha256(header bytes) || repo uuid || object uuid || kind byte (pack, idx, snap) || chunk index uint64 || last-chunk flag uint8
```

Without it, anyone who can write the bucket can reorder chunks, drop the tail of a pack, or swap a pack from one repository into another, and every chunk would still decrypt. With it, a reordered chunk, a moved object, or a truncated object fails the GCM tag. A truncated object is caught because no remaining chunk carries the last-chunk flag. The reader computes the expected chunk count from the object length and the header and refuses an object whose final chunk is missing the flag. A reader can decrypt chunk `i` with one ranged GET because the chunk size is fixed and the header length is known. go-git's pack reader needs `io.ReaderAt`. The custom reader maps an offset to a chunk, fetches that range, and decrypts. A fetch does not download the repository onto the container disk.

The pack index is a second object in the same format, with its own data-encryption key. Indexes are small and are decrypted fully when a repository is opened.

Object keys are random ids, not Git hashes:

```text
repos/<repo-uuid>/packs/<pack-uuid>.pack
repos/<repo-uuid>/packs/<pack-uuid>.idx
```

The header is not secret. It does not contain paths, ref names, or Git hashes. Those are inside the ciphertext.

**The object index in memory.** Which pack holds which Git object is derived from the pack indexes. It is not a table. On first use of a repository the process decrypts every index for that repository and builds `object-id -> (pack id, offset)`. A restart rebuilds it. There is nothing to migrate when the map is wrong. Drop it and read the indexes again.

More than one instance can serve the same repository. Instance A takes a push. Instance B then advertises the new tip from PostgreSQL. If B's map does not know the new pack, B's clone fails on a missing object. So every Git request starts with `select id, idx_key from packs where repo_id = $1`, compares the ids with the packs already in the map, and loads the missing indexes before it advertises or serves. Advertising refs and loading packs use the same snapshot: read the refs and the pack list in one read-only transaction, so a ref is never advertised before its pack is visible.

**Chunk cache.** A clone does not read once per object. A request with no haves streams each stored pack front to back: one header read per pack and one read per 4 MiB chunk. The cache does not speed that first pass. It keeps decrypted chunks so a later fetch of the same push, a push check, or a repeated clone of a small repository reads them from memory. The cache is process-wide, in the decryption layer, and a chunk is stored only after authentication succeeds. The key is the repository, the object, the kind, the header digest, the ciphertext size, and the chunk index. One LRU counts `cap(plaintext) + 128` bytes per entry. The joined path inserts only when the repository's pack bytes are at most a quarter of the limit, so a large clone does not empty the cache. Incremental reads always insert. Each request shares one go-git object cache of 96 MiB across the packs it opens. The size is `chunkCacheBytes` in `cmd/littlekittyvcs`, provisional at 256 MiB for the 1 GiB instance. Build step 15 sets it. It is not an environment variable. Each fetch logs `fetch served` with the repository uuid, `joined` or `generated`, the pack count, range reads, cache hits, response bytes, and milliseconds. That line has no ref names and no object ids.

**Ref snapshot for a lost database.** After the PostgreSQL transaction commits, the server writes one more encrypted object, `repos/<repo-uuid>/refs/<snapshot-uuid>.snap`, under a fresh random id (a delete-only push has no pack id to name it after), containing the repository name, the repository's `push_seq` after that transaction, and the **full** ref set of the repository as committed by that transaction, including `HEAD`. A snapshot of only the refs that push changed would make the newest snapshot drop every other branch. The ref set is read inside the push transaction, after the ref updates, so it is exactly what committed.

The snapshot is not the commit point. A crash between the commit and the snapshot leaves PostgreSQL right and the snapshot missing. Recovery reads PostgreSQL when it is there. When PostgreSQL has been replaced empty, `littlekittyvcs restore` decrypts every snapshot of the repository and imports the newest one whose every named object is in a restored pack. `push_seq` is bumped in the push transaction, so concurrent pushes get distinct, ordered numbers. A missing newest snapshot means the import lands on the previous push, which is a consistent state. It does not invent a ref whose object is missing.

The packs table is lost with the database, so restore also lists `repos/<uuid>/packs/` and inserts a row for every pack whose `.pack` and `.idx` both exist and decrypt. That includes packs from pushes whose transaction failed. This is safe: every uploaded pack passed the connectivity check against packs that had committed rows, and nothing deletes packs yet, so every pack in the bucket is closed over the others. The restored `push_seq` is the highest of any readable snapshot, not of the one chosen, so the next push numbers above every snapshot already in the bucket. The full design is in [piece_05_ref_snapshot_and_restore.md](piece_05_ref_snapshot_and_restore.md) (4 Nov 2026). A repository that was never pushed has no snapshot and is not restored.

**`gc` after a lost database would delete everything.** With an empty database every pack has no row, so the age rule alone would delete every pack older than 24 hours before anyone runs restore. `gc` must therefore never delete under a repository uuid that has no repository row and whose newest readable snapshot is a normal one: that is a lost database, not a deleted repository, and `gc` reports it and tells the operator to run restore. To tell the two apart, repository delete writes a **delete marker**, a snapshot with `"deleted": true`, numbered above every push, and only then deletes the row, if `push_seq` has not moved. A uuid with no row whose newest readable snapshot is a marker was deleted. (The earlier rule, delete the `.snap` objects before the row, raced with a push's snapshot write, which lands up to 30 seconds after its commit.) Design: [piece_06_gc_and_repository_delete.md](piece_06_gc_and_repository_delete.md), 4 Nov 2026.

**Auth.** Tokens. The database stores SHA-256 of the token, not the token. A token is either read-only or read-write, and optionally limited to one repository. Anonymous read is off unless `ANONYMOUS_READ=true`. Admin routes require a token with the admin bit. No passwords, no sessions, no OAuth in the first version.

A stock `git` client does not send `Authorization: Bearer` by itself. On a 401 it asks its credential helper or the terminal for a username and password and sends HTTP Basic. So the server accepts the token in two places: `Authorization: Bearer <token>`, and as the **password** of `Authorization: Basic` (the username is ignored). Every 401 carries `WWW-Authenticate: Basic realm="littlekittyvcs"`, or git never prompts.

The first admin token cannot come from `POST /admin/tokens`, which needs an admin token. It comes from the `littlekittyvcs admin-token --label <text>` subcommand, run with the same `DATABASE_URL` and bucket settings as the server, because it writes an encrypted token backup to the bucket (for example `docker compose run --rm app admin-token --label ops`). It prints the token once to stdout and stores only its hash.

**SQL migrator.** A directory of numbered `.sql` files embedded in the binary, and a `schema_migrations` table. The process applies any missing file inside a transaction on startup, under a PostgreSQL advisory lock, so several instances starting at once apply each file exactly once. No migration library.

## PostgreSQL

The server does not embed PostgreSQL. Contabo runs it beside the container, or App Platform points `DATABASE_URL` at a managed database. The schema is the server's. **The recovery path for a lost database is `littlekittyvcs restore` and `littlekittyvcs restore-tokens` into an empty database, not a database dump.** Loading a dump older than the bucket loses data. Packs pushed after the dump have no row in it, so `gc` deletes them as failed-push leftovers once past its grace period (`internal/gc/gc.go`, the live-repository branch of `gcRepo`). And the dump's `push_seq` is behind the bucket's snapshots, so later pushes reuse snapshot numbers, and `restore` then refuses those repositories (`internal/snapshot/restore.go`: `duplicatedSeq`). Tokens, the one table `restore` cannot rebuild, get their own backup in [piece_08_backup_bucket_and_token_backup.md](piece_08_backup_bucket_and_token_backup.md).

```text
repositories
  id            uuid primary key
  name          text unique not null
  push_seq      bigint not null default 0   bumped by every committed push, written into the ref snapshot
  created_at    timestamptz not null

refs
  repo_id       uuid not null references repositories
  name          text not null
  object_id     bytea
  symbolic      text
  peeled        bytea                  null unless object_id names an annotated tag; the first non-tag object down that tag's chain
  primary key (repo_id, name)
  check: exactly one of object_id, symbolic is set
  check: peeled is null, or object_id is set and peeled is 20 bytes

packs
  id            uuid primary key
  repo_id       uuid not null references repositories
  pack_key      text not null
  idx_key       text not null
  bytes         bigint not null   ciphertext size of the .pack object; the encrypted reader needs it to open the object
  idx_bytes     bigint not null   ciphertext size of the .idx object, same reason
  created_at    timestamptz not null

tokens
  id            uuid primary key
  token_hash    bytea unique not null
  label         text not null     what the operator called it, so a token can be found and revoked
  write         boolean not null
  admin         boolean not null
  repo_id       uuid references repositories
  created_at    timestamptz not null
```

The wrapped data-encryption keys live in the object headers, not in this schema. A database backup without the master key cannot decrypt packs. A bucket without the master key cannot either. The master key is an operator secret, 32 bytes, base64 in `MASTER_KEY`. The process refuses to start if it is missing or the wrong length.

`HEAD` is a row in `refs` whose name is `HEAD` and whose `symbolic` is `refs/heads/main` (or whichever branch was pushed first). An empty repository has a row and no branch rows. `git clone` of an empty repository must get a valid advertisement, not an HTTP 500.

### State of each stored thing

**Repository row**

- Create: `POST /admin/repos`, one insert.
- Read: every Git route loads it by name.
- Update: no rename in the first version.
- Rebuild: `littlekittyvcs restore` creates the row, with the same uuid, from the repository's snapshots. The snapshot format includes the repository name. `--repo <name>` limits it to one repository. Without it, every repository in the bucket that has no row is restored.
- Delete: `DELETE /admin/repos/{name}` bumps `push_seq`, writes a delete marker snapshot with that number, then deletes the repository row only if `push_seq` is unchanged. When the row delete fails for any reason and the row is still there, the route deletes the marker it wrote, so a live repository never has a marker as its newest snapshot. The cascade removes its token, ref and pack rows. It deletes no bucket objects. `littlekittyvcs gc` removes them after the grace period, the marker last, by which time no late snapshot write can still land. For a live repository, `gc` deletes pack and index keys with no pack row **and** an S3 `LastModified` older than 24 hours. The age check is not optional. A push uploads its pack and index before the transaction inserts the pack row. Without the grace period, `gc` running in that window deletes the pack, the transaction then commits, and a branch points at a deleted pack. The 24 hours is far above the 600 second App Platform upload limit and any push Contabo will see. A `.snap` key is kept while its repository row exists. `gc` never deletes under a uuid that has no repository row and whose newest readable snapshot is not a delete marker, because that is a lost database (see "Ref snapshot for a lost database"). The first version includes this command. It does not repack.

**Ref row**

- Create: inside the push transaction, insert when the old id is the all-zero id and no row exists. The insert writes `peeled` when the new object is an annotated tag.
- Read: `info/refs` and the fast-forward walk. Upload-pack advertises `peeled` as `<id> <name>^{}`.
- Update: `update ... set object_id = $new, peeled = $peeled where repo_id = $1 and name = $2 and object_id = $old`. A ref that moves off a tag stores `peeled` null. Zero rows means the client was stale. The whole transaction rolls back, including the pack insert, and every ref in that push is reported failed. A push that asked for atomic behavior and a push that did not are treated the same. Partial ref updates are not worth a second code path.
- Rebuild: snapshot import, described above. `restore` recomputes `peeled` from the tag objects. The snapshot JSON does not store it.
- Delete: new id is zero and the old id matches, same transaction. Deleting the repository deletes the rows. `peeled` goes with the row.

**Pack object and index object**

- Create: the pack has to be readable while it is parsed. Building the index means resolving deltas, and resolving a delta reads its base from earlier in the same pack. A multipart upload is not readable until it completes, so the bytes cannot be read back from S3 mid-push. The request body is therefore written to a spool file on the container's local disk **in the final encrypted format**: a fresh DEK held only in memory, the `S3G1` header, sealed chunks with the additional authenticated data above. Plaintext never touches the disk. go-git's pack parser reads the spool through the same encrypted `io.ReaderAt` used for S3. When parsing and the checks pass, the spool file is uploaded as it is: it already is the stored pack object. The index is built in memory and written the same way. The spool file is removed when the request ends, success or failure.
- Upload parts: S3 and Spaces reject multipart parts smaller than 5 MiB, except the last part. A 4 MiB chunk plus its overhead is below that. Upload parts of 16 MiB of ciphertext (four chunks), with the last part short. Objects at or under 16 MiB use one `PutObject`.
- Push size: bounded by the local disk. On App Platform treat about 1.5 GiB as the largest push, under the 2 GiB ephemeral ceiling, and reject a larger body early with an error the client prints. On Contabo the bound is the VM disk.
- The SQL insert happens only after both objects exist.
- Read: ranged GET plus decrypt, during fetch and during the fast-forward walk.
- Update: never. A new push writes new keys.
- Rebuild: the object index is rebuilt from indexes. The pack rows are rebuilt by `littlekittyvcs restore` from the bucket listing. The pack bytes have no second copy unless the backup bucket is enabled.
- Delete: the repository delete, and `littlekittyvcs gc` for objects whose pack row is gone. Unreferenced packs from a failed transaction stay until `gc` finds them older than its grace period. They are not reachable from any ref.

**Token**

- Create: admin route, store the hash, return the secret once.
- Read: every non-health request hashes the presented bearer token and looks it up.
- Update: no.
- Rebuild: an encrypted backup of the tokens table is written to the primary bucket after every token change and at the start of each backup run, and to the backup bucket when that is on. `littlekittyvcs restore-tokens` loads the newest one and prints the time of the backup it used. A revoke whose backup write failed, followed by a database loss before the next write, brings that token back. Old token backups are left in the bucket.
- Delete: admin route, delete the row.

**In-memory object index**

- Create: lazily from indexes.
- Read: fetch and the fast-forward walk.
- Update: after a committed push, add that pack's index. Do not serve the new pack until the transaction commits.
- Rebuild: drop the map.
- Delete: drop the repository's map when the repository is deleted.

## Push and fetch

Push:

1. Authorize the token for write on that repository.
2. Read the pkt-line commands. Spool the pack from the body to local disk in the encrypted format (see "Pack object and index object").
3. Parse the pack from the spool and build its index. Reject it if any delta base is outside the pack.
4. Connectivity check, by closure. Every object id that any commit, tree, or tag **in this pack** points at must be in this pack or in the object index. Tree entries of mode 160000 (submodule commits) are skipped. Every command's new id must also be in one of the two. Every stored pack passed the same rule against the packs before it, so by induction everything any stored object points at is stored, and only the new pack's own objects need checking. This reads indexes, not stored packs. It replaced the earlier design, a walk from each new tip that stopped at objects reachable from current refs, on 3 Nov 2026. That walk had to compute reachability from refs, and it trusted any object found in a stored pack, even one no ref reaches. Checking only the new tip is not enough: a client can push a commit whose parent, tree, or blob does not exist anywhere, and the repository is then corrupt for good.
5. Fast-forward check, outside any transaction. For each update with `DENY_NON_FAST_FORWARD` true, walk from the new commit and require that the old commit is reachable. This walk reads S3 and must not hold row locks. It stays valid because step 7 only updates a ref whose current id is still the old id the walk used.
6. Upload the pack and the index.
7. One short transaction: bump `repositories.push_seq` (this takes the row lock that orders pushes and deletes), conditional ref writes (`where object_id = $old`), insert the pack row, read the full ref set for the snapshot. On conflict, roll back. The two uploaded objects are left for `gc`, which removes them after its grace period. Nothing deletes them right away.
8. Write report-status. A retry of the same push, whose refs already point at the new ids, returns success without uploading another pack.
9. Best-effort ref snapshot, with the full ref set and `push_seq` from step 7.

Fetch:

1. Authorize.
2. Advertise refs from PostgreSQL. Capabilities, in this order:
   - Push (`git-receive-pack`): `report-status`, `delete-refs`, `atomic`, `ofs-delta`, `side-band-64k`, `no-thin`, `agent=littlekittyvcs`. `atomic` is honest because every push is all-or-nothing, and without it `git push --atomic` refuses to run.
   - Fetch (`git-upload-pack`): `multi_ack_detailed`, `side-band-64k`, `ofs-delta`, `agent=littlekittyvcs`, and `symref=HEAD:<branch>` when `HEAD`'s branch exists.
   - Do not advertise `thin-pack`, protocol v2, or partial-clone filters.
3. On `git-upload-pack`, a client that did not ask for `multi_ack_detailed`, or whose `done` request has no have in the object index, gets `NAK` and the joined pack (below). A clone sends no haves, so it takes this path and does not load the object index. A negotiation round that asked for `multi_ack_detailed` and is not `done` answers `ACK <id> common` for each have the index holds, then `NAK`. It never answers `ACK ready`. A `done` request that asked for `multi_ack_detailed` and names at least one indexed have answers those `ACK <id> common` lines, a plain `ACK` of the last, and a generated pack. The walk reads commits newest first, then compares each commit's tree with its parents' trees, and the pack writer streams one whole object at a time, zlib-compressed, with no deltas. The generated pack is never stored. The stored packs stay as the client sent them. go-git's revlist was rejected: in v5.19.3 `revlist.Objects` reads every commit and every tree the client has, through S3, on every fetch. go-git's pack encoder was rejected too: it loads every object into memory before writing the first byte. The walk may send an object the client already has. It does not leave out an object unless that object is reachable from a have the index holds. A request body with `Content-Encoding: gzip` is decoded: git compresses fetch requests over 1024 bytes.
4. Joined pack: concatenate the stored packs of the repository in `created_at` order into one response pack. Write a new header with the summed object count, copy each stored pack's object section without its 12-byte header and 20-byte trailer, and write a new SHA-1 trailer over the result. An offset delta points backwards inside its own pack, and each pack's objects stay contiguous, so every delta offset is still valid. Reference deltas are not present because thin packs are refused. A clone made this way can carry objects no current ref reaches, for example a deleted branch. Git accepts that. It also accepts an object that appears in two of the joined packs (measured with git 2.34.1).

The pack must end exactly where the push body ends. go-git checks the trailer at the end of the last object it parsed. Bytes appended after a valid pack would otherwise pass, be stored, and be copied into every joined pack. The spool also checks that the last 20 bytes of the body are the SHA-1 of everything before them.

`HEAD`: a new repository's `HEAD` points at `refs/heads/main`. If, after a push, `HEAD`'s target does not exist and the push created a branch, `HEAD` moves to the first branch that push created. So a repository whose first push is `master` clones with files checked out.

The walk reads commits, trees, and tags through the encrypted reader, one object at a time, and writes the pack the same way. Blobs are copied into the pack without being parsed. `plumbing/revlist` is not used, for the reason in fetch step 3.

## Configuration

Environment variables only. No config file. Subcommands: `serve`, `admin-token --label <text>`, `backup`, `restore [--repo <name>]` (the snapshot import), `restore-tokens`, and `gc [--dry-run] [--backup]`. A subcommand flag names what to act on. It never replaces an environment variable.

| Variable | Required | Meaning |
|---|---|---|
| `DATABASE_URL` | yes | PostgreSQL connection string |
| `MASTER_KEY` | yes | Base64 of 32 bytes |
| `S3_ENDPOINT` | yes | `https://fra1.digitaloceanspaces.com` or another S3 endpoint |
| `S3_REGION` | yes | The region string that endpoint expects. Spaces often wants the datacenter name. A wrong value fails the first PUT. Find it with a throwaway bucket, do not guess in code. |
| `S3_BUCKET` | yes | Bucket the operator already created. The server does not create buckets. |
| `S3_ACCESS_KEY` | yes | |
| `S3_SECRET_KEY` | yes | |
| `S3_PATH_STYLE` | no | Default false. Spaces wants virtual-host addressing. |
| `BACKUP_ENABLED` | no | Default false. When false, the `_BACKUP` variables and `BACKUP_INTERVAL` are not read. |
| `S3_ENDPOINT_BACKUP` | when backup is on | Backup bucket endpoint. The `_BACKUP` names are the primary names with `_BACKUP` appended. |
| `S3_REGION_BACKUP` | when backup is on | Region for the backup bucket. |
| `S3_BUCKET_BACKUP` | when backup is on | Backup bucket name. The same endpoint string and the same bucket name as the primary is a startup error. |
| `S3_ACCESS_KEY_BACKUP` | when backup is on | |
| `S3_SECRET_KEY_BACKUP` | when backup is on | |
| `S3_PATH_STYLE_BACKUP` | no | Default false. Read only when backup is on. |
| `BACKUP_INTERVAL` | no | A duration such as `6h`. When set and backup is on, `serve` runs `backup` on that interval. The first run is one interval after startup. |
| `PORT` | no | Default `8080` |
| `PUBLIC_URL` | no | Printed in logs and in the admin token response. Git clients use the URL they were given. |
| `ANONYMOUS_READ` | no | Default false |
| `DENY_NON_FAST_FORWARD` | no | Default true |
| `AUTO_CREATE` | no | Default false. A push to an unknown name fails. Operators create repositories on purpose. |

## Where it runs

The same image runs in both places.

**Contabo.** Docker Compose with two services: this image, and PostgreSQL with a volume. The Compose file has an optional Caddy service (`docker compose --profile caddy`). Leave the profile off when a proxy is already in front. The server itself speaks HTTP. Do not build a second proxy into the binary.

**DigitalOcean App Platform.** The spec is `deploy/appplatform/app.yaml`: a service component from this image, and `gc` as a scheduled job. `DATABASE_URL` points at a database outside the container. The first admin token is minted from the operator's machine, because the image has no shell. The documented limits that this design already fits, from the App Platform limits page (verified 20 May 2026) and the storage how-to:

- No persistent volume. The process stores nothing it needs on local disk.
- Local disk is ephemeral. The limits page says 4 GiB. The "why does my app keep restarting" page (verified 13 Jul 2026) says 2 GiB. Treat 2 GiB as the ceiling and still do not depend on it.
- Uploads time out after 600 seconds. A push larger than the client can send in that window fails on App Platform and can still succeed against the same image on Contabo.
- The process must listen on `0.0.0.0:$PORT`.
- More than one instance is allowed. Ref updates are a SQL compare-and-swap. Packs are immutable. No instance owns the repository.

A clone's maximum response time is not on the limits page. Measure one large clone against a real App Platform app before calling that path fine. The 100 second figure that shows up in PHP threads is a PHP limit, not this server's limit.

App Platform in front of Spaces in the same region can use the private path DigitalOcean added on 8 Apr 2026 for Droplets. App Platform containers are not Droplets. Assume that traffic counts as normal Spaces egress until a test shows otherwise.

## First version does not include

SSH, `git://`, Git LFS, protocol v2, partial clone, shallow clone, a web UI, pull requests, repacking, signed pushes, key rotation beyond the `key_id` byte, repository rename, and serving the bucket through a CDN. The CDN cannot decrypt these objects.

Two of those are the first things after version 1, not "someday":

- **Repacking.** Every push adds one pack and nothing merges them. After a thousand pushes, opening a repository decrypts a thousand indexes, a full clone concatenates a thousand packs, and an incremental fetch touches all of them. A repack writes one new pack from everything reachable, inserts its row and deletes the old pack rows in one transaction, and leaves the old objects for `gc`. The connectivity check's closure rule constrains it. A concurrent push may have passed its check by finding an object in an old pack that the repack dropped, because no ref reached it. That push's transaction must then fail. One way: give the repository a pack-generation counter that the repack bumps, and have the push transaction require the value it read before its check. Repacking also breaks an assumption of `restore`, which inserts a row for every pack in the bucket because "nothing deletes packs, so every pack is closed over the others". Once a repack drops old pack rows and `gc` deletes those old objects, a younger pack from a failed push can still be in the bucket while the packs it relied on are gone. The repack piece must change `restore` to run the connectivity check on the packs it restores, and leave out any pack that fails it.
- **Shallow clone.** CI checkouts usually ask for one (GitHub Actions `checkout` uses `--depth=1` by default). Until it exists, a CI job against this server must set full depth, or the clone fails.

## Build order

Each step is done when the test under it passes against a local PostgreSQL and a local S3 (MinIO or rustfs). Spaces is a later pass, not the first one.

1. Schema, token hashing, health check. Test: a bad master key refuses to start. A good one serves `/healthz` with Postgres up and S3 down.
2. Encryption reader and writer, with a ranged read of chunk 2 that does not decrypt chunk 0. Test uses an in-memory S3 stand-in. Tamper tests: swapping chunks 1 and 2, dropping the last chunk, and copying a pack object under another repository's prefix all fail to decrypt. Uploading an object of 20 MiB uses parts of at least 5 MiB except the last.
3. Admin create-repo and the empty `info/refs` advertisement. Test: `git ls-remote` against the test server.
4. Push of one commit, then `git clone` into a second directory, then a file compare. The S3 object downloaded without the master key is not a Git pack.
5. Two concurrent pushes of different commits onto the same branch. One succeeds. The other is rejected. The branch tip matches the winner. No ref points at a missing object.
6. Non-fast-forward is rejected while the flag is true, and accepted after the flag is false, still only when the old id matches.
7. Delete a branch. Clone no longer shows it.
8. Kill the process after the pack upload and before the SQL commit, by a test hook. Restart. The branch does not contain that push. `littlekittyvcs gc` keeps the orphan object while it is younger than the grace period, and removes it once it is older (the test sets the clock, not the constant).
9. Hold a push at the hook between upload and commit, run `littlekittyvcs gc` while the uploaded objects are still inside the grace period, release the push. The push commits and a clone returns it.
10. Push a commit whose parent object is in neither the pack nor the repository, built with a hand-written pack in the test. The push is rejected and no ref moves.
11. Two server processes on the same database and bucket. Push through the first, clone through the second immediately after. The clone has the new commit.
12. Restart with a fresh process and the same database. Clone still works. That is the index rebuild.
13. Push to two branches in separate pushes. Drop the database, keep the bucket and the master key, run `littlekittyvcs restore`, clone again. Both branches are there.
14. Repeat step 4 against a real Spaces bucket. Record the region string and path-style value that worked. Also record the command the scheduled `gc` job actually ran. The docs do not say whether the image's `CMD` is dropped when `run_command` is set.
15. Clone a repository with real history (at least a few thousand commits, pushed in many small pushes) against App Platform and against Contabo. The range reads, cache hits, and time come from the `fetch served` lines, and the memory comes from the instance's memory graph. Set `chunkCacheBytes` and the quarter rule from those measurements. Also record peak memory during a fetch of a large increment.

The `git` binary is installed on the developer machine and in CI. It is not installed in the runtime image.

## Open choices that can wait

Whether the admin API stays bearer tokens or later grows a single bootstrap token printed on first boot. That choice does not change the storage layout above.
