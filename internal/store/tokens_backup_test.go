package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

func TestReadTokensForBackup(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	first := insertBackupToken(t, st, "one", nil)
	taken, rows, err := st.ReadTokensForBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != first.ID || rows[0].Hash != first.Hash || rows[0].Label != "one" {
		t.Fatalf("%+v", rows)
	}
	if taken.IsZero() || taken.Location() == nil {
		t.Fatalf("taken_at %v", taken)
	}
	second := insertBackupToken(t, st, "two", nil)
	_, again, err := st.ReadTokensForBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Fatalf("again %d", len(again))
	}
	for _, row := range rows {
		if row.ID == second.ID {
			t.Fatal("token inserted after the read is in it")
		}
	}
	if rows[0].CreatedAt.After(taken) {
		t.Fatalf("created_at %s after taken_at %s", rows[0].CreatedAt, taken)
	}
}

func TestRestoreTokens(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	present := insertBackupToken(t, st, "keep", nil)
	missingRepo, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	freshID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	var freshHash [32]byte
	freshHash[0] = 7
	created := time.Date(2019, 8, 7, 6, 5, 4, 0, time.UTC)
	counts, err := st.RestoreTokens(ctx, []TokenBackup{
		present,
		{ID: freshID, Hash: freshHash, Label: "new", Write: true, RepoID: &repo.ID, CreatedAt: created},
		{ID: mustID(t), Hash: [32]byte{9}, Label: "gone", Write: false, RepoID: &missingRepo, CreatedAt: created},
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Restored != 1 || counts.Present != 1 || counts.NoRepository != 1 {
		t.Fatalf("%+v", counts)
	}
	row, err := st.TokenByHash(ctx, freshHash)
	if err != nil || row.Label != "new" || row.RepoID == nil || *row.RepoID != repo.ID {
		t.Fatalf("%+v %v", row, err)
	}
	kept, err := st.TokenByHash(ctx, present.Hash)
	if err != nil || kept.Label != "keep" {
		t.Fatalf("present changed %+v %v", kept, err)
	}
	listed, err := st.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, item := range listed {
		if item.ID == freshID {
			saw = true
			if !item.CreatedAt.Equal(created) {
				t.Fatalf("created_at %s", item.CreatedAt)
			}
		}
	}
	if !saw {
		t.Fatal("inserted token missing")
	}
	plain, hash, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	_ = plain
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.RestoreTokens(ctx, []TokenBackup{{
		ID: id, Hash: hash, Label: "auth", Write: true, CreatedAt: when,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TokenByHash(ctx, auth.Hash(plain)); err != nil {
		t.Fatal(err)
	}
}

func insertBackupToken(t *testing.T, st *Store, label string, repo *[16]byte) TokenBackup {
	t.Helper()
	plain, hash, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	_ = plain
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	row := TokenRow{ID: id, Label: label, Write: true, RepoID: repo}
	if err := st.InsertToken(context.Background(), hash, row); err != nil {
		t.Fatal(err)
	}
	_, rows, err := st.ReadTokensForBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range rows {
		if got.ID == id {
			return got
		}
	}
	t.Fatal("inserted token missing from backup read")
	return TokenBackup{}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return st
}
