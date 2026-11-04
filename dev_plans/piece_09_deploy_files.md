# Piece 9: deploy files for a VM and for App Platform

After this piece:
- **VM.** `deploy/compose/` has a `Caddyfile` and a `caddy` service in `docker-compose.yml`, so an operator gets HTTPS by editing one hostname. The README already says this folder holds a Caddyfile. Today it does not.
- **App Platform.** `deploy/appplatform/app.yaml` is a complete app spec: the server, a managed PostgreSQL, and a daily `gc` job. Today the folder is empty, and the README says it holds a spec.
- **README.** Each deploy shape says how to get the first admin token, and the App Platform shape says not to use the managed database's restore feature.

Nothing in Go changes. No new environment variable for the server.

Start after piece 8. Both edit `README.md`. Piece 10 (the chunk cache) also edits the README, so do these one after the other.

## Facts this plan rests on

Checked on 4 Nov 2026.

**Caddy reverse proxy defaults** ([reverse_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy), [global options](https://caddyserver.com/docs/caddyfile/options)):
- The server's `read_body` and `write` timeouts default to no timeout. `read_header` is 1 minute, and `idle` is 5 minutes. A long push upload or a long clone download is not cut off.
- No default request body size limit.
- The proxy transport's `response_header_timeout`, `read_timeout` and `write_timeout` default to no timeout. `dial_timeout` is 3 s.
- A response with no `Content-Length` is flushed immediately. The server never sets `Content-Length` (no match in `internal/`). Go's `net/http` adds one only to a response that finishes within its first write buffer, a few KiB. A pack response is larger, so it goes out as a chunked stream with no length, and Caddy passes it through as it arrives.

So the Caddyfile needs nothing beyond the site address and `reverse_proxy`. Do not add timeout or buffer directives. Each one would be a guess that could break a large push.

**App Platform app spec** ([reference](https://docs.digitalocean.com/products/app-platform/reference/app-spec/)):
- A service builds from a GitHub repository with `github: {repo, branch, deploy_on_push}` and `dockerfile_path`.
- `http_port` defaults to 8080. When no `PORT` is set, App Platform adds one with that value.
- `health_check.http_path` turns on an HTTP check.
- `envs` entries take `key`, `value`, `scope` (`RUN_TIME`) and `type` (`GENERAL` or `SECRET`).
- A `databases` entry with `engine: PG`, `production: true` and `cluster_name` attaches an existing managed cluster. Its URL is `${<name>.DATABASE_URL}`.
- `jobs` take `kind: SCHEDULED` with `schedule: {cron, time_zone}`, plus their own `envs` and `instance_size_slug`.
- `run_command` "overrides the Dockerfile's entrypoint" ([create apps](https://docs.digitalocean.com/products/app-platform/how-to/create-apps/)). Our image has `ENTRYPOINT ["/littlekittyvcs"]` and `CMD ["serve"]`. So the gc job's `run_command` is the full `/littlekittyvcs gc`, not `gc`. The docs do not say whether `CMD` is then dropped. Build step 14 records what actually ran.

**Instance size.** Use `apps-s-1vcpu-1gb`: 1 GiB, $12 a month, and it can scale out. `apps-s-1vcpu-0.5gb` is single-instance only ([pricing](https://docs.digitalocean.com/products/app-platform/details/pricing/)). Piece 10 sizes its chunk cache against 1 GiB.

**The image has no shell.** It is `distroless/static-debian12:nonroot` (`Dockerfile`). App Platform's web console needs a shell in the container, so `admin-token` cannot be run from the console. It is run as a one-off container from the operator's machine (below).

## `deploy/compose/Caddyfile`

```caddyfile
# Replace with your hostname. Its DNS must point at this VM, and ports 80 and
# 443 must be open, so Caddy can get a certificate.
git.example.com {
	reverse_proxy app:8080
}
```

## `deploy/compose/docker-compose.yml`

Add a `caddy` service under the profile `caddy`. An operator who already runs a proxy on the host leaves the profile off, and uses the existing `127.0.0.1:8080` port mapping, which stays.

- `image`: `caddy:<the current 2.x release>`, pinned to an exact tag. Record the tag in the done table.
- `profiles: ["caddy"]`
- `ports`: `"80:80"`, `"443:443"`, `"443:443/udp"`
- `volumes`: `./Caddyfile:/etc/caddy/Caddyfile:ro`, plus named volumes `caddy_data:/data` and `caddy_config:/config`. `/data` holds the certificates. Losing it means getting them again, which Let's Encrypt rate-limits.
- `depends_on: [app]`
- `restart: unless-stopped`

Start with `docker compose --profile caddy up -d`.

## `deploy/appplatform/app.yaml`

One file. Placeholders are in angle brackets, with a comment on each. Every secret is a placeholder: no real value goes in this repository.

- `name: littlekittyvcs`, and `region` as a placeholder. The comment says to pick the same region as the Spaces bucket.
- **`services`**: one service, `app`.
  - `github` with `repo: <owner>/littlekittyvcs`, `branch: main`, `deploy_on_push: false`. A deploy is the operator's decision.
  - `dockerfile_path: Dockerfile`
  - `http_port: 8080`
  - `instance_size_slug: apps-s-1vcpu-1gb`, `instance_count: 1`
  - `health_check: {http_path: /healthz}`
  - `envs`: every server variable from the README configuration table.
    - `DATABASE_URL: ${db.DATABASE_URL}`
    - `MASTER_KEY`, `S3_ACCESS_KEY` and `S3_SECRET_KEY` as `type: SECRET`
    - the rest as `GENERAL`
    - `BACKUP_ENABLED: "false"`, with the six `_BACKUP` variables and `BACKUP_INTERVAL` commented out under it, and a comment saying what to set to turn backup on
- **`databases`**: one entry, `db`, with `engine: PG`, `version: "17"`, `production: true` and `cluster_name: <your cluster>`. The comment says:
  - the cluster is created first in the DigitalOcean control panel
  - the spec attaches an existing production cluster (the app spec reference requires `cluster_name` for one) rather than having App Platform create a dev database
- **`jobs`**: one job, `gc`.
  - `kind: SCHEDULED`
  - `schedule: {cron: "17 3 * * *", time_zone: UTC}`. Once a day matches the README. Minute 17 is arbitrary and avoids the top of the hour.
  - the same `github` source and `dockerfile_path` as the service
  - `run_command: /littlekittyvcs gc`
  - `instance_size_slug: apps-s-1vcpu-0.5gb`
  - `envs`: the same values as the service, except `PORT`, `PUBLIC_URL`, `ANONYMOUS_READ`, `DENY_NON_FAST_FORWARD` and `AUTO_CREATE`, which `gc` does not read. `gc` reads `config.LoadStorage`: the database, the master key, the bucket and the backup variables. Check that list against `cmd/littlekittyvcs/run.go` `runGC` before writing it.

The comment at the top of the file explains how the spec is used: `doctl apps create --spec deploy/appplatform/app.yaml`, then replace each secret placeholder in the control panel. App Platform encrypts a `SECRET` value on first submission.

## README

1. **Where it runs.** Each shape links its file: `deploy/compose/` for the VM, and `deploy/appplatform/app.yaml` for App Platform.
2. **The first admin token.**
   - On the VM: `docker compose run --rm app admin-token --label ops`. The command already appears in the main plan.
   - On App Platform, from the operator's machine:
     1. Add your IP to the managed database's trusted sources.
     2. Run the image locally with `DATABASE_URL` set to the database's public connection string, and the same `MASTER_KEY` and `S3_*` values, for example `docker run --rm --env-file prod.env <image> admin-token --label ops`.
     3. Remove your IP from the trusted sources afterwards.

     It needs the bucket because it writes a token backup (piece 8). It cannot run in the App Platform console, because the image has no shell.
3. **Never use the managed database's restore or fork to recover this server.** It is the same thing as loading an old dump, for the reasons the Recovery section already gives. Recovery is `restore` and `restore-tokens` into an empty database. Put this sentence in the Recovery section, next to the dump paragraph.
4. **Repository layout.** The `deploy/` lines stay. They are true after this piece.

## Main plan

In `dev_plans/git_s3_server_plan.md`, "Where it runs": under Contabo, say the Compose file has an optional Caddy service. Under App Platform, say the spec is `deploy/appplatform/app.yaml`, `gc` runs as a scheduled job, and the first admin token is minted from the operator's machine.

## Tests

No Go code changes, so the Go suite must still pass unchanged. The checks for this piece:

1. `docker compose -f deploy/compose/docker-compose.yml config` with a filled scratch `.env`, with and without `--profile caddy`. Both succeed. Only the first lists `caddy`.
2. `caddy validate --adapter caddyfile --config /etc/caddy/Caddyfile`, run inside the pinned `caddy` image with the Caddyfile mounted, exits 0.
3. **Through Caddy, end to end, on the machine.** Do this in a scratch copy of `deploy/compose/`, not in the repository:
   - Change the site address to `localhost`, so Caddy uses its own local certificate authority and no DNS is needed.
   - Map Caddy's port as `"18443:443"` in the scratch copy, so nothing already on the host's 443 gets in the way.
   - The test rustfs container publishes only on the host's `127.0.0.1:19000`, which a container cannot reach. After `up`, run `docker network connect <the scratch project's default network> lkv-piece01-rustfs`, and set `S3_ENDPOINT=http://lkv-piece01-rustfs:9000` with `S3_PATH_STYLE=true` in the scratch `.env`. `DATABASE_URL` points at the Compose `db`.
   - Run `docker compose --profile caddy up -d --build`, mint a token with `docker compose run --rm app admin-token --label e2e`, and create a repository through `https://localhost:18443/admin/repos` with `curl -k`.
   - Push a repository whose pack is larger than 4 MiB (more than one encryption chunk) over `https://localhost:18443`, with `http.sslVerify=false` for that command only. Then clone it into a second directory and compare the files.
   - Record the push and clone output and the clone time. Then run `docker network disconnect` for the rustfs container, and `docker compose --profile caddy down`. Leave the volumes and the bucket objects. The reviewer decides what to remove.
4. App Platform spec: `doctl apps spec validate deploy/appplatform/app.yaml` if `doctl` is installed and authenticated. Otherwise write "not run", with the reason. The real check is build step 14 of the main plan, which deploys it.
5. No secret value in any file this piece adds: `grep` the two new files for `SECRET` entries and confirm each value is a placeholder.

## Done

| Item | Evidence |
|---|---|
| `go vet ./...` and `go test ./...` unchanged and passing | |
| Compose `config` with and without the profile | |
| `caddy validate` exit code and the pinned tag | |
| End to end through Caddy: push and clone output, clone time, file compare | |
| `doctl apps spec validate` result, or "not run" with the reason | |
| The `gc` job's env list, checked against `runGC` | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
