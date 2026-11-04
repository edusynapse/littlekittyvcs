package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

func TestWithAdvisoryLock(t *testing.T) {
	testdb.BackupLock(t)
	// Two store.Open calls make two pools, so the two sessions are not one
	// shared connection from a single pool.
	raw := testdb.URL(t)
	ctx := context.Background()
	st1, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st1.Close)
	st2, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st2.Close)
	t.Log("EVIDENCE connections: two store.Open pools on one database URL")

	const key int64 = 0x6c6b760000000002
	hold := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ran, err := st1.WithAdvisoryLock(ctx, key, func(context.Context) error {
			close(hold)
			<-release
			return nil
		})
		if !ran && err == nil {
			err = errors.New("store: lock function did not run")
		}
		done <- err
	}()
	select {
	case <-hold:
	case <-time.After(10 * time.Second):
		t.Fatal("first call did not take the lock")
	}
	start := time.Now()
	ran, err := st2.WithAdvisoryLock(ctx, key, func(context.Context) error {
		t.Error("second function ran")
		return nil
	})
	elapsed := time.Since(start)
	if err != nil || ran {
		t.Fatalf("second ran %v err %v", ran, err)
	}
	if elapsed > time.Second {
		t.Fatalf("second call blocked for %s", elapsed)
	}
	t.Logf("EVIDENCE second call ran=false in %s", elapsed)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ran, err = st2.WithAdvisoryLock(ctx, key, func(context.Context) error { return nil })
	if err != nil || !ran {
		t.Fatalf("after release ran %v err %v", ran, err)
	}
}
