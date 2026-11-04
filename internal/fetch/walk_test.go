package fetch

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestCommitWalkLinear(t *testing.T) {
	dir, env, ids := buildLinear(t, 100)
	objs := loadGitPack(t, dir, env)
	got := commitsIn(t, objs, mustSelect(t, objs, [][20]byte{ids[99]}, [][20]byte{ids[59]}))
	want := reverseIDs(ids[60:])
	if !sameIDs(got, want) {
		t.Fatalf("linear commits %d, want %d", len(got), len(want))
	}
}

func TestCommitWalkMerge(t *testing.T) {
	dir, env, main, sideTip, merge := buildMerge(t)
	objs := loadGitPack(t, dir, env)
	got := commitsIn(t, objs, mustSelect(t, objs, [][20]byte{merge}, [][20]byte{main[59], sideTip}))
	want := [][20]byte{merge}
	for i := 98; i >= 60; i-- {
		want = append(want, main[i])
	}
	if !sameIDs(got, want) {
		t.Fatalf("merge commits %d, want %d", len(got), len(want))
	}
	for i := 0; i <= 9; i++ {
		if containsID(got, main[i]) {
			t.Fatal("walk entered commit 10's history")
		}
	}
}

// The client has main at 60 and not the side branch, which forked from
// commit 10. Commit 10 is reached from the side branch as interesting, and
// only the uninteresting walk down from 60 stops it being sent. The merge
// test above cannot see that, because there the side tip itself is common.
func TestCommitWalkSideForkedBelowCommon(t *testing.T) {
	dir, env, main, sideTip, merge := buildMerge(t)
	objs := loadGitPack(t, dir, env)
	got := commitsIn(t, objs, mustSelect(t, objs, [][20]byte{merge}, [][20]byte{main[59]}))
	want := [][20]byte{merge, sideTip}
	for i := 98; i >= 60; i-- {
		want = append(want, main[i])
	}
	for _, id := range want {
		if !containsID(got, id) {
			t.Fatal("a commit the client lacks was not sent")
		}
	}
	// buildMerge makes 3 side commits.
	if len(got) != len(want)-1+3 {
		t.Fatalf("sent %d commits, want %d", len(got), len(want)-1+3)
	}
	for i := 0; i <= 59; i++ {
		if containsID(got, main[i]) {
			t.Fatal("sent a commit the client has")
		}
	}
}

func TestCommitWalkWantIsCommon(t *testing.T) {
	dir, env, ids := buildLinear(t, 1)
	objs := loadGitPack(t, dir, env)
	got := mustSelect(t, objs, [][20]byte{ids[0]}, [][20]byte{ids[0]})
	if len(got) != 0 {
		t.Fatalf("sent %d objects", len(got))
	}
	var buf bytes.Buffer
	if err := writeGenerated(&buf, objs, got); err != nil {
		t.Fatal(err)
	}
	indexPack(t, env, buf.Bytes())
}

func TestCommitWalkClockSkew(t *testing.T) {
	dir, env, r, p, c, s := buildSkew(t)
	objs := loadGitPack(t, dir, env)
	got := commitsIn(t, objs, mustSelect(t, objs, [][20]byte{s}, [][20]byte{c}))
	allow := map[[20]byte]struct{}{s: {}, p: {}, r: {}}
	for _, id := range got {
		if _, ok := allow[id]; !ok {
			t.Fatal("clock skew sent a commit outside the graph")
		}
	}
	if !containsID(got, s) {
		t.Fatal("clock skew missed the want")
	}
	if containsID(got, c) {
		t.Fatal("clock skew sent the common commit")
	}
	t.Logf("EVIDENCE clock-skew commits sent=%d exact=1", len(got))
}

func TestTreeStep(t *testing.T) {
	dir, env := initWork(t)
	writeWorkFile(t, dir, "dir/sub/file.txt", "base\n")
	writeWorkFile(t, dir, "dir/sub/keep.txt", "keep\n")
	writeWorkFile(t, dir, "other.txt", "other\n")
	gitOut(t, dir, env, "add", "-A")
	oldLink := strings.Repeat("ab", 20)
	newLink := strings.Repeat("cd", 20)
	gitOut(t, dir, env, "update-index", "--add", "--cacheinfo", "160000,"+oldLink+",modules/lib")
	treeA := writeTree(t, dir, env)
	when := time.Date(2020, 6, 1, 0, 0, 1, 0, time.UTC)
	commitA := commitTree(t, dir, env, treeA, when, "base")
	gitOut(t, dir, env, "update-ref", "refs/heads/main", commitA)

	writeWorkFile(t, dir, "dir/sub/file.txt", "changed\n")
	gitOut(t, dir, env, "add", "dir/sub/file.txt")
	gitOut(t, dir, env, "update-index", "--add", "--cacheinfo", "160000,"+newLink+",modules/lib")
	treeB := writeTree(t, dir, env)
	commitB := commitTree(t, dir, env, treeB, when.Add(time.Second), "changed", commitA)
	gitOut(t, dir, env, "update-ref", "refs/heads/main", commitB)

	objs := loadGitPack(t, dir, env)
	idA := mustID(t, commitA)
	idB := mustID(t, commitB)
	got := mustSelect(t, objs, [][20]byte{idB}, [][20]byte{idA})
	root := mustID(t, treeB)
	dirTree := mustID(t, revParse(t, dir, env, treeB+":dir"))
	subTree := mustID(t, revParse(t, dir, env, treeB+":dir/sub"))
	blob := mustID(t, revParse(t, dir, env, commitB+":dir/sub/file.txt"))
	modTree := mustID(t, revParse(t, dir, env, treeB+":modules"))
	link := mustID(t, newLink)
	// The tree that holds the gitlink changes with it. The gitlink itself is not an object we send.
	want := [][20]byte{idB, root, dirTree, subTree, modTree, blob}
	if !sameIDs(got, want) {
		t.Fatalf("tree step sent %d objects, want %d", len(got), len(want))
	}
	if containsID(got, link) {
		t.Fatal("submodule was sent")
	}

	writeWorkFile(t, dir, "dir/sub/file.txt", "base\n")
	gitOut(t, dir, env, "add", "dir/sub/file.txt")
	treeC := writeTree(t, dir, env)
	commitC := commitTree(t, dir, env, treeC, when.Add(2*time.Second), "restored", commitB)
	gitOut(t, dir, env, "update-ref", "refs/heads/main", commitC)
	objs = loadGitPack(t, dir, env)
	idC := mustID(t, commitC)
	baseBlob := mustID(t, revParse(t, dir, env, commitA+":dir/sub/file.txt"))
	restored := mustSelect(t, objs, [][20]byte{idC}, [][20]byte{idB})
	if !containsID(restored, baseBlob) {
		t.Fatal("restored blob was not sent")
	}
}

func TestCompleteness(t *testing.T) {
	check := func(name, dir string, env []string, objs *packstore.Objects, wants, common [][20]byte) {
		t.Helper()
		ids := mustSelect(t, objs, wants, common)
		var buf bytes.Buffer
		if err := writeGenerated(&buf, objs, ids); err != nil {
			t.Fatal(err)
		}
		exit := fsckPack(t, env, dir, hexList(common), buf.Bytes(), hexList(wants))
		t.Logf("EVIDENCE graph=%s command=%q exit=%d", name, "git fsck --connectivity-only", exit)
		if exit != 0 {
			t.Fatalf("%s fsck exit %d", name, exit)
		}
	}

	dir, env, ids := buildLinear(t, 100)
	objs := loadGitPack(t, dir, env)
	check("linear", dir, env, objs, [][20]byte{ids[99]}, [][20]byte{ids[59]})

	dir, env, main, sideTip, merge := buildMerge(t)
	objs = loadGitPack(t, dir, env)
	check("merge", dir, env, objs, [][20]byte{merge}, [][20]byte{main[59], sideTip})

	dir, env, ids = buildLinear(t, 1)
	objs = loadGitPack(t, dir, env)
	check("want-is-common", dir, env, objs, [][20]byte{ids[0]}, [][20]byte{ids[0]})

	dir, env, _, _, c, s := buildSkew(t)
	objs = loadGitPack(t, dir, env)
	check("clock-skew", dir, env, objs, [][20]byte{s}, [][20]byte{c})

	dir, env, ids = buildLinear(t, 100)
	objs = loadGitPack(t, dir, env)
	full := mustSelect(t, objs, [][20]byte{ids[99]}, [][20]byte{ids[59]})
	dropped, ok := dropOneBlob(t, objs, full)
	if !ok {
		t.Fatal("linear pack had no blob to drop")
	}
	var buf bytes.Buffer
	if err := writeGenerated(&buf, objs, dropped); err != nil {
		t.Fatal(err)
	}
	client := fetchCommons(t, env, dir, hexList([][20]byte{ids[59]}))
	if err := indexPackIn(t, env, client, buf.Bytes()); err != nil {
		t.Fatalf("index-pack of a pack missing one blob: %v", err)
	}
	gitOut(t, client, env, "update-ref", "refs/wants/w0", hex.EncodeToString(ids[99][:]))
	exit := fsckExit(t, client, env)
	t.Logf("EVIDENCE graph=%s command=%q exit=%d", "linear-dropped-blob", "git fsck --connectivity-only", exit)
	if exit == 0 {
		t.Fatal("fsck passed after a blob was dropped")
	}
}

func TestPackWriter(t *testing.T) {
	dir, env := initWork(t)
	body := make([]byte, 200*1024)
	for i := range body {
		body[i] = byte(i)
	}
	writeWorkFile(t, dir, "big.bin", string(body))
	gitOut(t, dir, env, "add", "-A")
	tree := writeTree(t, dir, env)
	commit := commitTree(t, dir, env, tree, time.Date(2020, 3, 1, 0, 0, 1, 0, time.UTC), "big")
	gitOut(t, dir, env, "update-ref", "refs/heads/main", commit)
	objs := loadGitPack(t, dir, env)
	blobID := mustID(t, revParse(t, dir, env, commit+":big.bin"))
	ids := [][20]byte{mustID(t, commit), mustID(t, tree), blobID}
	var dst maxWrite
	if err := writeGenerated(&dst, objs, ids); err != nil {
		t.Fatal(err)
	}
	if dst.longest > sidebandDataMax {
		t.Fatalf("write of %d bytes", dst.longest)
	}
	indexPack(t, env, dst.buf.Bytes())
	t.Logf("EVIDENCE index-pack exit=0 longest write=%d", dst.longest)
}

type maxWrite struct {
	buf     bytes.Buffer
	longest int
}

func (m *maxWrite) Write(p []byte) (int, error) {
	if len(p) > m.longest {
		m.longest = len(p)
	}
	return m.buf.Write(p)
}

func mustSelect(t *testing.T, objs *packstore.Objects, wants, common [][20]byte) [][20]byte {
	t.Helper()
	got, err := selectObjects(objs, wants, common)
	if err != nil {
		t.Fatal(redactHex(err.Error()))
	}
	return got
}

func commitsIn(t *testing.T, objs *packstore.Objects, ids [][20]byte) [][20]byte {
	t.Helper()
	var out [][20]byte
	for _, id := range ids {
		typ, _, err := objs.Get(id)
		if err != nil {
			t.Fatal(redactHex(err.Error()))
		}
		if typ == plumbing.CommitObject {
			out = append(out, id)
		}
	}
	return out
}

func dropOneBlob(t *testing.T, objs *packstore.Objects, ids [][20]byte) ([][20]byte, bool) {
	t.Helper()
	out := make([][20]byte, 0, len(ids)-1)
	dropped := false
	for _, id := range ids {
		typ, _, err := objs.Get(id)
		if err != nil {
			t.Fatal(redactHex(err.Error()))
		}
		if !dropped && typ == plumbing.BlobObject {
			dropped = true
			continue
		}
		out = append(out, id)
	}
	return out, dropped
}

func buildLinear(t *testing.T, n int) (string, []string, [][20]byte) {
	t.Helper()
	dir, env := initWork(t)
	ids := make([][20]byte, n)
	var parent string
	for i := 1; i <= n; i++ {
		writeWorkFile(t, dir, fmt.Sprintf("f%d.txt", (i-1)%7), fmt.Sprintf("c %d\n", i))
		gitOut(t, dir, env, "add", "-A")
		tree := writeTree(t, dir, env)
		when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		id := commitTree(t, dir, env, tree, when, fmt.Sprintf("c %d", i), parents...)
		parent = id
		ids[i-1] = mustID(t, id)
		gitOut(t, dir, env, "update-ref", "refs/heads/main", id)
	}
	return dir, env, ids
}

func buildMerge(t *testing.T) (string, []string, [][20]byte, [20]byte, [20]byte) {
	t.Helper()
	dir, env, main := buildLinear(t, 99)
	tree10 := revParse(t, dir, env, hex.EncodeToString(main[9][:])+"^{tree}")
	gitOut(t, dir, env, "read-tree", tree10)
	var parent = hex.EncodeToString(main[9][:])
	var side string
	for i := 1; i <= 3; i++ {
		writeWorkFile(t, dir, "side.txt", fmt.Sprintf("side %d\n", i))
		gitOut(t, dir, env, "add", "side.txt")
		tree := writeTree(t, dir, env)
		when := time.Date(2020, 2, 1, 0, 0, i, 0, time.UTC)
		side = commitTree(t, dir, env, tree, when, fmt.Sprintf("side %d", i), parent)
		parent = side
	}
	sideTip := mustID(t, side)
	gitOut(t, dir, env, "update-ref", "refs/holds/side", side)
	tree99 := revParse(t, dir, env, hex.EncodeToString(main[98][:])+"^{tree}")
	mergeText := commitTree(t, dir, env, tree99, time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC), "merge",
		hex.EncodeToString(main[98][:]), side)
	merge := mustID(t, mergeText)
	gitOut(t, dir, env, "update-ref", "refs/heads/main", mergeText)
	return dir, env, main, sideTip, merge
}

func buildSkew(t *testing.T) (string, []string, [20]byte, [20]byte, [20]byte, [20]byte) {
	t.Helper()
	dir, env := initWork(t)
	writeWorkFile(t, dir, "a.txt", "a\n")
	gitOut(t, dir, env, "add", "-A")
	tree := writeTree(t, dir, env)
	rText := commitTree(t, dir, env, tree, time.Unix(1, 0).UTC(), "R")
	pText := commitTree(t, dir, env, tree, time.Unix(300, 0).UTC(), "P", rText)
	cText := commitTree(t, dir, env, tree, time.Unix(20, 0).UTC(), "C", pText)
	sText := commitTree(t, dir, env, tree, time.Unix(200, 0).UTC(), "S", pText)
	gitOut(t, dir, env, "update-ref", "refs/heads/main", sText)
	gitOut(t, dir, env, "update-ref", "refs/holds/c", cText)
	return dir, env, mustID(t, rText), mustID(t, pText), mustID(t, cText), mustID(t, sText)
}

func fsckPack(t *testing.T, env []string, work string, commons []string, pack []byte, wants []string) int {
	t.Helper()
	client := fetchCommons(t, env, work, commons)
	if err := indexPackIn(t, env, client, pack); err != nil {
		t.Fatalf("index-pack: %v", err)
	}
	for i, id := range wants {
		gitOut(t, client, env, "update-ref", fmt.Sprintf("refs/wants/w%d", i), id)
	}
	return fsckExit(t, client, env)
}

func fetchCommons(t *testing.T, env []string, work string, commons []string) string {
	t.Helper()
	client := filepath.Join(t.TempDir(), "client")
	gitOut(t, "", env, "init", "-b", "main", client)
	if len(commons) == 0 {
		return client
	}
	args := []string{"-c", "protocol.file.allow=always", "fetch", "--no-tags", work}
	for i, id := range commons {
		ref := fmt.Sprintf("refs/holds/c%d", i)
		gitOut(t, work, env, "update-ref", ref, id)
		args = append(args, ref+":"+ref)
	}
	gitOut(t, client, env, args...)
	return client
}

func fsckExit(t *testing.T, dir string, env []string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "fsck", "--connectivity-only")
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if ok := errorAs(err, &exit); !ok {
		t.Fatal(redactHex(err.Error()))
	}
	return exit.ExitCode()
}

func indexPack(t *testing.T, env []string, pack []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bare.git")
	gitOut(t, "", env, "init", "--bare", dir)
	if err := indexPackIn(t, env, dir, pack); err != nil {
		t.Fatal(err)
	}
}

func indexPackIn(t *testing.T, env []string, dir string, pack []byte) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "index-pack", "--stdin")
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(pack)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("index-pack: %s", redactHex(stderr.String()))
	}
	return nil
}

func loadGitPack(t *testing.T, dir string, env []string) *packstore.Objects {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rev := exec.CommandContext(ctx, "git", "-C", dir, "rev-list", "--objects", "--all")
	rev.Env = env
	pk := exec.CommandContext(ctx, "git", "-C", dir, "pack-objects", "--stdout")
	pk.Env = env
	var revErr, packErr bytes.Buffer
	rev.Stderr = &revErr
	pk.Stderr = &packErr
	pipe, err := rev.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	pk.Stdin = pipe
	var out bytes.Buffer
	pk.Stdout = &out
	if err := rev.Start(); err != nil {
		t.Fatal(err)
	}
	if err := pk.Start(); err != nil {
		t.Fatal(err)
	}
	errRev := rev.Wait()
	errPack := pk.Wait()
	if errRev != nil || errPack != nil {
		t.Fatalf("pack objects: %v %v %s %s", errRev, errPack, redactHex(revErr.String()), redactHex(packErr.String()))
	}
	objs := openPackBytes(t, out.Bytes())
	t.Cleanup(func() { objs.Close() })
	return objs
}

func openPackBytes(t *testing.T, raw []byte) *packstore.Objects {
	t.Helper()
	var master [32]byte
	copy(master[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	var repoID, packID [16]byte
	repoID[0] = 7
	packID[0] = 8
	binding := objcrypt.Binding{RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindPack}
	sp, err := packstore.Spool(context.Background(), bytes.NewReader(raw), t.TempDir(), kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	parsed, pk, err := packstore.Parse(context.Background(), sp, kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := pk.Close(); err != nil {
		t.Fatal(err)
	}
	mem := blob.NewMem()
	f, err := os.Open(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	err = mem.Put(context.Background(), packstore.PackKey(repoID, packID), f, sp.Size)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	row := store.Pack{
		ID: packID, PackKey: packstore.PackKey(repoID, packID), IdxKey: packstore.IdxKey(repoID, packID),
		Bytes: sp.Size, IdxBytes: 1,
	}
	return packstore.NewObjects(context.Background(), mem, kr, repoID, func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) {
		if _, ok := parsed.Objects[id]; !ok {
			return store.Pack{}, nil, false
		}
		return row, parsed.Index, true
	}, nil, nil)
}

func initWork(t *testing.T) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	env := []string{
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Ada Lovelace",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada Lovelace",
		"GIT_COMMITTER_EMAIL=ada@example.com",
		"PATH=" + os.Getenv("PATH"),
	}
	dir := filepath.Join(t.TempDir(), "work")
	gitOut(t, "", env, "init", "-b", "main", dir)
	return dir, env
}

func gitOut(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v %s", strings.Join(args, " "), err, redactHex(stderr.String()))
	}
	return stdout.String()
}

func writeTree(t *testing.T, dir string, env []string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, dir, env, "write-tree"))
}

func commitTree(t *testing.T, dir string, env []string, tree string, when time.Time, msg string, parents ...string) string {
	t.Helper()
	args := []string{"commit-tree", tree}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", msg)
	dated := append(append([]string{}, env...),
		"GIT_AUTHOR_DATE="+when.UTC().Format(time.RFC3339),
		"GIT_COMMITTER_DATE="+when.UTC().Format(time.RFC3339),
	)
	return strings.TrimSpace(gitOut(t, dir, dated, args...))
}

func revParse(t *testing.T, dir string, env []string, expr string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, dir, env, "rev-parse", expr))
}

func writeWorkFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustID(t *testing.T, text string) [20]byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != 20 {
		t.Fatal("object id")
	}
	var id [20]byte
	copy(id[:], raw)
	return id
}

func hexList(ids [][20]byte) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = hex.EncodeToString(id[:])
	}
	return out
}

func sameIDs(a, b [][20]byte) bool {
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

func containsID(ids [][20]byte, id [20]byte) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

func reverseIDs(ids [][20]byte) [][20]byte {
	out := make([][20]byte, len(ids))
	for i := range ids {
		out[len(ids)-1-i] = ids[i]
	}
	return out
}

var hex40 = regexp.MustCompile(`[0-9a-fA-F]{40}`)

func redactHex(s string) string {
	return hex40.ReplaceAllString(s, "object")
}

func errorAs(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	*target = exit
	return true
}
