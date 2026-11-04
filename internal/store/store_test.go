package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("LKV_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("LKV_TEST_DATABASE_URL is not set")
	}
	return raw
}

func testSchema(t *testing.T) (string, *Store) {
	t.Helper()
	raw := testdb.URL(t)
	st, err := Open(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return raw, st
}

func TestMigrationFiles(t *testing.T) {
	files, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].version != 1 || files[0].name != "0001_init.sql" || files[0].sql == "" ||
		files[1].version != 2 || files[1].name != "0002_ref_peeled.sql" || files[1].sql == "" {
		t.Fatalf("%+v", files)
	}
	if _, err := parseMigrationNames([]string{"0001_a.sql", "0003_b.sql"}); err == nil {
		t.Fatal("gap accepted")
	}
	if _, err := parseMigrationNames([]string{"0001_a.sql", "0001_b.sql"}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestNewID(t *testing.T) {
	seen := make(map[[16]byte]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := seen[id]; ok {
			t.Fatal("duplicate id")
		}
		seen[id] = struct{}{}
		if id[6]&0xf0 != 0x40 {
			t.Fatalf("version nibble %x", id[6])
		}
		if id[8]&0xc0 != 0x80 {
			t.Fatalf("variant %x", id[8])
		}
	}
}

func TestMigrateFreshAndAgain(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	var tables int
	err := st.pool.QueryRow(ctx, `
		select count(*) from information_schema.tables
		where table_schema = current_schema()
		  and table_name in ('repositories', 'refs', 'packs', 'tokens')`).Scan(&tables)
	if err != nil {
		t.Fatal(err)
	}
	if tables != 4 {
		t.Fatalf("tables %d", tables)
	}
	var version int
	var rows int
	err = st.pool.QueryRow(ctx, `select count(*), min(version) from schema_migrations`).Scan(&rows, &version)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 || version != 1 {
		t.Fatalf("schema_migrations rows %d version %d", rows, version)
	}
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	err = st.pool.QueryRow(ctx, `select count(*) from schema_migrations`).Scan(&rows)
	if err != nil || rows != 2 {
		t.Fatalf("after second migrate rows %d err %v", rows, err)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	raw, _ := testSchema(t)
	ctx := context.Background()
	errCh := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() {
			st, err := Open(ctx, raw)
			if err != nil {
				errCh <- err
				return
			}
			defer st.Close()
			errCh <- st.Migrate(ctx, discardLog())
		}()
	}
	for i := 0; i < 5; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	st, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rows int
	err = st.pool.QueryRow(ctx, `select count(*) from schema_migrations`).Scan(&rows)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("schema_migrations rows %d", rows)
	}
}

func TestMigrateNewerThanBinary(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `insert into schema_migrations (version) values (99)`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, discardLog()); err == nil {
		t.Fatal("newer database was accepted")
	}
}

func TestConstraints(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo := insertRepo(t, st, "demo")
	reject := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, sql, args...); err == nil {
			t.Fatalf("accepted %s", sql)
		}
	}
	twenty := bytes20()
	nineteen := twenty[:19]
	reject(`insert into refs (repo_id, name, object_id, symbolic) values ($1, 'both', $2, 'refs/heads/main')`, repo, twenty)
	reject(`insert into refs (repo_id, name, object_id, symbolic) values ($1, 'neither', null, null)`, repo)
	reject(`insert into refs (repo_id, name, object_id) values ($1, 'short', $2)`, repo, nineteen)
	reject(`insert into refs (repo_id, name, symbolic, peeled) values ($1, 'sym-peeled', 'refs/heads/main', $2)`, repo, twenty)
	reject(`insert into refs (repo_id, name, object_id, peeled) values ($1, 'short-peeled', $2, $3)`, repo, twenty, nineteen)
	hash := bytes32()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	reject(`insert into tokens (id, token_hash, label, write, admin) values ($1, $2, 'ro-admin', false, true)`, id, hash[:])
	id, err = NewID()
	if err != nil {
		t.Fatal(err)
	}
	reject(`insert into tokens (id, token_hash, label, write, admin, repo_id) values ($1, $2, 'admin-repo', true, true, $3)`, id, hash[:], repo)
	reject(`insert into repositories (id, name) values ($1, 'Bad')`, mustID(t))
	reject(`insert into repositories (id, name) values ($1, 'a..b')`, mustID(t))
	reject(`insert into repositories (id, name) values ($1, '-x')`, mustID(t))
}

func TestTokens(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo := insertRepo(t, st, "demo")
	hash := bytes32()
	id := mustID(t)
	row := TokenRow{ID: id, Label: "ci", Write: true, Admin: false, RepoID: &repo}
	if err := st.InsertToken(ctx, hash, row); err != nil {
		t.Fatal(err)
	}
	got, err := st.TokenByHash(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Label != "ci" || !got.Write || got.Admin || got.RepoID == nil || *got.RepoID != repo {
		t.Fatalf("%+v", got)
	}
	var missing [32]byte
	missing[0] = 9
	_, err = st.TokenByHash(ctx, missing)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	again := row
	again.ID = mustID(t)
	if err := st.InsertToken(ctx, hash, again); err == nil {
		t.Fatal("duplicate hash accepted")
	}
}

func TestCascade(t *testing.T) {
	_, st := testSchema(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, discardLog()); err != nil {
		t.Fatal(err)
	}
	repo := insertRepo(t, st, "demo")
	hash := bytes32()
	if err := st.InsertToken(ctx, hash, TokenRow{
		ID: mustID(t), Label: "limited", Write: false, RepoID: &repo,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `delete from repositories where id = $1`, repo); err != nil {
		t.Fatal(err)
	}
	_, err := st.TokenByHash(ctx, hash)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("token after delete: %v", err)
	}
}

func TestOpenServerRefusalStopsImmediately(t *testing.T) {
	base := testDatabaseURL(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	wrong := *u
	wrong.User = url.UserPassword(user, "WRONG")
	assertRefused(t, wrong.String(), "28P01")

	missing := *u
	missing.Path = "/lkv_does_not_exist"
	assertRefused(t, missing.String(), "3D000")
}

func assertRefused(t *testing.T, databaseURL, code string) {
	t.Helper()
	start := time.Now()
	_, err := Open(context.Background(), databaseURL)
	elapsed := time.Since(start)
	if elapsed >= 2*time.Second {
		t.Fatalf("SQLSTATE %s took %s", code, elapsed)
	}
	want := "store: database refused the connection (SQLSTATE " + code + ")"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "WRONG") || strings.Contains(err.Error(), "lkv_does_not_exist") {
		t.Fatalf("error includes a connection detail: %v", err)
	}
}

func TestOpenRetriesWhileDatabaseIsStarting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var connects atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connects.Add(1)
			go replyStartingUp(c)
		}
	}()
	dsn := fmt.Sprintf("postgres://lkv:secret@127.0.0.1:%d/lkv?sslmode=disable", ln.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Open(ctx, dsn)
	if ctx.Err() == nil {
		t.Fatal("Open returned before the context ended")
	}
	if connects.Load() < 2 {
		t.Fatalf("connections %d", connects.Load())
	}
	const want = "store: database refused the connection (SQLSTATE 57P03)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
}

func replyStartingUp(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	var sizeBuf [4]byte
	if _, err := io.ReadFull(c, sizeBuf[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint32(sizeBuf[:])
	if n < 4 || n > 10000 {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, n-4)); err != nil {
		return
	}
	const payload = "SFATAL\x00VFATAL\x00C57P03\x00Mthe database system is starting up\x00\x00"
	msg := make([]byte, 5+len(payload))
	msg[0] = 'E'
	binary.BigEndian.PutUint32(msg[1:5], uint32(4+len(payload)))
	copy(msg[5:], payload)
	_, _ = c.Write(msg)
}

func TestOpenRetriesWhenNothingAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Open(ctx, "postgres://lkv:secret@127.0.0.1:1/lkv?sslmode=disable")
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond || elapsed >= 5*time.Second {
		t.Fatalf("took %s", elapsed)
	}
	if !errors.Is(err, errUnreachable) {
		t.Fatalf("got %v", err)
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func insertRepo(t *testing.T, st *Store, name string) [16]byte {
	t.Helper()
	id := mustID(t)
	if _, err := st.pool.Exec(context.Background(), `insert into repositories (id, name) values ($1, $2)`, id, name); err != nil {
		t.Fatal(err)
	}
	return id
}

func mustID(t *testing.T) [16]byte {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func bytes20() []byte {
	b := make([]byte, 20)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func bytes32() [32]byte {
	var b [32]byte
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}
