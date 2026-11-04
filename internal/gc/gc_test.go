package gc

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/httpapi"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

func TestLiveOrphan(t *testing.T) {
	st, mem, kr := openGC(t)
	packPlain, idxPlain, commit := gitCommitPack(t)
	ctx := context.Background()
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	packID := mustID(t)
	orphan := mustID(t)
	packBytes := putEncrypted(t, mem, kr, repo.ID, packID, objcrypt.KindPack, packstore.PackKey(repo.ID, packID), packPlain)
	idxBytes := putEncrypted(t, mem, kr, repo.ID, packID, objcrypt.KindIdx, packstore.IdxKey(repo.ID, packID), idxPlain)
	putEncrypted(t, mem, kr, repo.ID, orphan, objcrypt.KindPack, packstore.PackKey(repo.ID, orphan), packPlain)
	putEncrypted(t, mem, kr, repo.ID, orphan, objcrypt.KindIdx, packstore.IdxKey(repo.ID, orphan), idxPlain)
	committed, err := st.CommitPush(ctx, store.Push{
		RepoID: repo.ID,
		Pack: &store.NewPack{
			ID: packID, PackKey: packstore.PackKey(repo.ID, packID), IdxKey: packstore.IdxKey(repo.ID, packID),
			Bytes: packBytes, IdxBytes: idxBytes,
		},
		Updates: []store.RefUpdate{{Name: "refs/heads/main", New: &commit}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Write(ctx, mem, kr, snapshot.Snapshot{
		RepoID: repo.ID, Name: "site", PushSeq: committed.PushSeq, Refs: committed.Refs,
	}); err != nil {
		t.Fatal(err)
	}
	prefix := "repos/" + store.FormatID(repo.ID) + "/"
	before := listKeys(t, mem, prefix)
	res, text := runAt(t, st, mem, kr, time.Now, false)
	if res.Deleted != 0 || res.Refused != 0 || res.Failures != 0 || text == "" {
		t.Fatalf("inside %+v\n%s", res, text)
	}
	if !sameKeys(before, listKeys(t, mem, prefix)) {
		t.Fatal("inside grace deleted a key")
	}
	res, text = runAt(t, st, mem, kr, past, false)
	t.Logf("EVIDENCE live orphan\n%s", text)
	if res.Deleted != 2 || res.Failures != 0 {
		t.Fatalf("past %+v\n%s", res, text)
	}
	left := listKeys(t, mem, prefix)
	for _, key := range []string{packstore.PackKey(repo.ID, orphan), packstore.IdxKey(repo.ID, orphan)} {
		if strings.Contains(strings.Join(left, "\n"), key) {
			t.Fatalf("orphan remains %s", key)
		}
		if !strings.Contains(text, "deleted "+key) {
			t.Fatalf("missing deleted line %s", key)
		}
	}
	if !strings.Contains(strings.Join(left, "\n"), packstore.PackKey(repo.ID, packID)) {
		t.Fatal("committed pack deleted")
	}
	snaps := listKeys(t, mem, "repos/"+store.FormatID(repo.ID)+"/refs/")
	if len(snaps) != 1 {
		t.Fatalf("snapshots %d", len(snaps))
	}
	if strings.Contains(text, "deleted "+snaps[0]) {
		t.Fatal("snapshot deleted")
	}
}

func TestUnexpectedKeys(t *testing.T) {
	st, mem, kr := openGC(t)
	repo := mustID(t)
	obj := mustID(t)
	stray := "repos/not-a-uuid/x"
	tmp := "repos/" + store.FormatID(repo) + "/packs/" + store.FormatID(obj) + ".tmp"
	mem.SetRaw(stray, []byte("x"))
	mem.SetRaw(tmp, []byte("y"))
	before := listKeys(t, mem, "")
	res, text := runAt(t, st, mem, kr, past, false)
	if res.Deleted != 0 || res.Refused != 0 || !strings.Contains(text, "unexpected keys=2") {
		t.Fatalf("%+v\n%s", res, text)
	}
	if !sameKeys(before, listKeys(t, mem, "")) {
		t.Fatal("unexpected key deleted")
	}
}

func TestLostDatabase(t *testing.T) {
	_, mem, kr := openGC(t)
	empty := openStore(t)
	ctx := context.Background()
	repo := mustID(t)
	if _, err := snapshot.Write(ctx, mem, kr, snapshot.Snapshot{
		RepoID: repo, Name: "site", PushSeq: 1,
		Refs: []store.Ref{{Name: "HEAD", Symbolic: "refs/heads/main"}},
	}); err != nil {
		t.Fatal(err)
	}
	before := listKeys(t, mem, "")
	res, text := runAt(t, empty, mem, kr, past, false)
	t.Logf("EVIDENCE lost database\n%s", text)
	want := "refused " + store.FormatID(repo) + " site has snapshots and no database row; run littlekittyvcs restore"
	if !strings.Contains(text, want) || res.Refused != 1 || res.Deleted != 0 {
		t.Fatalf("%+v\n%s", res, text)
	}
	after := listKeys(t, mem, "")
	if !sameKeys(before, after) {
		t.Fatalf("keys %d to %d", len(before), len(after))
	}
	t.Logf("EVIDENCE keys before=%d after=%d", len(before), len(after))
}

func TestUnreadableSnapshots(t *testing.T) {
	_, mem, kr := openGC(t)
	empty := openStore(t)
	repo := mustID(t)
	if _, err := snapshot.Write(context.Background(), mem, kr, snapshot.Snapshot{
		RepoID: repo, Name: "site", PushSeq: 1,
		Refs: []store.Ref{{Name: "HEAD", Symbolic: "refs/heads/main"}},
	}); err != nil {
		t.Fatal(err)
	}
	var master [32]byte
	copy(master[:], "other-littlekittyvcs-master-key!")
	other, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	before := listKeys(t, mem, "")
	res, text := runAt(t, empty, mem, other, past, false)
	want := "refused " + store.FormatID(repo) + " snapshots cannot be read; check MASTER_KEY"
	if !strings.Contains(text, want) || res.Refused != 1 || res.Deleted != 0 {
		t.Fatalf("%+v\n%s", res, text)
	}
	if !sameKeys(before, listKeys(t, mem, "")) {
		t.Fatal("unreadable snapshot deleted")
	}
}

func TestDeletedRepository(t *testing.T) {
	st, mem, kr, repo := deletedRepo(t)
	prefix := "repos/" + store.FormatID(repo) + "/"
	before := listKeys(t, mem, prefix)
	res, text := runAt(t, st, mem, kr, time.Now, false)
	if res.Deleted != 0 || !sameKeys(before, listKeys(t, mem, prefix)) {
		t.Fatalf("inside %+v\n%s", res, text)
	}
	marker := markerOf(t, mem, kr, repo)
	res, text = runAt(t, st, mem, kr, past, false)
	t.Logf("EVIDENCE deleted repository\n%s", text)
	if res.Failures != 0 || res.Refused != 0 || res.Deleted != len(before) {
		t.Fatalf("past %+v keys %d\n%s", res, len(before), text)
	}
	if lastDeleted(text) != marker {
		t.Fatalf("marker last got %s want %s\n%s", lastDeleted(text), marker, text)
	}
	if len(listKeys(t, mem, prefix)) != 0 {
		t.Fatal("keys remain")
	}
}

func TestMarkerStaysWhenDeleteFails(t *testing.T) {
	st, mem, kr, repo := deletedRepo(t)
	marker := markerOf(t, mem, kr, repo)
	var packKey string
	for _, key := range listKeys(t, mem, "repos/"+store.FormatID(repo)+"/") {
		if strings.HasSuffix(key, ".pack") {
			packKey = key
			break
		}
	}
	if packKey == "" {
		t.Fatal("no pack")
	}
	wrapped := failKey{Store: mem, key: packKey}
	res, text := runAt(t, st, wrapped, kr, past, false)
	if res.Failures != 1 || !strings.Contains(text, "failed "+packKey) || strings.Contains(text, "deleted "+marker) {
		t.Fatalf("%+v\n%s", res, text)
	}
	if !containsKey(listKeys(t, mem, ""), marker) {
		t.Fatal("marker was deleted")
	}
	res, text = runAt(t, st, mem, kr, past, false)
	t.Logf("EVIDENCE second run\n%s", text)
	if res.Failures != 0 || lastDeleted(text) != marker {
		t.Fatalf("%+v\n%s", res, text)
	}
	if len(listKeys(t, mem, "repos/"+store.FormatID(repo)+"/")) != 0 {
		t.Fatal("keys remain")
	}
}

func TestDryRun(t *testing.T) {
	st, mem, kr, repo := deletedRepo(t)
	marker := markerOf(t, mem, kr, repo)
	before := listKeys(t, mem, "repos/"+store.FormatID(repo)+"/")
	res, text := runAt(t, st, mem, kr, past, true)
	if res.Deleted != len(before) || res.Failures != 0 || lastWould(text) != marker {
		t.Fatalf("%+v\n%s", res, text)
	}
	for _, key := range before {
		if !strings.Contains(text, "would delete "+key) {
			t.Fatalf("missing would delete %s", key)
		}
	}
	if !sameKeys(before, listKeys(t, mem, "repos/"+store.FormatID(repo)+"/")) {
		t.Fatal("dry run deleted a key")
	}
}

func TestAutoCreateLoser(t *testing.T) {
	st, mem, kr := openGC(t)
	packPlain, idxPlain, _ := gitCommitPack(t)
	repo := mustID(t)
	packID := mustID(t)
	putEncrypted(t, mem, kr, repo, packID, objcrypt.KindPack, packstore.PackKey(repo, packID), packPlain)
	putEncrypted(t, mem, kr, repo, packID, objcrypt.KindIdx, packstore.IdxKey(repo, packID), idxPlain)
	prefix := "repos/" + store.FormatID(repo) + "/"
	before := listKeys(t, mem, prefix)
	res, _ := runAt(t, st, mem, kr, time.Now, false)
	if res.Deleted != 0 || !sameKeys(before, listKeys(t, mem, prefix)) {
		t.Fatalf("inside %+v", res)
	}
	res, text := runAt(t, st, mem, kr, past, false)
	if res.Deleted != 2 || len(listKeys(t, mem, prefix)) != 0 {
		t.Fatalf("past %+v\n%s", res, text)
	}
}

func deletedRepo(t *testing.T) (*store.Store, *blob.Mem, *objcrypt.Keyring, [16]byte) {
	t.Helper()
	st, mem, kr := openGC(t)
	packPlain, idxPlain, _ := gitCommitPack(t)
	ctx := context.Background()
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	packID := mustID(t)
	putEncrypted(t, mem, kr, repo.ID, packID, objcrypt.KindPack, packstore.PackKey(repo.ID, packID), packPlain)
	putEncrypted(t, mem, kr, repo.ID, packID, objcrypt.KindIdx, packstore.IdxKey(repo.ID, packID), idxPlain)
	seq, err := st.BumpPushSeq(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Write(ctx, mem, kr, snapshot.Snapshot{
		RepoID: repo.ID, Name: "site", PushSeq: seq,
		Refs: []store.Ref{{Name: "HEAD", Symbolic: "refs/heads/main"}},
	}); err != nil {
		t.Fatal(err)
	}
	token := mintAdmin(t, st)
	srv := httptest.NewServer(httpapi.New(slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.Deps{
		Store: st, Bucket: mem, Keyring: kr, Index: objindex.New(),
	}, httpapi.Config{}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/admin/repos/site", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("delete %d %s", resp.StatusCode, body)
	}
	return st, mem, kr, repo.ID
}

type failKey struct {
	blob.Store
	key string
}

func (f failKey) Delete(ctx context.Context, key string) error {
	if key == f.key {
		return errDelete
	}
	return f.Store.Delete(ctx, key)
}

var errDelete = errString("blob: delete failed")

type errString string

func (e errString) Error() string { return string(e) }

func mintAdmin(t *testing.T, st *store.Store) string {
	t.Helper()
	plain, hash, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	err = st.InsertToken(context.Background(), hash, store.TokenRow{
		ID: id, Label: "admin", Write: true, Admin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func markerOf(t *testing.T, mem *blob.Mem, kr *objcrypt.Keyring, repoID [16]byte) string {
	t.Helper()
	var key string
	var seq int64
	err := mem.List(context.Background(), "repos/"+store.FormatID(repoID)+"/refs/", func(info blob.Info) error {
		got, err := snapshot.Read(context.Background(), mem, kr, repoID, idFromKey(t, info.Key), info.Size)
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

func runAt(t *testing.T, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring, now func() time.Time, dry bool) (Result, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := Run(context.Background(), st, bucket, kr, Options{Now: now, DryRun: dry, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
}

func past() time.Time { return time.Now().Add(25 * time.Hour) }

func lastDeleted(text string) string { return lastPrefixed(text, "deleted ") }

func lastWould(text string) string { return lastPrefixed(text, "would delete ") }

func lastPrefixed(text, prefix string) string {
	var last string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			last = strings.TrimPrefix(line, prefix)
		}
	}
	return last
}

func listKeys(t *testing.T, bucket blob.Store, prefix string) []string {
	t.Helper()
	var keys []string
	err := bucket.List(context.Background(), prefix, func(info blob.Info) error {
		keys = append(keys, info.Key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
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

func openGC(t *testing.T) (*store.Store, *blob.Mem, *objcrypt.Keyring) {
	t.Helper()
	return openStore(t), blob.NewMem(), testKeyring(t)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	raw := testdb.URL(t)
	st, err := store.Open(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return st
}

func putEncrypted(t *testing.T, mem *blob.Mem, kr *objcrypt.Keyring, repoID, objID [16]byte, kind objcrypt.Kind, key string, plain []byte) int64 {
	t.Helper()
	var buf bytes.Buffer
	w, err := objcrypt.NewWriter(&buf, kr, objcrypt.Binding{
		RepoID: repoID, ObjectID: objID, Kind: kind,
	}, objcrypt.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), key, bytes.NewReader(buf.Bytes()), w.CiphertextSize()); err != nil {
		t.Fatal(err)
	}
	return w.CiphertextSize()
}

func gitCommitPack(t *testing.T) (pack, idx []byte, commit [20]byte) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, nil, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, nil, "add", "f")
	git(t, dir, nil, "-c", "user.name=Ada Lovelace", "-c", "user.email=ada@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "m")
	rev := strings.TrimSpace(git(t, dir, nil, "rev-parse", "HEAD"))
	raw, err := hex.DecodeString(rev)
	if err != nil || len(raw) != 20 {
		t.Fatal("commit id")
	}
	copy(commit[:], raw)
	base := filepath.Join(t.TempDir(), "pack")
	git(t, dir, []byte(rev+"\n"), "pack-objects", base)
	packs, err := filepath.Glob(base + "-*.pack")
	if err != nil || len(packs) != 1 {
		t.Fatalf("packs %d %v", len(packs), err)
	}
	idxs, err := filepath.Glob(base + "-*.idx")
	if err != nil || len(idxs) != 1 {
		t.Fatalf("idxs %d %v", len(idxs), err)
	}
	pack, err = os.ReadFile(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	idx, err = os.ReadFile(idxs[0])
	if err != nil {
		t.Fatal(err)
	}
	return pack, idx, commit
}

func git(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Ada Lovelace",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada Lovelace",
		"GIT_COMMITTER_EMAIL=ada@example.com",
		"PATH=" + os.Getenv("PATH"),
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func idFromKey(t *testing.T, key string) [16]byte {
	t.Helper()
	base := strings.TrimSuffix(key, ".snap")
	i := strings.LastIndex(base, "/")
	id, err := store.ParseID(base[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustID(t *testing.T) [16]byte {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testKeyring(t *testing.T) *objcrypt.Keyring {
	t.Helper()
	var master [32]byte
	copy(master[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}
