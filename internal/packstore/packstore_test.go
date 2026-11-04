package packstore

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
)

// sentinel is 40 bytes written into a blob. Ciphertext must not contain it.
const sentinel = "lkv-sentinel-0123456789abcdef0123456789a"

func TestRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if len(sentinel) != 40 {
		t.Fatalf("sentinel is %d bytes", len(sentinel))
	}
	dir := initRepo(t)
	writeFile(t, filepath.Join(dir, "a", "b", "c", "deep.txt"), "nested\n")
	writeFile(t, filepath.Join(dir, "note.txt"), sentinel+"\n")
	bin := bytes.Repeat([]byte{0, 1, 2, 3, 255}, 3000)
	writeFile(t, filepath.Join(dir, "bin.dat"), string(bin))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "c0")
	for i := 1; i < 50; i++ {
		writeFile(t, filepath.Join(dir, "note.txt"), fmt.Sprintf("%s\nedit %d\n", sentinel, i))
		if i%7 == 0 {
			writeFile(t, filepath.Join(dir, "a", "b", "c", "deep.txt"), fmt.Sprintf("nested %d\n", i))
		}
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-m", fmt.Sprintf("c%d", i))
	}

	tip := rev(t, dir, "HEAD")
	pack := packObjects(t, dir, tip+"\n", false)
	sp, parsed, pk := spoolParse(t, pack)
	defer pk.Close()

	raw, err := os.ReadFile(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(raw, []byte("PACK")) {
		t.Fatal("spool file starts with PACK")
	}
	if bytes.Contains(raw, []byte(sentinel)) {
		t.Fatal("spool file contains the sentinel")
	}

	want := objectSet(t, dir, "HEAD")
	if len(parsed.Objects) != len(want) {
		t.Fatalf("objects %d, rev-list %d", len(parsed.Objects), len(want))
	}
	for id, typ := range want {
		got, ok := parsed.Objects[id]
		if !ok {
			t.Fatalf("missing %x", id)
		}
		if got.String() != typ {
			t.Fatalf("%x type %s want %s", id, got, typ)
		}
		_, content, err := pk.Get(id)
		if err != nil {
			t.Fatalf("get %x: %v", id, err)
		}
		cat := catFile(t, dir, typ, id)
		if !bytes.Equal(content, cat) {
			t.Fatalf("%s %x content mismatch %d %d", typ, id, len(content), len(cat))
		}
	}
}

func TestRefusals(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := initRepo(t)
	body := bytes.Repeat([]byte("abcdefghijklmnopqrstuvwxyz\n"), 8000)
	writeFile(t, filepath.Join(dir, "big.txt"), string(body))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "parent")
	parent := rev(t, dir, "HEAD")
	body[len(body)/2] = 'Z'
	writeFile(t, filepath.Join(dir, "big.txt"), string(body))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "child")
	child := rev(t, dir, "HEAD")

	full := packObjects(t, dir, child+"\n", false)
	thin := packObjects(t, dir, child+"\n^"+parent+"\n", true)

	t.Run("thin", func(t *testing.T) {
		sp := mustSpool(t, thin)
		_, _, err := Parse(context.Background(), sp, testKeyring(t), testBinding())
		if !errors.Is(err, ErrThinPack) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		// The declared size is larger than the truncated stream can produce.
		// go-git otherwise reports an inflated-size mismatch when leftover
		// trailer bytes are read as deflate input, which hides the EOF.
		sp := mustSpool(t, truncatedBlobPack(t))
		_, _, err := Parse(context.Background(), sp, testKeyring(t), testBinding())
		if !errors.Is(err, ErrTruncated) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("flipped byte", func(t *testing.T) {
		p := append([]byte{}, full...)
		p[len(p)/2] ^= 0x01
		sp := mustSpool(t, resign(p))
		_, _, err := Parse(context.Background(), sp, testKeyring(t), testBinding())
		if !errors.Is(err, ErrBadPack) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("wrong trailer", func(t *testing.T) {
		p := append([]byte{}, full...)
		p[len(p)-1] ^= 0xff
		_, err := Spool(context.Background(), bytes.NewReader(p), t.TempDir(), testKeyring(t), testBinding())
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("%v", err)
		}
	})
	// Appending sha1(pack) is the case the parser checksum accepts.
	// Repeating the trailer fails the spool checksum, so that is not this case.
	t.Run("extra bytes", func(t *testing.T) {
		sum := sha1.Sum(full)
		sp := mustSpool(t, append(append([]byte{}, full...), sum[:]...))
		_, _, err := Parse(context.Background(), sp, testKeyring(t), testBinding())
		if !errors.Is(err, ErrTrailing) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("version 4", func(t *testing.T) {
		p := append([]byte{}, full...)
		binary.BigEndian.PutUint32(p[4:8], 4)
		_, err := Spool(context.Background(), bytes.NewReader(resign(p)), t.TempDir(), testKeyring(t), testBinding())
		if !errors.Is(err, ErrBadVersion) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("ten bytes", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Spool(context.Background(), bytes.NewReader(make([]byte, 10)), dir, testKeyring(t), testBinding())
		if !errors.Is(err, ErrTooSmall) {
			t.Fatalf("%v", err)
		}
		left, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(left) != 0 {
			t.Fatalf("spool file left behind: %d", len(left))
		}
	})
}

func TestMaxPushBytes(t *testing.T) {
	t.Cleanup(func() { maxPushBytes = MaxPushBytes })
	maxPushBytes = 1000
	var got int64
	body := &countReader{r: bytes.NewReader(bytes.Repeat([]byte("x"), 200<<10)), n: &got}
	dir := t.TempDir()
	_, err := Spool(context.Background(), body, dir, testKeyring(t), testBinding())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
	if got > maxPushBytes+spoolChunk {
		t.Fatalf("read %d, limit %d chunk %d", got, maxPushBytes, spoolChunk)
	}
	left, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(left) != 0 {
		t.Fatalf("spool file left behind: %d", len(left))
	}
}

func TestRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := initRepo(t)
	writeFile(t, filepath.Join(dir, "file.txt"), "hello\n")
	git(t, dir, "add", "file.txt")
	const sub = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+sub+",vendor/lib")
	git(t, dir, "commit", "-m", "with-submodule")

	commitRaw := catType(t, dir, "commit", "HEAD")
	got, err := Refs(plumbing.CommitObject, commitRaw)
	if err != nil {
		t.Fatal(err)
	}
	tree := hashID(t, rev(t, dir, "HEAD^{tree}"))
	if len(got) != 1 || got[0] != tree {
		t.Fatalf("commit refs %x", got)
	}

	treeRaw := catType(t, dir, "tree", "HEAD^{tree}")
	got, err = Refs(plumbing.TreeObject, treeRaw)
	if err != nil {
		t.Fatal(err)
	}
	blob := hashID(t, rev(t, dir, "HEAD:file.txt"))
	vendor := hashID(t, rev(t, dir, "HEAD:vendor"))
	if len(got) != 2 || got[0] != blob || got[1] != vendor {
		t.Fatalf("root tree refs %x", got)
	}
	// git stores path vendor/lib as a tree vendor whose only entry is the gitlink.
	subTree := catType(t, dir, "tree", "HEAD:vendor")
	got, err = Refs(plumbing.TreeObject, subTree)
	if err != nil {
		t.Fatal(err)
	}
	subID := hashID(t, sub)
	for _, id := range got {
		if id == subID {
			t.Fatal("submodule id was not skipped")
		}
	}
	if len(got) != 0 {
		t.Fatalf("submodule tree refs %x", got)
	}

	git(t, dir, "tag", "-a", "v1", "-m", "annotated")
	tagRaw := catType(t, dir, "tag", "v1")
	got, err = Refs(plumbing.TagObject, tagRaw)
	if err != nil {
		t.Fatal(err)
	}
	target := hashID(t, rev(t, dir, "v1^{}"))
	if len(got) != 1 || got[0] != target {
		t.Fatalf("tag refs %x", got)
	}

	blobRefs, err := Refs(plumbing.BlobObject, []byte("hello\n"))
	if err != nil || blobRefs != nil {
		t.Fatalf("blob refs %x %v", blobRefs, err)
	}
	if _, err := Refs(plumbing.ObjectType(9), []byte("x")); err == nil {
		t.Fatal("unknown type accepted")
	}
}

type countReader struct {
	r io.Reader
	n *int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

func spoolParse(t *testing.T, pack []byte) (*Spooled, *Parsed, *Pack) {
	t.Helper()
	sp := mustSpool(t, pack)
	parsed, pk, err := Parse(context.Background(), sp, testKeyring(t), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	return sp, parsed, pk
}

func mustSpool(t *testing.T, pack []byte) *Spooled {
	t.Helper()
	sp, err := Spool(context.Background(), bytes.NewReader(pack), t.TempDir(), testKeyring(t), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func truncatedBlobPack(t *testing.T) []byte {
	t.Helper()
	content := bytes.Repeat([]byte("abc"), 40)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	stream := zbuf.Bytes()
	if len(stream) < 8 {
		t.Fatalf("zlib stream %d", len(stream))
	}
	var body []byte
	body = append(body, 'P', 'A', 'C', 'K')
	var counts [8]byte
	binary.BigEndian.PutUint32(counts[0:4], 2)
	binary.BigEndian.PutUint32(counts[4:8], 1)
	body = append(body, counts[:]...)
	body = append(body, objectHeader(3, 100000)...)
	body = append(body, stream[:len(stream)-6]...)
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

func objectHeader(typ byte, size int) []byte {
	var out []byte
	b := (typ << 4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	out = append(out, b)
	for size > 0 {
		c := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			c |= 0x80
		}
		out = append(out, c)
	}
	return out
}

func resign(pack []byte) []byte {
	body := pack[:len(pack)-20]
	sum := sha1.Sum(body)
	out := append([]byte{}, body...)
	return append(out, sum[:]...)
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

func testBinding() objcrypt.Binding {
	var repo, pack [16]byte
	repo[0] = 1
	pack[0] = 2
	return objcrypt.Binding{RepoID: repo, ObjectID: pack, Kind: objcrypt.KindPack}
}

func objectSet(t *testing.T, dir, rev string) map[[20]byte]string {
	t.Helper()
	out := git(t, dir, "rev-list", "--objects", rev)
	set := map[[20]byte]string{}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		id := strings.Fields(line)[0]
		ids = append(ids, id)
		set[hashID(t, id)] = ""
	}
	types := gitInput(t, dir, strings.Join(ids, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	for _, line := range strings.Split(strings.TrimSpace(types), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("batch-check %q", line)
		}
		set[hashID(t, fields[0])] = fields[1]
	}
	return set
}

func catFile(t *testing.T, dir, typ string, id [20]byte) []byte {
	t.Helper()
	hexID := fmt.Sprintf("%x", id)
	return []byte(git(t, dir, "cat-file", typ, hexID))
}

func catType(t *testing.T, dir, typ, rev string) []byte {
	t.Helper()
	return []byte(git(t, dir, "cat-file", typ, rev))
}

func hashID(t *testing.T, hexID string) [20]byte {
	t.Helper()
	raw, err := hex.DecodeString(hexID)
	if err != nil || len(raw) != 20 {
		t.Fatalf("id %q: %v", hexID, err)
	}
	var id [20]byte
	copy(id[:], raw)
	return id
}

func packObjects(t *testing.T, dir, revs string, thin bool) []byte {
	t.Helper()
	args := []string{"-C", dir, "pack-objects", "--revs", "--stdout"}
	if thin {
		args = []string{"-C", dir, "pack-objects", "--thin", "--revs", "--stdout"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv(t)
	cmd.Stdin = strings.NewReader(revs)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v %s", err, stderr.String())
	}
	if !bytes.HasPrefix(stdout.Bytes(), []byte("PACK")) {
		t.Fatalf("pack does not start with PACK (%d bytes)", stdout.Len())
	}
	return stdout.Bytes()
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", "-b", "main", dir)
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rev(t *testing.T, dir, expr string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", expr))
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitInput(t, dir, "", args...)
}

func gitInput(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	full := []string{"-c", "commit.gpgsign=false"}
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv(t)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
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
