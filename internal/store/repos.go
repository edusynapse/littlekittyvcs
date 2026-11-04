package store

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var repoNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,100}$`)

// Repo is one repository row.
type Repo struct {
	ID      [16]byte
	Name    string
	PushSeq int64
}

// Ref is one ref row. ObjectID is nil for a symbolic ref. Symbolic is empty for a direct ref.
// Peeled is set only when ObjectID names an annotated tag. It is the first object
// down the tag chain that is not a tag.
type Ref struct {
	Name     string
	ObjectID *[20]byte
	Symbolic string
	Peeled   *[20]byte
}

// ValidRepoName is the database check, plus a refusal of a trailing ".git".
func ValidRepoName(name string) bool {
	if strings.HasSuffix(name, ".git") || strings.Contains(name, "..") {
		return false
	}
	return repoNameRE.MatchString(name)
}

// CreateRepo inserts the repository and its HEAD row in one transaction.
// A taken name returns ErrExists.
func (s *Store) CreateRepo(ctx context.Context, id [16]byte, name string) (Repo, error) {
	if !ValidRepoName(name) {
		return Repo{}, errors.New("store: invalid repository name")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Repo{}, filterDB(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var seq int64
	err = tx.QueryRow(ctx, `
		insert into repositories (id, name) values ($1, $2)
		returning push_seq`, id, name).Scan(&seq)
	if code, ok := sqlState(err); ok && code == "23505" {
		return Repo{}, ErrExists
	}
	if err != nil {
		return Repo{}, filterDB(err)
	}
	_, err = tx.Exec(ctx, `
		insert into refs (repo_id, name, symbolic) values ($1, 'HEAD', 'refs/heads/main')`, id)
	if err != nil {
		return Repo{}, filterDB(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Repo{}, filterDB(err)
	}
	return Repo{ID: id, Name: name, PushSeq: seq}, nil
}

// RepoByName returns ErrNotFound when absent.
func (s *Store) RepoByName(ctx context.Context, name string) (Repo, error) {
	var repo Repo
	err := s.pool.QueryRow(ctx, `
		select id, name, push_seq from repositories where name = $1`, name).
		Scan(&repo.ID, &repo.Name, &repo.PushSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repo{}, ErrNotFound
	}
	if err != nil {
		return Repo{}, filterDB(err)
	}
	return repo, nil
}

// RepoByID returns ErrNotFound when absent.
func (s *Store) RepoByID(ctx context.Context, id [16]byte) (Repo, error) {
	var repo Repo
	err := s.pool.QueryRow(ctx, `
		select id, name, push_seq from repositories where id = $1`, id).
		Scan(&repo.ID, &repo.Name, &repo.PushSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repo{}, ErrNotFound
	}
	if err != nil {
		return Repo{}, filterDB(err)
	}
	return repo, nil
}

// Pack is one stored pack row. Bytes and IdxBytes are ciphertext sizes.
type Pack struct {
	ID       [16]byte
	PackKey  string
	IdxKey   string
	Bytes    int64
	IdxBytes int64
}

// ReadRepo returns the repository, its refs, and its packs, read in one
// repeatable-read, read-only transaction. Packs are ordered by created_at, id,
// so a ref is never seen without the pack that holds its objects.
func (s *Store) ReadRepo(ctx context.Context, name string) (Repo, []Ref, []Pack, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Repo{}, nil, nil, filterDB(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var repo Repo
	err = tx.QueryRow(ctx, `
		select id, name, push_seq from repositories where name = $1`, name).
		Scan(&repo.ID, &repo.Name, &repo.PushSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repo{}, nil, nil, ErrNotFound
	}
	if err != nil {
		return Repo{}, nil, nil, filterDB(err)
	}
	refs, err := readRefs(ctx, tx, repo.ID)
	if err != nil {
		return Repo{}, nil, nil, err
	}
	packRows, err := tx.Query(ctx, `
		select id, pack_key, idx_key, bytes, idx_bytes from packs
		where repo_id = $1
		order by created_at, id`, repo.ID)
	if err != nil {
		return Repo{}, nil, nil, filterDB(err)
	}
	defer packRows.Close()
	var packs []Pack
	for packRows.Next() {
		var p Pack
		if err := packRows.Scan(&p.ID, &p.PackKey, &p.IdxKey, &p.Bytes, &p.IdxBytes); err != nil {
			return Repo{}, nil, nil, filterDB(err)
		}
		packs = append(packs, p)
	}
	if err := packRows.Err(); err != nil {
		return Repo{}, nil, nil, filterDB(err)
	}
	return repo, refs, packs, nil
}

// readRefs returns every ref of the repository ordered by name.
// CommitPush and ReadRepo both use it, so the snapshot and a later read cannot drift.
func readRefs(ctx context.Context, tx pgx.Tx, repoID [16]byte) ([]Ref, error) {
	rows, err := tx.Query(ctx, `
		select name, object_id, symbolic, peeled from refs
		where repo_id = $1
		order by name`, repoID)
	if err != nil {
		return nil, filterDB(err)
	}
	defer rows.Close()
	var refs []Ref
	for rows.Next() {
		var ref Ref
		var objectID, peeled []byte
		var symbolic *string
		if err := rows.Scan(&ref.Name, &objectID, &symbolic, &peeled); err != nil {
			return nil, filterDB(err)
		}
		if symbolic != nil && peeled != nil {
			return nil, errors.New("store: symbolic ref has peeled")
		}
		switch {
		case objectID == nil && symbolic != nil:
			ref.Symbolic = *symbolic
		case objectID != nil && symbolic == nil:
			if len(objectID) != 20 {
				return nil, errors.New("store: ref object id is not 20 bytes")
			}
			var id [20]byte
			copy(id[:], objectID)
			ref.ObjectID = &id
			if peeled != nil {
				if len(peeled) != 20 {
					return nil, errors.New("store: ref peeled id is not 20 bytes")
				}
				var p [20]byte
				copy(p[:], peeled)
				ref.Peeled = &p
			}
		default:
			return nil, errors.New("store: ref row has neither or both targets")
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, filterDB(err)
	}
	return refs, nil
}

// RestoredPack is one pack row rebuild puts back. Bytes and IdxBytes are ciphertext sizes.
type RestoredPack struct {
	ID       [16]byte
	PackKey  string
	IdxKey   string
	Bytes    int64
	IdxBytes int64
}

// Restore is one repository to insert from a snapshot and the packs that decrypted.
type Restore struct {
	ID      [16]byte
	Name    string
	PushSeq int64
	Refs    []Ref
	Packs   []RestoredPack
}

// RestoreRepo inserts the repository, its refs, and its packs in one transaction.
// A taken id or name returns ErrExists and inserts nothing.
func (s *Store) RestoreRepo(ctx context.Context, r Restore) error {
	if !ValidRepoName(r.Name) {
		return errors.New("store: invalid repository name")
	}
	for _, ref := range r.Refs {
		hasObject := ref.ObjectID != nil
		hasSymbolic := ref.Symbolic != ""
		if hasObject == hasSymbolic {
			return errors.New("store: ref has neither or both targets")
		}
		if !hasObject && ref.Peeled != nil {
			return errors.New("store: peeled set on a symbolic ref")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return filterDB(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		insert into repositories (id, name, push_seq) values ($1, $2, $3)`,
		r.ID, r.Name, r.PushSeq)
	if code, ok := sqlState(err); ok && code == "23505" {
		return ErrExists
	}
	if err != nil {
		return filterDB(err)
	}
	for _, ref := range r.Refs {
		if ref.ObjectID != nil {
			_, err = tx.Exec(ctx, `
				insert into refs (repo_id, name, object_id, peeled) values ($1, $2, $3, $4)`,
				r.ID, ref.Name, ref.ObjectID[:], peeledBytes(ref.Peeled))
		} else {
			_, err = tx.Exec(ctx, `
				insert into refs (repo_id, name, symbolic) values ($1, $2, $3)`,
				r.ID, ref.Name, ref.Symbolic)
		}
		if err != nil {
			return filterDB(err)
		}
	}
	for _, p := range r.Packs {
		_, err = tx.Exec(ctx, `
			insert into packs (id, repo_id, pack_key, idx_key, bytes, idx_bytes)
			values ($1, $2, $3, $4, $5, $6)`,
			p.ID, r.ID, p.PackKey, p.IdxKey, p.Bytes, p.IdxBytes)
		if err != nil {
			return filterDB(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return filterDB(err)
	}
	return nil
}

// BumpPushSeq commits push_seq + 1 and returns it. ErrNotFound when the row is gone.
// The statement is the same row lock CommitPush takes.
func (s *Store) BumpPushSeq(ctx context.Context, id [16]byte) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `
		update repositories set push_seq = push_seq + 1
		where id = $1
		returning push_seq`, id).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, filterDB(err)
	}
	return seq, nil
}

// DeleteRepo deletes the row only while push_seq still equals pushSeq.
// ErrNotFound when the row is gone. ErrStale when push_seq moved.
// The schema's on delete cascade removes that repository's refs, packs, and tokens.
func (s *Store) DeleteRepo(ctx context.Context, id [16]byte, pushSeq int64) error {
	tag, err := s.pool.Exec(ctx, `
		delete from repositories where id = $1 and push_seq = $2`, id, pushSeq)
	if err != nil {
		return filterDB(err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var one int
	err = s.pool.QueryRow(ctx, `select 1 from repositories where id = $1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return filterDB(err)
	}
	return ErrStale
}

// PackIDs returns the pack ids that have a row for this repository.
// The map is empty when the repository has no packs.
func (s *Store) PackIDs(ctx context.Context, repoID [16]byte) (map[[16]byte]struct{}, error) {
	rows, err := s.pool.Query(ctx, `select id from packs where repo_id = $1`, repoID)
	if err != nil {
		return nil, filterDB(err)
	}
	defer rows.Close()
	out := map[[16]byte]struct{}{}
	for rows.Next() {
		var id [16]byte
		if err := rows.Scan(&id); err != nil {
			return nil, filterDB(err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, filterDB(err)
	}
	return out, nil
}
