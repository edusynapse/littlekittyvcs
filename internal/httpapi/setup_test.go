package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/testdb"
)

type testAPI struct {
	t    *testing.T
	raw  string
	st   *store.Store
	log  *slog.Logger
	logs *bytes.Buffer
	h    http.Handler
}

func newAPI(t *testing.T, cfg Config) *testAPI {
	t.Helper()
	raw := testdb.URL(t)
	st, err := store.Open(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return &testAPI{
		t: t, raw: raw, st: st, log: log, logs: &logs,
		h: New(log, Deps{Store: st}, cfg),
	}
}

func (a *testAPI) insertToken(label string, write, admin bool, repoID *[16]byte) string {
	a.t.Helper()
	plain, hash, err := auth.Generate()
	if err != nil {
		a.t.Fatal(err)
	}
	id, err := store.NewID()
	if err != nil {
		a.t.Fatal(err)
	}
	err = a.st.InsertToken(context.Background(), hash, store.TokenRow{
		ID: id, Label: label, Write: write, Admin: admin, RepoID: repoID,
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return plain
}

func (a *testAPI) call(method, path, token, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if strings.Contains(path, "/admin/") && rec.Header().Get("Cache-Control") != "no-store" {
		a.t.Fatalf("%s %s cache %q", method, path, rec.Header().Get("Cache-Control"))
	}
	return rec
}

func (a *testAPI) callHandler(h http.Handler, method, path, header string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
