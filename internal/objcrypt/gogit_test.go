package objcrypt

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

func TestGoGitParsesEncryptedPack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is not on PATH")
	}

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "littlekittyvcs")
	runGit(t, dir, "config", "user.email", "littlekittyvcs@example.com")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	rng := rand.New(rand.NewSource(20261103))
	files := newTextFiles(rng)
	binary := make([]byte, 200<<10)
	rng.Read(binary)
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	const commits = 60
	for c := 0; c < commits; c++ {
		if c > 0 {
			mutateText(rng, files, c)
		}
		writeTextFiles(t, dir, files)
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-m", fmt.Sprintf("c%d", c))
	}
	runGit(t, dir, "repack", "-a", "-d", "-f", "--depth=50", "--window=50")

	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 {
		t.Fatalf("pack files: %v", packs)
	}
	packPath := packs[0]
	idxPath := strings.TrimSuffix(packPath, ".pack") + ".idx"
	verify := runGit(t, dir, "verify-pack", "-v", idxPath)
	deltas := countDeltaLines(verify)
	if deltas < 1 {
		t.Fatalf("verify-pack reported no deltas\n%s", verify)
	}

	pack, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatal(err)
	}
	const chunkSize = 16 << 10
	kr, err := NewKeyring(testMaster())
	if err != nil {
		t.Fatal(err)
	}
	raw := encrypt(t, pack, chunkSize, kr, testBinding())
	m := store(t, raw)
	m.ResetCalls()
	rd := openObj(t, m, kr, testBinding())
	m.ResetCalls()

	sr := io.NewSectionReader(rd, 0, rd.Size())
	scanner := packfile.NewScanner(sr)
	if !scanner.IsSeekable {
		t.Fatal("go-git NewScanner did not see an io.ReadSeeker; SectionReader should provide one")
	}
	var idxWriter idxfile.Writer
	parser, err := packfile.NewParser(scanner, &idxWriter)
	if err != nil {
		t.Fatalf("packfile.NewParser: %v", err)
	}
	sum, err := parser.Parse()
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}
	if !bytes.Equal(sum[:], pack[len(pack)-20:]) {
		t.Fatalf("checksum %x, pack tail %x", sum[:], pack[len(pack)-20:])
	}

	idx, err := idxWriter.Index()
	if err != nil {
		t.Fatal(err)
	}
	gotIDs, err := indexIDs(idx)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := gitObjectIDs(t, dir)
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("object count go-git %d git %d", len(gotIDs), len(wantIDs))
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Fatalf("missing object %s", id)
		}
	}

	gotPack := make([]byte, rd.Size())
	n, err := rd.ReadAt(gotPack, 0)
	if err != nil || int64(n) != rd.Size() {
		t.Fatalf("read pack back n=%d err=%v", n, err)
	}
	if !bytes.Equal(gotPack, pack) {
		t.Fatal("decrypted pack bytes differ from the original pack")
	}

	calls := m.RangeCalls()
	seen := map[[2]int64]int{}
	repeated := 0
	for _, c := range calls {
		k := [2]int64{c.Off, c.N}
		if seen[k] > 0 {
			repeated++
		}
		seen[k]++
	}
	chunkCount := len(pack) / chunkSize
	if len(pack)%chunkSize != 0 {
		chunkCount++
	}
	t.Logf("pack_bytes=%d chunk_count=%d delta_objects=%d object_count=%d range_calls=%d repeated_calls=%d",
		len(pack), chunkCount, deltas, len(gotIDs), len(calls), repeated)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=littlekittyvcs",
		"GIT_AUTHOR_EMAIL=littlekittyvcs@example.com",
		"GIT_COMMITTER_NAME=littlekittyvcs",
		"GIT_COMMITTER_EMAIL=littlekittyvcs@example.com",
		"GIT_AUTHOR_DATE=2026-11-03T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-11-03T00:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newTextFiles(rng *rand.Rand) [][]string {
	const (
		files = 5
		lines = 200
		width = 80
	)
	out := make([][]string, files)
	for i := range out {
		out[i] = make([]string, lines)
		for line := range out[i] {
			out[i][line] = fmt.Sprintf("f%d l%03d %s", i, line, randText(rng, width))
		}
	}
	return out
}

func mutateText(rng *rand.Rand, files [][]string, commit int) {
	for i := range files {
		for k := 0; k < 4; k++ {
			line := rng.Intn(len(files[i]))
			files[i][line] = fmt.Sprintf("f%d c%d k%d %s", i, commit, k, randText(rng, 80))
		}
	}
}

func randText(rng *rand.Rand, n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789 "
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[rng.Intn(len(alpha))]
	}
	return string(b)
}

func writeTextFiles(t *testing.T, dir string, files [][]string) {
	t.Helper()
	for i, lines := range files {
		body := strings.Join(lines, "\n") + "\n"
		path := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func countDeltaLines(verify string) int {
	n := 0
	for _, line := range strings.Split(verify, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 7 && isSHA1(fields[0]) && isSHA1(fields[6]) {
			n++
		}
	}
	return n
}

func isSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func indexIDs(idx *idxfile.MemoryIndex) (map[string]bool, error) {
	iter, err := idx.Entries()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	ids := map[string]bool{}
	for {
		e, err := iter.Next()
		if err == io.EOF {
			return ids, nil
		}
		if err != nil {
			return nil, err
		}
		ids[e.Hash.String()] = true
	}
}

func gitObjectIDs(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := runGit(t, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	ids := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !isSHA1(line) {
			t.Fatalf("unexpected cat-file line %q", line)
		}
		ids[line] = true
	}
	return ids
}
