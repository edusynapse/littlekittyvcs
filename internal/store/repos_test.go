package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidRepoName(t *testing.T) {
	for _, name := range []string{"a", "site", "my-repo.v2", "x_y", strings.Repeat("a", 101)} {
		if !ValidRepoName(name) {
			t.Fatalf("rejected %q", name)
		}
	}
	for _, name := range []string{"", "Site", "-x", ".x", "a..b", "a/b", "site.git", strings.Repeat("a", 102)} {
		if ValidRepoName(name) {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestCreateAndReadRepo(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	repo, err := st.CreateRepo(ctx, id, "site")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != id || repo.Name != "site" || repo.PushSeq != 0 {
		t.Fatalf("%+v", repo)
	}
	got, refs, packs, err := st.ReadRepo(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if got != repo {
		t.Fatalf("read %+v created %+v", got, repo)
	}
	if len(refs) != 1 || refs[0].Name != "HEAD" || refs[0].ObjectID != nil || refs[0].Symbolic != "refs/heads/main" {
		t.Fatalf("%+v", refs)
	}
	if len(packs) != 0 {
		t.Fatalf("%d packs", len(packs))
	}
	if _, err := st.CreateRepo(ctx, mustID(t), "site"); !errors.Is(err, ErrExists) {
		t.Fatalf("taken name: %v", err)
	}
	if _, _, _, err := st.ReadRepo(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestReadRepoOrdersRefs(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	insert := []struct {
		name string
		b    byte
	}{
		{"refs/tags/v1", 0x33},
		{"refs/heads/main", 0x11},
		{"refs/heads/dev", 0x22},
	}
	for _, ref := range insert {
		id := bytes.Repeat([]byte{ref.b}, 20)
		if _, err := st.pool.Exec(ctx, `insert into refs (repo_id, name, object_id) values ($1, $2, $3)`, repo.ID, ref.name, id); err != nil {
			t.Fatal(err)
		}
	}
	_, refs, packs, err := st.ReadRepo(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 0 {
		t.Fatalf("%d packs", len(packs))
	}
	want := []string{"HEAD", "refs/heads/dev", "refs/heads/main", "refs/tags/v1"}
	if len(refs) != len(want) {
		t.Fatalf("%d refs", len(refs))
	}
	for i, name := range want {
		if refs[i].Name != name {
			t.Fatalf("ref %d %s", i, refs[i].Name)
		}
		if name == "HEAD" {
			if refs[i].ObjectID != nil || refs[i].Symbolic != "refs/heads/main" {
				t.Fatalf("HEAD %+v", refs[i])
			}
			continue
		}
		if refs[i].ObjectID == nil || refs[i].Symbolic != "" {
			t.Fatalf("%s %+v", name, refs[i])
		}
	}
}

func TestListAndDeleteTokens(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	type made struct {
		label string
		write bool
		admin bool
		repo  string
		hash  [32]byte
	}
	rows := []made{
		{label: "admin", write: true, admin: true},
		{label: "ro", write: false, repo: "site"},
		{label: "rw", write: true},
	}
	var hashes [][32]byte
	for i, row := range rows {
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		var repoID *[16]byte
		if row.repo != "" {
			repoID = &repo.ID
		}
		hash := bytes32()
		hash[0] = byte(i + 1)
		hashes = append(hashes, hash)
		err := st.InsertToken(ctx, hash, TokenRow{
			ID: mustID(t), Label: row.label, Write: row.write, Admin: row.admin, RepoID: repoID,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	list, err := st.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Label != "admin" || list[1].Label != "ro" || list[2].Label != "rw" {
		t.Fatalf("%+v", list)
	}
	if list[0].RepoName != "" || !list[0].Admin || !list[0].Write {
		t.Fatalf("admin %+v", list[0])
	}
	if list[1].RepoName != "site" || list[1].Write || list[1].Admin {
		t.Fatalf("ro %+v", list[1])
	}
	if list[2].RepoName != "" || !list[2].Write || list[2].Admin {
		t.Fatalf("rw %+v", list[2])
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, hash := range hashes {
		encodings := []string{
			hex.EncodeToString(hash[:]),
			base64.StdEncoding.EncodeToString(hash[:]),
			base64.RawStdEncoding.EncodeToString(hash[:]),
			base64.URLEncoding.EncodeToString(hash[:]),
			base64.RawURLEncoding.EncodeToString(hash[:]),
		}
		for _, enc := range encodings {
			if strings.Contains(text, enc) {
				t.Fatalf("listing contains a hash encoding")
			}
		}
	}
	if err := st.DeleteToken(ctx, list[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TokenByHash(ctx, hashes[1]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := st.DeleteToken(ctx, list[1].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestRequestErrorsHideConnectionDetails(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://sentinel_user:pw@127.0.0.1:1/sentinel_db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := &Store{pool: pool}
	t.Cleanup(st.Close)
	_, err = st.RepoByName(context.Background(), "site")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, secret := range []string{"sentinel_user", "sentinel_db", "127.0.0.1", "pw"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error contains %s", secret)
		}
	}
	if msg != "store: database request failed" && !strings.HasPrefix(msg, "store: database error (SQLSTATE ") {
		t.Fatalf("%s", msg)
	}
}

func TestFormatID(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := mustID(t)
		text := FormatID(id)
		parsed, err := ParseID(text)
		if err != nil || parsed != id {
			t.Fatalf("%s %v", text, err)
		}
		upper, err := ParseID(strings.ToUpper(text))
		if err != nil || upper != id {
			t.Fatalf("upper %s %v", text, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 35), strings.Repeat("a", 37), strings.Repeat("a", 32), "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"} {
		if _, err := ParseID(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	id := mustID(t)
	text := FormatID(id)
	missingDash := text[:8] + "a" + text[9:]
	if _, err := ParseID(missingDash); err == nil {
		t.Fatal("missing dash accepted")
	}
}

func TestRestoreRepo(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	if err := st.RestoreRepo(ctx, Restore{ID: id, Name: "Bad", PushSeq: 4}); err == nil {
		t.Fatal("invalid name accepted")
	}
	main := id20(3)
	both := Restore{
		ID: mustID(t), Name: "other", PushSeq: 1,
		Refs: []Ref{{Name: "refs/heads/main", ObjectID: main, Symbolic: "refs/heads/other"}},
	}
	if err := st.RestoreRepo(ctx, both); err == nil {
		t.Fatal("both targets accepted")
	}
	neither := Restore{
		ID: mustID(t), Name: "other", PushSeq: 1,
		Refs: []Ref{{Name: "HEAD"}},
	}
	if err := st.RestoreRepo(ctx, neither); err == nil {
		t.Fatal("neither target accepted")
	}
	if _, err := st.RepoByName(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected restore inserted a row: %v", err)
	}
	pack := packRow(t, id)
	err := st.RestoreRepo(ctx, Restore{
		ID: id, Name: "site", PushSeq: 7,
		Refs: []Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/heads/main", ObjectID: main},
		},
		Packs: []RestoredPack{{
			ID: pack.ID, PackKey: pack.PackKey, IdxKey: pack.IdxKey, Bytes: pack.Bytes, IdxBytes: pack.IdxBytes,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, refs, packs, err := st.ReadRepo(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != id || repo.Name != "site" || repo.PushSeq != 7 {
		t.Fatalf("%+v", repo)
	}
	if len(refs) != 2 || refs[0].Name != "HEAD" || refs[0].Symbolic != "refs/heads/main" || refs[0].ObjectID != nil {
		t.Fatalf("HEAD %+v", refs)
	}
	if refs[1].Name != "refs/heads/main" || refs[1].ObjectID == nil || *refs[1].ObjectID != *main {
		t.Fatalf("main %+v", refs[1])
	}
	if len(packs) != 1 || packs[0].ID != pack.ID || packs[0].PackKey != pack.PackKey || packs[0].Bytes != pack.Bytes || packs[0].IdxBytes != pack.IdxBytes {
		t.Fatalf("%+v", packs)
	}
	if err := st.RestoreRepo(ctx, Restore{ID: id, Name: "elsewhere", PushSeq: 1}); !errors.Is(err, ErrExists) {
		t.Fatalf("same id: %v", err)
	}
	if err := st.RestoreRepo(ctx, Restore{ID: mustID(t), Name: "site", PushSeq: 1}); !errors.Is(err, ErrExists) {
		t.Fatalf("same name: %v", err)
	}
	var repos, packRows int
	if err := st.pool.QueryRow(ctx, `select count(*) from repositories`).Scan(&repos); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `select count(*) from packs`).Scan(&packRows); err != nil {
		t.Fatal(err)
	}
	if repos != 1 || packRows != 1 {
		t.Fatalf("repos %d packs %d", repos, packRows)
	}
	if _, err := st.RepoByName(ctx, "elsewhere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("elsewhere: %v", err)
	}
}

func TestRepoByID(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	created, err := st.CreateRepo(ctx, id, "site")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.RepoByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != created {
		t.Fatalf("got %+v created %+v", got, created)
	}
	if _, err := st.RepoByID(ctx, mustID(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestBumpPushSeq(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.BumpPushSeq(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.BumpPushSeq(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 2 {
		t.Fatalf("seq %d then %d", first, second)
	}
	if _, err := st.BumpPushSeq(ctx, mustID(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestDeleteRepoCascade(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateRepo(ctx, mustID(t), "other")
	if err != nil {
		t.Fatal(err)
	}
	packID := mustID(t)
	_, err = st.pool.Exec(ctx, `
		insert into packs (id, repo_id, pack_key, idx_key, bytes, idx_bytes)
		values ($1, $2, $3, $4, 10, 11)`,
		packID, repo.ID,
		"repos/"+FormatID(repo.ID)+"/packs/"+FormatID(packID)+".pack",
		"repos/"+FormatID(repo.ID)+"/packs/"+FormatID(packID)+".idx")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.pool.Exec(ctx, `
		insert into refs (repo_id, name, object_id) values ($1, 'refs/heads/main', $2)`,
		repo.ID, bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}
	limited := bytes32()
	limited[0] = 1
	admin := bytes32()
	admin[0] = 2
	otherHash := bytes32()
	otherHash[0] = 3
	repoID := repo.ID
	otherID := other.ID
	for _, row := range []struct {
		hash  [32]byte
		label string
		admin bool
		repo  *[16]byte
	}{
		{limited, "limited", false, &repoID},
		{admin, "admin", true, nil},
		{otherHash, "other", false, &otherID},
	} {
		err := st.InsertToken(ctx, row.hash, TokenRow{
			ID: mustID(t), Label: row.label, Write: true, Admin: row.admin, RepoID: row.repo,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	seq, err := st.BumpPushSeq(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRepo(ctx, repo.ID, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepoByID(ctx, repo.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row: %v", err)
	}
	for _, table := range []string{"refs", "packs", "tokens"} {
		var n int
		err := st.pool.QueryRow(ctx, `select count(*) from `+table+` where repo_id = $1`, repo.ID).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s %d", table, n)
		}
	}
	if _, err := st.TokenByHash(ctx, limited); !errors.Is(err, ErrNotFound) {
		t.Fatalf("limited token: %v", err)
	}
	if _, err := st.TokenByHash(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TokenByHash(ctx, otherHash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepoByName(ctx, "other"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRepoStale(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	bumped, err := st.BumpPushSeq(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	var oid [20]byte
	oid[0] = 0xab
	if _, err := st.CommitPush(ctx, Push{
		RepoID:  repo.ID,
		Updates: []RefUpdate{{Name: "refs/heads/main", New: &oid}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRepo(ctx, repo.ID, bumped); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	got, err := st.RepoByID(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PushSeq != bumped+1 {
		t.Fatalf("push_seq %d", got.PushSeq)
	}
	if err := st.DeleteRepo(ctx, repo.ID, got.PushSeq); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRepo(ctx, repo.ID, got.PushSeq); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if err := st.DeleteRepo(ctx, mustID(t), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestPackIDs(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateRepo(ctx, mustID(t), "other")
	if err != nil {
		t.Fatal(err)
	}
	insertPack := func(repoID [16]byte) [16]byte {
		t.Helper()
		id := mustID(t)
		_, err := st.pool.Exec(ctx, `
			insert into packs (id, repo_id, pack_key, idx_key, bytes, idx_bytes)
			values ($1, $2, $3, $4, 4, 5)`,
			id, repoID,
			"repos/"+FormatID(repoID)+"/packs/"+FormatID(id)+".pack",
			"repos/"+FormatID(repoID)+"/packs/"+FormatID(id)+".idx")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := insertPack(repo.ID)
	b := insertPack(repo.ID)
	insertPack(other.ID)
	got, err := st.PackIDs(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d", len(got))
	}
	if _, ok := got[a]; !ok {
		t.Fatal("missing a")
	}
	if _, ok := got[b]; !ok {
		t.Fatal("missing b")
	}
	none, err := st.PackIDs(ctx, mustID(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("%d", len(none))
	}
}

func TestRestoreRepoPeeled(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	tag := id20(4)
	peeled := id20(6)
	err := st.RestoreRepo(ctx, Restore{
		ID: id, Name: "site", PushSeq: 3,
		Refs: []Ref{
			{Name: "HEAD", Symbolic: "refs/heads/main"},
			{Name: "refs/tags/v1", ObjectID: tag, Peeled: peeled},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, refs, _, err := st.ReadRepo(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ref := range refs {
		if ref.Name != "refs/tags/v1" {
			continue
		}
		found = true
		if ref.ObjectID == nil || *ref.ObjectID != *tag || ref.Peeled == nil || *ref.Peeled != *peeled {
			t.Fatal("peeled was not restored")
		}
	}
	if !found {
		t.Fatal("tag ref missing")
	}
	if err := st.RestoreRepo(ctx, Restore{
		ID: mustID(t), Name: "other", PushSeq: 1,
		Refs: []Ref{{Name: "HEAD", Symbolic: "refs/heads/main", Peeled: peeled}},
	}); err == nil {
		t.Fatal("peeled on a symbolic ref was accepted")
	}
}
