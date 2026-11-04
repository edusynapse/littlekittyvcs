// Package store is the PostgreSQL connection, the schema, and the token table.
package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a token hash has no row.
var ErrNotFound = errors.New("store: not found")

var errUnreachable = errors.New("store: database unreachable")

const (
	databaseRetryFor  = 30 * time.Second
	databaseRetryWait = 2 * time.Second
)

// Store is a pool of PostgreSQL connections.
type Store struct {
	pool *pgxpool.Pool
}

// Open parses databaseURL, creates a pool, and pings until the database
// answers. It tries immediately and then every 2 seconds, for up to 30 seconds.
// A bad login (SQLSTATE class 28) or a missing database (3D000) stops at once.
// Any other error, including a server that is still starting, keeps retrying.
// If the deadline passes on a server error, the error names that last SQLSTATE
// and does not include the connection string.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errUnreachable
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errUnreachable
	}
	deadline := time.Now().Add(databaseRetryFor)
	var lastErr error
	for {
		if ctx.Err() != nil {
			pool.Close()
			return nil, connectResult(lastErr)
		}
		pingCtx, cancel := context.WithTimeout(ctx, databaseRetryWait)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return &Store{pool: pool}, nil
		}
		lastErr = err
		if refused, ok := databaseRefused(err); ok {
			pool.Close()
			return nil, refused
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			pool.Close()
			return nil, connectResult(lastErr)
		}
		wait := databaseRetryWait
		if remain < wait {
			wait = remain
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			pool.Close()
			return nil, connectResult(lastErr)
		case <-timer.C:
		}
	}
}

// databaseRefused reports a failure that cannot fix itself: SQLSTATE class 28
// or 3D000. The text is only the code.
func databaseRefused(err error) (error, bool) {
	code, ok := sqlState(err)
	if !ok || !permanentSQLState(code) {
		return nil, false
	}
	return sqlStateError(code), true
}

func permanentSQLState(code string) bool {
	return strings.HasPrefix(code, "28") || code == "3D000"
}

func connectResult(err error) error {
	code, ok := sqlState(err)
	if !ok {
		return errUnreachable
	}
	return sqlStateError(code)
}

func sqlStateError(code string) error {
	return fmt.Errorf("store: database refused the connection (SQLSTATE %s)", code)
}

func sqlState(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return "", false
	}
	return pgErr.Code, true
}

// Close releases the pool.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Ping checks that the database still answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return errUnreachable
	}
	return nil
}

// NewID returns a random version 4 UUID as 16 bytes.
func NewID() ([16]byte, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return [16]byte{}, err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id, nil
}
