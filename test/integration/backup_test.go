package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/backup"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/gc"
	"github.com/edusynapse/littlekittyvcs/internal/httpapi"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
	"github.com/edusynapse/littlekittyvcs/internal/tokenbackup"
)

func TestWholeBucketRecovery(t *testing.T) {
	testdb.BackupLock(t)
	h := newHarness(t, deny(), nil)
	repo := h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("one\n"))
	h.commitAll(dir, "one")
	stdout, stderr := h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	t.Logf("EVIDENCE git push HEAD:refs/heads/main stdout=%q stderr=%q", redact(stdout), redact(stderr))

	h.must(dir, "checkout", "-b", "topic")
	writeFile(t, filepath.Join(dir, "topic.txt"), []byte("topic\n"))
	h.commitAll(dir, "topic")
	stdout, stderr = h.push(dir, h.remote("site"), "HEAD:refs/heads/topic")
	t.Logf("EVIDENCE git push HEAD:refs/heads/topic stdout=%q stderr=%q", redact(stdout), redact(stderr))

	h.must(dir, "checkout", "main")
	h.must(dir, "tag", "v1")
	stdout, stderr = h.push(dir, h.remote("site"), "refs/tags/v1")
	t.Logf("EVIDENCE git push refs/tags/v1 stdout=%q stderr=%q", redact(stdout), redact(stderr))
	pre := h.rev(dir, "HEAD")

	repoID := repo.ID
	limited := h.insertToken("limited", true, &repoID)
	dst := blob.NewMem()
	var bout bytes.Buffer
	res, err := backup.Run(context.Background(), h.st, h.mem, dst, h.kr, backup.Options{Log: h.log, Out: &bout})
	t.Logf("EVIDENCE littlekittyvcs backup %s", strings.TrimSpace(bout.String()))
	if err != nil || !res.Ran || res.Failed != 0 || res.Copied == 0 {
		t.Fatalf("backup %+v %v", res, err)
	}

	writeFile(t, filepath.Join(dir, "later.txt"), []byte("later\n"))
	later := h.commitAll(dir, "later")
	stdout, stderr = h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	t.Logf("EVIDENCE git push later stdout=%q stderr=%q", redact(stdout), redact(stderr))

	wipeBucket(t, h.mem)
	st := h.openEmpty()
	var rout bytes.Buffer
	failed, err := snapshot.Restore(context.Background(), st, dst, h.kr, snapshot.Options{Out: &rout})
	t.Logf("EVIDENCE littlekittyvcs restore\n%s", rout.String())
	if err != nil || failed != 0 {
		t.Fatalf("restore failed=%d %v", failed, err)
	}
	tr, err := tokenbackup.Restore(context.Background(), st, dst, h.kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("EVIDENCE littlekittyvcs restore-tokens %s", tokenbackup.FormatSummary(tr))
	if tr.Restored != 2 || tr.Present != 0 || tr.NoRepository != 0 {
		t.Fatalf("tokens %+v", tr)
	}

	srv := httptest.NewServer(httpapi.New(h.log, httpapi.Deps{
		Store: st, Bucket: dst, Keyring: h.kr, Index: objindex.New(),
	}, deny()))
	t.Cleanup(srv.Close)
	remote := srv.URL + "/site.git"
	clone := h.cloneToken(remote, limited)
	head := h.rev(clone, "HEAD")
	tag := h.rev(clone, "refs/tags/v1")
	topic := h.rev(clone, "refs/remotes/origin/topic")
	note, err := os.ReadFile(filepath.Join(clone, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(clone, "later.txt")); !os.IsNotExist(statErr) {
		t.Fatal("later.txt present in the restored clone")
	}
	if head != pre || tag != pre || string(note) != "one\n" || topic == "" {
		t.Fatalf("clone HEAD=%s tag=%s topic=%s note=%q want %s", head, tag, topic, note, pre)
	}
	t.Logf("EVIDENCE git clone HEAD=%s tag=%s topic=%s later.txt absent", head, tag, topic)

	stdout, stderr = h.must(dir, "-c", bearer(limited), "push", remote, "HEAD:refs/heads/main")
	t.Logf("EVIDENCE git push later again stdout=%q stderr=%q", redact(stdout), redact(stderr))
	h.must(clone, "-c", bearer(limited), "fetch", "origin")
	got := h.rev(clone, "refs/remotes/origin/main")
	blobID := h.rev(clone, "refs/remotes/origin/main:later.txt")
	if got != later || blobID == "" {
		t.Fatalf("origin/main %s want %s later.txt %s", got, later, blobID)
	}
	t.Logf("EVIDENCE git fetch origin/main=%s later=%s", got, later)
}

func TestRevocationSurvivesRestore(t *testing.T) {
	h := newHarness(t, deny(), nil)
	h.createRepo("site")
	dir := h.initRepo("main")
	writeFile(t, filepath.Join(dir, "note.txt"), []byte("one\n"))
	h.commitAll(dir, "one")
	h.push(dir, h.remote("site"), "HEAD:refs/heads/main")
	admin := h.insertAdmin("ops")

	code, body := httpJSON(t, http.MethodPost, h.srv.URL+"/admin/tokens", admin, `{"label":"doomed","write":true,"repo":"site"}`)
	if code != http.StatusCreated {
		t.Fatalf("create %d %s", code, body)
	}
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Token == "" {
		t.Fatal("create response")
	}
	time.Sleep(5 * time.Millisecond)
	code, body = httpDo(t, http.MethodDelete, h.srv.URL+"/admin/tokens/"+created.ID, admin)
	if code != http.StatusNoContent {
		t.Fatalf("revoke %d %s", code, body)
	}

	best, _, err := tokenbackup.Newest(context.Background(), h.bucket, h.kr)
	if err != nil {
		t.Fatal(err)
	}
	revoked := auth.Hash(created.Token)
	for _, row := range best.Tokens {
		if row.Hash == revoked {
			t.Fatal("newest backup still has the revoked token")
		}
	}

	st := h.openEmpty()
	var rout bytes.Buffer
	failed, err := snapshot.Restore(context.Background(), st, h.bucket, h.kr, snapshot.Options{Out: &rout})
	if err != nil || failed != 0 {
		t.Fatalf("restore failed=%d %v\n%s", failed, err, rout.String())
	}
	if _, err := tokenbackup.Restore(context.Background(), st, h.bucket, h.kr); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(h.log, httpapi.Deps{
		Store: st, Bucket: h.bucket, Keyring: h.kr, Index: objindex.New(),
	}, deny()))
	t.Cleanup(srv.Close)
	code, body = httpDo(t, http.MethodGet, srv.URL+"/site.git/info/refs?service=git-upload-pack", created.Token)
	t.Logf("EVIDENCE revoked info/refs status=%d body=%q", code, body)
	if code != http.StatusUnauthorized || body != "authentication required\n" {
		t.Fatalf("status %d body %q", code, body)
	}
}

func TestGCLeavesTokenBackups(t *testing.T) {
	h := newHarness(t, deny(), nil)
	keep := pushNamed(h, "keep", "keep.txt", "keep\n")
	gone := pushNamed(h, "gone", "gone.txt", "gone\n")
	admin := h.insertAdmin("ops")
	code, body := httpDo(t, http.MethodDelete, h.srv.URL+"/admin/repos/gone", admin)
	if code != http.StatusNoContent {
		t.Fatalf("delete %d %s", code, body)
	}
	marker := markerKey(t, h.bucket, h.kr, gone.ID)
	beforeTokens := listKeys(t, h.bucket, "tokens/")
	if len(beforeTokens) == 0 {
		t.Fatal("no token backup")
	}
	beforeKeep := keysUnder(t, h.bucket, keep.ID)
	if len(beforeKeep) == 0 {
		t.Fatal("live repo has no keys")
	}

	var out bytes.Buffer
	res, err := gc.Run(context.Background(), h.st, h.bucket, h.kr, gc.Options{
		Now: func() time.Time { return time.Now().Add(48 * time.Hour) },
		Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertGCText(t, out.String())
	if res.Deleted == 0 {
		t.Fatalf("gc deleted nothing\n%s", out.String())
	}
	if keys := listKeys(t, h.bucket, "tokens/"); !sameKeys(beforeTokens, keys) {
		t.Fatalf("tokens before %v after %v", beforeTokens, keys)
	}
	if _, err := h.bucket.Head(context.Background(), marker); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("marker remains %v", err)
	}
	if len(keysUnder(t, h.bucket, gone.ID)) != 0 {
		t.Fatal("deleted repository keys remain")
	}
	if !sameKeys(beforeKeep, keysUnder(t, h.bucket, keep.ID)) {
		t.Fatal("live repository keys changed")
	}
}

func TestGCBackupBucket(t *testing.T) {
	testdb.BackupLock(t)
	h := newHarness(t, deny(), nil)
	keep := pushNamed(h, "keep", "keep.txt", "keep\n")
	gone := pushNamed(h, "gone", "gone.txt", "gone\n")
	admin := h.insertAdmin("ops")
	code, body := httpDo(t, http.MethodDelete, h.srv.URL+"/admin/repos/gone", admin)
	if code != http.StatusNoContent {
		t.Fatalf("delete %d %s", code, body)
	}
	primaryGone := keysUnder(t, h.bucket, gone.ID)
	if len(primaryGone) == 0 {
		t.Fatal("primary has no deleted-repository keys")
	}
	dst := blob.NewMem()
	res, err := backup.Run(context.Background(), h.st, h.mem, dst, h.kr, backup.Options{Log: h.log})
	if err != nil || !res.Ran || res.Failed != 0 {
		t.Fatalf("backup %+v %v", res, err)
	}
	keepBefore := keysUnder(t, dst, keep.ID)
	if len(keepBefore) == 0 || len(keysUnder(t, dst, gone.ID)) == 0 || len(listKeys(t, dst, "tokens/")) == 0 {
		t.Fatal("backup bucket missing keys")
	}

	var out bytes.Buffer
	gres, err := gc.Run(context.Background(), h.st, dst, h.kr, gc.Options{
		Now: func() time.Time { return time.Now().Add(48 * time.Hour) },
		Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertGCText(t, out.String())
	if gres.Deleted == 0 {
		t.Fatalf("gc deleted nothing\n%s", out.String())
	}
	if len(keysUnder(t, dst, gone.ID)) != 0 {
		t.Fatal("deleted repository remains on the backup bucket")
	}
	if !sameKeys(keepBefore, keysUnder(t, dst, keep.ID)) {
		t.Fatal("other repository changed on the backup bucket")
	}
	if len(listKeys(t, dst, "tokens/")) == 0 {
		t.Fatal("token backups removed")
	}
	if !sameKeys(primaryGone, keysUnder(t, h.bucket, gone.ID)) {
		t.Fatal("primary bucket changed")
	}
}

func TestScheduleCopiesPack(t *testing.T) {
	testdb.BackupLock(t)
	h := newHarness(t, deny(), nil)
	pushNamed(h, "site", "note.txt", "one\n")
	packs := h.packs("site")
	if len(packs) != 1 {
		t.Fatalf("packs %d", len(packs))
	}
	packKey := packs[0].PackKey
	dst := blob.NewMem()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		waitBackupIdle(t, h.st)
	})
	backup.Schedule(ctx, 200*time.Millisecond, h.st, h.bucket, dst, h.kr, h.log)
	time.Sleep(40 * time.Millisecond)
	if dst.Raw(packKey) != nil {
		t.Fatal("pack copied before the first interval")
	}
	deadline := time.Now().Add(3 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		got = dst.Raw(packKey)
		if got != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("pack was not copied")
	}
	if !bytes.Equal(got, h.mem.Raw(packKey)) {
		t.Fatal("pack bytes differ")
	}
}

func TestBackupRustfs(t *testing.T) {
	if os.Getenv("LKV_TEST_DATABASE_URL") == "" {
		t.Skip("LKV_TEST_DATABASE_URL is not set")
	}
	for _, name := range []string{
		"LKV_TEST_S3_ENDPOINT",
		"LKV_TEST_S3_REGION",
		"LKV_TEST_S3_ACCESS_KEY",
		"LKV_TEST_S3_SECRET_KEY",
	} {
		if os.Getenv(name) == "" {
			t.Skip(name + " is not set")
		}
	}
	testdb.BackupLock(t)
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	hexSuffix := hex.EncodeToString(suffix[:])
	srcName := "lkv-piece08-src-" + hexSuffix
	dstName := "lkv-piece08-backup-" + hexSuffix
	client := rustfsAdmin()
	sweepPiece08Buckets(t, client)
	var made []madeBucket
	t.Cleanup(func() {
		cleanRustfsBuckets(t, client, made)
	})
	src := makeRustfsBucket(t, client, srcName, &made)
	dst := makeRustfsBucket(t, client, dstName, &made)
	t.Logf("EVIDENCE rustfs buckets src=%s backup=%s", srcName, dstName)

	h := newHarness(t, deny(), src)
	const outside = "other/primary-only"
	body := []byte("primary-only")
	if err := src.Put(context.Background(), outside, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	pushNamed(h, "site", "note.txt", "one\n")

	var first bytes.Buffer
	res, err := backup.Run(context.Background(), h.st, src, dst, h.kr, backup.Options{Log: h.log, Out: &first})
	t.Logf("EVIDENCE rustfs backup %s", strings.TrimSpace(first.String()))
	if err != nil || !res.Ran || res.Failed != 0 || res.Copied == 0 {
		t.Fatalf("backup %+v %v", res, err)
	}
	assertPrefixCopy(t, src, dst)
	if _, err := dst.Head(context.Background(), outside); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("outside key on backup %v", err)
	}

	var second bytes.Buffer
	again, err := backup.Run(context.Background(), h.st, src, dst, h.kr, backup.Options{Log: h.log, Out: &second})
	t.Logf("EVIDENCE rustfs backup again %s", strings.TrimSpace(second.String()))
	if err != nil || !again.Ran || again.Copied != 0 || again.Failed != 0 {
		t.Fatalf("second %+v %v", again, err)
	}
}

func pushNamed(h *harness, name, file, body string) store.Repo {
	h.t.Helper()
	repo := h.createRepo(name)
	dir := h.initRepo("main")
	writeFile(h.t, filepath.Join(dir, file), []byte(body))
	h.commitAll(dir, "one")
	h.push(dir, h.remote(name), "HEAD:refs/heads/main")
	return repo
}

func wipeBucket(t *testing.T, bucket blob.Store) {
	t.Helper()
	for _, key := range listKeys(t, bucket, "") {
		if err := bucket.Delete(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	if keys := listKeys(t, bucket, ""); len(keys) != 0 {
		t.Fatalf("bucket not empty %v", keys)
	}
}

func listKeys(t *testing.T, bucket blob.Store, prefix string) []string {
	t.Helper()
	var keys []string
	err := bucket.List(context.Background(), prefix, func(info blob.Info) error {
		keys = append(keys, info.Key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	return keys
}

func httpJSON(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(got)
}

func waitBackupIdle(t *testing.T, st *store.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ran, err := st.WithAdvisoryLock(context.Background(), backup.LockKey, func(context.Context) error {
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if ran {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("backup lock still held")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertPrefixCopy(t *testing.T, src, dst blob.Store) {
	t.Helper()
	for _, prefix := range []string{"repos/", "tokens/"} {
		srcKeys := listKeys(t, src, prefix)
		dstKeys := listKeys(t, dst, prefix)
		if len(srcKeys) == 0 || !sameKeys(srcKeys, dstKeys) {
			t.Fatalf("%s src %v dst %v", prefix, srcKeys, dstKeys)
		}
		for _, key := range srcKeys {
			left, lerr := src.Head(context.Background(), key)
			right, rerr := dst.Head(context.Background(), key)
			if lerr != nil || rerr != nil || left.Size != right.Size || left.Size <= 0 {
				t.Fatalf("%s size %d %d %v %v", key, left.Size, right.Size, lerr, rerr)
			}
			a, err := src.GetRange(context.Background(), key, 0, left.Size)
			if err != nil {
				t.Fatal(err)
			}
			b, err := dst.GetRange(context.Background(), key, 0, right.Size)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Fatalf("bytes differ %s", key)
			}
		}
	}
}

type madeBucket struct {
	name   string
	store  *blob.S3
	before map[string]bool
}

func sweepPiece08Buckets(t *testing.T, client *s3.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("list buckets %s", redactErr(err))
	}
	for _, b := range out.Buckets {
		name := aws.ToString(b.Name)
		if !strings.HasPrefix(name, "lkv-piece08-") {
			continue
		}
		t.Logf("EVIDENCE sweep leftover bucket %s", name)
		pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(name)})
		for pager.HasMorePages() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				t.Fatalf("sweep list %s %s", name, redactErr(err))
			}
			for _, obj := range page.Contents {
				key := aws.ToString(obj.Key)
				_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(name),
					Key:    aws.String(key),
				})
				if err != nil {
					t.Fatalf("sweep delete %s %s", key, redactErr(err))
				}
			}
		}
		if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)}); err != nil {
			t.Fatalf("sweep DeleteBucket %s %s", name, redactErr(err))
		}
	}
}

func rustfsAdmin() *s3.Client {
	pathStyle := false
	switch strings.ToLower(os.Getenv("LKV_TEST_S3_PATH_STYLE")) {
	case "1", "true", "yes":
		pathStyle = true
	}
	endpoint := os.Getenv("LKV_TEST_S3_ENDPOINT")
	cfg := aws.Config{
		Region:                     os.Getenv("LKV_TEST_S3_REGION"),
		BaseEndpoint:               aws.String(endpoint),
		Credentials:                credentials.NewStaticCredentialsProvider(os.Getenv("LKV_TEST_S3_ACCESS_KEY"), os.Getenv("LKV_TEST_S3_SECRET_KEY"), ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = pathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
}

func makeRustfsBucket(t *testing.T, client *s3.Client, name string, made *[]madeBucket) *blob.S3 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	input := &s3.CreateBucketInput{Bucket: aws.String(name)}
	region := os.Getenv("LKV_TEST_S3_REGION")
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(region),
		}
	}
	_, err := client.CreateBucket(ctx, input)
	if err != nil && !bucketExistsCode(err) {
		t.Skipf("CreateBucket %s", apiCode(err))
	}
	pathStyle := false
	switch strings.ToLower(os.Getenv("LKV_TEST_S3_PATH_STYLE")) {
	case "1", "true", "yes":
		pathStyle = true
	}
	store, err := blob.NewS3(ctx, blob.S3Config{
		Endpoint:  os.Getenv("LKV_TEST_S3_ENDPOINT"),
		Region:    region,
		Bucket:    name,
		AccessKey: os.Getenv("LKV_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("LKV_TEST_S3_SECRET_KEY"),
		PathStyle: pathStyle,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]bool{}
	if err := store.List(ctx, "", func(info blob.Info) error {
		before[info.Key] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	*made = append(*made, madeBucket{name: name, store: store, before: before})
	return store
}

func cleanRustfsBuckets(t *testing.T, client *s3.Client, made []madeBucket) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, b := range made {
		var keys []string
		err := b.store.List(ctx, "", func(info blob.Info) error {
			if !b.before[info.Key] {
				keys = append(keys, info.Key)
			}
			return nil
		})
		if err != nil {
			t.Errorf("list %s %s", b.name, redactErr(err))
			continue
		}
		for _, key := range keys {
			keyCtx, keyCancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := client.DeleteObject(keyCtx, &s3.DeleteObjectInput{
				Bucket: aws.String(b.name),
				Key:    aws.String(key),
			})
			keyCancel()
			if err != nil {
				t.Errorf("delete %s %s", key, redactErr(err))
			}
		}
		var delErr error
		for try := 0; try < 5; try++ {
			_, delErr = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(b.name)})
			if delErr == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if delErr != nil {
			t.Errorf("DeleteBucket %s %s", b.name, redactErr(delErr))
		}
	}
}

func redactErr(err error) string {
	if err == nil {
		return ""
	}
	s := fmt.Sprintf("%T: %s", err, apiCode(err))
	if errors.Is(err, context.DeadlineExceeded) {
		s += " deadline"
	}
	return s
}

func bucketExistsCode(err error) bool {
	switch apiCode(err) {
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return true
	default:
		return false
	}
}

func apiCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return "error"
}
