package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestCommitPush(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repoID := insertRepo(t, st, "site")

	a := id20(1)
	b := id20(2)
	got, err := st.CommitPush(ctx, Push{
		RepoID: repoID,
		Pack:   packRow(t, repoID),
		Updates: []RefUpdate{{
			Name: "refs/heads/main", Old: nil, New: a,
		}},
	})
	if err != nil || got.PushSeq != 1 {
		t.Fatalf("create seq %d err %v", got.PushSeq, err)
	}
	if got := refObject(t, st, "site", "refs/heads/main"); *got != *a {
		t.Fatalf("created %x", got)
	}

	got, err = st.CommitPush(ctx, Push{
		RepoID: repoID,
		Pack:   packRow(t, repoID),
		Updates: []RefUpdate{{
			Name: "refs/heads/main", Old: a, New: b,
		}},
	})
	if err != nil || got.PushSeq != 2 {
		t.Fatalf("update seq %d err %v", got.PushSeq, err)
	}
	if got := refObject(t, st, "site", "refs/heads/main"); *got != *b {
		t.Fatalf("updated %x", got)
	}

	got, err = st.CommitPush(ctx, Push{
		RepoID: repoID,
		Updates: []RefUpdate{{
			Name: "refs/heads/main", Old: b, New: nil,
		}},
	})
	if err != nil || got.PushSeq != 3 {
		t.Fatalf("delete seq %d err %v", got.PushSeq, err)
	}
	if refObject(t, st, "site", "refs/heads/main") != nil {
		t.Fatal("ref still present")
	}
	if _, _, packs, err := st.ReadRepo(ctx, "site"); err != nil || len(packs) != 2 {
		t.Fatalf("packs %d err %v", len(packs), err)
	}
}

func TestCommitPushReturnsFullRefSet(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo, err := st.CreateRepo(ctx, mustID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	mainID := id20(1)
	devID := id20(2)
	if _, err := st.CommitPush(ctx, Push{
		RepoID:  repo.ID,
		Updates: []RefUpdate{{Name: "refs/heads/main", Old: nil, New: mainID}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.CommitPush(ctx, Push{
		RepoID:  repo.ID,
		Updates: []RefUpdate{{Name: "refs/heads/dev", Old: nil, New: devID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PushSeq != 2 {
		t.Fatalf("seq %d", got.PushSeq)
	}
	if len(got.Refs) != 3 {
		t.Fatalf("%d refs", len(got.Refs))
	}
	if got.Refs[0].Name != "HEAD" || got.Refs[0].Symbolic != "refs/heads/main" || got.Refs[0].ObjectID != nil {
		t.Fatalf("HEAD %+v", got.Refs[0])
	}
	if got.Refs[1].Name != "refs/heads/dev" || got.Refs[1].Symbolic != "" || got.Refs[1].ObjectID == nil || *got.Refs[1].ObjectID != *devID {
		t.Fatalf("dev %+v", got.Refs[1])
	}
	if got.Refs[2].Name != "refs/heads/main" || got.Refs[2].Symbolic != "" || got.Refs[2].ObjectID == nil || *got.Refs[2].ObjectID != *mainID {
		t.Fatalf("main %+v", got.Refs[2])
	}
}

func TestCommitPushStale(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repoID := insertRepo(t, st, "site")
	current := id20(1)
	if _, err := st.CommitPush(ctx, Push{
		RepoID:  repoID,
		Pack:    packRow(t, repoID),
		Updates: []RefUpdate{{Name: "refs/heads/main", Old: nil, New: current}},
	}); err != nil {
		t.Fatal(err)
	}
	before := packCount(t, st, repoID)
	stale := id20(9)
	next := id20(2)
	_, err := st.CommitPush(ctx, Push{
		RepoID: repoID,
		Pack:   packRow(t, repoID),
		Updates: []RefUpdate{{
			Name: "refs/heads/main", Old: stale, New: next,
		}},
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("%v", err)
	}
	if got := refObject(t, st, "site", "refs/heads/main"); *got != *current {
		t.Fatalf("moved to %x", got)
	}
	repo, _, packs, err := st.ReadRepo(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if repo.PushSeq != 1 {
		t.Fatalf("push_seq %d", repo.PushSeq)
	}
	if len(packs) != before {
		t.Fatalf("packs %d want %d", len(packs), before)
	}
}

func TestCommitPushCreateExisting(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repoID := insertRepo(t, st, "site")
	current := id20(1)
	if _, err := st.CommitPush(ctx, Push{
		RepoID:  repoID,
		Updates: []RefUpdate{{Name: "refs/heads/main", Old: nil, New: current}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := st.CommitPush(ctx, Push{
		RepoID:  repoID,
		Pack:    packRow(t, repoID),
		Updates: []RefUpdate{{Name: "refs/heads/main", Old: nil, New: id20(2)}},
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("%v", err)
	}
	if packCount(t, st, repoID) != 0 {
		t.Fatal("pack row inserted")
	}
	if got := refObject(t, st, "site", "refs/heads/main"); *got != *current {
		t.Fatalf("moved to %x", got)
	}
}

func TestCommitPushRace(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repoID := insertRepo(t, st, "site")
	old := id20(1)
	if _, err := st.CommitPush(ctx, Push{
		RepoID:  repoID,
		Updates: []RefUpdate{{Name: "refs/heads/main", Old: nil, New: old}},
	}); err != nil {
		t.Fatal(err)
	}
	news := [2]*[20]byte{id20(2), id20(3)}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = st.CommitPush(ctx, Push{
				RepoID: repoID,
				Pack:   packRow(t, repoID),
				Updates: []RefUpdate{{
					Name: "refs/heads/main", Old: old, New: news[i],
				}},
			})
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	var winner *[20]byte
	for i, err := range errs {
		if err == nil {
			wins++
			winner = news[i]
			continue
		}
		if !errors.Is(err, ErrStale) {
			t.Fatalf("loser %d: %v", i, err)
		}
	}
	if wins != 1 || winner == nil {
		t.Fatalf("wins %d", wins)
	}
	if got := refObject(t, st, "site", "refs/heads/main"); *got != *winner {
		t.Fatalf("ref %x winner %x", got, winner)
	}
	if packCount(t, st, repoID) != 1 {
		t.Fatalf("packs %d", packCount(t, st, repoID))
	}
}

func TestCommitPushCreateName(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repoID := mustID(t)
	tip := id20(4)
	if _, err := st.CommitPush(ctx, Push{
		RepoID:     repoID,
		CreateName: "created",
		Updates:    []RefUpdate{{Name: "refs/heads/main", Old: nil, New: tip}},
	}); err != nil {
		t.Fatal(err)
	}
	repo, refs, _, err := st.ReadRepo(ctx, "created")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != repoID || repo.PushSeq != 1 {
		t.Fatalf("%+v", repo)
	}
	head := symbolic(refs, "HEAD")
	if head != "refs/heads/main" {
		t.Fatalf("HEAD %q", head)
	}
	_, err = st.CommitPush(ctx, Push{
		RepoID:     mustID(t),
		CreateName: "created",
		Updates:    []RefUpdate{{Name: "refs/heads/main", Old: nil, New: id20(5)}},
	})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("%v", err)
	}
}

func TestCommitPushHeadFix(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}

	masterRepo := mustID(t)
	if _, err := st.CommitPush(ctx, Push{
		RepoID:     masterRepo,
		CreateName: "masteronly",
		Updates:    []RefUpdate{{Name: "refs/heads/master", Old: nil, New: id20(6)}},
	}); err != nil {
		t.Fatal(err)
	}
	_, refs, _, err := st.ReadRepo(ctx, "masteronly")
	if err != nil {
		t.Fatal(err)
	}
	if head := symbolic(refs, "HEAD"); head != "refs/heads/master" {
		t.Fatalf("HEAD %q", head)
	}

	mainRepo := insertRepo(t, st, "withmain")
	if _, err := st.pool.Exec(ctx, `
		insert into refs (repo_id, name, symbolic) values ($1, 'HEAD', 'refs/heads/main')`, mainRepo); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitPush(ctx, Push{
		RepoID:  mainRepo,
		Updates: []RefUpdate{{Name: "refs/heads/main", Old: nil, New: id20(7)}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitPush(ctx, Push{
		RepoID:  mainRepo,
		Updates: []RefUpdate{{Name: "refs/heads/dev", Old: nil, New: id20(8)}},
	}); err != nil {
		t.Fatal(err)
	}
	_, refs, _, err = st.ReadRepo(ctx, "withmain")
	if err != nil {
		t.Fatal(err)
	}
	if head := symbolic(refs, "HEAD"); head != "refs/heads/main" {
		t.Fatalf("HEAD %q", head)
	}
}

func TestCommitPushPeeled(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repoID := insertRepo(t, st, "site")
	tag := id20(4)
	commit := id20(5)
	peeled := id20(6)
	if _, err := st.CommitPush(ctx, Push{
		RepoID: repoID,
		Updates: []RefUpdate{{
			Name: "refs/tags/v1", Old: nil, New: tag, Peeled: peeled,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	got := refByName(t, st, "site", "refs/tags/v1")
	if got.Peeled == nil || *got.Peeled != *peeled || got.ObjectID == nil || *got.ObjectID != *tag {
		t.Fatal("peeled was not stored")
	}
	if _, err := st.CommitPush(ctx, Push{
		RepoID: repoID,
		Updates: []RefUpdate{{
			Name: "refs/tags/v1", Old: tag, New: nil, Peeled: peeled,
		}},
	}); err == nil {
		t.Fatal("peeled on a delete was accepted")
	}
	if _, err := st.CommitPush(ctx, Push{
		RepoID: repoID,
		Updates: []RefUpdate{{
			Name: "refs/tags/v1", Old: tag, New: commit, Peeled: nil,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	got = refByName(t, st, "site", "refs/tags/v1")
	if got.ObjectID == nil || *got.ObjectID != *commit || got.Peeled != nil {
		t.Fatal("peeled was not cleared")
	}
}

func refByName(t *testing.T, st *Store, repo, name string) Ref {
	t.Helper()
	_, refs, _, err := st.ReadRepo(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.Name == name {
			return ref
		}
	}
	t.Fatalf("ref missing")
	return Ref{}
}

func packRow(t *testing.T, repoID [16]byte) *NewPack {
	t.Helper()
	id := mustID(t)
	key := FormatID(repoID) + "/" + FormatID(id)
	return &NewPack{
		ID:       id,
		PackKey:  "repos/" + key + ".pack",
		IdxKey:   "repos/" + key + ".idx",
		Bytes:    1,
		IdxBytes: 1,
	}
}

func id20(b byte) *[20]byte {
	var id [20]byte
	for i := range id {
		id[i] = b
	}
	return &id
}

func refObject(t *testing.T, st *Store, name, ref string) *[20]byte {
	t.Helper()
	_, refs, _, err := st.ReadRepo(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == ref {
			return r.ObjectID
		}
	}
	return nil
}

func symbolic(refs []Ref, name string) string {
	for _, r := range refs {
		if r.Name == name {
			return r.Symbolic
		}
	}
	return ""
}

func packCount(t *testing.T, st *Store, repoID [16]byte) int {
	t.Helper()
	var n int
	err := st.pool.QueryRow(context.Background(), `select count(*) from packs where repo_id = $1`, repoID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
