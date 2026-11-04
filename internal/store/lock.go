package store

import (
	"context"
	"errors"
	"time"
)

var errLockNotHeld = errors.New("store: advisory lock was not held")

// WithAdvisoryLock runs fn while this process holds a session advisory lock
// on one connection. ran is false when another session already holds key,
// and fn is not called. The lock is released on that same connection, including
// when ctx is cancelled, so a pooled connection cannot return to the pool
// still holding it.
func (s *Store) WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (ran bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, filterDB(err)
	}
	defer conn.Release()

	var locked bool
	if err = conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return false, filterDB(err)
	}
	if !locked {
		return false, nil
	}
	defer func() {
		// The caller's context may already be cancelled. Unlock anyway.
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		uerr := conn.QueryRow(uctx, `select pg_advisory_unlock($1)`, key).Scan(&unlocked)
		if uerr != nil {
			uerr = filterDB(uerr)
		} else if !unlocked {
			uerr = errLockNotHeld
		}
		if err == nil && uerr != nil {
			err = uerr
		}
	}()
	if err = fn(ctx); err != nil {
		return true, err
	}
	return true, nil
}
