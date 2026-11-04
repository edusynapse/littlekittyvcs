package blob

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	mrand "math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
)

func TestS3(t *testing.T) {
	endpoint := os.Getenv("LKV_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("LKV_TEST_S3_ENDPOINT is not set")
	}
	region := os.Getenv("LKV_TEST_S3_REGION")
	bucket := os.Getenv("LKV_TEST_S3_BUCKET")
	access := os.Getenv("LKV_TEST_S3_ACCESS_KEY")
	secret := os.Getenv("LKV_TEST_S3_SECRET_KEY")
	if region == "" || bucket == "" || access == "" || secret == "" {
		t.Fatal("LKV_TEST_S3_ENDPOINT is set but region, bucket, or keys are empty")
	}
	pathStyle := false
	switch strings.ToLower(os.Getenv("LKV_TEST_S3_PATH_STYLE")) {
	case "1", "true", "yes":
		pathStyle = true
	}

	ctx := context.Background()
	store, err := NewS3(ctx, S3Config{
		Endpoint:  endpoint,
		Region:    region,
		Bucket:    bucket,
		AccessKey: access,
		SecretKey: secret,
		PathStyle: pathStyle,
	})
	if err != nil {
		t.Fatal(err)
	}

	var rnd [8]byte
	if _, err := crand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "lkv-test/" + hex.EncodeToString(rnd[:]) + "/"
	bigKey := prefix + "big"
	smallKey := prefix + "small"
	encKey := prefix + "enc"
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, key := range []string{bigKey, smallKey, encKey} {
			_ = store.Delete(cctx, key)
		}
	})

	rng := mathrand(1)
	big := make([]byte, 40<<20)
	rng.Read(big)
	if err := store.Put(ctx, bigKey, bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatalf("put 40 MiB: %v", err)
	}
	info, err := store.Head(ctx, bigKey)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(big)) {
		t.Fatalf("Head size %d", info.Size)
	}
	const rangeOff = 20 << 20
	const rangeN = 1 << 20
	got, err := store.GetRange(ctx, bigKey, rangeOff, rangeN)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big[rangeOff:rangeOff+rangeN]) {
		t.Fatal("40 MiB range mismatch")
	}

	small := make([]byte, 1024)
	rng.Read(small)
	if err := store.Put(ctx, smallKey, bytes.NewReader(small), int64(len(small))); err != nil {
		t.Fatalf("put 1 KiB: %v", err)
	}
	got, err = store.GetRange(ctx, smallKey, 0, int64(len(small)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, small) {
		t.Fatal("1 KiB mismatch")
	}

	var listed []string
	if err := store.List(ctx, prefix, func(info Info) error {
		listed = append(listed, info.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || !hasKey(listed, bigKey) || !hasKey(listed, smallKey) {
		t.Fatalf("list %v", listed)
	}

	plain := make([]byte, 20<<20)
	rng.Read(plain)
	var master [32]byte
	for i := range master {
		master[i] = 0x11
	}
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	var binding objcrypt.Binding
	binding.Kind = objcrypt.KindPack
	binding.RepoID[0] = 1
	binding.ObjectID[0] = 2
	var enc bytes.Buffer
	w, err := objcrypt.NewWriter(&enc, kr, binding, objcrypt.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, encKey, bytes.NewReader(enc.Bytes()), int64(enc.Len())); err != nil {
		t.Fatalf("put encrypted: %v", err)
	}
	rd, err := objcrypt.Open(ctx, store, encKey, int64(enc.Len()), kr, binding)
	if err != nil {
		t.Fatal(err)
	}
	chunkOff := int64(3 * objcrypt.DefaultChunkSize)
	chunk := make([]byte, objcrypt.DefaultChunkSize)
	n, err := rd.ReadAt(chunk, chunkOff)
	if err != nil || n != len(chunk) {
		t.Fatalf("read chunk 3: n=%d err=%v", n, err)
	}
	if !bytes.Equal(chunk, plain[chunkOff:chunkOff+int64(len(chunk))]) {
		t.Fatal("chunk 3 mismatch")
	}

	for _, key := range []string{bigKey, smallKey, encKey} {
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("delete %s: %v", key, err)
		}
		if _, err := store.Head(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("head %s after delete: %v", key, err)
		}
	}
}

func hasKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

func mathrand(seed int64) *mrand.Rand {
	return mrand.New(mrand.NewSource(seed))
}
