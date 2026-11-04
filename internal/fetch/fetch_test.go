package fetch

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestParseHaves(t *testing.T) {
	a := idHex(1)
	b := idHex(2)
	c := idHex(3)
	body := pktLine("want "+a+" multi_ack_detailed side-band-64k\n") + "0000" +
		pktLine("have "+b+"\n") +
		pktLine("have "+a+"\n") +
		pktLine("have "+b+"\n") +
		pktLine("have "+c+"\n") +
		"0000"
	wants, haves, done, side, multi, err := parseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if done || !side || !multi || len(wants) != 1 {
		t.Fatalf("done %v side %v multi %v wants %d", done, side, multi, len(wants))
	}
	if got := hexes(haves); got != b+" "+a+" "+c {
		t.Fatalf("haves %s", got)
	}

	second := pktLine("want "+a+" side-band-64k\n") +
		pktLine("want "+b+" multi_ack_detailed\n") + "0000" +
		pktLine("have "+a+"\n") + "0000"
	_, haves, _, side, multi, err = parseRequest([]byte(second))
	if err != nil {
		t.Fatal(err)
	}
	if !side || multi {
		t.Fatalf("side %v multi %v", side, multi)
	}
	if got := hexes(haves); got != a {
		t.Fatalf("haves %s", got)
	}
}

func TestRoundAnswers(t *testing.T) {
	a, b, idx, repoID := twoBlobs(t)
	unknown := idOf(9)
	wantID := idOf(8)
	deps, target := roundDeps(idx, repoID, wantID)
	body := roundBody(wantID, true, a, unknown, b)
	rec := postUpload(deps, target, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	want := ack(a, "common") + ack(b, "common") + pktLine("NAK\n")
	if rec.Body.String() != want {
		t.Fatalf("got %q\nwant %q", rec.Body.String(), want)
	}

	rec = postUpload(deps, target, roundBody(wantID, true, unknown))
	if rec.Body.String() != pktLine("NAK\n") {
		t.Fatalf("unknown round %q", rec.Body.String())
	}
	rec = postUpload(deps, target, roundBody(wantID, false, a, b))
	if rec.Body.String() != pktLine("NAK\n") {
		t.Fatalf("no capability %q", rec.Body.String())
	}
}

func roundDeps(idx *objindex.Index, repoID [16]byte, want [20]byte) (Deps, Target) {
	return Deps{Bucket: blob.NewMem(), Index: idx}, Target{
		Repo: store.Repo{ID: repoID, Name: "site"},
		Refs: []store.Ref{{Name: "refs/heads/main", ObjectID: &want}},
	}
}

func roundBody(want [20]byte, multi bool, haves ...[20]byte) string {
	caps := "side-band-64k"
	if multi {
		caps = "multi_ack_detailed side-band-64k"
	}
	s := pktLine("want "+hex.EncodeToString(want[:])+" "+caps+"\n") + "0000"
	for _, id := range haves {
		s += pktLine("have " + hex.EncodeToString(id[:]) + "\n")
	}
	return s + "0000"
}

func postUpload(deps Deps, target Target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/site.git/git-upload-pack", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	rec := httptest.NewRecorder()
	Handle(deps, rec, req, target)
	return rec
}

func twoBlobs(t *testing.T) (a, b [20]byte, idx *objindex.Index, repoID [16]byte) {
	t.Helper()
	st := memory.NewStorage()
	when := time.Unix(1_700_000_000, 0).UTC()
	_ = when
	ah := putBlob(t, st, "a")
	bh := putBlob(t, st, "b")
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, st, false).Encode([]plumbing.Hash{ah, bh}, 0); err != nil {
		t.Fatal(err)
	}
	var master [32]byte
	copy(master[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	repoID[0] = 3
	var packID [16]byte
	packID[0] = 4
	binding := objcrypt.Binding{RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindPack}
	sp, err := packstore.Spool(context.Background(), bytes.NewReader(buf.Bytes()), t.TempDir(), kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	parsed, pk, err := packstore.Parse(context.Background(), sp, kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = pk.Close()
	idx = objindex.New()
	idx.Add(repoID, store.Pack{ID: packID}, parsed.Index)
	return [20]byte(ah), [20]byte(bh), idx, repoID
}

func putBlob(t *testing.T, st *memory.Storage, content string) plumbing.Hash {
	t.Helper()
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.BlobObject)
	if _, err := mo.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	h, err := st.SetEncodedObject(mo)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func ack(id [20]byte, suffix string) string {
	s := "ACK " + hex.EncodeToString(id[:]) + " " + suffix + "\n"
	return pktLine(s)
}

func idOf(b byte) [20]byte {
	var id [20]byte
	for i := range id {
		id[i] = b
	}
	return id
}

func idHex(b byte) string {
	id := idOf(b)
	return hex.EncodeToString(id[:])
}

func hexes(ids [][20]byte) string {
	var b bytes.Buffer
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(hex.EncodeToString(id[:]))
	}
	return b.String()
}
