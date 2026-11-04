package integration

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
)

// rangeCount counts GetRange calls. The fetch served line is the measurement.
type rangeCount struct {
	blob.Store
	n int64
}

func (r *rangeCount) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	r.n++
	return r.Store.GetRange(ctx, key, off, n)
}

var fetchObjectID = regexp.MustCompile(`[0-9a-f]{40}`)

type servedLine struct {
	Msg        string `json:"msg"`
	Repository string `json:"repository"`
	Mode       string `json:"mode"`
	Packs      int    `json:"packs"`
	RangeReads int    `json:"range_reads"`
	CacheHits  int    `json:"cache_hits"`
	Bytes      int    `json:"bytes"`
	MS         int64  `json:"ms"`
	raw        string
}

func fetchServed(t *testing.T, logs string) []servedLine {
	t.Helper()
	var out []servedLine
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"msg":"fetch served"`) {
			continue
		}
		if fetchObjectID.MatchString(line) {
			t.Fatalf("fetch served line contains an object id: %s", line)
		}
		for _, key := range []string{`"repository":`, `"mode":`, `"packs":`, `"range_reads":`, `"cache_hits":`, `"bytes":`, `"ms":`} {
			if !strings.Contains(line, key) {
				t.Fatalf("fetch served line missing %s: %s", key, line)
			}
		}
		var got servedLine
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("fetch served json: %v", err)
		}
		if got.Mode != "joined" && got.Mode != "generated" {
			t.Fatalf("mode %q", got.Mode)
		}
		if len(got.Repository) != 36 {
			t.Fatalf("repository %q", got.Repository)
		}
		got.raw = line
		out = append(out, got)
	}
	return out
}

func (h *harness) fetch(dir string) {
	h.t.Helper()
	h.must(dir, "-c", bearer(h.token), "fetch", "origin")
}

func TestRepeatedFetchHits(t *testing.T) {
	mem := blob.NewMem()
	counter := &rangeCount{Store: mem}
	h := newHarnessCache(t, deny(), counter, objcrypt.NewChunkCache(1<<20))
	repo := h.createRepo("cache-fetch")
	dir := h.initRepo("main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.commitAll(dir, "one")
	remote := h.remote(repo.Name)
	h.push(dir, remote, "HEAD:refs/heads/main")
	a, _, _ := h.clone(remote)
	b, _, _ := h.clone(remote)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.commitAll(dir, "two")
	h.push(dir, remote, "HEAD:refs/heads/main")

	before := len(fetchServed(t, h.logs.String()))
	readsBefore := counter.n
	h.fetch(a)
	firstReads := counter.n - readsBefore
	h.fetch(b)
	secondReads := counter.n - (readsBefore + firstReads)
	lines := fetchServed(t, h.logs.String())
	got := lines[before:]
	if len(got) != 2 {
		t.Fatalf("fetches logged %d lines", len(got))
	}
	t.Logf("first %s", got[0].raw)
	t.Logf("second %s", got[1].raw)
	t.Logf("wrapper range reads first %d second %d", firstReads, secondReads)
	if got[1].CacheHits <= 0 {
		t.Fatalf("second fetch cache_hits %d", got[1].CacheHits)
	}
	if got[1].RangeReads >= got[0].RangeReads {
		t.Fatalf("second range_reads %d, first %d", got[1].RangeReads, got[0].RangeReads)
	}
	h.must(a, "fsck")
	h.must(b, "fsck")
}

func TestRepeatedSmallCloneHits(t *testing.T) {
	mem := blob.NewMem()
	counter := &rangeCount{Store: mem}
	h := newHarnessCache(t, deny(), counter, objcrypt.NewChunkCache(1<<20))
	repo := h.createRepo("cache-small-clone")
	dir := h.initRepo("main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.commitAll(dir, "small")
	remote := h.remote(repo.Name)
	h.push(dir, remote, "HEAD:refs/heads/main")
	h.clone(remote)
	h.clone(remote)
	lines := fetchServed(t, h.logs.String())
	if len(lines) != 2 {
		t.Fatalf("clones logged %d lines", len(lines))
	}
	t.Logf("first %s", lines[0].raw)
	t.Logf("second %s", lines[1].raw)
	t.Logf("wrapper range reads %d", counter.n)
	if lines[1].RangeReads != lines[1].Packs {
		t.Fatalf("second clone range_reads %d packs %d", lines[1].RangeReads, lines[1].Packs)
	}
	if lines[1].CacheHits <= 0 {
		t.Fatalf("second clone cache_hits %d", lines[1].CacheHits)
	}
}

func TestLargeCloneDoesNotInsert(t *testing.T) {
	cache := objcrypt.NewChunkCache(64 << 10)
	h := newHarnessCache(t, deny(), nil, cache)
	large := h.createRepo("cache-large")
	dir := h.initRepo("main")
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), bytesOf(32<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	h.commitAll(dir, "big")
	remote := h.remote(large.Name)
	h.push(dir, remote, "HEAD:refs/heads/main")
	h.clone(remote)
	entries, accounted, _ := cache.View()
	t.Logf("after large clone only entries=%d bytes=%d", entries, accounted)
	if entries != 0 || accounted != 0 {
		t.Fatalf("large clone filled the cache entries=%d bytes=%d", entries, accounted)
	}

	small := h.createRepo("cache-tiny")
	sdir := h.initRepo("main")
	if err := os.WriteFile(filepath.Join(sdir, "a.txt"), []byte("tiny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.commitAll(sdir, "tiny")
	sremote := h.remote(small.Name)
	h.push(sdir, sremote, "HEAD:refs/heads/main")
	h.clone(sremote)
	beforeN, beforeB, beforeK := cache.View()
	t.Logf("before second large clone entries=%d bytes=%d keys=%d", beforeN, beforeB, len(beforeK))
	if beforeN == 0 {
		t.Fatal("small clone did not fill the cache")
	}
	h.clone(remote)
	afterN, afterB, afterK := cache.View()
	t.Logf("after second large clone entries=%d bytes=%d keys=%d", afterN, afterB, len(afterK))
	if afterN != beforeN || afterB != beforeB || strings.Join(afterK, "\n") != strings.Join(beforeK, "\n") {
		t.Fatal("large clone changed cache contents")
	}
}

func TestFetchServedLineHasNoIDs(t *testing.T) {
	h := newHarnessCache(t, deny(), nil, objcrypt.NewChunkCache(1<<20))
	repo := h.createRepo("cache-line")
	dir := h.initRepo("main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.commitAll(dir, "line")
	remote := h.remote(repo.Name)
	h.push(dir, remote, "HEAD:refs/heads/main")
	h.clone(remote)
	lines := fetchServed(t, h.logs.String())
	if len(lines) == 0 {
		t.Fatal("no fetch served line")
	}
	for _, line := range lines {
		t.Logf("%s", line.raw)
	}
}

func bytesOf(n int) []byte {
	buf := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(buf)
	return buf
}
