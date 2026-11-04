package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
	"github.com/edusynapse/littlekittyvcs/internal/tokenbackup"
)

func TestLockKeyDiffersFromMigrate(t *testing.T) {
	const migrateLock int64 = 0x6c6b760000000001
	if LockKey == migrateLock {
		t.Fatal("backup lock equals the migrate lock")
	}
	t.Logf("EVIDENCE backup lock %x migrate lock %x", LockKey, migrateLock)
}

func TestCopyPrefixes(t *testing.T) {
	testdb.BackupLock(t)
	st, kr := openBackup(t)
	primary := blob.NewMem()
	dst := blob.NewMem()
	ctx := context.Background()
	put(t, primary, "repos/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/packs/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.pack", []byte("pack"))
	put(t, primary, "tokens/cccccccc-cccc-4ccc-8ccc-cccccccccccc.tok", []byte("tok"))
	put(t, primary, "other/leave-me", []byte("no"))
	before := tokenKeys(t, primary)

	res, err := Run(ctx, st, primary, dst, kr, Options{Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	if !bytes.Equal(dst.Raw("repos/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/packs/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.pack"), []byte("pack")) {
		t.Fatal("pack was not copied")
	}
	if !bytes.Equal(dst.Raw("tokens/cccccccc-cccc-4ccc-8ccc-cccccccccccc.tok"), []byte("tok")) {
		t.Fatal("token object was not copied")
	}
	if dst.Raw("other/leave-me") != nil {
		t.Fatal("key outside the prefixes was copied")
	}
	if primary.Raw("other/leave-me") == nil {
		t.Fatal("primary lost the outside key")
	}
	after := tokenKeys(t, primary)
	if len(after) != len(before)+1 {
		t.Fatalf("token backups before %d after %d", len(before), len(after))
	}
	var fresh string
	for _, key := range after {
		if !containsKey(before, key) {
			fresh = key
		}
	}
	if fresh == "" || !bytes.Equal(primary.Raw(fresh), dst.Raw(fresh)) {
		t.Fatal("run did not write the same token backup to both buckets")
	}
	second, err := Run(ctx, st, primary, dst, kr, Options{Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	if second.Copied != 0 || second.Failed != 0 || second.Skipped == 0 {
		t.Fatalf("second %+v", second)
	}
}

func TestWrongSizeAndBackupOnly(t *testing.T) {
	testdb.BackupLock(t)
	st, kr := openBackup(t)
	primary := blob.NewMem()
	dst := blob.NewMem()
	key := "repos/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/packs/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.pack"
	put(t, primary, key, []byte("pack-bytes"))
	put(t, dst, key, []byte("short"))
	only := "repos/dddddddd-dddd-4ddd-8ddd-dddddddddddd/packs/eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee.pack"
	put(t, dst, only, []byte("keep"))
	res, err := Run(context.Background(), st, primary, dst, kr, Options{Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Copied < 1 || !bytes.Equal(dst.Raw(key), []byte("pack-bytes")) {
		t.Fatalf("not recopied %+v", res)
	}
	if !bytes.Equal(dst.Raw(only), []byte("keep")) {
		t.Fatal("backup-only key was deleted")
	}
}

func TestFailingPut(t *testing.T) {
	testdb.BackupLock(t)
	st, kr := openBackup(t)
	primary := blob.NewMem()
	dst := blob.NewMem()
	bad := "repos/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/packs/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.pack"
	good := "repos/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/packs/cccccccc-cccc-4ccc-8ccc-cccccccccccc.idx"
	put(t, primary, bad, []byte("bad"))
	put(t, primary, good, []byte("good"))
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	res, err := Run(context.Background(), st, primary, &failKey{Store: dst, key: bad}, kr, Options{Log: log})
	if !errors.Is(err, ErrCopyFailed) {
		t.Fatalf("err %v", err)
	}
	if res.Failed != 1 || len(res.FailedKeys) != 1 || res.FailedKeys[0] != bad {
		t.Fatalf("%+v", res)
	}
	if !bytes.Equal(dst.Raw(good), []byte("good")) {
		t.Fatal("other key was not copied")
	}
	if !stringsContains(logs.String(), bad) {
		t.Fatalf("log %s", logs.String())
	}
}

func TestOneRunAtATime(t *testing.T) {
	testdb.BackupLock(t)
	st, kr := openBackup(t)
	primary := blob.NewMem()
	dst := blob.NewMem()
	put(t, primary, "repos/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/packs/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.pack", []byte("pack"))
	gate := &blockList{Store: primary, entered: make(chan struct{}), release: make(chan struct{})}
	type outcome struct {
		res Result
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		res, err := Run(context.Background(), st, gate, dst, kr, Options{Log: discardLog()})
		first <- outcome{res, err}
	}()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first run did not start the copy")
	}
	start := time.Now()
	res, err := Run(context.Background(), st, primary, dst, kr, Options{Log: discardLog()})
	if err != nil || res.Ran {
		t.Fatalf("second ran %+v err %v", res, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("second blocked for %s", time.Since(start))
	}
	close(gate.release)
	got := <-first
	if got.err != nil || !got.res.Ran || got.res.Copied < 1 {
		t.Fatalf("first %+v err %v", got.res, got.err)
	}
}

func TestTokenBackupBeforeCopy(t *testing.T) {
	testdb.BackupLock(t)
	st, kr := openBackup(t)
	primary := blob.NewMem()
	dst := blob.NewMem()
	put(t, primary, "repos/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/packs/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.pack", []byte("pack"))
	order := &orderList{Store: primary, dst: dst}
	res, err := Run(context.Background(), st, order, dst, kr, Options{Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	if !order.saw || !res.Ran {
		t.Fatalf("saw %v %+v", order.saw, res)
	}
	if len(tokenKeys(t, primary)) != 1 || len(tokenKeys(t, dst)) != 1 {
		t.Fatal("token backup missing")
	}
	best, _, err := tokenbackup.Newest(context.Background(), dst, kr)
	if err != nil {
		t.Fatal(err)
	}
	if len(best.Tokens) != 0 {
		t.Fatalf("tokens %d", len(best.Tokens))
	}
}

type failKey struct {
	blob.Store
	key string
}

func (f *failKey) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if key == f.key {
		return errors.New("put failed")
	}
	return f.Store.Put(ctx, key, r, size)
}

type blockList struct {
	blob.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockList) List(ctx context.Context, prefix string, fn func(blob.Info) error) error {
	if prefix == "repos/" {
		b.once.Do(func() {
			close(b.entered)
			<-b.release
		})
	}
	return b.Store.List(ctx, prefix, fn)
}

type orderList struct {
	blob.Store
	dst *blob.Mem
	saw bool
}

func (o *orderList) List(ctx context.Context, prefix string, fn func(blob.Info) error) error {
	if prefix == "repos/" {
		n := 0
		if err := o.dst.List(ctx, "tokens/", func(blob.Info) error {
			n++
			return nil
		}); err != nil {
			return err
		}
		if n == 0 {
			return errors.New("token backup was not on the backup bucket before the copy")
		}
		o.saw = true
	}
	return o.Store.List(ctx, prefix, fn)
}

func openBackup(t *testing.T) (*store.Store, *objcrypt.Keyring) {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background(), discardLog()); err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	copy(key[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	return st, kr
}

func put(t *testing.T, bucket blob.Store, key string, body []byte) {
	t.Helper()
	if err := bucket.Put(context.Background(), key, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
}

func tokenKeys(t *testing.T, bucket blob.Store) []string {
	t.Helper()
	var keys []string
	err := bucket.List(context.Background(), "tokens/", func(info blob.Info) error {
		if _, ok := tokenbackup.ParseKey(info.Key); ok {
			keys = append(keys, info.Key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func containsKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func stringsContains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
