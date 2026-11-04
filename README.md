# littlekittyvcs

A small, self-hosted Git server that keeps your repositories **encrypted** in **S3-compatible object storage**, and that a stock `git` client can clone from and push to.

> **Status: design stage.** Nothing is built yet. The design is in [dev_plans/git_s3_server_plan.md](dev_plans/git_s3_server_plan.md). This README describes what the project is aiming for, not what works today.

## Why this exists

If you are a small developer or a small team, your code probably lives in one place: GitHub, GitLab, Bitbucket, or similar. That is fine until it is not:

- **You are locked out.** An account suspension, a billing dispute, a region block, a lost 2FA device, or an automated flag you cannot appeal quickly.
- **The provider is down.** An outage on the day you need to ship or roll back.
- **The account is compromised.** Someone force-pushes over your history, deletes repositories, or you simply no longer trust what the hosted copy contains.
- **You want a copy you control.** For your own peace of mind, or because a client or contract asks where the code lives and who can read it.

A laptop clone is not enough for this. It goes stale, it is one disk, and it is not something your other machines or your CI can push to.

littlekittyvcs is meant to be the **second remote you own**: a cheap server you run yourself that behaves like any Git host. You push to it alongside your main host, and if the main host is gone tomorrow you clone from it and carry on.

## What it is

- **Plain Git on the wire.** Smart HTTP, the same protocol `git clone https://...` already uses. No client plugin, no special CLI. `git remote add backup https://git.example.com/myproject.git` and `git push backup` is the whole integration.
- **Encrypted before it leaves the server.** Every pack and index is encrypted with AES-256-GCM inside the server process before it is uploaded. The storage provider only ever holds ciphertext. Someone with your bucket keys alone cannot read your code.
- **Cheap storage.** Repository data lives in any S3-compatible bucket: DigitalOcean Spaces, Backblaze B2, Wasabi, Cloudflare R2, AWS S3, or your own MinIO. Object storage costs a few cents per GiB per month, far below block storage or a hosted Git plan with private repos and seats.
- **One container.** A single static Go binary in a small Docker image. It runs on a cheap VM with Docker Compose, or on a container platform such as DigitalOcean App Platform. The container keeps nothing on its own disk that it needs after a restart.
- **MIT licensed.** No copyleft dependencies in the binary.

## What it is not

- Not a GitHub replacement. No web UI, pull requests, issues, or CI in the first version.
- Not a backup of your GitHub *account*. It holds Git repositories only: branches, tags, and commits. Issues, PR discussions, wikis, and releases are not copied.
- Not a mirroring service. It does not pull from GitHub on a schedule. You (or your CI) push to it, the same way you push to any remote.
- Not Git LFS, SSH, or `git://` in the first version.

## Architecture

```text
 developer laptops / CI                     your server (one Docker image)
 ┌──────────────────────┐   HTTPS (TLS     ┌───────────────────────────────────┐
 │ git clone / fetch    │   proxy in       │ littlekittyvcs                    │
 │ git push             │ ─────────────▶   │  - Git smart HTTP (v0 / v1)       │
 │ (stock git client)   │   front)         │  - bearer-token auth              │
 └──────────────────────┘                  │  - encrypt / decrypt packs        │
                                           │  - in-memory object index         │
                                           └───────┬───────────────────┬───────┘
                                                   │ SQL               │ S3 API
                                                   ▼                   ▼
                                     ┌──────────────────────┐ ┌──────────────────────────┐
                                     │ PostgreSQL           │ │ S3-compatible bucket     │
                                     │  - branch and tag    │ │  - encrypted packs       │
                                     │    tips              │ │  - encrypted pack indexes│
                                     │  - list of packs     │ │  - encrypted ref         │
                                     │  - token hashes      │ │    snapshots             │
                                     └──────────────────────┘ └──────────────────────────┘
```

**Why two stores.** Object storage is cheap and durable, but most S3-compatible services (Spaces included) cannot do a safe "update this only if nobody else changed it" write. Two pushes to the same branch at the same moment could silently overwrite each other. So the small, hot, must-be-atomic part (where each branch points) lives in PostgreSQL, and the large, immutable part (the Git objects) lives in the bucket.

**How a push works.**

1. The client sends its pack. The server writes it to a temporary file, already encrypted.
2. The server checks the pack: every object it references exists, nothing is missing, and a branch update is a fast-forward unless you have allowed force pushes.
3. The encrypted pack and its index are uploaded to the bucket under random names.
4. One PostgreSQL transaction moves the branch tips, only if they still point where the client thought they did. If another push got there first, this push is rejected and nothing changes.
5. An encrypted snapshot of all branch tips is written to the bucket, so the repository can be rebuilt even if the database is lost.

**How a fetch works.** A clone receives every stored pack joined into one. A fetch negotiates and receives only the objects the client does not already have, written whole, with no deltas. Annotated tags are advertised with the object they point at, so `git fetch` follows a new tag. The server decrypts in memory and streams the pack. Plaintext is not written to disk.

**Stateless containers.** Because branch tips live in PostgreSQL and packs never change once written, you can run more than one container behind a load balancer, and you can kill and replace a container at any time.

## Encryption and what it protects

| Who has what | Can they read your code? |
|---|---|
| The storage provider, or anyone with only your bucket keys | No. They see random object names and ciphertext. |
| Anyone with only a copy of the database | No. The database holds branch names, commit ids, and token hashes, not file contents. |
| Anyone with the bucket **and** the master key | Yes. The master key is the one secret that matters. |
| The server process while it runs | Yes. It decrypts to serve clones. Protect the host. |

Things to know before you rely on it:

- **Lose the master key and the data is gone.** There is no recovery. Keep an offline copy (a password manager, a printed copy in a safe, whatever you trust).
- **Branch and tag names are not hidden from whoever runs the database.** They sit in PostgreSQL in the clear. Put the database on an encrypted disk if that matters.
- **Repository sizes and push times are visible** to the storage provider from object sizes and upload times.

## Recovery

**App container lost.** Start a new one with the same environment. The container stores nothing the server needs. PostgreSQL and the bucket do.

**Database lost, bucket fine.**

1. Start a new empty PostgreSQL and point `DATABASE_URL` at it. Keep the same bucket settings and the same `MASTER_KEY`.
2. Run `littlekittyvcs restore` before starting the server, for example `docker compose run --rm app restore`. Restore can run while a server is up, but running it first means clients never see a half-restored list.
3. Run `littlekittyvcs restore-tokens`.
4. Read the output. A `failed` line names the repository uuid and why. `restore-tokens` prints the time of the backup it used.
5. Start `serve`.

Repositories come back at their last snapshot. That is normally the last push. It is one push behind if that push's snapshot write failed. Developers re-push anything missing.

A revoke whose backup write failed, followed by a database loss before the next token backup, brings that token back. Revoke it again after recovery.

**Never load an old PostgreSQL dump.** Packs pushed after the dump have no row in it, so `gc` deletes them as leftovers once they are 24 hours old. Those pushes are then gone. The dump's `push_seq` is behind the bucket's snapshots, so later pushes reuse snapshot numbers, and `restore` refuses repositories with two snapshots of the same number. If you dump PostgreSQL for other reasons, do not restore this server's database from it.

**Never use the managed database's restore or fork to recover this server.** It is the same hazard as an old dump, for the reasons above. Recovery is `restore` and `restore-tokens` into an empty database.

**Primary bucket lost, backup bucket on.**

1. Set every `S3_*` variable to the old backup bucket's values. It is now the primary.
2. Either turn `BACKUP_ENABLED` off, or point the `_BACKUP` variables at a new, empty bucket.
3. Rebuild the database even if it survived: stop the servers, start an empty database, run `littlekittyvcs restore`, then `littlekittyvcs restore-tokens`.
4. Run `littlekittyvcs backup` to seed the new backup bucket, if there is one.

The surviving database would name packs pushed after the last backup, which the backup bucket does not have, and every clone of those repositories would fail. Pushes since the last backup are lost from the server. Developers re-push them from their clones.

**Both the bucket and the database lost.** The same steps as a lost primary bucket, using the backup bucket.

**Master key lost.** Nothing can be recovered. Keep an offline copy.

**Practise it.** Point a scratch database and the `S3_*` variables of a second, temporary setup at the backup bucket, run `restore` and `restore-tokens`, and clone. That proves the backup is usable without touching production.

Repository delete writes one more snapshot, a delete marker, numbered above every push. It then removes the database row only if that number is still current. If a push commits in between, the delete is refused and that marker is removed, so a later database loss cannot treat the live repository as deleted. The bucket is left as it is. `littlekittyvcs gc` removes a deleted repository's objects after 24 hours, and it removes the marker last. By then a snapshot write, which gives up after 30 seconds, cannot still be in flight.

`gc` deletes objects under a repository uuid with no database row only when the newest snapshot it can read is a delete marker. A normal newest snapshot means the database was lost: `gc` deletes nothing and tells you to run `littlekittyvcs restore`. When it cannot read any snapshot, it deletes nothing and tells you to check `MASTER_KEY`. `restore` skips a repository whose newest snapshot is a delete marker.

For a repository that still has a database row, `gc` deletes only pack and index files that have no pack row and are older than 24 hours. It keeps every snapshot. `gc` lists `repos/` only, so it leaves every `tokens/` object.

## Operations

Run `littlekittyvcs gc` from cron. Once a day is enough. Run it with `--dry-run` first and read the list. A `refused` line after a database loss means run `littlekittyvcs restore` before `gc`. `littlekittyvcs gc --backup` is the same cleanup against the backup bucket. Run it by hand when you want the backup bucket cleaned. It requires `BACKUP_ENABLED`.

Each fetch logs one `fetch served` line. `repository` is the repository uuid. `mode` is `joined` or `generated`. `packs` is how many stored packs that response opened. `range_reads` counts header and chunk reads. `cache_hits` counts chunks served from memory. `bytes` is the response size. `ms` is how long the response took. The line has no ref names and no object ids.

When `BACKUP_INTERVAL` is unset, run `littlekittyvcs backup` from cron. One example, every six hours:

```text
0 */6 * * * littlekittyvcs backup
```

When `BACKUP_INTERVAL` is set, `serve` runs the backup on that interval. A PostgreSQL lock means a cron run and a `serve` run do not overlap: the one that finds the lock held logs that and exits 0.

`littlekittyvcs restore-tokens` loads the newest encrypted copy of the tokens table from the primary bucket. Run it after `restore`. Each backup run and each token change adds one object under `tokens/`. Old ones may be deleted by hand. Keep the newest few.

## Where it runs

The same image runs in both of these. Pick whichever is cheaper for you.

- **A small VM with Docker Compose** ([deploy/compose/](deploy/compose/)). littlekittyvcs, PostgreSQL with a volume, and an optional Caddy service for HTTPS. This is the cheapest shape and has no push-size limit beyond the VM's disk. Set the hostname in [deploy/compose/Caddyfile](deploy/compose/Caddyfile), then start Caddy with `docker compose --profile caddy up -d`. Leave the profile off if you already have a proxy, and use `127.0.0.1:8080`. The first admin token is `docker compose run --rm app admin-token --label ops`.
- **A container platform** such as DigitalOcean App Platform, with the spec in [deploy/appplatform/app.yaml](deploy/appplatform/app.yaml): the server on a 1 GiB instance, an existing managed PostgreSQL cluster, and a daily `gc` job. No server to patch, at the cost of platform limits: on App Platform a single push must finish uploading within 600 seconds and fit in roughly 1.5 GiB. The first admin token is minted from your own machine. The image has no shell, so the App Platform console cannot run it.
  1. Add your IP to the managed database's trusted sources.
  2. Run the image with `DATABASE_URL` set to the database's public connection string, and the same `MASTER_KEY` and `S3_*` values: `docker run --rm --env-file prod.env <image> admin-token --label ops`. It needs the bucket, because it writes a token backup.
  3. Remove your IP from the trusted sources afterwards.

Configuration is environment variables only: database URL, master key, bucket endpoint, region, name, and access keys. Backup adds `BACKUP_ENABLED` and `BACKUP_INTERVAL`, plus the primary bucket variables with `_BACKUP` appended: `S3_ENDPOINT_BACKUP`, `S3_REGION_BACKUP`, `S3_BUCKET_BACKUP`, `S3_ACCESS_KEY_BACKUP`, `S3_SECRET_KEY_BACKUP`, and `S3_PATH_STYLE_BACKUP`. With backup off, those variables are ignored. See the plan for the full list.

## Cost, roughly

For a handful of small-to-medium repositories, the bucket is the cheapest part. As one example, DigitalOcean Spaces is $5 a month for 250 GiB of storage and 1 TiB of outbound transfer (check current pricing; it changes). Most small teams' entire Git history fits in a few GiB.

The rest is whatever you already pay for a small VM, or a container plus managed PostgreSQL on a platform. There are no per-seat or per-repository fees.

## Planned usage

Once it exists, day-to-day use should look like this:

```bash
# one time, per repository
git remote add backup https://git.example.com/myproject.git

# every push, or from CI after each merge
git push backup --all
git push backup --tags
```

```bash
# the bad day: the main host is unreachable
git clone https://git.example.com/myproject.git
```

Repositories and access tokens are created by an operator through two admin HTTP endpoints. Tokens are read-only or read-write, and can be limited to one repository.

## Roadmap

**Version 1:** push, fetch, clone over smart HTTP; encrypted packs in any S3-compatible bucket; PostgreSQL for branch tips; bearer tokens; fast-forward protection; rebuilding branch tips from the bucket if the database is lost; a cleanup command for leftover objects.

**Right after version 1:**

- Repacking, so a repository with thousands of pushes stays fast.
- Shallow clone, so CI checkouts that ask for `--depth=1` work.

**Later, maybe:** protocol v2, a minimal read-only web view, master key rotation, SSH.

## Repository layout

```text
cmd/littlekittyvcs/      the binary: `serve`, `admin-token`, `backup`, `restore`, `restore-tokens`, and `gc`
internal/
  config/                environment variables, parsed and validated once at startup
  objcrypt/              the encrypted object format (S3G1): writer, ranged-read reader, and the process-wide decrypted-chunk cache
  blob/                  object storage: S3 client (single and multipart upload, ranged GET, list, delete) and an in-memory fake for tests
  store/                 PostgreSQL: repositories, refs, packs, tokens
    migrations/          numbered .sql files, applied on startup
  auth/                  token hashing and the bearer-token check
  gitproto/              Git wire format: pkt-lines, ref advertisement, capabilities
  objindex/              in-memory map of object id -> (pack, offset)
  packstore/             go-git object storer over encrypted packs; full-clone pack concatenation
  push/                  receive-pack: spool, parse, connectivity check, fast-forward check, commit
  fetch/                 upload-pack: want/have negotiation and pack generation
  snapshot/              encrypted ref snapshots: write after a push, import after a lost database
  gc/                    removal of objects with no pack row, after the grace period
  backup/                copy of repos/ and tokens/ into the backup bucket
  tokenbackup/           encrypted copy of the tokens table
  httpapi/               routes, admin endpoints, /healthz
test/integration/        tests that drive a real `git` client against the server, PostgreSQL, and MinIO
deploy/
  compose/               example Docker Compose file and Caddyfile for a VM
  appplatform/           example DigitalOcean App Platform spec
dev_plans/               design documents
```

Dependencies point downward only: `httpapi` uses `push` and `fetch`; those use `packstore`, `objindex`, `store`, and `snapshot`; everything that touches the bucket goes through `objcrypt` and `blob`. Nothing under `internal/` imports `httpapi` or `cmd`.

## Contributing

The design is open for review in [dev_plans/git_s3_server_plan.md](dev_plans/git_s3_server_plan.md). Issues that find holes in it are the most useful contribution right now.

## License

MIT. Third-party libraries keep their own licenses. A `NOTICE` file will list the Apache-2.0 notices of the libraries the binary includes.
