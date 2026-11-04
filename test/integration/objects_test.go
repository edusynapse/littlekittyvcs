package integration

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingBlob(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("only\n"))
	commit := h.commitAll(dir, "only")
	tree := h.rev(dir, "HEAD^{tree}")
	body := receiveBody(t, zeroID, commit, "refs/heads/main", h.packExact(dir, commit, tree))
	before := h.objectCount()
	status, report := postReceive(t, h.srv.URL+"/repo.git/git-receive-pack", h.token, body)
	if status != http.StatusOK || !bytes.Contains(report, []byte("ng refs/heads/main missing object")) {
		t.Fatalf("status %d body %q", status, report)
	}
	if strings.Contains(h.lsRemote(h.remote("repo")), "refs/heads/main") {
		t.Fatal("ref was created")
	}
	if len(h.packs("repo")) != 0 || h.objectCount() != before {
		t.Fatal("pack stored")
	}
}

func TestTreeOnBranch(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("tree\n"))
	h.commitAll(dir, "tree")
	tree := h.rev(dir, "HEAD^{tree}")
	blob := h.rev(dir, "HEAD:note.txt")
	body := receiveBody(t, zeroID, tree, "refs/heads/main", h.packExact(dir, tree, blob))
	status, report := postReceive(t, h.srv.URL+"/repo.git/git-receive-pack", h.token, body)
	if status != http.StatusOK || !bytes.Contains(report, []byte("ng refs/heads/main not a commit")) {
		t.Fatalf("status %d body %q", status, report)
	}
	if strings.Contains(h.lsRemote(h.remote("repo")), "refs/heads/main") {
		t.Fatal("ref was created")
	}
}

func TestAnnotatedTagAndStoredCommitBranch(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("tag me\n"))
	h.commitAll(dir, "tagged")
	h.must(dir, "tag", "-a", "v1", "-m", "release one")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")

	// Every object of this push is already stored, so no pack row is added.
	before := len(h.packs("repo"))
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/same")
	if got := len(h.packs("repo")); got != before {
		t.Fatalf("packs %d before %d", got, before)
	}

	h.push(dir, h.remote("repo"), "refs/tags/v1")
	clone, _, _ := h.clone(h.remote("repo"))
	typ, _ := h.must(clone, "cat-file", "-t", "v1")
	if strings.TrimSpace(typ) != "tag" {
		t.Fatalf("v1 is %q", typ)
	}
	h.must(clone, "fsck", "--full")
	if _, _, err := h.git(clone, "rev-parse", "--verify", "origin/same"); err != nil {
		t.Fatal("origin/same missing")
	}
}

func TestWantNotATip(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("repo")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("a\n"))
	first := h.commitAll(dir, "a")
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("b\n"))
	h.commitAll(dir, "b")
	h.push(dir, h.remote("repo"), "HEAD:refs/heads/main")

	body := []byte("0032want " + first + "\n00000009done\n")
	status, resp := postGit(t, h.srv.URL+"/repo.git/git-upload-pack", h.token, "application/x-git-upload-pack-request", body)
	if status != http.StatusOK || string(resp) != "004aERR upload-pack: not our ref "+first+"\n" {
		t.Fatalf("status %d body %q", status, resp)
	}
}
