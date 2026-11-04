package tokenbackup

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestRoundTripAndRefusals(t *testing.T) {
	kr := testKeyring(t)
	mem := blob.NewMem()
	repo, _ := store.ParseID("11111111-1111-4111-8111-111111111111")
	taken := time.Date(2026, 3, 4, 5, 6, 7, 890000000, time.UTC)
	created := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	row := store.TokenBackup{
		ID: mustID(t, "22222222-2222-4222-8222-222222222222"), Hash: hash,
		Label: "ci", Write: true, Admin: false, RepoID: &repo, CreatedAt: created,
	}
	var hash2 [32]byte
	hash2[0] = 9
	global := store.TokenBackup{
		ID: mustID(t, "33333333-3333-4333-8333-333333333333"), Hash: hash2,
		Label: "ops", Write: true, Admin: true, CreatedAt: created,
	}
	wrote, err := Write(context.Background(), mem, kr, [16]byte{}, taken, []store.TokenBackup{row, global})
	if err != nil {
		t.Fatal(err)
	}
	info, err := mem.Head(context.Background(), wrote.Key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(context.Background(), mem, kr, wrote.ID, info.Size)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TakenAt.Equal(taken) || len(got.Tokens) != 2 {
		t.Fatalf("%+v", got)
	}
	if got.Tokens[0].Label != "ci" || got.Tokens[0].RepoID == nil || *got.Tokens[0].RepoID != repo || !got.Tokens[0].Write || got.Tokens[0].Admin {
		t.Fatalf("limited %+v", got.Tokens[0])
	}
	if got.Tokens[1].RepoID != nil || !got.Tokens[1].Admin || got.Tokens[1].Hash != hash2 {
		t.Fatalf("global %+v", got.Tokens[1])
	}
	if !got.Tokens[0].CreatedAt.Equal(created) {
		t.Fatalf("created %s", got.Tokens[0].CreatedAt)
	}

	plain := sealedPlain(t, kr, wrote.ID, taken, []store.TokenBackup{row})
	for _, bad := range []struct {
		name string
		body string
	}{
		{"missing", strings.Replace(string(plain), `"label":"ci",`, "", 1)},
		{"unknown", strings.Replace(string(plain), `"label":"ci"`, `"label":"ci","extra":1`, 1)},
		{"trailing", string(plain) + `{"n":1}`},
		{"null repo missing", strings.Replace(string(plain), `"repo_id":"11111111-1111-4111-8111-111111111111"`, `"repo_id":null`, 1)},
	} {
		if bad.name == "null repo missing" {
			// null repo_id is valid. A missing repo_id is not, covered below.
			if _, _, err := unmarshal([]byte(bad.body)); err != nil {
				t.Fatalf("null repo_id: %v", err)
			}
			continue
		}
		if _, _, err := unmarshal([]byte(bad.body)); err == nil {
			t.Fatalf("%s was accepted", bad.name)
		}
	}
	missingRepo := strings.Replace(string(plain), `,"repo_id":"11111111-1111-4111-8111-111111111111"`, "", 1)
	if _, _, err := unmarshal([]byte(missingRepo)); err == nil {
		t.Fatal("missing repo_id was accepted")
	}
}

func TestBinding(t *testing.T) {
	kr := testKeyring(t)
	mem := blob.NewMem()
	id := mustID(t, "44444444-4444-4444-8444-444444444444")
	other := mustID(t, "55555555-5555-4555-8555-555555555555")
	taken := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	var hash [32]byte
	hash[0] = 1
	row := store.TokenBackup{
		ID: mustID(t, "66666666-6666-4666-8666-666666666666"), Hash: hash,
		Label: "a", Write: false, CreatedAt: taken,
	}
	wrote, err := Write(context.Background(), mem, kr, id, taken, []store.TokenBackup{row})
	if err != nil {
		t.Fatal(err)
	}
	raw := mem.Raw(wrote.Key)
	mem.SetRaw(Key(other), raw)
	info, err := mem.Head(context.Background(), Key(other))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(context.Background(), mem, Key(other), info.Size, kr, id); err != nil {
		t.Fatalf("own uuid: %v", err)
	}
	if _, err := Read(context.Background(), mem, kr, other, info.Size); err == nil {
		t.Fatal("other uuid decrypted")
	}

	snapID := mustID(t, "77777777-7777-4777-8777-777777777777")
	var buf bytes.Buffer
	w, err := objcrypt.NewWriter(&buf, kr, objcrypt.Binding{RepoID: id, ObjectID: snapID, Kind: objcrypt.KindSnap}, objcrypt.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`{"format":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	tokID := mustID(t, "88888888-8888-4888-8888-888888888888")
	if err := mem.Put(context.Background(), Key(tokID), bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), mem, kr, tokID, int64(buf.Len())); err == nil {
		t.Fatal("snapshot decrypted as a token backup")
	}
}

func TestNewest(t *testing.T) {
	kr := testKeyring(t)
	mem := blob.NewMem()
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tie := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	low := mustID(t, "00000000-0000-4000-8000-000000000001")
	high := mustID(t, "00000000-0000-4000-8000-000000000002")
	if _, err := Write(context.Background(), mem, kr, low, tie, []store.TokenBackup{sample(t, "a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(context.Background(), mem, kr, high, tie, []store.TokenBackup{sample(t, "b")}); err != nil {
		t.Fatal(err)
	}
	older := mustID(t, "00000000-0000-4000-8000-000000000003")
	if _, err := Write(context.Background(), mem, kr, older, early, []store.TokenBackup{sample(t, "c")}); err != nil {
		t.Fatal(err)
	}
	mem.SetRaw("tokens/not-a-key", []byte("no"))
	mem.SetRaw(Key(mustID(t, "99999999-9999-4999-8999-999999999999")), []byte("garbage"))
	best, unread, err := Newest(context.Background(), mem, kr)
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != high || !best.TakenAt.Equal(tie) {
		t.Fatalf("best %s %s", store.FormatID(best.ID), best.TakenAt)
	}
	if unread != 2 {
		t.Fatalf("unreadable %d", unread)
	}
	if _, _, err := Newest(context.Background(), blob.NewMem(), kr); err == nil {
		t.Fatal("empty bucket returned a backup")
	}
}

func sealedPlain(t *testing.T, kr *objcrypt.Keyring, id [16]byte, taken time.Time, rows []store.TokenBackup) []byte {
	t.Helper()
	plain, err := marshal(id, taken, rows)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func sample(t *testing.T, label string) store.TokenBackup {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	copy(hash[:], label)
	hash[31] = 1
	return store.TokenBackup{ID: id, Hash: hash, Label: label, Write: true, CreatedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func mustID(t *testing.T, s string) [16]byte {
	t.Helper()
	id, err := store.ParseID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testKeyring(t *testing.T) *objcrypt.Keyring {
	t.Helper()
	var key [32]byte
	copy(key[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}
