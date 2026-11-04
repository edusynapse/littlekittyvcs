package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/httpapi"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
)

const sentinel = "lkv-sentinel-0123456789abcdef0123456789a"

func TestPushThenClone(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte(sentinel+"\n"))
	writeFile(t, filepath.Join(dir, "bin.dat"), []byte{0, 1, 2, 255, 10})
	writeFile(t, filepath.Join(dir, "a", "b", "c.txt"), []byte("nested\n"))
	h.commitAll(dir, "one")

	stdout, stderr := h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	combined := stdout + stderr
	if !strings.Contains(combined, "[new branch]") {
		t.Fatalf("push output:\n%s", redact(combined))
	}
	t.Logf("EVIDENCE push stdout=%q", redact(stdout))
	t.Logf("EVIDENCE push stderr=%q", redact(stderr))

	clone, _, cloneErr := h.clone(h.remote("repo"))
	if cloneErr != "" && strings.Contains(cloneErr, "fatal") {
		t.Fatalf("clone stderr %s", redact(cloneErr))
	}
	n := compareTrees(t, dir, clone)
	t.Logf("EVIDENCE files identical count=%d", n)
	fsckOut, fsckErr := h.must(clone, "fsck", "--full")
	if strings.Contains(fsckOut+fsckErr, "error") || strings.Contains(fsckOut+fsckErr, "missing") || strings.Contains(fsckOut+fsckErr, "broken") {
		t.Fatalf("fsck stdout %q stderr %q", fsckOut, fsckErr)
	}
	t.Logf("EVIDENCE fsck stdout=%q stderr=%q", fsckOut, fsckErr)
	srcLog, _ := h.must(dir, "log", "--format=%H")
	cloneLog, _ := h.must(clone, "log", "--format=%H")
	if srcLog != cloneLog {
		t.Fatalf("log\nsrc %s\nclone %s", srcLog, cloneLog)
	}

	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(srcLog), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	checked := h.checkBucket(ids)
	t.Logf("EVIDENCE bucket objects=%d each checked: does not start with PACK, does not contain the sentinel, does not contain a commit id as hex or raw", checked)

	logs := h.logs.String()
	for _, bad := range append([]string{"refs/", sentinel, "lkv_"}, ids...) {
		if strings.Contains(logs, bad) {
			t.Fatalf("log contains %q", bad)
		}
	}
	for _, id := range ids {
		raw, err := hex.DecodeString(id)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(h.logs.Bytes(), raw) {
			t.Fatal("log contains a raw commit id")
		}
	}
}

func TestSecondPushFetchPull(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("one\n"))
	h.commitAll(dir, "one")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	first, _, _ := h.clone(h.remote("repo"))

	writeFile(t, filepath.Join(dir, "note.txt"), []byte("two\n"))
	h.commitAll(dir, "two")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("three\n"))
	h.commitAll(dir, "three")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	if packs := h.packs("repo"); len(packs) != 2 {
		t.Fatalf("packs %d", len(packs))
	}

	fresh, _, _ := h.clone(h.remote("repo"))
	srcLog, _ := h.must(dir, "log", "--format=%H")
	freshLog, _ := h.must(fresh, "log", "--format=%H")
	if srcLog != freshLog || strings.Count(freshLog, "\n") != 3 {
		t.Fatalf("fresh log\n%s", freshLog)
	}
	h.must(fresh, "fsck", "--full")

	h.must(first, "-c", bearer(h.token), "pull")
	pulled, _ := h.must(first, "log", "--format=%H")
	if pulled != srcLog {
		t.Fatalf("pull log\n%s", pulled)
	}
	h.must(first, "fsck", "--full")
}

func TestGzipFetch(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("base\n"))
	h.commitAll(dir, "base")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	clone, _, _ := h.clone(h.remote("repo"))
	// git 2.34.1 stateless fetch flushes after 16, 32, then 64 haves.
	// The first two requests are 16 haves (under 1024 bytes, so not gzipped).
	// The third is 32 haves, which is over 1024 and under http.postBuffer, so git gzips it.
	// Forty commits never reach that third window.
	for i := 0; i < 80; i++ {
		h.must(clone, "commit", "--allow-empty", "-m", fmt.Sprintf("local %d", i))
	}
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("remote\n"))
	h.commitAll(dir, "remote")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")

	h.cap.reset()
	stdout, stderr, err := h.git(clone, "-c", bearer(h.token), "fetch")
	snaps := h.cap.snapshot()
	h.logPosts(snaps)
	if err != nil {
		t.Fatalf("fetch err %v stdout %s stderr %s", err, redact(stdout), redact(stderr))
	}
	if len(snaps) == 0 || !snaps[len(snaps)-1].done {
		t.Fatal("fetch did not end with done")
	}
	gzipN := 0
	for _, s := range snaps {
		if s.gzip {
			gzipN++
		}
	}
	if gzipN == 0 {
		t.Fatal("no gzip fetch request")
	}
	var dones []bool
	for _, s := range snaps {
		dones = append(dones, s.done)
	}
	t.Logf("EVIDENCE gzip fetch posts=%d gzip=%d done=%v", len(snaps), gzipN, dones)
}

func TestNonFastForward(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	h.must(dir, "commit", "--allow-empty", "-m", "one")
	first := h.rev(dir, "HEAD")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	h.must(dir, "commit", "--amend", "--allow-empty", "-m", "amended")

	_, stderr, err := h.git(dir, "-c", bearer(h.token), "push", "--force", h.remote("repo"), "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(stderr, "non-fast-forward") {
		t.Fatalf("err %v stderr %s", err, redact(stderr))
	}
	t.Logf("EVIDENCE non-fast-forward stderr=%q", redact(stderr))
	remote := h.lsRemote(h.remote("repo"))
	if !strings.Contains(remote, first) {
		t.Fatalf("ls-remote changed\n%s", remote)
	}

	open := h.serve(httpapi.Config{DenyNonFastForward: false}, objindex.New())
	h.must(dir, "-c", bearer(h.token), "push", "--force", open.URL+"/repo.git", "HEAD:refs/heads/main")
	amended := h.rev(dir, "HEAD")
	remote = h.lsRemote(open.URL + "/repo.git")
	if !strings.Contains(remote, amended) || strings.Contains(remote, first+"\t") {
		t.Fatalf("forced ref\n%s", remote)
	}
}

func TestDeleteBranch(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("main\n"))
	h.commitAll(dir, "main")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	h.must(dir, "checkout", "-b", "dev")
	writeFile(t, filepath.Join(dir, "dev.txt"), []byte("dev\n"))
	h.commitAll(dir, "dev")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/dev")
	before := len(h.packs("repo"))
	h.push(dir, h.remote("repo"), ":refs/heads/dev")
	if len(h.packs("repo")) != before {
		t.Fatalf("packs %d before %d", len(h.packs("repo")), before)
	}
	if strings.Contains(h.lsRemote(h.remote("repo")), "refs/heads/dev") {
		t.Fatal("dev still listed")
	}
	clone, _, _ := h.clone(h.remote("repo"))
	if _, _, err := h.git(clone, "rev-parse", "--verify", "origin/dev"); err == nil {
		t.Fatal("origin/dev exists")
	}
}

func TestConcurrentPush(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("parent\n"))
	parent := h.commitAll(dir, "parent")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")

	writeFile(t, filepath.Join(dir, "note.txt"), []byte("child-1\n"))
	c1 := h.commitAll(dir, "child-1")
	h.must(dir, "reset", "--hard", parent)
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("child-2\n"))
	c2 := h.commitAll(dir, "child-2")
	p1 := h.pack(dir, c1+"\n^"+parent+"\n", false)
	p2 := h.pack(dir, c2+"\n^"+parent+"\n", false)
	b1 := receiveBody(t, parent, c1, "refs/heads/main", p1)
	b2 := receiveBody(t, parent, c2, "refs/heads/main", p2)

	url := h.srv.URL + "/repo.git/git-receive-pack"
	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([][]byte, 2)
	wg.Add(2)
	start := make(chan struct{})
	for i, body := range [][]byte{b1, b2} {
		go func(i int, body []byte) {
			defer wg.Done()
			<-start
			codes[i], bodies[i] = postReceive(t, url, h.token, body)
		}(i, body)
	}
	close(start)
	wg.Wait()

	okN, ngN := 0, 0
	var winner string
	for i, body := range bodies {
		t.Logf("EVIDENCE concurrent report %d status=%d body=%q", i, codes[i], body)
		if codes[i] != http.StatusOK {
			t.Fatalf("status %d", codes[i])
		}
		switch {
		case bytes.Contains(body, []byte("ok refs/heads/main")) && !bytes.Contains(body, []byte("ng refs/heads/main")):
			okN++
			if i == 0 {
				winner = c1
			} else {
				winner = c2
			}
		case bytes.Contains(body, []byte("ng refs/heads/main failed to update ref")):
			ngN++
		default:
			t.Fatalf("unexpected report %q", body)
		}
	}
	if okN != 1 || ngN != 1 {
		t.Fatalf("ok %d ng %d", okN, ngN)
	}
	remote := h.lsRemote(h.remote("repo"))
	if !strings.Contains(remote, winner+"\trefs/heads/main") {
		t.Fatalf("ls-remote\n%s\nwinner %s", remote, winner)
	}
	clone, _, _ := h.clone(h.remote("repo"))
	h.must(clone, "fsck", "--full")
}

func TestMissingParent(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("parent\n"))
	parent := h.commitAll(dir, "parent")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("child\n"))
	child := h.commitAll(dir, "child")
	pack := h.pack(dir, child+"\n^"+parent+"\n", false)
	body := receiveBody(t, zeroID, child, "refs/heads/main", pack)
	before := h.objectCount()
	status, report := postReceive(t, h.srv.URL+"/repo.git/git-receive-pack", h.token, body)
	t.Logf("EVIDENCE missing-parent status=%d body=%q", status, report)
	if status != http.StatusOK || !bytes.Contains(report, []byte("ng refs/heads/main missing object")) {
		t.Fatalf("status %d body %q", status, report)
	}
	if strings.Contains(h.lsRemote(h.remote("repo")), "refs/heads/main") {
		t.Fatal("ref was created")
	}
	if len(h.packs("repo")) != 0 {
		t.Fatal("pack row inserted")
	}
	if h.objectCount() != before {
		t.Fatalf("bucket objects %d before %d", h.objectCount(), before)
	}
}

func TestThinPack(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	body := bytes.Repeat([]byte("abcdefghijklmnopqrstuvwxyz\n"), 8000)
	writeFile(t, filepath.Join(dir, "big.txt"), body)
	parent := h.commitAll(dir, "parent")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	body[len(body)/2] = 'Z'
	writeFile(t, filepath.Join(dir, "big.txt"), body)
	child := h.commitAll(dir, "child")
	pack := h.pack(dir, child+"\n^"+parent+"\n", true)
	req := receiveBody(t, parent, child, "refs/heads/main", pack)
	status, report := postReceive(t, h.srv.URL+"/repo.git/git-receive-pack", h.token, req)
	t.Logf("EVIDENCE thin-pack status=%d body=%q", status, report)
	if status != http.StatusOK || bytes.Contains(report, []byte("unpack ok")) {
		t.Fatalf("status %d body %q", status, report)
	}
	remote := h.lsRemote(h.remote("repo"))
	if !strings.Contains(remote, parent+"\trefs/heads/main") || strings.Contains(remote, child) {
		t.Fatalf("ref moved\n%s", remote)
	}
}

func TestRetryAppliedPush(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("retry\n"))
	tip := h.commitAll(dir, "retry")
	pack := h.pack(dir, tip+"\n", false)
	body := receiveBody(t, zeroID, tip, "refs/heads/retry", pack)
	url := h.srv.URL + "/repo.git/git-receive-pack"
	status, report := postReceive(t, url, h.token, body)
	t.Logf("EVIDENCE retry first status=%d body=%q", status, report)
	if status != http.StatusOK || !bytes.Contains(report, []byte("ok refs/heads/retry")) {
		t.Fatalf("first %d %q", status, report)
	}
	objects := h.objectCount()
	packs := len(h.packs("repo"))
	status, report = postReceive(t, url, h.token, body)
	t.Logf("EVIDENCE retry second status=%d body=%q", status, report)
	if status != http.StatusOK || !bytes.Contains(report, []byte("ok refs/heads/retry")) {
		t.Fatalf("second %d %q", status, report)
	}
	if h.objectCount() != objects || len(h.packs("repo")) != packs {
		t.Fatalf("uploaded objects %d packs %d", h.objectCount(), len(h.packs("repo")))
	}
}

func TestTwoProcesses(t *testing.T) {
	h := newHarness(t, deny(), nil)
	other := h.serve(deny(), objindex.New())
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("from-a\n"))
	h.commitAll(dir, "from-a")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")

	cloneB, _, _ := h.cloneURL(other.URL + "/repo.git")
	got, _ := h.must(cloneB, "log", "--format=%H")
	want, _ := h.must(dir, "log", "--format=%H")
	if got != want {
		t.Fatalf("clone through B\n%s", got)
	}

	writeFile(t, filepath.Join(dir, "note.txt"), []byte("from-b\n"))
	h.commitAll(dir, "from-b")
	h.must(dir, "-c", bearer(h.token), "push", other.URL+"/repo.git", "HEAD:refs/heads/main")
	cloneA, _, _ := h.clone(h.remote("repo"))
	got, _ = h.must(cloneA, "log", "--format=%H")
	want, _ = h.must(dir, "log", "--format=%H")
	if got != want {
		t.Fatalf("clone through A\n%s", got)
	}
	h.must(cloneA, "fsck", "--full")
}

func TestRestart(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("one\n"))
	h.commitAll(dir, "one")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("two\n"))
	h.commitAll(dir, "two")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")

	restarted := h.serve(deny(), objindex.New())
	clone, _, _ := h.cloneURL(restarted.URL + "/repo.git")
	h.must(clone, "fsck", "--full")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("three\n"))
	h.commitAll(dir, "three")
	h.must(dir, "-c", bearer(h.token), "push", restarted.URL+"/repo.git", "HEAD:refs/heads/main")
	h.must(clone, "-c", bearer(h.token), "fetch")
	h.must(clone, "fsck", "--full")
	remote := h.lsRemote(restarted.URL + "/repo.git")
	tip := h.rev(dir, "HEAD")
	if !strings.Contains(remote, tip+"\trefs/heads/main") {
		t.Fatalf("fetch remote\n%s", remote)
	}
}

func TestAutoCreate(t *testing.T) {
	h := newHarness(t, httpapi.Config{AutoCreate: true, DenyNonFastForward: true}, nil)
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("created\n"))
	h.commitAll(dir, "created")
	h.push(dir, h.remote("fresh"), "HEAD:refs/heads/main")
	if _, _, _, err := h.st.ReadRepo(context.Background(), "fresh"); err != nil {
		t.Fatal(err)
	}
	clone, _, _ := h.clone(h.remote("fresh"))
	compareTrees(t, dir, clone)

	closed := newHarness(t, deny(), nil)
	src := closed.initRepo("main")
	writeFile(t, filepath.Join(src, "note.txt"), []byte("nope\n"))
	closed.commitAll(src, "nope")
	_, stderr, err := closed.git(src, "-c", bearer(closed.token), "push", closed.remote("missing"), "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(stderr, "404") {
		t.Fatalf("err %v stderr %s", err, redact(stderr))
	}
	req, err := http.NewRequest(http.MethodGet, closed.srv.URL+"/missing.git/info/refs?service=git-receive-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+closed.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("info/refs %d", resp.StatusCode)
	}
}

func TestFirstBranchMaster(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("master")
	writeFile(t, filepath.Join(dir, "README"), []byte("master\n"))
	h.commitAll(dir, "master")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/master")
	clone, _, stderr := h.clone(h.remote("repo"))
	if strings.Contains(stderr, "remote HEAD refers to nonexistent ref") {
		t.Fatalf("stderr %s", redact(stderr))
	}
	compareTrees(t, dir, clone)
}

func TestSubmodulePush(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	const sub = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h.must(dir, "update-index", "--add", "--cacheinfo", "160000,"+sub+",vendor/lib")
	h.must(dir, "commit", "-m", "submodule")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	clone, _, _ := h.clone(h.remote("repo"))
	out, _ := h.must(clone, "ls-tree", "-r", "HEAD")
	if !strings.Contains(out, "160000") || !strings.Contains(out, sub) {
		t.Fatalf("ls-tree\n%s", out)
	}
}

func TestDeleteWithPack(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("keep\n"))
	tip := h.commitAll(dir, "keep")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/dev")
	before := len(h.packs("repo"))
	pack := h.pack(dir, tip+"\n", false)
	body := receiveBody(t, tip, zeroID, "refs/heads/dev", pack)
	status, report := postReceive(t, h.srv.URL+"/repo.git/git-receive-pack", h.token, body)
	if status != http.StatusBadRequest || string(report) != "unexpected pack\n" {
		t.Fatalf("status %d body %q", status, report)
	}
	if !strings.Contains(h.lsRemote(h.remote("repo")), "refs/heads/dev") {
		t.Fatal("dev was deleted")
	}
	if len(h.packs("repo")) != before {
		t.Fatal("pack rows changed")
	}
}

func TestReadOnlyPost(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	ro := h.insertToken("ro", false, nil)
	status, body := postReceive(t, h.srv.URL+"/repo.git/git-receive-pack", ro, []byte("not-a-pack"))
	if status != http.StatusForbidden || string(body) != "this token is read-only\n" {
		t.Fatalf("status %d body %q", status, body)
	}
}

func TestTokenForOtherRepo(t *testing.T) {
	h := newHarness(t, deny(), nil)
	a := h.createRepo("alpha")
	h.createRepo("beta")
	token := h.insertToken("alpha-only", true, &a.ID)
	status, _ := postGit(t, h.srv.URL+"/beta.git/git-upload-pack", token, "application/x-git-upload-pack-request", []byte("0000"))
	if status != http.StatusNotFound {
		t.Fatalf("status %d", status)
	}
}

func TestPushExceedsPostBuffer(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	big := make([]byte, 256<<10)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "wide.bin"), big)
	h.commitAll(dir, "wide")
	// Above LARGE_PACKET_MAX so the first outer pkt-line fits, and below this
	// pack, so git 2.34.1 probes with "0000" and then sends the body chunked.
	h.must(dir, "-c", "http.postBuffer=100000", "-c", bearer(h.token), "push", h.remote("repo"), "HEAD:refs/heads/main")
	clone, _, _ := h.clone(h.remote("repo"))
	got, err := os.ReadFile(filepath.Join(clone, "wide.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("wide.bin mismatch")
	}
}

func TestRustfsLargeFile(t *testing.T) {
	if os.Getenv("LKV_TEST_DATABASE_URL") == "" {
		t.Skip("LKV_TEST_DATABASE_URL is not set")
	}
	if os.Getenv("LKV_TEST_S3_ENDPOINT") == "" {
		t.Skip("LKV_TEST_S3_ENDPOINT is not set")
	}
	s3 := s3Bucket(t)
	h := newHarness(t, deny(), s3)
	h.timeout = 3 * time.Minute
	h.createRepo("repo")
	dir := h.initRepo("main")
	big := make([]byte, 20<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(big)
	writeFile(t, filepath.Join(dir, "big.bin"), big)
	h.commitAll(dir, "big")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")
	packs := h.packs("repo")
	if len(packs) != 1 {
		t.Fatalf("packs %d", len(packs))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s3.Delete(ctx, packs[0].PackKey)
		_ = s3.Delete(ctx, packs[0].IdxKey)
	})
	if packs[0].Bytes <= 16<<20 {
		t.Fatalf("pack ciphertext %d", packs[0].Bytes)
	}
	clone, _, _ := h.clone(h.remote("repo"))
	got, err := os.ReadFile(filepath.Join(clone, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sum {
		t.Fatal("sha256 mismatch")
	}
	t.Logf("EVIDENCE rustfs pack ciphertext bytes=%d sha256 match", packs[0].Bytes)
}

func s3Bucket(t *testing.T) *blob.S3 {
	t.Helper()
	pathStyle := false
	switch strings.ToLower(os.Getenv("LKV_TEST_S3_PATH_STYLE")) {
	case "1", "true", "yes":
		pathStyle = true
	}
	s3, err := blob.NewS3(context.Background(), blob.S3Config{
		Endpoint:  os.Getenv("LKV_TEST_S3_ENDPOINT"),
		Region:    os.Getenv("LKV_TEST_S3_REGION"),
		Bucket:    os.Getenv("LKV_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("LKV_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("LKV_TEST_S3_SECRET_KEY"),
		PathStyle: pathStyle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s3
}

func (h *harness) cloneURL(remote string) (string, string, string) {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), "clone")
	stdout, stderr := h.must("", "-c", bearer(h.token), "clone", remote, dir)
	return dir, stdout, stderr
}

func (h *harness) lsRemote(remote string) string {
	h.t.Helper()
	out, _ := h.must("", "-c", bearer(h.token), "ls-remote", remote)
	return out
}

func (h *harness) objectCount() int {
	h.t.Helper()
	if h.mem == nil {
		h.t.Fatal("object count needs the memory bucket")
	}
	n := 0
	err := h.mem.List(context.Background(), "repos/", func(blob.Info) error {
		n++
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) checkBucket(commitIDs []string) int {
	h.t.Helper()
	var rawIDs [][]byte
	for _, id := range commitIDs {
		raw, err := hex.DecodeString(id)
		if err != nil {
			h.t.Fatal(err)
		}
		rawIDs = append(rawIDs, raw)
	}
	n := 0
	err := h.mem.List(context.Background(), "repos/", func(info blob.Info) error {
		raw := h.mem.Raw(info.Key)
		if bytes.HasPrefix(raw, []byte("PACK")) {
			h.t.Fatalf("%s starts with PACK", info.Key)
		}
		if bytes.Contains(raw, []byte(sentinel)) {
			h.t.Fatalf("%s contains the sentinel", info.Key)
		}
		for i, id := range commitIDs {
			if bytes.Contains(raw, []byte(id)) || bytes.Contains(raw, rawIDs[i]) {
				h.t.Fatalf("%s contains a commit id", info.Key)
			}
		}
		n++
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if n == 0 {
		h.t.Fatal("no bucket objects")
	}
	return n
}

func (h *harness) logPosts(snaps []postSnap) {
	h.t.Helper()
	for i, s := range snaps {
		h.t.Logf("EVIDENCE fetch post %d encoding=%q gzip=%v done=%v bytes=%d body=%q", i, s.encoding, s.gzip, s.done, s.n, s.head)
	}
}
