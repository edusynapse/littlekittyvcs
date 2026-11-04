package integration

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/httpapi"
)

func TestFetchFastForward(t *testing.T) {
	h := newHarness(t, deny(), nil)
	fetchFastForward(h)
}

func TestFetchDiverged(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("site")
	srv, counter := h.serveUploadCounter()
	remote := srv.URL + "/site.git"
	dir := h.initRepo("main")
	commitFiles(h, dir, 1, 60, "c")
	h.push(dir, remote, "HEAD:refs/heads/main")
	client, _, _ := h.cloneURL(remote)
	commitFiles(h, client, 1, 40, "local")
	commitFiles(h, dir, 61, 40, "c")
	h.push(dir, remote, "HEAD:refs/heads/main")
	tip := h.rev(dir, "HEAD")
	before := inPack(h, client)
	counter.reset()
	fetchOrigin(h, client)
	after := inPack(h, client)
	posts, _ := counter.snapshot()
	t.Logf("EVIDENCE diverged posts=%d in-pack %d -> %d", posts, before, after)
	if after-before != 120 {
		t.Fatalf("in-pack grew %d", after-before)
	}
	if h.rev(client, "origin/main") != tip {
		t.Fatal("origin/main")
	}
	fsckClean(h, client)
}

func TestFetchTags(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("site")
	srv, _ := h.serveUploadCounter()
	remote := srv.URL + "/site.git"
	dir := h.initRepo("main")
	ids := commitFiles(h, dir, 1, 60, "c")
	h.push(dir, remote, "HEAD:refs/heads/main")
	client, _, _ := h.cloneURL(remote)
	commitFiles(h, dir, 61, 10, "c")
	h.must(dir, "tag", "-a", "tag70", "HEAD", "-m", "tag70")
	h.must(dir, "tag", "-a", "tag20", ids[19], "-m", "tag20")
	h.must(dir, "-c", bearer(h.token), "push", remote, "HEAD:refs/heads/main", "refs/tags/tag70", "refs/tags/tag20")
	_, stderr := fetchOrigin(h, client)
	logTagLines(t, stderr)
	kind, _ := h.must(client, "cat-file", "-t", "tag20")
	if strings.TrimSpace(kind) != "tag" {
		t.Fatalf("tag20 type %q", strings.TrimSpace(kind))
	}
	h.rev(client, "refs/tags/tag70")
	h.must(dir, "tag", "-a", "tagtag", "tag70", "-m", "tagtag")
	h.must(dir, "-c", bearer(h.token), "push", remote, "refs/tags/tagtag")
	_, stderr = fetchOrigin(h, client)
	logTagLines(t, stderr)
	if h.rev(client, "tagtag^{}") != h.rev(dir, "tag70^{}") {
		t.Fatal("tag of a tag did not peel to the commit")
	}
	t.Logf("EVIDENCE tagtag peels to the commit")
}

func TestFetchCloneNAK(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("site")
	srv, counter := h.serveUploadCounter()
	remote := srv.URL + "/site.git"
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("hello\n"))
	h.commitAll(dir, "hello")
	h.push(dir, remote, "HEAD:refs/heads/main")
	counter.reset()
	h.cloneURL(remote)
	posts, first := counter.snapshot()
	prefix := first
	if len(prefix) > 9 {
		prefix = prefix[:9]
	}
	t.Logf("EVIDENCE clone posts=%d first done prefix %q", posts, string(prefix))
	if !bytes.HasPrefix(first, []byte("0008NAK\n")) {
		t.Fatalf("first done response does not begin with NAK (len %d)", len(first))
	}
}

func TestFetchMergeOldBranch(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("site")
	srv, counter := h.serveUploadCounter()
	remote := srv.URL + "/site.git"
	dir := h.initRepo("main")
	ids := commitFiles(h, dir, 1, 30, "c")
	h.push(dir, remote, "HEAD:refs/heads/main")
	client, _, _ := h.cloneURL(remote)
	h.must(dir, "checkout", "-b", "side", ids[4])
	for i := 1; i <= 8; i++ {
		writeFile(t, filepath.Join(dir, "side.txt"), []byte(fmt.Sprintf("side %d\n", i)))
		h.commitAll(dir, fmt.Sprintf("side %d", i))
	}
	h.must(dir, "checkout", "main")
	h.must(dir, "merge", "--no-ff", "-m", "merge side", "side")
	h.push(dir, remote, "HEAD:refs/heads/main")
	full, _, _ := h.cloneURL(remote)
	fullN := inPack(h, full)
	before := inPack(h, client)
	counter.reset()
	fetchOrigin(h, client)
	after := inPack(h, client)
	grew := after - before
	t.Logf("EVIDENCE merge in-pack %d -> %d grew %d full clone in-pack %d", before, after, grew, fullN)
	if grew <= 0 || grew >= fullN {
		t.Fatalf("grew %d full %d", grew, fullN)
	}
	fsckClean(h, client)
	_, _ = counter.snapshot()
}

func TestFetchRestorePeeled(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("hello\n"))
	h.commitAll(dir, "hello")
	h.must(dir, "tag", "-a", "v1", "HEAD", "-m", "v1")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	h.push(dir, h.remote("site"), "refs/tags/v1")
	st := h.openEmpty()
	out, failed := h.restoreInto(st, "")
	if failed != 0 {
		t.Fatalf("failed %d", failed)
	}
	t.Logf("EVIDENCE restore %s", strings.TrimSpace(lineContaining(out, "restored site ")))
	srv, token := h.serveStore(st)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/site.git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info/refs %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("refs/tags/v1^{}")) {
		t.Fatal("peeled line missing")
	}
	t.Logf("EVIDENCE info/refs has the tag peeled line")
}

func TestFetchFastForwardRustfs(t *testing.T) {
	if os.Getenv("LKV_TEST_S3_ENDPOINT") == "" {
		t.Skip("LKV_TEST_S3_ENDPOINT is not set")
	}
	h := newHarness(t, deny(), s3Bucket(t))
	h.timeout = 3 * time.Minute
	fetchFastForward(h)
}

func fetchFastForward(h *harness) {
	t := h.t
	repo := h.createRepo("site")
	if h.mem == nil {
		id := repo.ID
		bucket := h.bucket
		t.Cleanup(func() { deleteRepoObjects(t, bucket, id) })
	}
	srv, counter := h.serveUploadCounter()
	remote := srv.URL + "/site.git"
	dir := h.initRepo("main")
	commitFiles(h, dir, 1, 60, "c")
	h.push(dir, remote, "HEAD:refs/heads/main")
	client, _, _ := h.cloneURL(remote)
	second, _, _ := h.cloneURL(remote)
	commitFiles(h, second, 61, 40, "c")
	h.push(second, remote, "HEAD:refs/heads/main")
	tip := h.rev(second, "HEAD")
	before := inPack(h, client)
	counter.reset()
	fetchOrigin(h, client)
	after := inPack(h, client)
	posts, _ := counter.snapshot()
	t.Logf("EVIDENCE fast-forward posts=%d in-pack %d -> %d", posts, before, after)
	if posts != 2 {
		t.Fatalf("posts %d", posts)
	}
	if after-before != 120 {
		t.Fatalf("in-pack grew %d", after-before)
	}
	if h.rev(client, "origin/main") != tip {
		t.Fatal("origin/main")
	}
	fsckClean(h, client)
}

func commitFiles(h *harness, dir string, start, n int, prefix string) []string {
	h.t.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		num := start + i
		name := fmt.Sprintf("f%d.txt", (num-1)%7)
		writeFile(h.t, filepath.Join(dir, name), []byte(fmt.Sprintf("%s %d\n", prefix, num)))
		ids[i] = h.commitAll(dir, fmt.Sprintf("%s %d", prefix, num))
	}
	return ids
}

func fetchOrigin(h *harness, dir string) (string, string) {
	h.t.Helper()
	return h.must(dir,
		"-c", bearer(h.token),
		"-c", "fetch.unpackLimit=1",
		"-c", "transfer.unpackLimit=1",
		"fetch", "origin")
}

func inPack(h *harness, dir string) int {
	h.t.Helper()
	out, _ := h.must(dir, "count-objects", "-v")
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "in-pack:") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "in-pack:")))
		if err != nil {
			h.t.Fatal(err)
		}
		return n
	}
	h.t.Fatal("in-pack missing")
	return 0
}

func fsckClean(h *harness, dir string) {
	h.t.Helper()
	out, errOut := h.must(dir, "fsck", "--full")
	text := out + errOut
	if strings.Contains(text, "error") || strings.Contains(text, "missing") || strings.Contains(text, "broken") {
		h.t.Fatalf("fsck failed")
	}
}

func logTagLines(t *testing.T, stderr string) {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "tag20") || strings.Contains(line, "tag70") || strings.Contains(line, "tagtag") {
			t.Logf("EVIDENCE %s", objectIDRE.ReplaceAllString(line, "object"))
		}
	}
}

func (h *harness) serveUploadCounter() (*httptest.Server, *uploadCounter) {
	h.t.Helper()
	counter := &uploadCounter{next: httpapi.New(h.log, httpapi.Deps{
		Store: h.st, Bucket: h.bucket, Keyring: h.kr, Index: h.idx,
	}, deny())}
	srv := httptest.NewServer(counter)
	h.t.Cleanup(srv.Close)
	return srv, counter
}

type uploadCounter struct {
	next      http.Handler
	mu        sync.Mutex
	posts     int
	firstDone []byte
}

func (c *uploadCounter) reset() {
	c.mu.Lock()
	c.posts = 0
	c.firstDone = nil
	c.mu.Unlock()
}

func (c *uploadCounter) snapshot() (int, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posts, append([]byte(nil), c.firstDone...)
}

func (c *uploadCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "invalid request\n", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	upload := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack")
	plain := body
	if upload && strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		if decoded, ok := gunzip(body); ok {
			plain = decoded
		}
	}
	done := upload && bytes.Contains(plain, []byte("done\n"))
	tap := &bodyTap{ResponseWriter: w}
	c.next.ServeHTTP(tap, r)
	if !upload {
		return
	}
	c.mu.Lock()
	c.posts++
	if done && c.firstDone == nil {
		c.firstDone = append([]byte(nil), tap.buf.Bytes()...)
	}
	c.mu.Unlock()
}

func gunzip(body []byte) ([]byte, bool) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	decoded, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return nil, false
	}
	return decoded, true
}

type bodyTap struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (b *bodyTap) Write(p []byte) (int, error) {
	n, err := b.ResponseWriter.Write(p)
	_, _ = b.buf.Write(p[:n])
	return n, err
}
