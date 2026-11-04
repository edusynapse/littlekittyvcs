package snapshot

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

func TestRestoreLogic(t *testing.T) {
	st := openStore(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	packPlain, idxPlain, commit := gitCommitPack(t)
	kr := testKeyring(t)
	mem := blob.NewMem()
	ctx := context.Background()

	good := mustID(t)
	complete := mustID(t)
	orphan := mustID(t)
	putEncrypted(t, mem, kr, good, complete, objcrypt.KindPack, packstore.PackKey(good, complete), packPlain)
	putEncrypted(t, mem, kr, good, complete, objcrypt.KindIdx, packstore.IdxKey(good, complete), idxPlain)
	putEncrypted(t, mem, kr, good, orphan, objcrypt.KindPack, packstore.PackKey(good, orphan), packPlain)
	mem.SetRaw("repos/"+store.FormatID(good)+"/other", []byte("x"))
	if _, err := Write(ctx, mem, kr, Snapshot{
		RepoID: good, Name: "site", PushSeq: 1,
		Refs: []store.Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/heads/main", ObjectID: &commit},
		},
	}); err != nil {
		t.Fatal(err)
	}

	noSnap := mustID(t)
	mem.SetRaw(packstore.PackKey(noSnap, mustID(t)), []byte("pack-without-snapshot"))

	dup := mustID(t)
	if _, err := Write(ctx, mem, kr, headSnap(dup, "othername", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(ctx, mem, kr, headSnap(dup, "othername", 1)); err != nil {
		t.Fatal(err)
	}
	mem.SetRaw("repos/not-a-uuid/junk", []byte("x"))

	var out bytes.Buffer
	failed, err := Restore(ctx, st, mem, kr, Options{Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	t.Logf("EVIDENCE restore\n%s", text)
	if failed != 1 {
		t.Fatalf("failed %d", failed)
	}
	if !strings.Contains(text, "skipped "+store.FormatID(noSnap)+" no snapshot") {
		t.Fatal("missing no-snapshot line")
	}
	if !strings.Contains(text, "failed "+store.FormatID(dup)+" two snapshots with push_seq 1") {
		t.Fatal("missing duplicate push_seq line")
	}
	restored := "restored site " + store.FormatID(good) + " push_seq=1 refs=2 packs=1 incomplete=1 unexpected=1"
	if !strings.Contains(text, restored) {
		t.Fatal("missing restored line")
	}
	if !strings.Contains(text, "unexpected keys=1") {
		t.Fatal("missing stray key line")
	}
	for _, secret := range []string{"refs/", "HEAD", hex.EncodeToString(commit[:])} {
		if strings.Contains(text, secret) {
			t.Fatal("restore output contains repository content")
		}
	}

	repo, refs, packs, err := st.ReadRepo(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != good || repo.PushSeq != 1 || len(refs) != 2 || len(packs) != 1 || packs[0].ID != complete {
		t.Fatal("restored rows")
	}
	if _, err := st.RepoByID(ctx, noSnap); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no snapshot inserted: %v", err)
	}
	if _, err := st.RepoByID(ctx, dup); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("duplicate inserted: %v", err)
	}
}

func TestRestoreMarkers(t *testing.T) {
	st := openStore(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	packPlain, idxPlain, commit := gitCommitPack(t)
	kr := testKeyring(t)
	mem := blob.NewMem()
	ctx := context.Background()

	hidden := mustID(t)
	hiddenPack := mustID(t)
	putEncrypted(t, mem, kr, hidden, hiddenPack, objcrypt.KindPack, packstore.PackKey(hidden, hiddenPack), packPlain)
	putEncrypted(t, mem, kr, hidden, hiddenPack, objcrypt.KindIdx, packstore.IdxKey(hidden, hiddenPack), idxPlain)
	if _, err := Write(ctx, mem, kr, Snapshot{
		RepoID: hidden, Name: "hidden", PushSeq: 1,
		Refs: []store.Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/heads/main", ObjectID: &commit},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(ctx, mem, kr, Snapshot{RepoID: hidden, Name: "hidden", PushSeq: 2, Deleted: true}); err != nil {
		t.Fatal(err)
	}

	kept := mustID(t)
	keptPack := mustID(t)
	putEncrypted(t, mem, kr, kept, keptPack, objcrypt.KindPack, packstore.PackKey(kept, keptPack), packPlain)
	putEncrypted(t, mem, kr, kept, keptPack, objcrypt.KindIdx, packstore.IdxKey(kept, keptPack), idxPlain)
	if _, err := Write(ctx, mem, kr, Snapshot{RepoID: kept, Name: "kept", PushSeq: 1, Deleted: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(ctx, mem, kr, Snapshot{
		RepoID: kept, Name: "kept", PushSeq: 2,
		Refs: []store.Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/heads/main", ObjectID: &commit},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Push 3 names an object in no pack, push 2 is a marker, push 1 is complete.
	// Choosing the marker would restore an empty repository, because a marker
	// has no object ids for objectsPresent to reject.
	older := mustID(t)
	olderPack := mustID(t)
	var absent [20]byte
	absent[0] = 0xee
	putEncrypted(t, mem, kr, older, olderPack, objcrypt.KindPack, packstore.PackKey(older, olderPack), packPlain)
	putEncrypted(t, mem, kr, older, olderPack, objcrypt.KindIdx, packstore.IdxKey(older, olderPack), idxPlain)
	if _, err := Write(ctx, mem, kr, Snapshot{
		RepoID: older, Name: "older", PushSeq: 1,
		Refs: []store.Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/heads/main", ObjectID: &commit},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(ctx, mem, kr, Snapshot{RepoID: older, Name: "older", PushSeq: 2, Deleted: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(ctx, mem, kr, Snapshot{
		RepoID: older, Name: "older", PushSeq: 3,
		Refs: []store.Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/heads/main", ObjectID: &absent},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	failed, err := Restore(ctx, st, mem, kr, Options{Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	t.Logf("EVIDENCE markers\n%s", text)
	if failed != 0 {
		t.Fatalf("failed %d\n%s", failed, text)
	}
	if !strings.Contains(text, "skipped "+store.FormatID(hidden)+" deleted") {
		t.Fatal("missing deleted skip")
	}
	if _, err := st.RepoByID(ctx, hidden); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("marker repository inserted: %v", err)
	}
	repo, refs, packs, err := st.ReadRepo(ctx, "kept")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != kept || repo.PushSeq != 2 || len(refs) != 2 || len(packs) != 1 {
		t.Fatalf("restored %+v refs %d packs %d", repo, len(refs), len(packs))
	}
	if !strings.Contains(text, "restored kept "+store.FormatID(kept)+" push_seq=2 refs=2 packs=1") {
		t.Fatal("missing restored line")
	}
	olderRepo, olderRefs, olderPacks, err := st.ReadRepo(ctx, "older")
	if err != nil {
		t.Fatal(err)
	}
	if olderRepo.ID != older || olderRepo.PushSeq != 3 || len(olderRefs) != 2 || len(olderPacks) != 1 {
		t.Fatalf("older %+v refs %d packs %d", olderRepo, len(olderRefs), len(olderPacks))
	}
	var olderMain bool
	for _, ref := range olderRefs {
		if ref.Name == "refs/heads/main" && ref.ObjectID != nil && *ref.ObjectID == commit {
			olderMain = true
		}
	}
	if !olderMain {
		t.Fatal("older restored a ref that is not the complete snapshot")
	}
	if !strings.Contains(text, "restored older "+store.FormatID(older)+" push_seq=3 refs=2 packs=1 used_older_snapshot=true") {
		t.Fatal("missing older restored line")
	}
	for _, secret := range []string{"refs/", "HEAD", hex.EncodeToString(commit[:])} {
		if strings.Contains(text, secret) {
			t.Fatal("restore output contains repository content")
		}
	}
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

func putEncrypted(t *testing.T, mem *blob.Mem, kr *objcrypt.Keyring, repoID, objID [16]byte, kind objcrypt.Kind, key string, plain []byte) {
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
}

func gitCommitPack(t *testing.T) (pack, idx []byte, commit [20]byte) {
	t.Helper()
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
