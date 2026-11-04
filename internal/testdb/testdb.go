// Package testdb creates a throwaway PostgreSQL schema for tests.
// It reads LKV_TEST_DATABASE_URL. Only tests call it.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"regexp"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
)

var schemaNameRe = regexp.MustCompile(`^lkv_test_[0-9a-f]{16}$`)

// BackupLock is an exclusive file lock for tests that take the backup
// advisory lock. That PostgreSQL lock is per database, and go test runs
// packages at the same time against one database.
func BackupLock(t testing.TB) {
	t.Helper()
	f, err := os.OpenFile("/tmp/lkv-test-backup.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}

// URL creates schema lkv_test_<16 hex>, registers a Cleanup that drops exactly
// that schema, and returns LKV_TEST_DATABASE_URL with search_path set to it.
// It skips the test when LKV_TEST_DATABASE_URL is unset.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv("LKV_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("LKV_TEST_DATABASE_URL is not set")
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	name := "lkv_test_" + hex.EncodeToString(buf[:])
	if !schemaNameRe.MatchString(name) {
		t.Fatalf("schema name %q", name)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "create schema "+quoted); err != nil {
		_ = conn.Close(ctx)
		t.Fatal(err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !schemaNameRe.MatchString(name) {
			t.Errorf("refuse to drop %q", name)
			return
		}
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "drop schema "+pgx.Identifier{name}.Sanitize()+" cascade"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	return u.String()
}
