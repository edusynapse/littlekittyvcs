package packstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestPeel(t *testing.T) {
	st := memory.NewStorage()
	when := time.Unix(1_700_000_000, 0).UTC()
	commitObj := &plumbing.MemoryObject{}
	commitObj.SetType(plumbing.CommitObject)
	if err := (&object.Commit{
		Author:    object.Signature{Name: "A", Email: "a@b.c", When: when},
		Committer: object.Signature{Name: "A", Email: "a@b.c", When: when},
		Message:   "m\n",
	}).Encode(commitObj); err != nil {
		t.Fatal(err)
	}
	commitHash, err := st.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatal(err)
	}
	tagHash := putTag(t, st, "v1", commitHash, plumbing.CommitObject, when)
	tagTag := putTag(t, st, "v2", tagHash, plumbing.TagObject, when)
	missing := plumbing.NewHash("0123456789abcdef0123456789abcdef01234567")
	missingTag := putTag(t, st, "gone", missing, plumbing.CommitObject, when)

	var buf bytes.Buffer
	enc := packfile.NewEncoder(&buf, st, false)
	if _, err := enc.Encode([]plumbing.Hash{commitHash, tagHash, tagTag, missingTag}, 0); err != nil {
		t.Fatal(err)
	}
	objs := openStored(t, buf.Bytes())
	defer objs.Close()

	got, err := Peel(objs, [20]byte(commitHash))
	if err != nil || got != nil {
		t.Fatalf("commit peel %v err %v", got != nil, err)
	}
	got, err = Peel(objs, [20]byte(tagHash))
	if err != nil || got == nil || *got != [20]byte(commitHash) {
		t.Fatal("tag did not peel to its commit")
	}
	got, err = Peel(objs, [20]byte(tagTag))
	if err != nil || got == nil || *got != [20]byte(commitHash) {
		t.Fatal("tag of a tag did not peel to the commit")
	}
	if _, err := Peel(objs, [20]byte(missingTag)); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("missing target: %v", err)
	}
}

func putTag(t *testing.T, st *memory.Storage, name string, target plumbing.Hash, typ plumbing.ObjectType, when time.Time) plumbing.Hash {
	t.Helper()
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.TagObject)
	err := (&object.Tag{
		Name:       name,
		Tagger:     object.Signature{Name: "A", Email: "a@b.c", When: when},
		Message:    name + "\n",
		Target:     target,
		TargetType: typ,
	}).Encode(mo)
	if err != nil {
		t.Fatal(err)
	}
	h, err := st.SetEncodedObject(mo)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func openStored(t *testing.T, raw []byte) *Objects {
	t.Helper()
	var master [32]byte
	copy(master[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	var repoID, packID [16]byte
	repoID[0] = 1
	packID[0] = 2
	binding := objcrypt.Binding{RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindPack}
	sp, err := Spool(context.Background(), bytes.NewReader(raw), t.TempDir(), kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	parsed, pk, err := Parse(context.Background(), sp, kr, binding)
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
	err = mem.Put(context.Background(), PackKey(repoID, packID), f, sp.Size)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	row := store.Pack{
		ID: packID, PackKey: PackKey(repoID, packID), IdxKey: IdxKey(repoID, packID),
		Bytes: sp.Size, IdxBytes: 1,
	}
	return NewObjects(context.Background(), mem, kr, repoID, func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) {
		if _, ok := parsed.Objects[id]; !ok {
			return store.Pack{}, nil, false
		}
		return row, parsed.Index, true
	}, nil, nil)
}

type countingObject struct {
	cache.Object
	puts int
}

func (c *countingObject) Put(o plumbing.EncodedObject) {
	c.puts++
	c.Object.Put(o)
}

func TestOneObjectCachePerRequest(t *testing.T) {
	var master [32]byte
	copy(master[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	var repoID [16]byte
	repoID[0] = 7
	mem := blob.NewMem()
	rows := make([]store.Pack, 3)
	indexes := make([]*idxfile.MemoryIndex, 3)
	ids := make([][20]byte, 3)
	for i := 0; i < 3; i++ {
		var packID [16]byte
		packID[0] = byte(i + 1)
		raw := oneBlobPack(t, "blob-"+string(rune('a'+i)))
		rows[i], indexes[i], ids[i] = putStoredPack(t, mem, kr, repoID, packID, raw)
	}
	lru := cache.NewObjectLRU(cache.DefaultMaxSize)
	counter := &countingObject{Object: lru}
	objs := newObjects(context.Background(), mem, kr, repoID, func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) {
		for i, want := range ids {
			if want == id {
				return rows[i], indexes[i], true
			}
		}
		return store.Pack{}, nil, false
	}, nil, nil, counter)
	defer objs.Close()
	for _, id := range ids {
		if _, _, err := objs.Get(id); err != nil {
			t.Fatal(err)
		}
	}
	if counter.puts < 3 {
		t.Fatalf("puts %d, one cache should see every pack", counter.puts)
	}
	size := reflect.ValueOf(lru).Elem().FieldByName("actualSize").Int()
	if size <= 0 || size > int64(lru.MaxSize) {
		t.Fatalf("object cache size %d, limit %d", size, lru.MaxSize)
	}
	shared := NewObjects(context.Background(), mem, kr, repoID, nil, nil, nil)
	got, ok := shared.objCache.(*cache.ObjectLRU)
	if !ok || got.MaxSize != cache.DefaultMaxSize {
		t.Fatal("NewObjects did not build one default object cache")
	}
}

func oneBlobPack(t *testing.T, payload string) []byte {
	t.Helper()
	st := memory.NewStorage()
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.BlobObject)
	if _, err := mo.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	hash, err := st.SetEncodedObject(mo)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, st, false).Encode([]plumbing.Hash{hash}, 0); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func putStoredPack(t *testing.T, mem *blob.Mem, kr *objcrypt.Keyring, repoID, packID [16]byte, raw []byte) (store.Pack, *idxfile.MemoryIndex, [20]byte) {
	t.Helper()
	binding := objcrypt.Binding{RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindPack}
	sp, err := Spool(context.Background(), bytes.NewReader(raw), t.TempDir(), kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	parsed, pk, err := Parse(context.Background(), sp, kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := pk.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	err = mem.Put(context.Background(), PackKey(repoID, packID), f, sp.Size)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var id [20]byte
	for hash := range parsed.Objects {
		id = hash
		break
	}
	row := store.Pack{
		ID: packID, PackKey: PackKey(repoID, packID), IdxKey: IdxKey(repoID, packID),
		Bytes: sp.Size, IdxBytes: 1,
	}
	return row, parsed.Index, id
}
