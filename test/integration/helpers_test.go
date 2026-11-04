package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/httpapi"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

const zeroID = "0000000000000000000000000000000000000000"

type harness struct {
	t       *testing.T
	st      *store.Store
	bucket  blob.Store
	mem     *blob.Mem
	kr      *objcrypt.Keyring
	idx     *objindex.Index
	log     *slog.Logger
	logs    *bytes.Buffer
	cap     *bodyCapture
	chunks  *objcrypt.ChunkCache
	srv     *httptest.Server
	token   string
	env     []string
	timeout time.Duration
}

func newHarness(t *testing.T, cfg httpapi.Config, bucket blob.Store) *harness {
	t.Helper()
	return newHarnessCache(t, cfg, bucket, nil)
}

func newHarnessCache(t *testing.T, cfg httpapi.Config, bucket blob.Store, chunks *objcrypt.ChunkCache) *harness {
	t.Helper()
	raw := testdb.URL(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	st, err := store.Open(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	var master [32]byte
	copy(master[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	var mem *blob.Mem
	if bucket == nil {
		mem = blob.NewMem()
		bucket = mem
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	idx := objindex.New()
	cap := &bodyCapture{next: httpapi.New(log, httpapi.Deps{
		Store: st, Bucket: bucket, Keyring: kr, Index: idx, ChunkCache: chunks,
	}, cfg)}
	srv := httptest.NewServer(cap)
	t.Cleanup(srv.Close)
	h := &harness{
		t: t, st: st, bucket: bucket, mem: mem, kr: kr, idx: idx,
		log: log, logs: &logs, cap: cap, chunks: chunks, srv: srv,
		env:     gitEnv(t),
		timeout: 90 * time.Second,
	}
	h.token = h.insertToken("rw", true, nil)
	return h
}

func (h *harness) insertToken(label string, write bool, repoID *[16]byte) string {
	h.t.Helper()
	plain, hash, err := auth.Generate()
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := store.NewID()
	if err != nil {
		h.t.Fatal(err)
	}
	err = h.st.InsertToken(context.Background(), hash, store.TokenRow{
		ID: id, Label: label, Write: write, Admin: false, RepoID: repoID,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return plain
}

func (h *harness) createRepo(name string) store.Repo {
	h.t.Helper()
	id, err := store.NewID()
	if err != nil {
		h.t.Fatal(err)
	}
	repo, err := h.st.CreateRepo(context.Background(), id, name)
	if err != nil {
		h.t.Fatal(err)
	}
	return repo
}

func (h *harness) remote(name string) string {
	return h.srv.URL + "/" + name + ".git"
}

func (h *harness) serve(cfg httpapi.Config, idx *objindex.Index) *httptest.Server {
	h.t.Helper()
	if idx == nil {
		idx = objindex.New()
	}
	srv := httptest.NewServer(httpapi.New(h.log, httpapi.Deps{
		Store: h.st, Bucket: h.bucket, Keyring: h.kr, Index: idx, ChunkCache: h.chunks,
	}, cfg))
	h.t.Cleanup(srv.Close)
	return srv
}

// serveHook is a server whose push path stops at BeforeCommit. Nil is production.
func (h *harness) serveHook(hook func(context.Context) error) *httptest.Server {
	h.t.Helper()
	srv := httptest.NewServer(httpapi.New(h.log, httpapi.Deps{
		Store: h.st, Bucket: h.bucket, Keyring: h.kr, Index: h.idx,
		ChunkCache: h.chunks,
		PushHook:   hook,
	}, deny()))
	h.t.Cleanup(srv.Close)
	return srv
}

func (h *harness) insertAdmin(label string) string {
	h.t.Helper()
	plain, hash, err := auth.Generate()
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := store.NewID()
	if err != nil {
		h.t.Fatal(err)
	}
	err = h.st.InsertToken(context.Background(), hash, store.TokenRow{
		ID: id, Label: label, Write: true, Admin: true,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return plain
}

func (h *harness) git(dir string, args ...string) (string, string, error) {
	h.t.Helper()
	full := []string{
		"-c", "commit.gpgsign=false",
		"-c", "user.name=Ada Lovelace",
		"-c", "user.email=ada@example.com",
	}
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (h *harness) must(dir string, args ...string) (string, string) {
	h.t.Helper()
	stdout, stderr, err := h.git(dir, args...)
	if err != nil {
		h.t.Fatalf("git %s\nerr %v\nstdout %s\nstderr %s", strings.Join(args, " "), err, redact(stdout), redact(stderr))
	}
	return stdout, stderr
}

func (h *harness) initRepo(branch string) string {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), "src")
	h.must("", "init", "-b", branch, dir)
	return dir
}

func (h *harness) commitAll(dir, msg string) string {
	h.t.Helper()
	h.must(dir, "add", "-A")
	h.must(dir, "commit", "-m", msg)
	return h.rev(dir, "HEAD")
}

func (h *harness) rev(dir, expr string) string {
	h.t.Helper()
	out, _ := h.must(dir, "rev-parse", expr)
	return strings.TrimSpace(out)
}

func (h *harness) push(dir, remote, spec string) (string, string) {
	h.t.Helper()
	return h.must(dir, "-c", bearer(h.token), "push", remote, spec)
}

func (h *harness) clone(remote string) (string, string, string) {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), "clone")
	stdout, stderr := h.must("", "-c", bearer(h.token), "clone", remote, dir)
	return dir, stdout, stderr
}

func (h *harness) pack(dir, revs string, thin bool) []byte {
	h.t.Helper()
	args := []string{"-c", "commit.gpgsign=false", "-C", dir, "pack-objects", "--revs", "--stdout"}
	if thin {
		args = []string{"-c", "commit.gpgsign=false", "-C", dir, "pack-objects", "--thin", "--revs", "--stdout"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(revs)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("pack-objects: %v %s", err, redact(stderr.String()))
	}
	return stdout.Bytes()
}

// packExact packs exactly the listed objects, without following any links,
// so a test can build a pack that leaves out an object.
func (h *harness) packExact(dir string, ids ...string) []byte {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "pack-objects", "--stdout")
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("pack-objects: %v %s", err, redact(stderr.String()))
	}
	return stdout.Bytes()
}

func (h *harness) packs(name string) []store.Pack {
	h.t.Helper()
	_, _, packs, err := h.st.ReadRepo(context.Background(), name)
	if err != nil {
		h.t.Fatal(err)
	}
	return packs
}

func bearer(token string) string {
	return "http.extraHeader=Authorization: Bearer " + token
}

func gitEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Ada Lovelace",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_AUTHOR_DATE=2020-01-02T03:04:05Z",
		"GIT_COMMITTER_NAME=Ada Lovelace",
		"GIT_COMMITTER_EMAIL=ada@example.com",
		"GIT_COMMITTER_DATE=2020-01-02T03:04:05Z",
		"PATH=" + os.Getenv("PATH"),
	}
}

var tokenRE = regexp.MustCompile(`lkv_[A-Za-z0-9_-]+`)

func redact(s string) string {
	return tokenRE.ReplaceAllString(s, "lkv_<redacted>")
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func compareTrees(t *testing.T, a, b string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(a, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(a, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		n++
		want, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(b, rel))
		if err != nil {
			t.Fatalf("clone missing %s: %v", rel, err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("%s differs", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no files compared")
	}
	return n
}

func receiveBody(t *testing.T, old, newID, ref string, pack []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	line := old + " " + newID + " " + ref + "\x00 report-status side-band-64k agent=git/2.34.1\n"
	if err := enc.EncodeString(line); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write(pack); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func postReceive(t *testing.T, url, token string, body []byte) (int, []byte) {
	t.Helper()
	return postGit(t, url, token, "application/x-git-receive-pack-request", body)
}

func postGit(t *testing.T, url, token, contentType string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, got
}

type postSnap struct {
	encoding string
	gzip     bool
	done     bool
	n        int
	head     string
}

type bodyCapture struct {
	next  http.Handler
	mu    sync.Mutex
	posts []postSnap
}

func (c *bodyCapture) reset() {
	c.mu.Lock()
	c.posts = nil
	c.mu.Unlock()
}

func (c *bodyCapture) snapshot() []postSnap {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]postSnap, len(c.posts))
	copy(out, c.posts)
	return out
}

func (c *bodyCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "invalid request\n", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			c.record(r.Header.Get("Content-Encoding"), body)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	c.next.ServeHTTP(w, r)
}

func (c *bodyCapture) record(enc string, body []byte) {
	plain := body
	gzipped := strings.EqualFold(strings.TrimSpace(enc), "gzip")
	if gzipped {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			decoded, err := io.ReadAll(zr)
			_ = zr.Close()
			if err == nil {
				plain = decoded
			}
		}
	}
	head := plain
	if len(head) > 300 {
		head = head[:300]
	}
	c.mu.Lock()
	c.posts = append(c.posts, postSnap{
		encoding: enc,
		gzip:     gzipped,
		done:     bytes.Contains(plain, []byte("done")),
		n:        len(plain),
		head:     string(head),
	})
	c.mu.Unlock()
}

func deny() httpapi.Config {
	return httpapi.Config{DenyNonFastForward: true}
}
