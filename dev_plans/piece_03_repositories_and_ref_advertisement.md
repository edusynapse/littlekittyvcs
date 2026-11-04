# Piece 3: repositories, the admin API, and the ref advertisement

After this piece, an operator can create a repository and mint, list, and revoke tokens over HTTP. A stock `git` client can then run `git ls-remote` and `git clone` against an empty repository and get a correct answer. Pushing and fetching objects come in the next pieces. This piece builds the part of the Git protocol every request starts with: the list of branch tips the server sends first, called the ref advertisement.

It builds:

1. **`internal/gitproto`**: writes the ref advertisement for `git-upload-pack` (clone and fetch) and `git-receive-pack` (push), with the right capability list for each.
2. **`internal/store`**: repository rows (create, read with refs in one snapshot), token listing and deletion, the repository-name rule, UUID text formatting, and database errors that never show the host or user.
3. **`internal/testdb`**: the throwaway-schema test helper that is now copied in two test files, moved to one place, because a third package needs it.
4. **`internal/httpapi`**: the token check on every non-health route, `GET /{repo}/info/refs`, and the four admin routes.
5. **`cmd/littlekittyvcs`**: passes the store and settings to `httpapi.New`.

Done means: `git ls-remote` and `git clone` of an empty repository succeed through Bearer and Basic tokens, `git ls-remote` of a repository with refs prints them in the right order with `HEAD`, and each refusal (no token, wrong token, read-only token for push, token for another repository) gives the status code in the table below. That is step 3 of the build order in [git_s3_server_plan.md](git_s3_server_plan.md).

The overall design is in that file. Its routes table, its "Auth" section, and its capability list were updated on 3 Nov 2026 for this piece. Where this document and that one disagree, report it. Do not silently pick one.

## Out of scope for this piece

- `POST /{repo}/git-upload-pack` and `POST /{repo}/git-receive-pack`. They stay 404 from the mux.
- Anything that reads or writes the bucket. The bucket client is still built and not called.
- `DELETE /admin/repos/{name}`: it has to delete bucket objects, so it comes with `gc`.
- Peeled tag lines (`refs/tags/v1^{}`) in the advertisement. The fetch piece adds them once it can read tag objects.
- Choosing `HEAD` from the first branch pushed. Create sets `HEAD` to `refs/heads/main`. The push piece decides whether to move it.
- No files in `internal/objindex`, `packstore`, `push`, `fetch`, `snapshot`, or `gc`.

## Rules for whoever implements this

- **Leave every change unstaged.** No `git add`, `git stash`, `git checkout`, `git reset`, `git restore`, `git clean`, no commits.
- **No recursive or forced deletes** (`rm -rf`, `rm -r`, `rm -f`, `find -delete`), never a variable in a delete path. Tests clean up files only through `t.TempDir()`. PostgreSQL tests drop only the schema each test created, through `internal/testdb`.
- **No backward compatibility.** Nothing has shipped. No new migration file: piece 3 needs no schema change. When a name or signature below replaces one from piece 2, change every caller and delete the old one in the same change.
- **No new settings.** The only environment variables are the ones in the piece 2 table and the test-only `LKV_TEST_*` ones.
- **Never print a secret.** Token plaintext appears only in the body of the `POST /admin/tokens` response and in `admin-token`'s stdout. It is never in a log line, an error, or any other response. The token hash is never returned by any route.
- **Check every go-git and pgx name below against the pinned module source** (go-git v5.19.3, pgx v5.12.0). The names were checked on 3 Nov 2026. If one differs, use the real one and list it in the report.
- **Do not touch** `internal/objcrypt` or `internal/blob`.
- Before you run the suite, wait until `docker exec lkv-piece02-postgres pg_isready -U lkv -d lkv -h 127.0.0.1` reports accepting connections. A suite started during PostgreSQL's own startup fails with SQLSTATE 57P03.

## Setup

No new dependencies. Use `lkv-piece02-postgres` from piece 2 (`docker start lkv-piece02-postgres`), and stop it again when you finish. Do not remove it or its volume.

```text
LKV_TEST_DATABASE_URL=postgres://lkv:lkvtest@127.0.0.1:15432/lkv?sslmode=disable
```

The Git tests run the `git` binary on the developer machine (2.34.1 here). The runtime image still has no `git`.

## Files

```text
internal/gitproto/
  advertise.go         Service, Ref, WriteAdvertisement, the two capability lists
  advertise_test.go    exact bytes, no database
internal/store/
  repos.go             ValidRepoName, CreateRepo, RepoByName, ReadRepo
  ids.go               FormatID, ParseID
  errors.go            ErrExists, the error filter every request-path method uses
  tokens.go            + ListTokens, DeleteToken (InsertToken and TokenByHash stay)
  repos_test.go
internal/testdb/
  testdb.go            URL(t): a fresh schema, dropped in Cleanup
internal/httpapi/
  httpapi.go           New(log, st, cfg), the mux
  authn.go             token lookup, 401 and 403 and 404 responses
  inforefs.go          GET /{repo}/info/refs
  admin.go             the admin routes
  inforefs_test.go     httptest, no git binary
  admin_test.go
  git_test.go          runs the real git client
```

`store_test.go` and `cmd/littlekittyvcs/run_test.go` lose their own copies of the schema helper and call `testdb.URL` instead. Leave the existing `.gitkeep` files alone, except `internal/gitproto/.gitkeep`, which may stay.

## `internal/gitproto`

```go
type Service string

const (
    UploadPack  Service = "git-upload-pack"
    ReceivePack Service = "git-receive-pack"
)

// ParseService returns ok=false for anything but the two names above.
func ParseService(s string) (Service, bool)

// Ref is one direct ref: a name and a 20-byte SHA-1 object id.
type Ref struct {
    Name string
    Hash [20]byte
}

// WriteAdvertisement writes the smart HTTP info/refs body for svc.
// head is the symbolic target of HEAD ("refs/heads/main"), or "" when there is no HEAD row.
// refs are the direct refs, in any order.
func WriteAdvertisement(w io.Writer, svc Service, head string, refs []Ref) error

// ContentType is "application/x-<svc>-advertisement".
func (s Service) ContentType() string
```

Build it on go-git's `packp.AdvRefs` and its `Encode` (`plumbing/protocol/packp/advrefs_encode.go`). That encoder, checked in v5.19.3:

- writes each `Prefix` entry as a pkt-line, and an entry equal to `pktline.Flush` as a flush packet
- puts `HEAD` first when `Head` is set, otherwise the first ref by name
- writes `<zero id> capabilities^{}\0<caps>` when there are no refs at all, which is what git expects from an empty repository
- sorts the remaining refs by name
- writes the capabilities in **insertion order**. Its comment says alphabetical; the code (`capability.List.String`) keeps insertion order. Add them in the order below.

Body layout:

1. `Prefix = [][]byte{[]byte("# service=" + svc), pktline.Flush}`. Git checks for this line and falls back to the old "dumb" protocol without it.
2. Capabilities, exactly these, in this order:
   - Upload pack: `side-band-64k`, `ofs-delta`, `agent=littlekittyvcs`, then `symref=HEAD:<head>` only when `head` names a ref in `refs`. The fetch piece adds its negotiation capabilities (`multi_ack_detailed`, `no-done`, or whatever it settles on) to this list. Do not add them now, because nothing answers the POST yet.
   - Receive pack: `report-status`, `delete-refs`, `atomic`, `ofs-delta`, `side-band-64k`, `no-thin`, `agent=littlekittyvcs`. `atomic` is honest: every push here is all-or-nothing anyway, and without it `git push --atomic` refuses to run. go-git has no `no-thin` constant; `capability.Capability("no-thin")` is accepted as an unknown name without arguments.
3. Upload pack: when `head` names a ref in `refs`, set `Head` to that ref's hash. The `HEAD` line then comes first. When `head` is empty or names a missing branch, there is no `HEAD` line and no `symref`.
4. Receive pack: never a `HEAD` line and never `symref`. git's own receive-pack does not send them.
5. Every ref in `References`. A ref name that does not start with `refs/`, or that `plumbing.ReferenceName(name).Validate()` rejects, is an error, not output. The store only holds valid names, so this is a bug guard. (`Validate` exists in v5.19.3, `plumbing/reference.go`. It accepts a bare `HEAD`, which is why the `refs/` check is needed too: a direct ref named `HEAD` would print a second `HEAD` line.)

There is no protocol version handling. git 2.26 and later send `Git-Protocol: version=2`. The server ignores the header and always answers version 0. A client that asked for version 1 or 2 accepts a version 0 answer. This was checked on 3 Nov 2026 with git 2.34.1 against a throwaway server: it sent `version=2` (and `version=1` with `-c protocol.version=1`), took the version 0 answer, and `ls-remote`, `--symref`, and an empty clone all worked. The Git tests below prove it again against the real handler.

## `internal/store`

### `ids.go`

```go
func FormatID(id [16]byte) string          // "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx", lowercase
func ParseID(s string) ([16]byte, error)   // exactly that form, either case; anything else is an error
```

`runAdminToken` logs `"id", store.FormatID(id)` instead of `%x`, so every id in logs and JSON has one form.

### `errors.go`

```go
var ErrExists = errors.New("store: already exists")
```

Every method a request calls (`TokenByHash`, `InsertToken`, `CreateRepo`, `RepoByName`, `ReadRepo`, `ListTokens`, `DeleteToken`) passes its error through one filter before returning it:

- `ErrNotFound`, `ErrExists`, `context.Canceled`, and `context.DeadlineExceeded` (checked with `errors.Is`) pass through unchanged.
- A `*pgconn.PgError` becomes `store: database error (SQLSTATE <code>)`.
- Anything else becomes `store: database request failed`.

Why: a pgx connection failure later in the process's life reads `failed to connect to "user=lkv database=lkv": 127.0.0.1:15432 ...`. The handlers log errors from these methods, and piece 2 kept the user, database name, and host out of every message. `Open`'s own errors already follow this rule. `Migrate` keeps its current errors: they carry the migration's SQL error, which is needed to fix a bad migration file.

### `repos.go`

```go
type Repo struct {
    ID      [16]byte
    Name    string
    PushSeq int64
}

type Ref struct {
    Name     string
    ObjectID *[20]byte // nil for a symbolic ref
    Symbolic string    // "" for a direct ref
}

// ValidRepoName is the same rule as the database check:
// ^[a-z0-9][a-z0-9._-]{0,100}$, no "..", and in addition no trailing ".git".
func ValidRepoName(name string) bool

// CreateRepo inserts the repository and its HEAD row (symbolic "refs/heads/main")
// in one transaction. A taken name returns ErrExists.
func (s *Store) CreateRepo(ctx context.Context, id [16]byte, name string) (Repo, error)

// RepoByName returns ErrNotFound when absent.
func (s *Store) RepoByName(ctx context.Context, name string) (Repo, error)

// ReadRepo returns the repository and all its refs, read in one
// repeatable-read, read-only transaction.
func (s *Store) ReadRepo(ctx context.Context, name string) (Repo, []Ref, error)
```

- **The trailing `.git` rule.** The database allows a name like `site.git`, but `GET /site.git/info/refs` strips `.git` and looks up `site`. Refuse such names at create time, in `ValidRepoName`, so a repository can always be reached. The database check stays as it is: no migration in this piece.
- `CreateRepo` detects a taken name from SQLSTATE `23505` on the insert. pgx has no named constant for it, so compare the code string. It validates the name first and returns a plain error for an invalid one. The database check is the backstop.
- `ReadRepo` uses `s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})` (checked in pgx v5.12.0). The fetch piece adds the pack list to this same transaction, so a ref is never advertised before its pack is visible. Do not read the packs now. Nothing uses them yet.
- Refs come back ordered by name. A row breaking the table's own check (both or neither column set) cannot exist. Scan `object_id` into `[]byte` and require length 20, as a bug guard.

### `tokens.go` additions

```go
type TokenListing struct {
    ID        [16]byte
    Label     string
    Write     bool
    Admin     bool
    RepoName  string    // "" means every repository
    CreatedAt time.Time
}

// ListTokens returns every token, ordered by created_at then id. Never the hash.
func (s *Store) ListTokens(ctx context.Context) ([]TokenListing, error)

// DeleteToken deletes one row. No row returns ErrNotFound.
func (s *Store) DeleteToken(ctx context.Context, id [16]byte) error
```

`ListTokens` gets the repository name with `left join repositories`.

### State of each stored thing touched here

| Thing | Create | Read | Update | Delete |
|---|---|---|---|---|
| Repository row | `CreateRepo` from `POST /admin/repos` | `RepoByName` (token create), `ReadRepo` (info/refs) | none in v1 | cascades from the repository delete, a later piece |
| `HEAD` ref row | `CreateRepo`, same transaction | `ReadRepo` | push piece | cascade with the repository |
| Token row | `POST /admin/tokens`, `admin-token` | `TokenByHash` (every request), `ListTokens` | none | `DELETE /admin/tokens/{id}`; cascade when its repository is deleted (already tested in piece 2) |

No new table, column, or index. Nothing derived is persisted.

## `internal/testdb`

```go
// URL creates schema lkv_test_<16 hex>, registers a Cleanup that drops exactly
// that schema (after re-checking the name against ^lkv_test_[0-9a-f]{16}$), and
// returns LKV_TEST_DATABASE_URL with search_path set to it. It skips the test
// when LKV_TEST_DATABASE_URL is unset.
func URL(t testing.TB) string
```

Move the current helper into this package. It must not import `store`: `store`'s own tests import `testdb`, and that would be an import cycle. `store_test.go` keeps its `Open` call on top of `testdb.URL`. Delete both old copies.

This is a normal package that only tests import. It reads `LKV_TEST_DATABASE_URL`, which is allowed because only tests call it. Say so in its package comment.

## `internal/httpapi`

```go
type Config struct {
    AnonymousRead bool
    AutoCreate    bool
    PublicURL     string
}

func New(log *slog.Logger, st *store.Store, cfg Config) http.Handler
```

`st` may be nil only in tests that call nothing but `/healthz`. `runServe` passes the real store and the three settings. Change `TestHealthz` and every other caller to the new signature.

Routes, Go 1.22 mux patterns:

```text
GET    /healthz                  (unchanged)
GET    /{repo}/info/refs
POST   /admin/repos
POST   /admin/tokens
GET    /admin/tokens
DELETE /admin/tokens/{id}
```

Write a test that builds the mux, so a conflict between patterns panics in the tests and not at startup.

### Reading the token (`authn.go`)

One function used by every route except `/healthz`:

1. No `Authorization` header: no token. The route decides whether that is allowed.
2. A header that `auth.FromRequest` cannot read: **401**.
3. `st.TokenByHash(auth.Hash(token))` returns `ErrNotFound`: **401**. A wrong token is never treated as "no token", even when anonymous read is on, or the user would never be prompted to fix it.
4. Any other store error: **500**, logged with the filtered error. Never the token.
5. Otherwise convert the `TokenRow` into an `auth.Token`.

Every 401 sets `WWW-Authenticate: ` + `auth.Challenge`, with body `authentication required\n` as `text/plain; charset=utf-8`. Without the header git does not ask for credentials.

### `GET /{repo}/info/refs`

1. Take the `{repo}` path value and strip one trailing `.git`. If `ValidRepoName` refuses what remains: **404**.
2. Query `service` through `ParseService`. Missing (an old dumb-protocol client) or unknown: **403**, body `only smart HTTP is supported\n`.
3. Read the token as above.
4. Decide, before touching the repository:

| Service | Caller | Result |
|---|---|---|
| upload pack | no token, `ANONYMOUS_READ` false | 401 |
| upload pack | no token, `ANONYMOUS_READ` true | continue as a reader of every repository |
| receive pack | no token | 401, even when anonymous read is on, so `git push` prompts |
| either | read-write or read-only token | continue |

5. `st.ReadRepo(name)`.
   - Not found: **404**. The one exception: receive pack, `AUTO_CREATE` true, a write token with no repository limit. That caller gets the **empty** receive-pack advertisement and **no row is created**. Creating a repository belongs in the push transaction, so a `git ls-remote` never makes one.
   - Found, but the token is limited to another repository (`Allows(id, false)` is false): **404**, the same answer as "does not exist", so a limited token cannot probe for names.
   - Found, receive pack, token cannot write (`Allows(id, true)` is false): **403**, body `this token is read-only\n`.
6. Advertise: `HEAD`'s symbolic target and the direct refs go to `gitproto.WriteAdvertisement`. Write into a `bytes.Buffer` first, then set the headers and write the body, so an encoder error becomes a 500 and not half a response.

Response headers, as `git http-backend` sends them:

```text
Content-Type: application/x-git-upload-pack-advertisement   (or receive-pack)
Cache-Control: no-cache, max-age=0, must-revalidate
Pragma: no-cache
Expires: Fri, 01 Jan 1980 00:00:00 GMT
```

### Admin routes (`admin.go`)

All four require a token with `Admin`: no token or a bad one is 401, a non-admin token is 403. Bodies are JSON in both directions, `Content-Type: application/json`. Every response sets `Cache-Control: no-store`. Errors are `{"error":"<one short sentence>"}`, which never repeats a submitted value.

Request decoding: `http.MaxBytesReader` at 64 KiB, `json.Decoder` with `DisallowUnknownFields`, and exactly one JSON value (a second `Decode` must return `io.EOF`). Anything else: **400**.

**`POST /admin/repos`**: request `{"name":"site"}`.

- An invalid name is **400**. A taken name is **409**.
- **201**: `{"id":"<uuid>","name":"site","url":"<PUBLIC_URL>/site.git"}`. Join with exactly one `/` between them. Leave out `url` when `PUBLIC_URL` is empty.

**`POST /admin/tokens`**: request `{"label":"ci","write":false,"admin":false,"repo":"site"}`. `write`, `admin`, and `repo` are optional, with defaults `false`, `false`, and every repository.

- `label` must be 1 to 200 characters (counted in runes, as `admin-token` does): otherwise **400**.
- `admin` true needs `write` true and no `repo`: otherwise **400**. Do not quietly switch `write` on. The database has the same checks, as a backstop.
- An unknown `repo` is **404**.
- **201**: `{"id":"<uuid>","token":"lkv_...","label":"ci","write":false,"admin":false,"repo":"site"}`, with `repo` left out for every repository. This is the only time the plaintext exists outside the client. Log `id` and `label` at info level, never `token`.

**`GET /admin/tokens`**

- **200**: `{"tokens":[{"id":"...","label":"...","write":true,"admin":true,"created_at":"<RFC 3339 UTC>"}]}`, with `repo` included only for limited tokens.

**`DELETE /admin/tokens/{id}`**

- An id that `ParseID` refuses is **400**. A missing row is **404**. Otherwise **204** with no body.
- An admin may delete its own token. The next request with it gets 401.

## `cmd/littlekittyvcs`

- `runServe` calls `httpapi.New(log, st, httpapi.Config{AnonymousRead: cfg.AnonymousRead, AutoCreate: cfg.AutoCreate, PublicURL: cfg.PublicURL})`.
- `runAdminToken` logs the id with `store.FormatID`.
- No other change.

## Tests

Run `go vet ./...` and `go test ./... -count=1` once at the end, with `LKV_TEST_DATABASE_URL` set (after `pg_isready`), and once without it. Every test passes. Tests needing PostgreSQL skip without the variable. Tests needing `git` skip when `exec.LookPath("git")` fails.

### `internal/gitproto/advertise_test.go` (no database)

Build each expected body in the test from a small `pkt(s string) string` helper (4 hex digits of `len(s)+4`, then `s`), and compare exact bytes.

1. **Empty, upload pack:** `pkt("# service=git-upload-pack\n") + "0000" + pkt("0000000000000000000000000000000000000000 capabilities^{}\x00side-band-64k ofs-delta agent=littlekittyvcs\n") + "0000"`. There is no `symref`, because `HEAD`'s target does not exist.
2. **Empty, receive pack:** the same shape, with the receive-pack capability list.
3. **Refs, upload pack:** head `refs/heads/main`, refs `main`, `dev`, `refs/tags/v1` passed in shuffled order. The first line is `<main id> HEAD\0... symref=HEAD:refs/heads/main\n`, then `dev`, `main`, `v1` sorted.
4. **Refs, receive pack:** same input. No `HEAD` line, no `symref`. The first line is `dev` with the capabilities.
5. **`HEAD` names a missing branch:** upload pack with head `refs/heads/main` and only `dev`. No `HEAD` line, no `symref`.
6. **A bad ref name** returns an error: `"refs/heads/a b"`, and a direct ref named `"HEAD"`.
7. `ParseService` accepts the two names and refuses `""`, `git-upload-archive`, and `GIT-UPLOAD-PACK`.

### `internal/store/repos_test.go`

1. **`ValidRepoName`:**
   - accepts `a`, `site`, `my-repo.v2`, `x_y`, and a name of 101 characters
   - refuses `""`, `Site`, `-x`, `.x`, `a..b`, `a/b`, `site.git`, and a name of 102 characters
2. **`CreateRepo` then `ReadRepo`:** one repository, `PushSeq` 0, and exactly one ref, `HEAD`, symbolic `refs/heads/main`.
3. **A taken name** returns `ErrExists`.
4. **`ReadRepo` of an unknown name** returns `ErrNotFound`.
5. **Ordering:** insert direct refs by hand (`refs/tags/v1`, `refs/heads/main`, `refs/heads/dev`). `ReadRepo` returns `HEAD` and the three refs ordered by name, each with a 20-byte id.
6. **`ListTokens` and `DeleteToken`:**
   - Insert three tokens: an admin token, a read-only token limited to a repository, and a read-write token for every repository.
   - The listing has all three in creation order, with the right repository name, and no field holds the hash.
   - `DeleteToken` of one makes `TokenByHash` return `ErrNotFound`. Deleting it again returns `ErrNotFound`.
7. **The error filter** (an internal test, no database needed):
   - Build a `Store` around a pool from `pgxpool.ParseConfig` on `postgres://sentinel_user:pw@127.0.0.1:1/sentinel_db?sslmode=disable&connect_timeout=1`, without `Open`'s ping.
   - Call `RepoByName`. The error must not contain `sentinel_user`, `sentinel_db`, or `127.0.0.1`. It must be `store: database request failed`, or `store: database error (SQLSTATE ...)` if a code comes back.
8. **`FormatID` and `ParseID`:** they round-trip 100 `NewID` values, accept the upper-case form, and refuse an empty string, 35 or 37 characters, a missing dash, and a non-hex character.

### `internal/httpapi/inforefs_test.go` (PostgreSQL, `httptest`, no git binary)

Set up one repository `site` with a `HEAD` row only, a second repository `other`, and these tokens: admin, read-write for every repository, read-only for every repository, and read-write limited to `other`. Each row is one request; assert the status, and for 200 the content type, the three cache headers, and the first pkt-line.

| Request | Token | Settings | Status |
|---|---|---|---|
| `/site.git/info/refs?service=git-upload-pack` | none | defaults | 401 with the challenge header |
| same | none | `ANONYMOUS_READ` true | 200 |
| same | `Bearer lkv_wrong` | `ANONYMOUS_READ` true | 401 |
| same | `Basic` of `x:<read-only>` | defaults | 200 |
| `/site/info/refs?service=git-upload-pack` (no `.git`) | read-only | defaults | 200 |
| `/site.git/info/refs` (no service) | read-only | defaults | 403 |
| `/site.git/info/refs?service=git-upload-archive` | read-only | defaults | 403 |
| `/site.git/info/refs?service=git-receive-pack` | none | `ANONYMOUS_READ` true | 401 |
| same | read-only | defaults | 403 |
| same | read-write | defaults | 200 |
| same | limited to `other` | defaults | 404 |
| `/nosuch.git/info/refs?service=git-upload-pack` | read-write | defaults | 404 |
| `/nosuch.git/info/refs?service=git-receive-pack` | read-write | `AUTO_CREATE` true | 200, empty advertisement, and `repositories` still has 2 rows |
| same | limited to `other` | `AUTO_CREATE` true | 404 |
| `/Bad.git/info/refs?service=git-upload-pack` | read-write | defaults | 404 |

Also: a request through the handler carrying a token, with the logger writing to a buffer. The token's plaintext is not in the buffer.

### `internal/httpapi/admin_test.go` (PostgreSQL)

1. **Auth:** each admin route with no token is 401 with the challenge, and with a read-write non-admin token is 403.
2. **`POST /admin/repos`:**
   - `{"name":"site"}` is 201, the `id` parses with `ParseID`, and `url` is `https://git.example.com/site.git` when `PublicURL` is `https://git.example.com/` (trailing slash).
   - With `PublicURL` empty there is no `url` key.
   - The same name again is 409.
   - Bad bodies are 400: `site.git`, `Bad`, `{"name":"x","extra":1}`, two JSON values back to back, and a 65 KiB body.
3. **`POST /admin/tokens`:**
   - A read-only token limited to `site` is 201. The returned token works for `GET /site.git/info/refs?service=git-upload-pack` and gets 403 for receive pack.
   - `{"label":"x","admin":true}` with `write` missing is 400.
   - `{"label":"x","admin":true,"write":true,"repo":"site"}` is 400.
   - An unknown repository is 404.
   - An empty label is 400, and so is a label of 201 runes.
   - The logger buffer never contains any returned token.
4. **`GET /admin/tokens`:**
   - It lists every token created in the test, in creation order.
   - The raw response body contains no returned plaintext token and none of the hashes as hex or base64.
5. **`DELETE /admin/tokens/{id}`:**
   - Deleting a read-only token is 204, and that token then gets 401.
   - Deleting it again is 404, and `not-a-uuid` is 400.
   - An admin deleting its own token gets 204, and its next request gets 401.
6. Every admin response has `Cache-Control: no-store`.

### `internal/httpapi/git_test.go` (PostgreSQL and the git binary)

Start `httptest.NewServer(New(...))` on a real store. Run git with `exec.Command`, never a shell, and with an environment built from scratch, not inherited:

```text
HOME=<t.TempDir()>
GIT_CONFIG_NOSYSTEM=1
GIT_CONFIG_GLOBAL=/dev/null
GIT_TERMINAL_PROMPT=0
PATH=<the test process's PATH>
```

Report git's stdout and stderr on failure. Each case:

1. **Empty repository, Bearer:**
   - `git -c "http.extraHeader=Authorization: Bearer <rw>" ls-remote <url>/site.git` exits 0 with empty stdout.
   - The same with `-c protocol.version=1`. git 2.34's default asks for version 2; this case and the previous one together prove that a version 0 answer is accepted either way.
2. **Empty repository, Basic through the challenge:**
   - `git ls-remote http://x:<rw>@127.0.0.1:<port>/site.git` exits 0. git sends credentials from the URL only after a 401 that carries `WWW-Authenticate`, so this proves the challenge works end to end.
   - Have the server count requests (a wrapping handler in the test). Expect two for `info/refs`: the first with no `Authorization` header, the second with it.
3. **No credentials:**
   - `git ls-remote <url>/site.git` exits non-zero.
   - stderr contains `could not read Username`. That shows git received the challenge and wanted to prompt.
4. **Clone of the empty repository:**
   - `git clone` with the Bearer header exits 0, and stderr contains `You appear to have cloned an empty repository.`
   - The clone directory has a `.git` directory.
5. **Refs and `HEAD`:**
   - Insert by hand `refs/heads/main` = 20 × `0x11`, `refs/heads/dev` = 20 × `0x22`, and `refs/tags/v1` = 20 × `0x33`. `git ls-remote` doesn't check that the objects exist.
   - `git ls-remote` prints exactly four lines: `1111… HEAD`, `2222… refs/heads/dev`, `1111… refs/heads/main`, `3333… refs/tags/v1`, tab-separated.
   - `git ls-remote --symref <url> HEAD` prints `ref: refs/heads/main\tHEAD` first.
6. **Push is refused for a read-only token:** `git push` of a fresh local commit with the read-only Bearer header exits non-zero, with `403` in stderr. Nothing reaches a POST route, because git stops after `info/refs`.

### `cmd/littlekittyvcs/run_test.go`

The existing three tests stay, switched to `testdb.URL`. `TestHealthyWithBucketDown` additionally checks that `GET /site.git/info/refs?service=git-upload-pack` with no token gets 401. That shows `runServe` passed a real store and the settings to `httpapi.New`.

## Done means

The report back must contain this table, every row filled with evidence (command output or file path), or the piece is not done:

| Item | Evidence |
|---|---|
| `go vet ./...` clean | |
| `go test ./... -count=1` without `LKV_TEST_DATABASE_URL`: passes, database and git tests skipped (list them) | |
| `go test ./... -count=1` with it, after `pg_isready`: every test passes | |
| The exact empty upload-pack advertisement bytes, shown with `%q` | |
| `git ls-remote` output for the repository with refs, and the `--symref` output | |
| The request count and `Authorization` presence from the Basic challenge test | |
| stderr of the no-credentials case and of the empty clone | |
| The two old copies of the schema helper are gone (`grep -rn "lkv_test_" --include=*.go`) | |
| Every go-git or pgx name in this document that turned out different | |
| Anything in this document that was wrong or ambiguous, and what was done about it | |
