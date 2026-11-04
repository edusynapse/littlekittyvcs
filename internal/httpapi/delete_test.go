package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestDeleteRepoAccess(t *testing.T) {
	api := newAPI(t, Config{})
	rec := api.call(http.MethodDelete, "/admin/repos/missing", "", "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != auth.Challenge {
		t.Fatalf("no token %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	rw := api.insertToken("rw", true, false, nil)
	rec = api.call(http.MethodDelete, "/admin/repos/missing", rw, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin %d", rec.Code)
	}
	admin := api.insertToken("admin", true, true, nil)
	rec = api.call(http.MethodDelete, "/admin/repos/missing", admin, "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "repository not found") {
		t.Fatalf("missing %d %s", rec.Code, rec.Body.String())
	}
	rec = api.call(http.MethodDelete, "/admin/repos/Bad", admin, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("invalid name %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteRemovesStaleMarker(t *testing.T) {
	api := newAPI(t, Config{})
	ctx := context.Background()
	repo, err := api.st.CreateRepo(ctx, mustStoreID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	admin := api.insertToken("admin", true, true, nil)
	mem := blob.NewMem()
	kr := deleteKeyring(t)
	var oid [20]byte
	oid[0] = 0x42

	h := &handler{
		log: api.log, st: api.st, bucket: mem, kr: kr, index: objindex.New(),
	}
	h.beforeDeleteRow = func() {
		_, err := api.st.CommitPush(ctx, store.Push{
			RepoID:  repo.ID,
			Updates: []store.RefUpdate{{Name: "refs/heads/dev", New: &oid}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rec := deleteAs(t, h, admin, "site", context.Background())
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != `{"error":"repository changed during delete; try again"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got, refs, _, err := api.st.ReadRepo(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != repo.ID {
		t.Fatal("row gone")
	}
	var dev bool
	for _, ref := range refs {
		if ref.Name == "refs/heads/dev" && ref.ObjectID != nil && *ref.ObjectID == oid {
			dev = true
		}
	}
	if !dev {
		t.Fatal("new ref missing")
	}
	if markers := readMarkers(t, mem, kr, repo.ID); len(markers) != 0 {
		t.Fatalf("marker remained %d", len(markers))
	}

	other, err := api.st.CreateRepo(ctx, mustStoreID(t), "other")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := failDelete{Store: mem}
	h2 := &handler{
		log: api.log, st: api.st, bucket: wrapped, kr: kr, index: objindex.New(),
	}
	var oid2 [20]byte
	oid2[0] = 0x43
	h2.beforeDeleteRow = func() {
		_, err := api.st.CommitPush(ctx, store.Push{
			RepoID:  other.ID,
			Updates: []store.RefUpdate{{Name: "refs/heads/dev", New: &oid2}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rec = deleteAs(t, h2, admin, "other", context.Background())
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete failed path %d %s", rec.Code, rec.Body.String())
	}
	if _, err := api.st.RepoByName(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	found := readMarkers(t, mem, kr, other.ID)
	if len(found) != 1 {
		t.Fatalf("markers %d", len(found))
	}
	if !strings.Contains(api.logs.String(), found[0]) {
		t.Fatal("log does not carry the marker key")
	}
}

type failDelete struct{ blob.Store }

func (f failDelete) Delete(ctx context.Context, key string) error {
	return errors.New("blob: delete failed")
}

func TestDeleteContinuesAfterClientHangup(t *testing.T) {
	api := newAPI(t, Config{})
	ctx := context.Background()
	repo, err := api.st.CreateRepo(ctx, mustStoreID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	admin := api.insertToken("admin", true, true, nil)
	mem := blob.NewMem()
	h := &handler{
		log: api.log, st: api.st, bucket: mem, kr: deleteKeyring(t), index: objindex.New(),
	}
	reqCtx, cancel := context.WithCancel(context.Background())
	h.beforeDeleteRow = cancel
	rec := deleteAs(t, h, admin, "site", reqCtx)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := api.st.RepoByName(ctx, "site"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row remained: %v", err)
	}
	if markers := readMarkers(t, mem, h.kr, repo.ID); len(markers) != 1 {
		t.Fatalf("markers %d", len(markers))
	}
}

func deleteAs(t *testing.T, h *handler, token, name string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/admin/repos/"+name, nil).WithContext(ctx)
	req.SetPathValue("name", name)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.deleteRepo(rec, req)
	return rec
}

func readMarkers(t *testing.T, mem *blob.Mem, kr *objcrypt.Keyring, repoID [16]byte) []string {
	t.Helper()
	var keys []string
	err := mem.List(context.Background(), "repos/"+store.FormatID(repoID)+"/refs/", func(info blob.Info) error {
		got, err := snapshot.Read(context.Background(), mem, kr, repoID, snapIDFromKey(t, info.Key), info.Size)
		if err != nil {
			return err
		}
		if got.Deleted {
			keys = append(keys, info.Key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func snapIDFromKey(t *testing.T, key string) [16]byte {
	t.Helper()
	base := strings.TrimSuffix(key, ".snap")
	i := strings.LastIndex(base, "/")
	id, err := store.ParseID(base[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func deleteKeyring(t *testing.T) *objcrypt.Keyring {
	t.Helper()
	var master [32]byte
	copy(master[:], "littlekittyvcs-test-master-key!!")
	kr, err := objcrypt.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}
