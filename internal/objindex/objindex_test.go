package objindex

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestRefreshAndAdd(t *testing.T) {
	ctx := context.Background()
	kr := testKeyring(t)
	mem := blob.NewMem()
	var repoID [16]byte
	repoID[0] = 7

	type built struct {
		row store.Pack
		idx *idxfile.MemoryIndex
		id  [20]byte
	}
	var packs []built
	for i, body := range []string{"one", "two", "three"} {
		var packID [16]byte
		packID[0] = byte(i + 1)
		raw, id := encodeBlob(t, []byte(body))
		sp, parsed := spoolPack(t, kr, repoID, packID, raw)
		packs = append(packs, built{
			row: putPack(t, ctx, mem, kr, repoID, packID, sp, parsed),
			idx: parsed.Index,
			id:  id,
		})
	}
	rows := []store.Pack{packs[0].row, packs[1].row, packs[2].row}

	idx := New()
	if err := idx.Refresh(ctx, mem, kr, repoID, rows[:2]); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := idx.Lookup(repoID, packs[0].id); !ok {
		t.Fatal("first id missing after refresh")
	}
	if _, _, ok := idx.Lookup(repoID, packs[1].id); !ok {
		t.Fatal("second id missing after refresh")
	}
	if _, _, ok := idx.Lookup(repoID, packs[2].id); ok {
		t.Fatal("third id visible before add")
	}
	var other [16]byte
	other[0] = 9
	if _, _, ok := idx.Lookup(other, packs[0].id); ok {
		t.Fatal("unknown repository returned an object")
	}
	idx.mu.Lock()
	_, created := idx.repos[other]
	idx.mu.Unlock()
	if created {
		t.Fatal("lookup created an entry for an unknown repository")
	}

	mem.ResetCalls()
	idx.Add(repoID, packs[2].row, packs[2].idx)
	if _, _, ok := idx.Lookup(repoID, packs[2].id); !ok {
		t.Fatal("third id missing after add")
	}
	if err := idx.Refresh(ctx, mem, kr, repoID, rows); err != nil {
		t.Fatal(err)
	}
	if calls := mem.RangeCalls(); len(calls) != 0 {
		t.Fatalf("second refresh read %d ranges", len(calls))
	}

	fresh := New()
	mem.ResetCalls()
	if err := fresh.Refresh(ctx, mem, kr, repoID, rows); err != nil {
		t.Fatal(err)
	}
	if calls := mem.RangeCalls(); len(calls) != 3 {
		t.Fatalf("fresh refresh reads %d", len(calls))
	}
	for i, p := range packs {
		got, _, ok := fresh.Lookup(repoID, p.id)
		if !ok || got.ID != p.row.ID {
			t.Fatalf("id %d pack %x ok %v", i, got.ID, ok)
		}
	}
}

func encodeBlob(t *testing.T, content []byte) ([]byte, [20]byte) {
	t.Helper()
	st := memory.NewStorage()
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.BlobObject)
	if _, err := obj.Write(content); err != nil {
		t.Fatal(err)
	}
	h, err := st.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := packfile.NewEncoder(&buf, st, false)
	if _, err := enc.Encode([]plumbing.Hash{h}, 0); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), [20]byte(h)
}

func spoolPack(t *testing.T, kr *objcrypt.Keyring, repoID, packID [16]byte, raw []byte) (*packstore.Spooled, *packstore.Parsed) {
	t.Helper()
	b := objcrypt.Binding{RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindPack}
	sp, err := packstore.Spool(context.Background(), bytes.NewReader(raw), t.TempDir(), kr, b)
	if err != nil {
		t.Fatal(err)
	}
	parsed, pk, err := packstore.Parse(context.Background(), sp, kr, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := pk.Close(); err != nil {
		t.Fatal(err)
	}
	return sp, parsed
}

func putPack(t *testing.T, ctx context.Context, mem *blob.Mem, kr *objcrypt.Keyring, repoID, packID [16]byte, sp *packstore.Spooled, parsed *packstore.Parsed) store.Pack {
	t.Helper()
	packKey := packstore.PackKey(repoID, packID)
	idxKey := packstore.IdxKey(repoID, packID)
	f, err := os.Open(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	err = mem.Put(ctx, packKey, f, sp.Size)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := objcrypt.NewWriter(&buf, kr, objcrypt.Binding{
		RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindIdx,
	}, objcrypt.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(parsed.IdxBytes); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	idxBytes := w.CiphertextSize()
	if err := mem.Put(ctx, idxKey, bytes.NewReader(buf.Bytes()), idxBytes); err != nil {
		t.Fatal(err)
	}
	return store.Pack{
		ID: packID, PackKey: packKey, IdxKey: idxKey, Bytes: sp.Size, IdxBytes: idxBytes,
	}
}

func TestDrop(t *testing.T) {
	x := New()
	var repo [16]byte
	repo[0] = 1
	ri := x.repo(repo)
	ri.mu.Lock()
	ri.loaded[repo] = struct{}{}
	ri.mu.Unlock()
	x.Drop(repo)
	x.mu.Lock()
	_, ok := x.repos[repo]
	x.mu.Unlock()
	if ok {
		t.Fatal("entry remains")
	}
	again := x.repo(repo)
	again.mu.Lock()
	n := len(again.loaded)
	again.mu.Unlock()
	if n != 0 {
		t.Fatal("next refresh would not start empty")
	}
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
