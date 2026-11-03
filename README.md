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

**How a fetch works.** The server reads branch tips from PostgreSQL, works out which objects the client needs, reads just those parts of the encrypted packs from the bucket with ranged requests, decrypts them in memory, and streams a normal Git pack back. Nothing is downloaded whole onto the server's disk.

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

## Where it runs

The same image runs in both of these. Pick whichever is cheaper for you.

- **A small VM with Docker Compose.** littlekittyvcs, PostgreSQL with a volume, and a TLS proxy such as Caddy in front. This is the cheapest shape and has no push-size limit beyond the VM's disk.
- **A container platform** such as DigitalOcean App Platform, with a managed PostgreSQL. No server to patch, at the cost of platform limits: on App Platform a single push must finish uploading within 600 seconds and fit in roughly 1.5 GiB.

Configuration is environment variables only: database URL, master key, bucket endpoint, region, name, and access keys. See the plan for the full list.

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

## Contributing

The design is open for review in [dev_plans/git_s3_server_plan.md](dev_plans/git_s3_server_plan.md). Issues that find holes in it are the most useful contribution right now.

## License

MIT. Third-party libraries keep their own licenses. A `NOTICE` file will list the Apache-2.0 notices of the libraries the binary includes.
