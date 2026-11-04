package integration

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/gc"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestPushDiesBeforeCommit(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("hello\n"))
	h.commitAll(dir, "one")
	srv := h.serveHook(func(context.Context) error {
		return errors.New("push stopped before commit")
	})
	remote := srv.URL + "/site.git"
	_, stderr, err := h.git(dir, "-c", bearer(h.token), "push", remote, "HEAD:refs/heads/main")
	t.Logf("EVIDENCE push stderr=%s", redact(stderr))
	if err == nil {
		t.Fatal("push succeeded")
	}
	ls, lsErr, err := h.git("", "-c", bearer(h.token), "ls-remote", remote)
	if err != nil {
		t.Fatalf("ls-remote %v %s", err, redact(lsErr))
	}
	if strings.Contains(ls, "refs/heads/") {
		t.Fatalf("branch advertised %s", ls)
	}
	keys := keysUnder(t, h.bucket, repo.ID)
	if len(keys) != 2 {
		t.Fatalf("keys %v", keys)
	}
	var packKey, idxKey string
	for _, key := range keys {
		switch {
		case strings.HasSuffix(key, ".pack"):
			packKey = key
		case strings.HasSuffix(key, ".idx"):
			idxKey = key
		default:
			t.Fatalf("unexpected %s", key)
		}
	}
	if h.packs("site") != nil && len(h.packs("site")) != 0 {
		t.Fatal("pack row")
	}
	t.Logf("EVIDENCE orphan keys %s %s", packKey, idxKey)
	fresh := h.serve(deny(), nil)
	ls, lsErr, err = h.git("", "-c", bearer(h.token), "ls-remote", fresh.URL+"/site.git")
	if err != nil {
		t.Fatalf("restart ls-remote %v %s", err, redact(lsErr))
	}
	if strings.Contains(ls, "refs/heads/") {
		t.Fatalf("restart advertised %s", ls)
	}
	res, inside := runGC(t, h, time.Now)
	t.Logf("EVIDENCE gc inside\n%s", inside)
	if res.Deleted != 0 || !sameKeys(keys, keysUnder(t, h.bucket, repo.ID)) {
		t.Fatalf("inside %+v", res)
	}
	res, past := runGC(t, h, func() time.Time { return time.Now().Add(25 * time.Hour) })
	t.Logf("EVIDENCE gc past\n%s", past)
	if res.Deleted != 2 || len(keysUnder(t, h.bucket, repo.ID)) != 0 {
		t.Fatalf("past %+v\n%s", res, past)
	}
	if !strings.Contains(past, "deleted "+packKey) || !strings.Contains(past, "deleted "+idxKey) {
		t.Fatal("orphan lines")
	}
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	clone, _, _ := h.clone(h.remote("site"))
	fsckOut, fsckErr := h.must(clone, "fsck", "--full")
	if strings.Contains(fsckOut+fsckErr, "error") || strings.Contains(fsckOut+fsckErr, "missing") || strings.Contains(fsckOut+fsckErr, "broken") {
		t.Fatalf("fsck %q %q", fsckOut, fsckErr)
	}
}

func TestPushHeldWhileGCRuns(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("hello\n"))
	commit := h.commitAll(dir, "one")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := h.serveHook(func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	type pushed struct {
		stderr string
		err    error
	}
	done := make(chan pushed, 1)
	go func() {
		_, stderr, err := h.git(dir, "-c", bearer(h.token), "push", srv.URL+"/site.git", "HEAD:refs/heads/main")
		done <- pushed{stderr, err}
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("hook not entered")
	}
	before := keysUnder(t, h.bucket, repo.ID)
	res, text := runGC(t, h, time.Now)
	t.Logf("EVIDENCE gc while held\n%s", text)
	if res.Deleted != 0 || !sameKeys(before, keysUnder(t, h.bucket, repo.ID)) {
		t.Fatalf("held gc %+v\n%s", res, text)
	}
	close(release)
	var got pushed
	select {
	case got = <-done:
	case <-time.After(h.timeout):
		t.Fatal("push did not finish")
	}
	t.Logf("EVIDENCE push stderr=%s", redact(got.stderr))
	if got.err != nil {
		t.Fatalf("push %v %s", got.err, redact(got.stderr))
	}
	clone, _, _ := h.clone(h.remote("site"))
	if h.rev(clone, "HEAD") != commit {
		t.Fatal("clone commit")
	}
	fsckOut, fsckErr := h.must(clone, "fsck", "--full")
	if strings.Contains(fsckOut+fsckErr, "error") || strings.Contains(fsckOut+fsckErr, "missing") || strings.Contains(fsckOut+fsckErr, "broken") {
		t.Fatalf("fsck %q %q", fsckOut, fsckErr)
	}
}

func TestDeleteRoute(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	limited := h.insertToken("limited", true, &repo.ID)
	admin := h.insertAdmin("admin")
	_ = limited
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("hello\n"))
	h.commitAll(dir, "one")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	h.must(dir, "checkout", "-b", "dev")
	writeFile(t, filepath.Join(dir, "dev.txt"), []byte("dev\n"))
	h.commitAll(dir, "two")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/dev")
	rows, err := h.st.ListTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var limitedID string
	for _, row := range rows {
		if row.Label == "limited" {
			limitedID = store.FormatID(row.ID)
		}
	}
	if limitedID == "" {
		t.Fatal("limited token")
	}
	before := keysUnder(t, h.bucket, repo.ID)
	status, body := httpDo(t, http.MethodDelete, h.srv.URL+"/admin/repos/site", admin)
	t.Logf("EVIDENCE delete %d %s", status, body)
	if status != http.StatusNoContent || body != "" {
		t.Fatalf("delete %d %s", status, body)
	}
	after := keysUnder(t, h.bucket, repo.ID)
	if len(after) != len(before)+1 {
		t.Fatalf("keys %d to %d", len(before), len(after))
	}
	for _, key := range before {
		if !containsKey(after, key) {
			t.Fatalf("missing %s", key)
		}
	}
	status, _ = httpDo(t, http.MethodGet, h.srv.URL+"/site.git/info/refs?service=git-upload-pack", h.token)
	t.Logf("EVIDENCE info/refs %d", status)
	if status != http.StatusNotFound {
		t.Fatalf("info/refs %d", status)
	}
	status, listed := httpDo(t, http.MethodGet, h.srv.URL+"/admin/tokens", admin)
	if status != http.StatusOK || strings.Contains(listed, limitedID) {
		t.Fatalf("tokens %d %s", status, listed)
	}
	fresh := h.openEmpty()
	text, failed := h.restoreInto(fresh, "")
	t.Logf("EVIDENCE restore\n%s", text)
	if failed != 0 || !strings.Contains(text, "skipped "+store.FormatID(repo.ID)+" deleted") {
		t.Fatalf("restore failed %d\n%s", failed, text)
	}
	again := h.createRepo("site")
	if again.ID == repo.ID {
		t.Fatal("uuid reused")
	}
	dir = h.initRepo("main")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("hello\n"))
	h.commitAll(dir, "again")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	clone, _, _ := h.clone(h.remote("site"))
	h.must(clone, "fsck", "--full")
}

func TestDeleteRefusedWhilePushCommits(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("hello\n"))
	h.commitAll(dir, "one")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := h.serveHook(func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := h.git(dir, "-c", bearer(h.token), "push", srv.URL+"/site.git", "HEAD:refs/heads/main")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("hook not entered")
	}
	seq, err := h.st.BumpPushSeq(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Write(context.Background(), h.bucket, h.kr, snapshot.Snapshot{
		RepoID: repo.ID, Name: repo.Name, PushSeq: seq, Deleted: true,
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err = <-done:
	case <-time.After(h.timeout):
		t.Fatal("push did not finish")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.DeleteRepo(context.Background(), repo.ID, seq); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale %v", err)
	}
	if _, err := h.st.RepoByName(context.Background(), "site"); err != nil {
		t.Fatal(err)
	}
	admin := h.insertAdmin("admin")
	status, body := httpDo(t, http.MethodDelete, h.srv.URL+"/admin/repos/site", admin)
	if status != http.StatusNoContent || body != "" {
		t.Fatalf("route %d %s", status, body)
	}
	text, failed := h.restoreInto(h.openEmpty(), "")
	t.Logf("EVIDENCE stale then route\n%s", text)
	if failed != 0 || !strings.Contains(text, "skipped "+store.FormatID(repo.ID)+" deleted") {
		t.Fatalf("restore failed %d\n%s", failed, text)
	}
}

func TestGCRustfs(t *testing.T) {
	if os.Getenv("LKV_TEST_DATABASE_URL") == "" {
		t.Skip("LKV_TEST_DATABASE_URL is not set")
	}
	if os.Getenv("LKV_TEST_S3_ENDPOINT") == "" {
		t.Skip("LKV_TEST_S3_ENDPOINT is not set")
	}
	s3 := s3Bucket(t)
	h := newHarness(t, deny(), s3)
	h.timeout = 3 * time.Minute
	rawID, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "gc" + hex.EncodeToString(rawID[:4])
	repo := h.createRepo(name)
	t.Cleanup(func() { deleteRepoObjects(t, s3, repo.ID) })
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("hello\n"))
	h.commitAll(dir, "one")
	h.push(dir, h.remote(name), "HEAD:refs/heads/main")
	admin := h.insertAdmin("admin")
	status, body := httpDo(t, http.MethodDelete, h.srv.URL+"/admin/repos/"+name, admin)
	if status != http.StatusNoContent || body != "" {
		t.Fatalf("delete %d %s", status, body)
	}
	before := keysUnder(t, s3, repo.ID)
	_, inside := runGC(t, h, time.Now)
	if !sameKeys(before, keysUnder(t, s3, repo.ID)) {
		t.Fatalf("inside\n%s", inside)
	}
	marker := markerKey(t, s3, h.kr, repo.ID)
	_, past := runGC(t, h, func() time.Time { return time.Now().Add(25 * time.Hour) })
	t.Logf("EVIDENCE rustfs gc\n%s", past)
	idText := store.FormatID(repo.ID)
	var own []string
	for _, line := range strings.Split(past, "\n") {
		rest, ok := strings.CutPrefix(line, "deleted ")
		if ok && strings.Contains(rest, idText) {
			own = append(own, rest)
		}
		if strings.HasPrefix(line, "failed ") && strings.Contains(line, idText) {
			t.Fatalf("failed %s", line)
		}
	}
	if len(own) != len(before) || own[len(own)-1] != marker {
		t.Fatalf("past marker %s deleted %v\n%s", marker, own, past)
	}
	if len(keysUnder(t, s3, repo.ID)) != 0 {
		t.Fatal("keys remain")
	}
}

func runGC(t *testing.T, h *harness, now func() time.Time) (gc.Result, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := gc.Run(context.Background(), h.st, h.bucket, h.kr, gc.Options{Now: now, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	assertGCText(t, text)
	return res, text
}

// A snapshot key is repos/<uuid>/refs/<uuid>.snap. That path is a bucket key.
// assertClean treats every "refs/" as a ref name, so it cannot judge gc output.
var snapshotKeyRE = regexp.MustCompile(`repos/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/refs/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.snap`)

func assertGCText(t *testing.T, text string) {
	t.Helper()
	cleaned := snapshotKeyRE.ReplaceAllString(text, "")
	if strings.Contains(cleaned, "refs/") || strings.Contains(text, "HEAD") {
		t.Fatalf("gc contains a ref name")
	}
	if objectIDRE.MatchString(text) {
		t.Fatalf("gc contains an object id")
	}
}

func httpDo(t *testing.T, method, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func keysUnder(t *testing.T, bucket blob.Store, repoID [16]byte) []string {
	t.Helper()
	var keys []string
	err := bucket.List(context.Background(), "repos/"+store.FormatID(repoID)+"/", func(info blob.Info) error {
		keys = append(keys, info.Key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func markerKey(t *testing.T, bucket blob.Store, kr *objcrypt.Keyring, repoID [16]byte) string {
	t.Helper()
	var key string
	var seq int64
	err := bucket.List(context.Background(), "repos/"+store.FormatID(repoID)+"/refs/", func(info blob.Info) error {
		base := strings.TrimSuffix(info.Key, ".snap")
		i := strings.LastIndex(base, "/")
		id, err := store.ParseID(base[i+1:])
		if err != nil {
			return err
		}
		got, err := snapshot.Read(context.Background(), bucket, kr, repoID, id, info.Size)
		if err != nil {
			return err
		}
		if got.Deleted && (key == "" || got.PushSeq >= seq) {
			seq = got.PushSeq
			key = info.Key
		}
		return nil
	})
	if err != nil || key == "" {
		t.Fatalf("marker %v", err)
	}
	return key
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

func lastPrefixed(text, prefix string) string {
	var last string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			last = strings.TrimPrefix(line, prefix)
		}
	}
	return last
}
