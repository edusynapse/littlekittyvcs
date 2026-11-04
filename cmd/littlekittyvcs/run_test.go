package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/config"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
	"github.com/edusynapse/littlekittyvcs/internal/tokenbackup"
)

func TestBadMasterKeyDoesNotWaitForDatabase(t *testing.T) {
	env := serveEnv()
	env["MASTER_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 31))
	env["DATABASE_URL"] = "postgres://lkv:secret@127.0.0.1:1/lkv?sslmode=disable"
	start := time.Now()
	err := runServe(context.Background(), mapGetenv(env), nil, discardLog())
	elapsed := time.Since(start)
	t.Logf("bad master key returned in %s", elapsed)
	if elapsed >= time.Second {
		t.Fatalf("took %s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "MASTER_KEY") {
		t.Fatalf("got %v", err)
	}
}

func TestRunServeClosesPassedListenerOnStartupFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env := serveEnv()
	env["MASTER_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 31))
	err = runServe(context.Background(), mapGetenv(env), ln, discardLog())
	if err == nil || !strings.Contains(err.Error(), "MASTER_KEY") {
		t.Fatalf("got %v", err)
	}
	conn, dialErr := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Fatal("listener still accepting")
	}
}

func TestHealthyWithBucketDown(t *testing.T) {
	db := testdb.URL(t)
	env := serveEnv()
	env["DATABASE_URL"] = db
	env["S3_ENDPOINT"] = "http://127.0.0.1:1"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(ctx, mapGetenv(env), ln, discardLog())
	}()
	url := "http://" + ln.Addr().String() + "/healthz"
	deadline := time.Now().Add(10 * time.Second)
	var ok bool
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(body) == "ok\n" {
				ok = true
				break
			}
		}
		select {
		case err := <-errCh:
			t.Fatalf("serve exited: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !ok {
		t.Fatal("healthz did not return 200")
	}
	refs, err := http.Get("http://" + ln.Addr().String() + "/site.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	refBody, _ := io.ReadAll(refs.Body)
	refs.Body.Close()
	if refs.StatusCode != http.StatusUnauthorized || refs.Header.Get("WWW-Authenticate") != auth.Challenge {
		t.Fatalf("info/refs %d %q challenge %q", refs.StatusCode, refBody, refs.Header.Get("WWW-Authenticate"))
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown took longer than 30s")
	}
	if _, err := ln.Accept(); err == nil {
		t.Fatal("listener still accepting")
	}
}

func TestAdminToken(t *testing.T) {
	db := testdb.URL(t)
	mem := blob.NewMem()
	prevBuckets := testBuckets
	testBuckets = func(context.Context, config.Storage) (blob.Store, blob.Store, error) {
		return mem, nil, nil
	}
	t.Cleanup(func() { testBuckets = prevBuckets })
	env := serveEnv()
	env["DATABASE_URL"] = db
	getenv := mapGetenv(env)
	var stdout, logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	err := runAdminToken(context.Background(), getenv, []string{"--label", "ops"}, &stdout, log)
	if err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if strings.Count(text, "\n") != 1 || !strings.HasPrefix(text, "lkv_") || !strings.HasSuffix(text, "\n") || strings.Contains(text, "{") {
		t.Fatalf("stdout %q", text)
	}
	if !strings.Contains(logs.String(), "migration applied") {
		t.Fatalf("logger %s", logs.String())
	}
	token := strings.TrimSuffix(text, "\n")
	if strings.Contains(logs.String(), token) {
		t.Fatal("token was written to the log")
	}
	t.Logf("admin-token stdout prefix %s… label ops", token[:8])
	st, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, err := st.TokenByHash(context.Background(), auth.Hash(token))
	if err != nil {
		t.Fatal(err)
	}
	if !row.Admin || !row.Write || row.RepoID != nil || row.Label != "ops" {
		t.Fatalf("%+v", row)
	}
	var keys int
	if err := mem.List(context.Background(), "tokens/", func(blob.Info) error {
		keys++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if keys != 1 {
		t.Fatalf("token backups %d", keys)
	}

	stdout.Reset()
	err = runAdminToken(context.Background(), getenv, nil, &stdout, log)
	if err == nil {
		t.Fatal("missing label was accepted")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestBackupRequiresEnabled(t *testing.T) {
	err := runBackup(context.Background(), mapGetenv(serveEnv()), nil, io.Discard, discardLog())
	if err == nil || !strings.Contains(err.Error(), "BACKUP_ENABLED") {
		t.Fatalf("backup: %v", err)
	}
	err = runGC(context.Background(), mapGetenv(serveEnv()), []string{"--backup"}, io.Discard, discardLog())
	if err == nil || !strings.Contains(err.Error(), "BACKUP_ENABLED") {
		t.Fatalf("gc: %v", err)
	}
	err = runGC(context.Background(), mapGetenv(serveEnv()), []string{"--dry-run", "--backup"}, io.Discard, discardLog())
	if err == nil || !strings.Contains(err.Error(), "BACKUP_ENABLED") {
		t.Fatalf("gc dry-run: %v", err)
	}
}

func TestRestoreTokensCommand(t *testing.T) {
	mem := blob.NewMem()
	prev := testBuckets
	testBuckets = func(context.Context, config.Storage) (blob.Store, blob.Store, error) {
		return mem, nil, nil
	}
	t.Cleanup(func() { testBuckets = prev })
	env := serveEnv()
	env["DATABASE_URL"] = testdb.URL(t)
	var stdout bytes.Buffer
	err := runRestoreTokens(context.Background(), mapGetenv(env), nil, &stdout, discardLog())
	if err == nil {
		t.Fatal("empty bucket exited 0")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}

	src, err := store.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	if err := src.Migrate(context.Background(), discardLog()); err != nil {
		t.Fatal(err)
	}
	plain, hash, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := src.InsertToken(context.Background(), hash, store.TokenRow{
		ID: id, Label: "ops", Write: true, Admin: true,
	}); err != nil {
		t.Fatal(err)
	}
	var master [32]byte
	for i := range master {
		master[i] = 0x22
	}
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokenbackup.WriteCurrent(context.Background(), src, kr, mem); err != nil {
		t.Fatal(err)
	}
	empty := testdb.URL(t)
	env["DATABASE_URL"] = empty
	stdout.Reset()
	err = runRestoreTokens(context.Background(), mapGetenv(env), nil, &stdout, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(line, "restored tokens=1 present=0 no_repository=0 unreadable=0 taken_at=") {
		t.Fatalf("stdout %q", line)
	}
	if strings.Contains(line, "ops") || strings.Contains(stdout.String(), plain) {
		t.Fatal("summary contains a label or token")
	}
	t.Logf("EVIDENCE %s", line)
}

func TestRestoreRefusesBeforeConnect(t *testing.T) {
	boom := func(string) string {
		t.Fatal("getenv called")
		return ""
	}
	err := runRestore(context.Background(), boom, []string{"extra"}, io.Discard, discardLog())
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("extra: %v", err)
	}
	err = runRestore(context.Background(), boom, []string{"--repo", "../x"}, io.Discard, discardLog())
	if err == nil || !strings.Contains(err.Error(), "invalid repository name") {
		t.Fatalf("name: %v", err)
	}
	err = runRestore(context.Background(), func(string) string { return "" }, nil, io.Discard, discardLog())
	if err == nil {
		t.Fatal("empty env accepted")
	}
	text := err.Error()
	want := []string{"DATABASE_URL", "MASTER_KEY", "S3_ACCESS_KEY", "S3_BUCKET", "S3_ENDPOINT", "S3_REGION", "S3_SECRET_KEY"}
	prev := 0
	for _, name := range want {
		at := strings.Index(text, name+": required")
		if at < prev {
			t.Fatalf("%s missing or out of order in %s", name, text)
		}
		prev = at + 1
	}
	for _, leaked := range []string{"postgres://", "lkvtest", "secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("error contains %q", leaked)
		}
	}
}

func TestGCRefusesBeforeConnect(t *testing.T) {
	boom := func(string) string {
		t.Fatal("getenv called")
		return ""
	}
	err := runGC(context.Background(), boom, []string{"extra"}, io.Discard, discardLog())
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("extra: %v", err)
	}
	err = runGC(context.Background(), func(string) string { return "" }, nil, io.Discard, discardLog())
	if err == nil {
		t.Fatal("empty env accepted")
	}
	text := err.Error()
	want := []string{"DATABASE_URL", "MASTER_KEY", "S3_ACCESS_KEY", "S3_BUCKET", "S3_ENDPOINT", "S3_REGION", "S3_SECRET_KEY"}
	prev := 0
	for _, name := range want {
		at := strings.Index(text, name+": required")
		if at < prev {
			t.Fatalf("%s missing or out of order in %s", name, text)
		}
		prev = at + 1
	}
	for _, leaked := range []string{"postgres://", "lkvtest", "secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("error contains %q", leaked)
		}
	}
}

func serveEnv() map[string]string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x22
	}
	return map[string]string{
		"DATABASE_URL":  "postgres://lkv:lkvtest@127.0.0.1:5432/lkv?sslmode=disable",
		"MASTER_KEY":    base64.StdEncoding.EncodeToString(key),
		"S3_ENDPOINT":   "http://127.0.0.1:9",
		"S3_REGION":     "us-east-1",
		"S3_BUCKET":     "lkv",
		"S3_ACCESS_KEY": "access",
		"S3_SECRET_KEY": "secret",
	}
}

func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
