package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// TokenRow is a stored token. RepoID nil means every repository.
type TokenRow struct {
	ID     [16]byte
	Label  string
	Write  bool
	Admin  bool
	RepoID *[16]byte
}

// InsertToken stores a token. t.ID is chosen by the caller with NewID.
// hash is SHA-256 of the plaintext, which is not stored.
func (s *Store) InsertToken(ctx context.Context, hash [32]byte, t TokenRow) error {
	var repo pgtype.UUID
	if t.RepoID != nil {
		repo = pgtype.UUID{Bytes: *t.RepoID, Valid: true}
	}
	_, err := s.pool.Exec(ctx, `
		insert into tokens (id, token_hash, label, write, admin, repo_id)
		values ($1, $2, $3, $4, $5, $6)`,
		t.ID, hash[:], t.Label, t.Write, t.Admin, repo)
	return filterDB(err)
}

// TokenByHash returns the token row for hash.
// A non-null uuid scans into [16]byte. A null repo_id does not, so that
// column is read as pgtype.UUID and copied into RepoID.
func (s *Store) TokenByHash(ctx context.Context, hash [32]byte) (TokenRow, error) {
	var row TokenRow
	var repo pgtype.UUID
	err := s.pool.QueryRow(ctx, `
		select id, label, write, admin, repo_id
		from tokens
		where token_hash = $1`, hash[:]).Scan(&row.ID, &row.Label, &row.Write, &row.Admin, &repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenRow{}, ErrNotFound
	}
	if err != nil {
		return TokenRow{}, filterDB(err)
	}
	if repo.Valid {
		id := repo.Bytes
		row.RepoID = &id
	}
	return row, nil
}

// TokenListing is one token without its hash.
type TokenListing struct {
	ID        [16]byte
	Label     string
	Write     bool
	Admin     bool
	RepoName  string
	CreatedAt time.Time
}

// ListTokens returns every token, ordered by created_at then id. Never the hash.
func (s *Store) ListTokens(ctx context.Context) ([]TokenListing, error) {
	rows, err := s.pool.Query(ctx, `
		select t.id, t.label, t.write, t.admin, coalesce(r.name, ''), t.created_at
		from tokens t
		left join repositories r on r.id = t.repo_id
		order by t.created_at, t.id`)
	if err != nil {
		return nil, filterDB(err)
	}
	defer rows.Close()
	list := []TokenListing{}
	for rows.Next() {
		var row TokenListing
		if err := rows.Scan(&row.ID, &row.Label, &row.Write, &row.Admin, &row.RepoName, &row.CreatedAt); err != nil {
			return nil, filterDB(err)
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		return nil, filterDB(err)
	}
	return list, nil
}

// TokenBackup is one token row as a backup stores it, including the hash.
type TokenBackup struct {
	ID        [16]byte
	Hash      [32]byte
	Label     string
	Write     bool
	Admin     bool
	RepoID    *[16]byte
	CreatedAt time.Time
}

// TokenRestoreCounts is what RestoreTokens did. Present rows were left unchanged.
type TokenRestoreCounts struct {
	Restored     int
	Present      int
	NoRepository int
}

// ReadTokensForBackup reads every token and the database time of that read.
// The timestamp is the first statement of one repeatable-read, read-only
// transaction, and the rows come from that same snapshot.
func (s *Store) ReadTokensForBackup(ctx context.Context) (time.Time, []TokenBackup, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return time.Time{}, nil, filterDB(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var taken time.Time
	if err := tx.QueryRow(ctx, `select statement_timestamp()`).Scan(&taken); err != nil {
		return time.Time{}, nil, filterDB(err)
	}
	rows, err := tx.Query(ctx, `
		select id, token_hash, label, write, admin, repo_id, created_at
		from tokens
		order by id`)
	if err != nil {
		return time.Time{}, nil, filterDB(err)
	}
	defer rows.Close()
	list := []TokenBackup{}
	for rows.Next() {
		var row TokenBackup
		var hash []byte
		var repo pgtype.UUID
		if err := rows.Scan(&row.ID, &hash, &row.Label, &row.Write, &row.Admin, &repo, &row.CreatedAt); err != nil {
			return time.Time{}, nil, filterDB(err)
		}
		if len(hash) != 32 {
			return time.Time{}, nil, errors.New("store: token hash length")
		}
		copy(row.Hash[:], hash)
		if repo.Valid {
			id := repo.Bytes
			row.RepoID = &id
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, nil, filterDB(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, nil, filterDB(err)
	}
	return taken, list, nil
}

// RestoreTokens inserts tokens that are absent. A row whose id or token_hash
// is already stored is left as it is. A token limited to a repository that
// has no row is skipped. The whole change is one transaction.
func (s *Store) RestoreTokens(ctx context.Context, tokens []TokenBackup) (TokenRestoreCounts, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenRestoreCounts{}, filterDB(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var counts TokenRestoreCounts
	for _, t := range tokens {
		var idExists, hashExists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from tokens where id = $1)`, t.ID).Scan(&idExists); err != nil {
			return TokenRestoreCounts{}, filterDB(err)
		}
		if err := tx.QueryRow(ctx, `select exists(select 1 from tokens where token_hash = $1)`, t.Hash[:]).Scan(&hashExists); err != nil {
			return TokenRestoreCounts{}, filterDB(err)
		}
		if idExists || hashExists {
			counts.Present++
			continue
		}
		if t.RepoID != nil {
			var repoExists bool
			if err := tx.QueryRow(ctx, `select exists(select 1 from repositories where id = $1)`, *t.RepoID).Scan(&repoExists); err != nil {
				return TokenRestoreCounts{}, filterDB(err)
			}
			if !repoExists {
				counts.NoRepository++
				continue
			}
		}
		var repo pgtype.UUID
		if t.RepoID != nil {
			repo = pgtype.UUID{Bytes: *t.RepoID, Valid: true}
		}
		_, err := tx.Exec(ctx, `
			insert into tokens (id, token_hash, label, write, admin, repo_id, created_at)
			values ($1, $2, $3, $4, $5, $6, $7)`,
			t.ID, t.Hash[:], t.Label, t.Write, t.Admin, repo, t.CreatedAt.UTC())
		if err != nil {
			return TokenRestoreCounts{}, filterDB(err)
		}
		counts.Restored++
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenRestoreCounts{}, filterDB(err)
	}
	return counts, nil
}

// DeleteToken deletes one row. No row returns ErrNotFound.
func (s *Store) DeleteToken(ctx context.Context, id [16]byte) error {
	tag, err := s.pool.Exec(ctx, `delete from tokens where id = $1`, id)
	if err != nil {
		return filterDB(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
