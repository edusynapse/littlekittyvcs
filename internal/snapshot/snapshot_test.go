package snapshot

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestRoundTrip(t *testing.T) {
	kr := testKeyring(t)
	mem := blob.NewMem()
	repoID := mustID(t)
	var oid [20]byte
	for i := range oid {
		oid[i] = byte(0xA0 + i)
	}
	want := Snapshot{
		RepoID:  repoID,
		Name:    "snapshotroundtrip",
		PushSeq: 7,
		Refs: []store.Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/heads/main", ObjectID: &oid},
		},
	}
	if _, err := Write(context.Background(), mem, kr, want); err != nil {
		t.Fatal(err)
	}
	info := oneSnap(t, mem, repoID)
	got, err := Read(context.Background(), mem, kr, repoID, snapID(t, info.Key), info.Size)
	if err != nil {
		t.Fatal(err)
	}
	if got.Deleted || got.RepoID != want.RepoID || got.Name != want.Name || got.PushSeq != want.PushSeq || len(got.Refs) != 2 {
		t.Fatalf("snapshot %+v", got.Name)
	}
	if got.Refs[0].Name != "HEAD" || got.Refs[0].Symbolic != "refs/heads/main" || got.Refs[0].ObjectID != nil {
		t.Fatal("HEAD")
	}
	if got.Refs[1].Name != "refs/heads/main" || got.Refs[1].Symbolic != "" || got.Refs[1].ObjectID == nil || *got.Refs[1].ObjectID != oid {
		t.Fatal("main")
	}
	raw := mem.Raw(info.Key)
	hexID := hex.EncodeToString(oid[:])
	for _, secret := range [][]byte{
		[]byte("refs/heads/main"),
		[]byte("HEAD"),
		[]byte(hexID),
		oid[:],
		[]byte(want.Name),
	} {
		if bytes.Contains(raw, secret) {
			t.Fatal("ciphertext contains plaintext")
		}
	}
}

func TestBinding(t *testing.T) {
	kr := testKeyring(t)
	mem := blob.NewMem()
	repoID := mustID(t)
	if _, err := Write(context.Background(), mem, kr, headSnap(repoID, "site", 1)); err != nil {
		t.Fatal(err)
	}
	info := oneSnap(t, mem, repoID)
	raw := mem.Raw(info.Key)
	otherRepo := mustID(t)
	otherSnap := mustID(t)
	mem.SetRaw(Key(otherRepo, snapID(t, info.Key)), append([]byte(nil), raw...))
	if _, err := Read(context.Background(), mem, kr, otherRepo, snapID(t, info.Key), info.Size); err == nil {
		t.Fatal("other repository accepted the snapshot")
	}
	mem.SetRaw(Key(repoID, otherSnap), append([]byte(nil), raw...))
	if _, err := Read(context.Background(), mem, kr, repoID, otherSnap, info.Size); err == nil {
		t.Fatal("other snapshot id accepted the snapshot")
	}
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)-1] ^= 0xff
	mem.SetRaw(info.Key, flipped)
	if _, err := Read(context.Background(), mem, kr, repoID, snapID(t, info.Key), int64(len(flipped))); err == nil {
		t.Fatal("flipped byte accepted")
	}
}

func TestFormatRefusals(t *testing.T) {
	kr := testKeyring(t)
	repoID := mustID(t)
	other := mustID(t)
	oid := strings.Repeat("ab", 20)
	valid := validJSON(repoID, oid)
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"valid", valid, true},
		{"unknown field", strings.Replace(valid, `"format":1`, `"format":1,"extra":true`, 1), false},
		{"trailing", valid + "\n{}", false},
		{"format", strings.Replace(valid, `"format":1`, `"format":2`, 1), false},
		{"repo id", strings.Replace(valid, store.FormatID(repoID), store.FormatID(other), 1), false},
		{"name", strings.Replace(valid, `"repository":"site"`, `"repository":"Site"`, 1), false},
		{"push seq zero", strings.Replace(valid, `"push_seq":1`, `"push_seq":0`, 1), false},
		{"push seq missing", strings.Replace(valid, `,"push_seq":1`, ``, 1), false},
		{"neither", strings.Replace(valid, `{"name":"refs/heads/main","object_id":"`+oid+`"}`, `{"name":"refs/heads/main"}`, 1), false},
		{"both", strings.Replace(valid, `{"name":"refs/heads/main","object_id":"`+oid+`"}`, `{"name":"refs/heads/main","symbolic":"refs/heads/other","object_id":"`+oid+`"}`, 1), false},
		{"uppercase object", strings.Replace(valid, oid, strings.ToUpper(oid), 1), false},
		{"short object", strings.Replace(valid, oid, "abcd", 1), false},
		{"no refs prefix", strings.Replace(valid, `{"name":"refs/heads/main","object_id":"`, `{"name":"heads/main","object_id":"`, 1), false},
		{"lock suffix", strings.Replace(valid, `{"name":"refs/heads/main","object_id":"`, `{"name":"refs/heads/a.lock","object_id":"`, 1), false},
		{"duplicate", strings.Replace(valid, `{"name":"refs/heads/main","object_id":"`+oid+`"}`, `{"name":"refs/heads/main","object_id":"`+oid+`"},{"name":"refs/heads/main","object_id":"`+oid+`"}`, 1), false},
		{"no head", strings.Replace(valid, `{"name":"HEAD","symbolic":"refs/heads/main"},`, ``, 1), false},
		{"direct head", strings.Replace(valid, `{"name":"HEAD","symbolic":"refs/heads/main"}`, `{"name":"HEAD","object_id":"`+oid+`"}`, 1), false},
		{"symbolic branch", strings.Replace(valid, `{"name":"refs/heads/main","object_id":"`+oid+`"}`, `{"name":"refs/heads/main","symbolic":"refs/heads/other"}`, 1), false},
		{"deleted missing", strings.Replace(valid, `,"deleted":false`, ``, 1), false},
		{"deleted null", strings.Replace(valid, `"deleted":false`, `"deleted":null`, 1), false},
		{"deleted with ref", strings.Replace(valid, `"deleted":false`, `"deleted":true`, 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := blob.NewMem()
			snap := mustID(t)
			putPlain(t, mem, kr, repoID, snap, []byte(tc.body))
			_, err := Read(context.Background(), mem, kr, repoID, snap, int64(len(mem.Raw(Key(repoID, snap)))))
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			msg := err.Error()
			for _, secret := range []string{"refs/", "HEAD", oid, strings.ToUpper(oid), "heads/main", "a.lock"} {
				if strings.Contains(msg, secret) {
					t.Fatalf("error echoes content")
				}
			}
		})
	}
}

func validJSON(repoID [16]byte, oid string) string {
	return fmt.Sprintf(`{"format":1,"repository":"site","repo_id":"%s","push_seq":1,"deleted":false,"refs":[{"name":"HEAD","symbolic":"refs/heads/main"},{"name":"refs/heads/main","object_id":"%s"}]}`, store.FormatID(repoID), oid)
}

func headSnap(repoID [16]byte, name string, seq int64) Snapshot {
	return Snapshot{
		RepoID: repoID, Name: name, PushSeq: seq,
		Refs: []store.Ref{{Name: "HEAD", Symbolic: "refs/heads/main"}},
	}
}

func putPlain(t *testing.T, mem *blob.Mem, kr *objcrypt.Keyring, repoID, snapID [16]byte, plain []byte) {
	t.Helper()
	var buf bytes.Buffer
	w, err := objcrypt.NewWriter(&buf, kr, objcrypt.Binding{
		RepoID: repoID, ObjectID: snapID, Kind: objcrypt.KindSnap,
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
	if err := mem.Put(context.Background(), Key(repoID, snapID), bytes.NewReader(buf.Bytes()), w.CiphertextSize()); err != nil {
		t.Fatal(err)
	}
}

func oneSnap(t *testing.T, mem *blob.Mem, repoID [16]byte) blob.Info {
	t.Helper()
	var infos []blob.Info
	err := mem.List(context.Background(), "repos/"+store.FormatID(repoID)+"/refs/", func(info blob.Info) error {
		infos = append(infos, info)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("snaps %d", len(infos))
	}
	return infos[0]
}

func snapID(t *testing.T, key string) [16]byte {
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

func TestMarkerRoundTrip(t *testing.T) {
	kr := testKeyring(t)
	mem := blob.NewMem()
	repoID := mustID(t)
	want := Snapshot{RepoID: repoID, Name: "snapshotroundtrip", PushSeq: 4, Deleted: true}
	key, err := Write(context.Background(), mem, kr, want)
	if err != nil {
		t.Fatal(err)
	}
	info := oneSnap(t, mem, repoID)
	if info.Key != key {
		t.Fatal("key")
	}
	got, err := Read(context.Background(), mem, kr, repoID, snapID(t, info.Key), info.Size)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deleted || got.Name != want.Name || got.PushSeq != want.PushSeq || len(got.Refs) != 0 {
		t.Fatal("marker")
	}
	if bytes.Contains(mem.Raw(info.Key), []byte(want.Name)) {
		t.Fatal("ciphertext contains the name")
	}

	marker := fmt.Sprintf(`{"format":1,"repository":"site","repo_id":"%s","push_seq":1,"deleted":true,"refs":[]}`, store.FormatID(repoID))
	snap := mustID(t)
	putPlain(t, mem, kr, repoID, snap, []byte(marker))
	got, err = Read(context.Background(), mem, kr, repoID, snap, int64(len(mem.Raw(Key(repoID, snap)))))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deleted || len(got.Refs) != 0 {
		t.Fatal("empty refs")
	}
}

func TestParseKey(t *testing.T) {
	repo := mustID(t)
	obj := mustID(t)
	pack := packstore.PackKey(repo, obj)
	idx := packstore.IdxKey(repo, obj)
	snap := Key(repo, obj)
	upper := "repos/" + strings.ToUpper(store.FormatID(repo)) + "/packs/" + strings.ToUpper(store.FormatID(obj)) + ".pack"
	extra := "repos/" + store.FormatID(repo) + "/packs/" + store.FormatID(obj) + "/x.pack"
	tmp := "repos/" + store.FormatID(repo) + "/packs/" + store.FormatID(obj) + ".tmp"
	cases := []struct {
		name    string
		key     string
		hasRepo bool
		ok      bool
		kind    KeyKind
	}{
		{"pack", pack, true, true, KeyPack},
		{"idx", idx, true, true, KeyIdx},
		{"snap", snap, true, true, KeySnap},
		{"upper", upper, true, false, 0},
		{"extra", extra, true, false, 0},
		{"tmp", tmp, true, false, 0},
		{"stray", "repos/not-a-uuid/x", false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hasRepo, ok := ParseKey(tc.key)
			if hasRepo != tc.hasRepo || ok != tc.ok || got.Kind != tc.kind {
				t.Fatalf("hasRepo %v ok %v kind %d", hasRepo, ok, got.Kind)
			}
			if ok && (got.RepoID != repo || got.ObjID != obj) {
				t.Fatal("ids")
			}
		})
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
