package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrExists is returned when a repository name is already taken.
var ErrExists = errors.New("store: already exists")

// ErrStale is returned when a ref changed since the push read it.
var ErrStale = errors.New("store: a ref changed since it was read")

// filterDB hides connection details. A request handler logs this error.
func filterDB(err error) error {
	if err == nil ||
		errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrExists) ||
		errors.Is(err, ErrStale) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("store: database error (SQLSTATE %s)", pgErr.Code)
	}
	return errors.New("store: database request failed")
}
