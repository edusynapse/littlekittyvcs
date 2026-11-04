package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RefUpdate is one compare-and-swap on a ref.
// Old nil creates the ref. New nil deletes it. Both nil is a bug.
// Peeled is set only when New names an annotated tag. It must be nil when New is nil.
type RefUpdate struct {
	Name   string
	Old    *[20]byte
	New    *[20]byte
	Peeled *[20]byte
}

// NewPack is the pack row written by a push. Bytes and IdxBytes are ciphertext sizes.
type NewPack struct {
	ID       [16]byte
	PackKey  string
	IdxKey   string
	Bytes    int64
	IdxBytes int64
}

// Push is one receive-pack transaction.
// CreateName set inserts the repository before the ref updates.
// Pack nil stores no pack row (a delete, or a pack with no objects).
type Push struct {
	RepoID     [16]byte
	CreateName string
	Pack       *NewPack
	Updates    []RefUpdate
}

// Committed is what one push committed: the new push_seq and every ref
// afterwards, HEAD included, ordered by name.
type Committed struct {
	PushSeq int64
	Refs    []Ref
}

// CommitPush applies the whole push in one read-committed transaction, or nothing.
// Refs is read after fixHead and before the commit, so it is exactly the set
// that committed. The caller writes the ref snapshot from it.
func (s *Store) CommitPush(ctx context.Context, p Push) (Committed, error) {
	if p.CreateName != "" && !ValidRepoName(p.CreateName) {
		return Committed{}, errors.New("store: invalid repository name")
	}
	for _, u := range p.Updates {
		if u.Old == nil && u.New == nil {
			return Committed{}, errors.New("store: ref update has neither old nor new")
		}
		if u.New == nil && u.Peeled != nil {
			return Committed{}, errors.New("store: peeled set without a new object")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Committed{}, filterDB(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if p.CreateName != "" {
		var seq int64
		err = tx.QueryRow(ctx, `
			insert into repositories (id, name) values ($1, $2)
			returning push_seq`, p.RepoID, p.CreateName).Scan(&seq)
		if code, ok := sqlState(err); ok && code == "23505" {
			return Committed{}, ErrExists
		}
		if err != nil {
			return Committed{}, filterDB(err)
		}
		_, err = tx.Exec(ctx, `
			insert into refs (repo_id, name, symbolic) values ($1, 'HEAD', 'refs/heads/main')`, p.RepoID)
		if err != nil {
			return Committed{}, filterDB(err)
		}
	}

	var seq int64
	err = tx.QueryRow(ctx, `
		update repositories set push_seq = push_seq + 1
		where id = $1
		returning push_seq`, p.RepoID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return Committed{}, ErrNotFound
	}
	if err != nil {
		return Committed{}, filterDB(err)
	}

	for _, u := range p.Updates {
		var n int64
		switch {
		case u.Old == nil:
			tag, err := tx.Exec(ctx, `
				insert into refs (repo_id, name, object_id, peeled) values ($1, $2, $3, $4)
				on conflict do nothing`, p.RepoID, u.Name, u.New[:], peeledBytes(u.Peeled))
			if err != nil {
				return Committed{}, filterDB(err)
			}
			n = tag.RowsAffected()
		case u.New == nil:
			tag, err := tx.Exec(ctx, `
				delete from refs
				where repo_id = $1 and name = $2 and object_id = $3`, p.RepoID, u.Name, u.Old[:])
			if err != nil {
				return Committed{}, filterDB(err)
			}
			n = tag.RowsAffected()
		default:
			tag, err := tx.Exec(ctx, `
				update refs set object_id = $1, peeled = $2
				where repo_id = $3 and name = $4 and object_id = $5`, u.New[:], peeledBytes(u.Peeled), p.RepoID, u.Name, u.Old[:])
			if err != nil {
				return Committed{}, filterDB(err)
			}
			n = tag.RowsAffected()
		}
		if n == 0 {
			return Committed{}, ErrStale
		}
	}

	if p.Pack != nil {
		_, err = tx.Exec(ctx, `
			insert into packs (id, repo_id, pack_key, idx_key, bytes, idx_bytes)
			values ($1, $2, $3, $4, $5, $6)`,
			p.Pack.ID, p.RepoID, p.Pack.PackKey, p.Pack.IdxKey, p.Pack.Bytes, p.Pack.IdxBytes)
		if err != nil {
			return Committed{}, filterDB(err)
		}
	}

	if err := fixHead(ctx, tx, p); err != nil {
		return Committed{}, err
	}
	refs, err := readRefs(ctx, tx, p.RepoID)
	if err != nil {
		return Committed{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Committed{}, filterDB(err)
	}
	return Committed{PushSeq: seq, Refs: refs}, nil
}

// fixHead points HEAD at the first branch this push created when HEAD's
// symbolic target is not a ref afterwards. A repository whose first push is
// master then clones with files checked out. Every other HEAD is left alone.
func fixHead(ctx context.Context, tx pgx.Tx, p Push) error {
	var head *string
	err := tx.QueryRow(ctx, `
		select symbolic from refs where repo_id = $1 and name = 'HEAD'`, p.RepoID).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return filterDB(err)
	}
	if head == nil {
		return nil
	}
	var exists bool
	err = tx.QueryRow(ctx, `
		select exists(select 1 from refs where repo_id = $1 and name = $2)`, p.RepoID, *head).Scan(&exists)
	if err != nil {
		return filterDB(err)
	}
	if exists {
		return nil
	}
	for _, u := range p.Updates {
		if u.Old == nil && u.New != nil && strings.HasPrefix(u.Name, "refs/heads/") {
			_, err = tx.Exec(ctx, `
				update refs set symbolic = $1 where repo_id = $2 and name = 'HEAD'`, u.Name, p.RepoID)
			if err != nil {
				return filterDB(err)
			}
			return nil
		}
	}
	return nil
}

func peeledBytes(p *[20]byte) []byte {
	if p == nil {
		return nil
	}
	return p[:]
}
