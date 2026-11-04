package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/tokenbackup"
)

func TestTokenChangesWriteBackups(t *testing.T) {
	api := newAPI(t, Config{})
	ctx := context.Background()
	repo, err := api.st.CreateRepo(ctx, mustStoreID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	admin := api.insertToken("admin", true, true, nil)
	primary := blob.NewMem()
	extra := blob.NewMem()
	h := New(api.log, Deps{
		Store: api.st, Bucket: primary, BackupBucket: extra,
		Keyring: deleteKeyring(t), Index: objindex.New(),
	}, Config{})

	rec := callH(t, h, http.MethodPost, "/admin/tokens", admin, `{"label":"limited","write":true,"repo":"site"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	assertBackupMatchesDB(t, api.st, primary, extra)

	rec = callH(t, h, http.MethodDelete, "/admin/tokens/"+created.ID, admin, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete token %d %s", rec.Code, rec.Body.String())
	}
	time.Sleep(5 * time.Millisecond)
	assertBackupMatchesDB(t, api.st, primary, extra)
	if backupHasLabel(t, primary, "limited") {
		t.Fatal("deleted token is still in the newest backup")
	}

	rec = callH(t, h, http.MethodPost, "/admin/tokens", admin, `{"label":"limited","write":true,"repo":"site"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create again %d %s", rec.Code, rec.Body.String())
	}
	time.Sleep(5 * time.Millisecond)
	rec = callH(t, h, http.MethodDelete, "/admin/repos/site", admin, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete repo %d %s", rec.Code, rec.Body.String())
	}
	assertBackupMatchesDB(t, api.st, primary, extra)
	if backupHasLabel(t, primary, "limited") {
		t.Fatal("repository delete left the limited token in the backup")
	}
	if _, err := api.st.RepoByName(ctx, "site"); err == nil || repo.ID == ([16]byte{}) {
		t.Fatal("repository row")
	}
}

func TestTokenBackupFailureDoesNotFailRequest(t *testing.T) {
	api := newAPI(t, Config{})
	admin := api.insertToken("admin", true, true, nil)
	mem := blob.NewMem()
	h := New(api.log, Deps{
		Store: api.st, Bucket: failAllPut{Store: mem}, Keyring: deleteKeyring(t), Index: objindex.New(),
	}, Config{})
	const label = "label-not-in-the-error"
	rec := callH(t, h, http.MethodPost, "/admin/tokens", admin, `{"label":"`+label+`","write":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	sum := auth.Hash(created.Token)
	hash := hex.EncodeToString(sum[:])
	logs := api.logs.String()
	if strings.Contains(logs, created.Token) || strings.Contains(logs, hash) {
		t.Fatal("log contains the token or its hash")
	}
	var saw bool
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "token backup") {
			continue
		}
		saw = true
		if strings.Contains(line, label) || strings.Contains(line, hash) || strings.Contains(line, created.Token) {
			t.Fatalf("error line %s", line)
		}
		if !strings.Contains(line, "put failed") {
			t.Fatalf("error line %s", line)
		}
	}
	if !saw {
		t.Fatalf("log %s", logs)
	}
}

type failAllPut struct{ blob.Store }

func (f failAllPut) Put(context.Context, string, io.Reader, int64) error {
	return errors.New("put failed")
}

func callH(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertBackupMatchesDB(t *testing.T, st *store.Store, primary, extra *blob.Mem) {
	t.Helper()
	_, rows, err := st.ReadTokensForBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kr := deleteKeyring(t)
	for _, bucket := range []*blob.Mem{primary, extra} {
		best, _, err := tokenbackup.Newest(context.Background(), bucket, kr)
		if err != nil {
			t.Fatal(err)
		}
		if !sameTokens(rows, best.Tokens) {
			t.Fatalf("backup does not match the database")
		}
	}
	var primaryKeys, extraKeys []string
	_ = primary.List(context.Background(), "tokens/", func(info blob.Info) error {
		primaryKeys = append(primaryKeys, info.Key)
		return nil
	})
	_ = extra.List(context.Background(), "tokens/", func(info blob.Info) error {
		extraKeys = append(extraKeys, info.Key)
		return nil
	})
	if len(primaryKeys) == 0 || len(primaryKeys) != len(extraKeys) {
		t.Fatalf("primary %d extra %d", len(primaryKeys), len(extraKeys))
	}
	for _, key := range primaryKeys {
		if !bytesEqual(primary.Raw(key), extra.Raw(key)) {
			t.Fatalf("buckets differ at %s", key)
		}
	}
}

func backupHasLabel(t *testing.T, bucket blob.Store, label string) bool {
	t.Helper()
	best, _, err := tokenbackup.Newest(context.Background(), bucket, deleteKeyring(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range best.Tokens {
		if row.Label == label {
			return true
		}
	}
	return false
}

func sameTokens(a, b []store.TokenBackup) bool {
	if len(a) != len(b) {
		return false
	}
	for _, row := range a {
		var found bool
		for _, other := range b {
			if row.ID != other.ID || row.Hash != other.Hash || row.Label != other.Label || row.Write != other.Write || row.Admin != other.Admin {
				continue
			}
			if (row.RepoID == nil) != (other.RepoID == nil) {
				continue
			}
			if row.RepoID != nil && *row.RepoID != *other.RepoID {
				continue
			}
			if row.CreatedAt.UTC().UnixMicro() != other.CreatedAt.UTC().UnixMicro() {
				continue
			}
			found = true
		}
		if !found {
			return false
		}
	}
	return true
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
