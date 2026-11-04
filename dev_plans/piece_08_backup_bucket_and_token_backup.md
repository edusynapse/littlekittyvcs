# Piece 8: backup bucket, token backup, and the recovery runbook

After this piece:
- **Backup bucket.** An optional second bucket holds a copy of every object in the primary bucket. `littlekittyvcs backup` copies whatever is missing. `serve` can also run that copy on a schedule. Losing the primary bucket is then recoverable, up to the last copy.
- **Token backup.** The tokens table is the one thing `restore` cannot rebuild (`dev_plans/git_s3_server_plan.md`, the token row: "A lost database loses tokens"). Every change to it writes an encrypted token backup to the primary bucket, and to the backup bucket when one is on. `littlekittyvcs restore-tokens` loads the newest one.
- **Runbook.** The README has a recovery section for each kind of loss. It says why an old database dump must never be loaded.

It builds:

1. **`internal/config`**: `BACKUP_ENABLED`, `BACKUP_INTERVAL`, and the backup bucket variables, which are the primary ones with `_BACKUP` appended.
2. **`internal/blob`**: `Copy`, a streaming copy of one object from one bucket to another.
3. **`internal/objcrypt`**: `KindTokens`.
4. **`internal/store`**: `ReadTokensForBackup`, `RestoreTokens`, and an advisory-lock helper.
5. **`internal/tokenbackup`** (new): write, read, and choose the newest token backup.
6. **`internal/backup`** (new): one copy run.
7. **`internal/httpapi`** and **`cmd/littlekittyvcs`**: write a token backup after every token change. New subcommands `backup` and `restore-tokens`, `gc --backup`, and the schedule inside `serve`.
8. **`README.md`** and the main plan.

Start this piece after piece 7 (fetch negotiation) is reviewed. Both edit `README.md`, the main plan and `internal/httpapi`.

Done means: the tests below pass. A primary bucket wiped in a test is recovered from the backup bucket with `restore` and `restore-tokens`, and a clone afterwards works with a restored token.

## Decisions already made

### Configuration

| Variable | Required | Meaning |
|---|---|---|
| `BACKUP_ENABLED` | no | Default false. True turns on the backup bucket for `backup`, `gc --backup`, the schedule, and token backups. |
| `S3_ENDPOINT_BACKUP` | when enabled | Same meaning and checks as `S3_ENDPOINT` |
| `S3_REGION_BACKUP` | when enabled | as `S3_REGION` |
| `S3_BUCKET_BACKUP` | when enabled | as `S3_BUCKET` |
| `S3_ACCESS_KEY_BACKUP` | when enabled | as `S3_ACCESS_KEY` |
| `S3_SECRET_KEY_BACKUP` | when enabled | as `S3_SECRET_KEY` |
| `S3_PATH_STYLE_BACKUP` | no | Default false, as `S3_PATH_STYLE` |
| `BACKUP_INTERVAL` | no | A Go duration such as `6h`. When set, `serve` runs a backup copy that often. When empty, nothing is scheduled inside the server, and the operator runs `littlekittyvcs backup` from cron. |

Rules:
- **When `BACKUP_ENABLED` is false,** the `_BACKUP` variables and `BACKUP_INTERVAL` are not read. Startup logs `backup` `false`, and nothing else. Turning backup off does not require deleting variables.
- **When it is true,** a missing required variable is a startup error that names the variable, as today (`internal/config`: `problems`).
- **The backup bucket must not be the primary.** The same endpoint and the same bucket name is a startup error naming `S3_BUCKET_BACKUP`. A backup in the same bucket protects nothing.
- **`BACKUP_INTERVAL`** must parse with `time.ParseDuration` and be greater than zero. Set while `BACKUP_ENABLED` is false, it is ignored like the others.
- **The secret is never logged.** The startup line logs the backup endpoint, region, bucket and path style, the same fields as the primary (`cmd/littlekittyvcs/run.go`, the `serving` line), and the interval.
- **One master key.** Objects are copied byte for byte. Their encryption is bound to the repository uuid, object uuid and kind (`objcrypt.Binding`), not to a bucket. So the same `MASTER_KEY` reads both buckets. No second key.

### What a copy run does

- **It lists the primary bucket under `repos/` and under `tokens/`, and the backup bucket under the same two prefixes.** For each primary key that is missing from the backup, or present with a different size, it copies the object. Objects are never rewritten under the same key: every pack, index, snapshot and token backup gets a new uuid. So a key that exists with the same size is the same object, and is skipped. No decryption, no checksum of contents.
- **It never deletes from the backup bucket.** A backup that mirrored deletes would also copy a mistaken or buggy delete. The backup bucket is cleaned only by `littlekittyvcs gc --backup` (below), when the operator runs it.
- **A copy is consistent enough without ordering tricks.** A listing is not a snapshot of the bucket. A snapshot can be copied while a pack it names, uploaded during the listing, is not. That is safe: `restore` uses the newest snapshot whose every object is present, and falls back to an older one otherwise (`internal/snapshot/restore.go`: the `objectsPresent` loop that picks `chosen`). The next run copies the missing pack.
- **One run at a time across every server process.** The run takes a PostgreSQL session advisory lock (`pg_try_advisory_lock` on one connection held for the whole run, released with `pg_advisory_unlock`). If another process holds it, the run is skipped, logged `backup skipped another run in progress`, and counts as success. The `backup` subcommand uses the same lock, so cron and the schedule never overlap.
- **The schedule** starts when `serve` starts. The first run is one interval after startup, not at startup, so restarts do not each start a copy. Runs are sequential. A run that is still going when the next is due delays the next one. Shutdown cancels the run through the context. A cancelled run leaves no partial object: a multipart upload that does not complete does not exist (`internal/blob/s3.go`: `abort`).
- **Each run also writes a token backup first**, so the copy that follows includes it.
- **Output.** One summary line, `backup copied=<n> skipped=<n> failed=<n> bytes=<n>`, in the log for a scheduled run and on stdout for the subcommand. Failed keys are logged one per line with the key. Keys contain only uuids: no ref names, no object ids.
- **The subcommand exits 1** when any copy failed or the listing failed, and 0 otherwise, including when skipped because of the lock.

### Streaming copy

`blob.Copy(ctx, src, dst Store, key string, size int64) error` reads `src` in ranges of `PartSize` (16 MiB, `internal/blob/parts.go`) through `GetRange`, and feeds them to `dst.Put` as one `io.Reader` of `size` bytes. One part is held in memory at a time. A pack can be up to about 1.5 GiB (main plan, push size), so reading a whole object into memory is not allowed. A server-side `CopyObject` is not used, because the two buckets may be in different regions or with different providers.

### Token backup

- **Format.** JSON, then encrypted with `objcrypt` under `KindTokens` (4), `Binding{RepoID: zero, ObjectID: backup uuid}`:
  ```json
  {"format":1,"taken_at":"<RFC 3339 with nanoseconds>","tokens":[
    {"id":"<uuid>","token_hash":"<64 hex>","label":"...","write":true,"admin":false,
     "repo_id":"<uuid>|null","created_at":"<RFC 3339>"}]}
  ```
  Every field is required. Unknown fields are refused, the same as snapshots (`internal/snapshot/snapshot.go`: `DisallowUnknownFields` and the trailing-data check in `unmarshal`). The plaintext tokens are never stored anywhere. Only their hashes are, as in the database.
- **Key.** `tokens/<backup uuid>.tok`. `gc` and `restore` list only `repos/` (`internal/gc/gc.go`: `bucket.List(ctx, "repos/", ...)`; `internal/snapshot/restore.go`: the same). So neither one ever sees, counts or deletes a token backup. A test locks that in.
- **`taken_at` comes from the database**, not the server's clock. `ReadTokensForBackup` runs in one repeatable-read, read-only transaction, the same options as `ReadRepo`. Its first statement is `select statement_timestamp()`, and the rows come from the same snapshot. Two token changes in different processes can write their backups in either order. Restore picks the backup with the greatest `taken_at`, ties broken by the greater key. So it gets the state that includes every change committed before that read.
- **When it is written:** after the database change commits, by:
  - `POST /admin/tokens` (`InsertToken`)
  - `DELETE /admin/tokens/{id}` (`DeleteToken`)
  - `DELETE /admin/repos/{name}` after a successful row delete. The cascade removed that repository's limited tokens.
  - the `admin-token` subcommand
  - and at the start of every backup run
  
  It goes to the primary bucket, and also to the backup bucket when enabled. The write uses a context detached from the request with a 30-second timeout, like the ref snapshot (`internal/push/push.go`: `writeSnapshot`). A failed write is logged at error level and does not fail the request. The token change already committed.
- **Why on every change, and not only on a schedule.** A revoked token must stay revoked after a recovery. If backups were only periodic, a token deleted after the last one would come back with `restore-tokens`. That is a leaked token working again.
- **The remaining risk is recorded.** A revocation whose backup write failed, followed by a database loss before any later backup, brings that token back. `restore-tokens` prints the `taken_at` it used, so the operator can compare it with when tokens were last revoked. The runbook says to revoke anything doubtful afterwards.
- **`admin-token` now needs the bucket.** It loads `config.LoadStorage`, the same as `restore`, instead of `LoadDatabase`, because it writes a token backup. Its token output on stdout is unchanged.
- **Old token backups are not pruned.** At about 250 bytes of JSON per token, 100 tokens is about 25 KB per backup. Four scheduled backups a day plus changes is about 1,500 a year, about 37 MB a year. That is small enough to leave for this piece. The README says the operator may delete old `tokens/` objects, keeping the newest few.

### `restore-tokens`

`littlekittyvcs restore-tokens` with no arguments. It needs `LoadStorage`, and runs `Migrate`, as `restore` does.
1. List `tokens/` in the **primary** bucket. After a primary-bucket loss, the operator has already pointed `S3_*` at the old backup bucket (runbook).
2. Decrypt each one. A key that does not parse as `tokens/<uuid>.tok`, or does not decrypt, is counted as unreadable and skipped.
3. Choose the greatest `taken_at`.
4. In one transaction, insert each token whose `id` and `token_hash` are both absent and whose `repo_id` is null or an existing repository. Tokens already present are left as they are. A token limited to a repository that does not exist is skipped and counted.
5. Print `restored tokens=<n> present=<n> no_repository=<n> unreadable=<n> taken_at=<time>`.
6. Exit 1 when no token backup is readable. Exit 0 otherwise, even when it inserted nothing.

It prints no labels, hashes or ids.

### `gc --backup`

The same `gc.Run`, with the same database and rules, against the backup bucket instead of the primary. It deletes:
- packs with no row once past the grace period
- deleted repositories, marker last

It refuses a uuid with no row and a normal newest snapshot, as on the primary. It never touches `tokens/`. It needs `BACKUP_ENABLED`. Without it, the command is an error naming `BACKUP_ENABLED`. `--dry-run` works with it. Never scheduled.

## Out of scope

Pruning token backups, verifying object contents in the backup bucket, more than one backup bucket, backing up anything outside `repos/` and `tokens/`, a cron syntax for the schedule, and a scheduled `gc`.

## Rules for this piece

- No secret in any log, error or output line: no secret key, `MASTER_KEY`, `DATABASE_URL`, token plaintext or token hash.
- No ref names or object ids in logs. Bucket keys contain only uuids and may be logged.
- No environment variables other than the eight in the table above.
- Tests that touch PostgreSQL skip without `LKV_TEST_DATABASE_URL`. Tests of a second real bucket skip without `LKV_TEST_S3_*`.

## Files

```
internal/config/config.go          Backup config, LoadStorage fills it
internal/blob/copy.go              new: Copy
internal/objcrypt/format.go        KindTokens = 4
internal/store/tokens.go           ReadTokensForBackup, RestoreTokens
internal/store/lock.go             new: WithAdvisoryLock(ctx, key, fn) (ran bool, err error)
internal/tokenbackup/tokenbackup.go new: Write, Newest, keys
internal/backup/backup.go          new: Run
internal/httpapi/admin.go          token backup after token create, token delete, repository delete
internal/httpapi/httpapi.go        Deps gains BackupBucket (nil when disabled)
cmd/littlekittyvcs/main.go, run.go backup, restore-tokens, gc --backup, the schedule, admin-token on LoadStorage
README.md
dev_plans/git_s3_server_plan.md
```

The advisory-lock key is one named constant in `internal/backup`. Its value is arbitrary. It must only differ from every other advisory-lock key in the code: `Migrate` takes one (`internal/store/migrate.go`: `withMigrateLock`). Check that key and pick a different one.

## Tests

### `internal/config`
1. Disabled: no `_BACKUP` variable is required. Set ones are ignored, including an invalid `BACKUP_INTERVAL`.
2. Enabled with every variable: the config is filled. `S3_PATH_STYLE_BACKUP` defaults to false.
3. Enabled with nothing else: the error names all five required `_BACKUP` variables and no value.
4. Enabled with the same endpoint and bucket as the primary: an error naming `S3_BUCKET_BACKUP`.
5. `BACKUP_INTERVAL` of `0`, `-1h` or `soon` is an error naming it. `6h` parses.

### `internal/blob`
6. `Copy` of an object of `3*PartSize + 5` bytes between two `blob.Mem` stores gives identical bytes. The source's recorded range calls are each at most `PartSize`.

### `internal/tokenbackup`
7. A backup round-trips. A missing field, an unknown field, and trailing data are each refused.
8. **The binding is checked:** a token backup copied to another key decrypts under its own uuid only. A snapshot object renamed to `tokens/<uuid>.tok` fails to decrypt, because the kind differs.
9. `Newest` picks the greatest `taken_at`, ties by greater key, and skips unreadable ones.

### `internal/store` (PostgreSQL)
10. `ReadTokensForBackup` returns every token and a `taken_at` from the database. A token inserted after the read is not in it.
11. `RestoreTokens`:
    - inserts absent tokens
    - leaves present ones as they are
    - skips one whose repository does not exist
    - the inserted token then authenticates through `TokenByHash`
12. `WithAdvisoryLock`: while one call holds the key, a second call on another connection returns `ran` false at once, and its function does not run.

### `internal/backup` (`blob.Mem` primary and backup, PostgreSQL for the lock)
13. A first run copies every key under `repos/` and `tokens/`, and nothing else. A key outside those prefixes stays in the primary only. A second run copies 0 and skips them all.
14. A backup object with the wrong size is copied again. A backup-only key is not deleted.
15. A failing `Put` for one key: that key is in `failed`, the others are copied, and the run reports failure.
16. Two runs at once: exactly one copies, the other reports skipped.
17. A run writes a token backup to both buckets before copying.

### `internal/httpapi` (PostgreSQL, `blob.Mem`)
18. **Token create, token delete and repository delete each leave a new token backup** whose content matches the database afterwards. A repository delete's backup no longer has the limited token. With backup enabled, the same backup is in both buckets.
19. A failing token backup write: the request still succeeds, and the log has the error, no hash and no label.

### `cmd/littlekittyvcs`
20. `backup` without `BACKUP_ENABLED` is an error naming it. `gc --backup` likewise.
21. `restore-tokens` with no token backup exits 1, and with one exits 0 and prints the summary line.
22. `admin-token` writes a token backup, and its stdout is still only the token.

### `test/integration` (real git, PostgreSQL, `blob.Mem` for both buckets unless noted)
23. **Whole-bucket recovery, the main test:**
    - push two branches and a tag, and create a limited read-write token
    - run `backup`
    - push one more commit, without another backup
    - wipe the primary bucket and the database, simulating losing both
    - point the primary at the old backup bucket
    - run `restore`, then `restore-tokens`

    Then:
    - a clone with the restored limited token works
    - the branches are at the state of the backup, not the later push
    - pushing that later commit again from the client succeeds
24. **Revocation survives:** create a token, revoke it, drop the database, `restore`, `restore-tokens`. The revoked token gets 401.
25. **`gc` never touches token backups:** a `gc` run past the grace period, with a deleted repository in the bucket, leaves every `tokens/` key.
26. **`gc --backup`:** after a repository delete, `backup`, and `gc --backup` with the clock past the grace period, that repository's keys are gone from the backup bucket and the others remain.
27. **Schedule:** `serve` with a short `BACKUP_INTERVAL` in a test copies a newly pushed pack to the backup bucket without any command. Use a test interval, not a sleep loop on a long one.
28. **rustfs (optional, skips without `LKV_TEST_S3_*`):** test 13 with a second bucket on rustfs. Create it in the test, and tell the reviewer its name.

## `README.md`

1. **Configuration table:** add the eight variables. Say the `_BACKUP` ones are the primary's names with `_BACKUP` appended.
2. **Operations:** `backup`, `restore-tokens` and `gc --backup`, with one cron example for `backup` when `BACKUP_INTERVAL` is not used.
3. **A new section, "Recovery"**, with one entry per loss:
   - **App container lost.** Start a new one. It stores nothing it needs.
   - **Database lost, bucket fine.**
     1. Start an empty database.
     2. Run `littlekittyvcs restore`.
     3. Run `littlekittyvcs restore-tokens`.
     4. Start `serve`.
     
     Repositories come back at their last snapshot. That is normally the last push. It is one push behind if that push's snapshot write failed. Developers re-push anything missing.
   - **Never load an old database dump.** Say why, in two sentences:
     - Packs pushed after the dump have no row in it, so `gc` deletes them as leftovers once they are 24 hours old. Those pushes are then gone.
     - The dump's `push_seq` is behind the bucket's snapshots, so later pushes reuse snapshot numbers, and `restore` refuses repositories with two snapshots of the same number.
     
     If you dump PostgreSQL for other reasons, do not restore this server's database from it.
   - **Primary bucket lost (backup enabled).**
     1. Set every `S3_*` variable to the old backup bucket's values. It is now the primary.
     2. Either turn `BACKUP_ENABLED` off, or point the `_BACKUP` variables at a new, empty bucket.
     3. **Rebuild the database even if it survived:** stop the servers, start an empty database, `restore`, `restore-tokens`.
     4. Run `backup` to seed the new backup bucket, if there is one.
     
     The surviving database would name packs pushed after the last backup, which the backup bucket does not have, and every clone of those repositories would fail. Pushes since the last backup are lost from the server. Developers re-push them from their clones.
   - **Both bucket and database lost.** The same steps as a primary-bucket loss.
   - **Master key lost.** Nothing can be recovered. Keep an offline copy. Keep the existing paragraph.
   - **Practise it.** Point a scratch database and the `S3_*` variables of a second, temporary setup at the backup bucket, run `restore` and `restore-tokens`, and clone. That proves the backup is usable without touching production.
4. Old `tokens/` objects may be deleted by hand. Keep the newest few.

## Main plan

In `dev_plans/git_s3_server_plan.md`:
- The PostgreSQL section was already corrected on 4 Nov 2026: recovery is `restore`, never an old dump. Add `restore-tokens` to that sentence.
- The token row's "Rebuild: no" becomes the token backup.
- The pack row's "The pack bytes have no second copy" gains "unless the backup bucket is enabled".
- The configuration table: the eight variables.
- The subcommand list: `backup`, `restore-tokens`, and `gc --backup`.

## Done

| Item | Evidence |
|---|---|
| `go vet ./...` clean | |
| `go test ./... -count=1` without the database: passes, list the skipped tests | |
| `go test ./... -count=1` with it, after `pg_isready`: every test passes | |
| Test 23: the commands run, their output lines, and the clone result | |
| Test 24: the 401 | |
| Test 12: how the two connections were made | |
| The advisory-lock key chosen, and the migrate lock key it differs from | |
| The rustfs run, or "skipped" with the reason | |
| Every name in this document that turned out different in the code | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
