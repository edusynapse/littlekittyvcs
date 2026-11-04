package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/httpapi"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

var objectIDRE = regexp.MustCompile(`[0-9a-f]{40}`)

func TestRestoreTwoBranches(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("hello\n"))
	mainID := h.commitAll(dir, "main")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	h.must(dir, "checkout", "-b", "dev")
	writeFile(t, filepath.Join(dir, "dev.txt"), []byte("dev\n"))
	devID := h.commitAll(dir, "dev")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/dev")

	snaps := h.readSnaps(repo.ID)
	if len(snaps) != 2 {
		t.Fatalf("snaps %d", len(snaps))
	}
	var second *snapshot.Snapshot
	for i := range snaps {
		if snaps[i].PushSeq == 2 {
			second = &snaps[i]
		}
	}
	if second == nil || !snapNames(second, "HEAD", "refs/heads/main", "refs/heads/dev") {
		t.Fatal("second snapshot refs")
	}

	st := h.openEmpty()
	out, failed := h.restoreInto(st, "")
	if failed != 0 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	line := lineContaining(out, "restored site ")
	if !strings.Contains(line, "push_seq=2 refs=3 packs=2") {
		t.Fatalf("line %q", line)
	}
	t.Logf("EVIDENCE restore line=%s", line)

	srv, token := h.serveStore(st)
	clone := h.cloneToken(srv.URL+"/site.git", token)
	branches, _ := h.must(clone, "branch", "-r")
	t.Logf("EVIDENCE git branch -r:\n%s", branches)
	if !strings.Contains(branches, "origin/main") || !strings.Contains(branches, "origin/dev") {
		t.Fatal("clone branches")
	}
	if h.rev(clone, "origin/main") != mainID || h.rev(clone, "origin/dev") != devID {
		t.Fatal("clone tips")
	}
	body, err := os.ReadFile(filepath.Join(clone, "readme.txt"))
	if err != nil || string(body) != "hello\n" {
		t.Fatal("checkout")
	}
	fsckOut, fsckErr := h.must(clone, "fsck", "--full")
	if strings.Contains(fsckOut+fsckErr, "error") || strings.Contains(fsckOut+fsckErr, "missing") || strings.Contains(fsckOut+fsckErr, "broken") {
		t.Fatalf("fsck stdout %q stderr %q", fsckOut, fsckErr)
	}
	t.Logf("EVIDENCE fsck stdout=%q stderr=%q", fsckOut, fsckErr)

	h.must(dir, "checkout", "main")
	writeFile(t, filepath.Join(dir, "third.txt"), []byte("three\n"))
	h.commitAll(dir, "third")
	h.must(dir, "-c", bearer(token), "push", srv.URL+"/site.git", "HEAD:refs/heads/main")
	if !hasPushSeq(h.readSnaps(repo.ID), 3) {
		t.Fatal("third snapshot push_seq")
	}
	t.Logf("EVIDENCE third snapshot push_seq=3")
}

func TestRestoreMasterBranch(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("site")
	dir := h.initRepo("master")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("master\n"))
	h.commitAll(dir, "master")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/master")

	st := h.openEmpty()
	out, failed := h.restoreInto(st, "")
	if failed != 0 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	_, refs, _, err := st.ReadRepo(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	if symbolicOf(refs, "HEAD") != "refs/heads/master" {
		t.Fatal("HEAD target")
	}
	srv, token := h.serveStore(st)
	clone := h.cloneToken(srv.URL+"/site.git", token)
	body, err := os.ReadFile(filepath.Join(clone, "readme.txt"))
	if err != nil || string(body) != "master\n" {
		t.Fatal("checkout")
	}
}

func TestRestoreSnapshotWriteFailure(t *testing.T) {
	mem := blob.NewMem()
	h := newHarness(t, deny(), &failSecondSnap{Store: mem})
	h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("hello\n"))
	h.commitAll(dir, "main")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	h.must(dir, "checkout", "-b", "dev")
	writeFile(t, filepath.Join(dir, "dev.txt"), []byte("dev\n"))
	h.commitAll(dir, "dev")
	_, stderr := h.push(dir, h.remote("site"), "HEAD:refs/heads/dev")
	if !strings.Contains(stderr, "[new branch]") {
		t.Fatalf("stderr %s", redact(stderr))
	}
	t.Logf("EVIDENCE push stderr=%q", redact(stderr))
	var snapLines int
	var snapLine string
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, `"msg":"ref snapshot"`) {
			snapLines++
			snapLine = line
		}
	}
	if snapLines != 1 {
		t.Fatalf("ref snapshot lines %d", snapLines)
	}
	t.Logf("EVIDENCE ref snapshot line=%s", snapLine)
	assertClean(t, "log", h.logs.String())

	st := h.openEmpty()
	out, failed := h.restoreInto(st, "")
	if failed != 0 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	line := lineContaining(out, "restored site ")
	if !strings.Contains(line, "push_seq=1") || !strings.Contains(line, "refs=2") {
		t.Fatalf("line %q", line)
	}
	_, refs, _, err := st.ReadRepo(context.Background(), "site")
	if err != nil || !snapNames(&snapshot.Snapshot{Refs: refs}, "HEAD", "refs/heads/main") || len(refs) != 2 {
		t.Fatal("restored refs")
	}
	srv, token := h.serveStore(st)
	clone := h.cloneToken(srv.URL+"/site.git", token)
	if _, _, err := h.git(clone, "rev-parse", "--verify", "origin/dev"); err == nil {
		t.Fatal("dev restored")
	}
	if _, err := os.ReadFile(filepath.Join(clone, "readme.txt")); err != nil {
		t.Fatal("main file")
	}
}

func TestRestoreFallsBack(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	var tips []string
	for i := 1; i <= 3; i++ {
		writeFile(t, filepath.Join(dir, "note.txt"), []byte(fmt.Sprintf("v%d\n", i)))
		tips = append(tips, h.commitAll(dir, fmt.Sprintf("c%d", i)))
		h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	}
	packs := h.packs("site")
	if len(packs) != 3 {
		t.Fatalf("packs %d", len(packs))
	}
	ctx := context.Background()
	if err := h.mem.Delete(ctx, packs[2].PackKey); err != nil {
		t.Fatal(err)
	}
	if err := h.mem.Delete(ctx, packs[2].IdxKey); err != nil {
		t.Fatal(err)
	}

	st := h.openEmpty()
	out, failed := h.restoreInto(st, "")
	if failed != 0 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	line := lineContaining(out, "restored site ")
	t.Logf("EVIDENCE restore line=%s", line)
	if !strings.Contains(line, "push_seq=3") || !strings.Contains(line, "used_older_snapshot=true") {
		t.Fatalf("line %q", line)
	}
	if strings.Contains(line, "incomplete=") || strings.Contains(line, "bad=") {
		t.Fatalf("line %q", line)
	}
	srv, token := h.serveStore(st)
	clone := h.cloneToken(srv.URL+"/site.git", token)
	if h.rev(clone, "HEAD") != tips[1] {
		t.Fatal("main tip")
	}
	writeFile(t, filepath.Join(clone, "note.txt"), []byte("v4\n"))
	h.commitAll(clone, "c4")
	h.must(clone, "-c", bearer(token), "push", srv.URL+"/site.git", "HEAD:refs/heads/main")
	var seq int64
	for _, s := range h.readSnaps(repo.ID) {
		if s.PushSeq > seq {
			seq = s.PushSeq
		}
	}
	if seq != 4 {
		t.Fatalf("next push_seq %d", seq)
	}
	t.Logf("EVIDENCE next snapshot push_seq=%d", seq)
}

func TestRestoreTamperedSnapshot(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("one\n"))
	h.commitAll(dir, "one")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("two\n"))
	h.commitAll(dir, "two")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")

	var newest blob.Info
	var newestSeq int64
	prefix := "repos/" + store.FormatID(repo.ID) + "/refs/"
	err := h.mem.List(context.Background(), prefix, func(info blob.Info) error {
		if !strings.HasSuffix(info.Key, ".snap") {
			return nil
		}
		got, err := snapshot.Read(context.Background(), h.mem, h.kr, repo.ID, idFromSnapKey(t, info.Key), info.Size)
		if err != nil {
			t.Fatal(err)
		}
		if got.PushSeq >= newestSeq {
			newestSeq = got.PushSeq
			newest = info
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := h.mem.Raw(newest.Key)
	raw[len(raw)-1] ^= 0xff
	h.mem.SetRaw(newest.Key, raw)

	st := h.openEmpty()
	out, failed := h.restoreInto(st, "")
	if failed != 0 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	line := lineContaining(out, "restored site ")
	t.Logf("EVIDENCE restore line=%s", line)
	if !strings.Contains(line, "unreadable=1") || !strings.Contains(line, "push_seq=1") {
		t.Fatalf("line %q", line)
	}
	if strings.Contains(line, "used_older_snapshot=true") {
		t.Fatalf("line %q", line)
	}
}

func TestRestoreTwice(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("one\n"))
	h.commitAll(dir, "one")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")

	st := h.openEmpty()
	if _, failed := h.restoreInto(st, ""); failed != 0 {
		t.Fatalf("failed %d", failed)
	}
	beforeRepo, beforeRefs, beforePacks, err := st.ReadRepo(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	out, failed := h.restoreInto(st, "")
	t.Logf("EVIDENCE second restore:\n%s", out)
	if failed != 0 {
		t.Fatalf("failed %d", failed)
	}
	want := "skipped " + store.FormatID(repo.ID) + " already in database\n"
	if out != want {
		t.Fatalf("output %q", out)
	}
	afterRepo, afterRefs, afterPacks, err := st.ReadRepo(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	if beforeRepo != afterRepo || !refsEqual(beforeRefs, afterRefs) || !packsEqual(beforePacks, afterPacks) {
		t.Fatal("rows changed")
	}
}

func TestRestoreOnlyName(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("alpha")
	h.createRepo("beta")
	for _, name := range []string{"alpha", "beta"} {
		dir := h.initRepo("main")
		writeFile(t, filepath.Join(dir, "note.txt"), []byte(name+"\n"))
		h.commitAll(dir, name)
		h.push(dir, h.remote(name), "HEAD:refs/heads/main")
	}
	st := h.openEmpty()
	out, failed := h.restoreInto(st, "alpha")
	if failed != 0 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	if _, err := st.RepoByName(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepoByName(context.Background(), "beta"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("beta: %v", err)
	}
	if strings.Contains(out, "beta") {
		t.Fatalf("output names the other repository:\n%s", out)
	}

	empty := h.openEmpty()
	out, failed = h.restoreInto(empty, "missing")
	if failed != 1 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	if out != "failed no repository named missing in the bucket\n" {
		t.Fatalf("output %q", out)
	}
}

func TestRestoreNameTaken(t *testing.T) {
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("one\n"))
	h.commitAll(dir, "one")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")

	st := h.openEmpty()
	other, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRepo(context.Background(), other, "site"); err != nil {
		t.Fatal(err)
	}
	out, failed := h.restoreInto(st, "")
	if failed != 1 {
		t.Fatalf("failed %d\n%s", failed, out)
	}
	want := "failed " + store.FormatID(repo.ID) + " name or id already taken\n"
	if out != want {
		t.Fatalf("output %q", out)
	}
	if _, err := st.RepoByID(context.Background(), repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bucket id inserted: %v", err)
	}
	got, err := st.RepoByName(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != other {
		t.Fatal("pre-created row replaced")
	}
}

func TestRestoreRustfs(t *testing.T) {
	if os.Getenv("LKV_TEST_DATABASE_URL") == "" {
		t.Skip("LKV_TEST_DATABASE_URL is not set")
	}
	if os.Getenv("LKV_TEST_S3_ENDPOINT") == "" {
		t.Skip("LKV_TEST_S3_ENDPOINT is not set")
	}
	s3 := s3Bucket(t)
	h := newHarness(t, deny(), s3)
	h.timeout = 3 * time.Minute
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	name := "restore-" + hex.EncodeToString(buf[:])
	repo := h.createRepo(name)
	t.Cleanup(func() { deleteRepoObjects(t, s3, repo.ID) })
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "readme.txt"), []byte("hello\n"))
	mainID := h.commitAll(dir, "main")
	h.push(dir, h.remote(name), "HEAD:refs/heads/main")
	h.must(dir, "checkout", "-b", "dev")
	writeFile(t, filepath.Join(dir, "dev.txt"), []byte("dev\n"))
	devID := h.commitAll(dir, "dev")
	h.push(dir, h.remote(name), "HEAD:refs/heads/dev")

	st := h.openEmpty()
	out, _ := h.restoreInto(st, name)
	line := lineContaining(out, "restored "+name+" ")
	t.Logf("EVIDENCE rustfs restore line=%s", line)
	if !strings.Contains(line, "push_seq=2 refs=3 packs=2") {
		t.Fatalf("line %q", line)
	}
	srv, token := h.serveStore(st)
	clone := h.cloneToken(srv.URL+"/"+name+".git", token)
	if h.rev(clone, "origin/main") != mainID || h.rev(clone, "origin/dev") != devID {
		t.Fatal("clone tips")
	}
	body, err := os.ReadFile(filepath.Join(clone, "readme.txt"))
	if err != nil || string(body) != "hello\n" {
		t.Fatal("checkout")
	}
}

type failSecondSnap struct {
	blob.Store
	mu sync.Mutex
	n  int
}

func (f *failSecondSnap) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if strings.Contains(key, "/refs/") {
		f.mu.Lock()
		f.n++
		n := f.n
		f.mu.Unlock()
		if n == 2 {
			return errors.New("bucket: snapshot put failed")
		}
	}
	return f.Store.Put(ctx, key, r, size)
}

func (h *harness) openEmpty() *store.Store {
	h.t.Helper()
	raw := testdb.URL(h.t)
	st, err := store.Open(context.Background(), raw)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(st.Close)
	if err := st.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) serveStore(st *store.Store) (*httptest.Server, string) {
	h.t.Helper()
	plain, hash, err := auth.Generate()
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := store.NewID()
	if err != nil {
		h.t.Fatal(err)
	}
	err = st.InsertToken(context.Background(), hash, store.TokenRow{
		ID: id, Label: "rw", Write: true, Admin: false,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(h.log, httpapi.Deps{
		Store: st, Bucket: h.bucket, Keyring: h.kr, Index: objindex.New(),
	}, deny()))
	h.t.Cleanup(srv.Close)
	return srv, plain
}

func (h *harness) restoreInto(st *store.Store, only string) (string, int) {
	h.t.Helper()
	var out bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(h.log)
	defer slog.SetDefault(prev)
	failed, err := snapshot.Restore(context.Background(), st, h.bucket, h.kr, snapshot.Options{
		OnlyName: only,
		Out:      &out,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	text := out.String()
	assertClean(h.t, "restore", text)
	assertClean(h.t, "log", h.logs.String())
	return text, failed
}

func (h *harness) cloneToken(remote, token string) string {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), "clone")
	h.must("", "-c", bearer(token), "clone", remote, dir)
	return dir
}

func (h *harness) readSnaps(repoID [16]byte) []snapshot.Snapshot {
	h.t.Helper()
	prefix := "repos/" + store.FormatID(repoID) + "/refs/"
	var out []snapshot.Snapshot
	err := h.bucket.List(context.Background(), prefix, func(info blob.Info) error {
		if !strings.HasSuffix(info.Key, ".snap") {
			return nil
		}
		got, err := snapshot.Read(context.Background(), h.bucket, h.kr, repoID, idFromSnapKey(h.t, info.Key), info.Size)
		if err != nil {
			return err
		}
		out = append(out, got)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func idFromSnapKey(t *testing.T, key string) [16]byte {
	t.Helper()
	base := strings.TrimSuffix(key, ".snap")
	i := strings.LastIndex(base, "/")
	id, err := store.ParseID(base[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func snapNames(s *snapshot.Snapshot, names ...string) bool {
	if len(s.Refs) != len(names) {
		return false
	}
	got := map[string]bool{}
	for _, ref := range s.Refs {
		got[ref.Name] = true
	}
	for _, name := range names {
		if !got[name] {
			return false
		}
	}
	return true
}

func hasPushSeq(snaps []snapshot.Snapshot, seq int64) bool {
	for _, s := range snaps {
		if s.PushSeq == seq {
			return true
		}
	}
	return false
}

func symbolicOf(refs []store.Ref, name string) string {
	for _, ref := range refs {
		if ref.Name == name {
			return ref.Symbolic
		}
	}
	return ""
}

func lineContaining(out, substr string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

func refsEqual(a, b []store.Ref) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Symbolic != b[i].Symbolic {
			return false
		}
		if (a[i].ObjectID == nil) != (b[i].ObjectID == nil) {
			return false
		}
		if a[i].ObjectID != nil && *a[i].ObjectID != *b[i].ObjectID {
			return false
		}
	}
	return true
}

func packsEqual(a, b []store.Pack) bool {
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

func assertClean(t *testing.T, label, text string) {
	t.Helper()
	if strings.Contains(text, "refs/") || strings.Contains(text, "HEAD") {
		t.Fatalf("%s contains a ref name", label)
	}
	if objectIDRE.MatchString(text) {
		t.Fatalf("%s contains an object id", label)
	}
}

func deleteRepoObjects(t *testing.T, bucket blob.Store, repoID [16]byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prefix := "repos/" + store.FormatID(repoID) + "/"
	var keys []string
	if err := bucket.List(ctx, prefix, func(info blob.Info) error {
		keys = append(keys, info.Key)
		return nil
	}); err != nil {
		t.Error(err)
		return
	}
	for _, key := range keys {
		if err := bucket.Delete(ctx, key); err != nil {
			t.Error(err)
		}
	}
}
