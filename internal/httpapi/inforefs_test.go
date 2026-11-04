package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestInfoRefs(t *testing.T) {
	api := newAPI(t, Config{})
	ctx := context.Background()
	if _, err := api.st.CreateRepo(ctx, mustStoreID(t), "site"); err != nil {
		t.Fatal(err)
	}
	other, err := api.st.CreateRepo(ctx, mustStoreID(t), "other")
	if err != nil {
		t.Fatal(err)
	}
	rw := api.insertToken("rw", true, false, nil)
	ro := api.insertToken("ro", false, false, nil)
	limited := api.insertToken("limited", true, false, &other.ID)
	anon := New(api.log, Deps{Store: api.st}, Config{AnonymousRead: true})
	auto := New(api.log, Deps{Store: api.st}, Config{AutoCreate: true})
	basicRO := "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+ro))
	upload := "/site.git/info/refs?service=git-upload-pack"
	receive := "/site.git/info/refs?service=git-receive-pack"

	cases := []struct {
		name   string
		h      http.Handler
		path   string
		auth   string
		status int
		ctype  string
	}{
		{"no token", api.h, upload, "", 401, ""},
		{"anonymous read", anon, upload, "", 200, "application/x-git-upload-pack-advertisement"},
		{"wrong token still 401", anon, upload, "Bearer lkv_wrong", 401, ""},
		{"basic read-only", api.h, upload, basicRO, 200, "application/x-git-upload-pack-advertisement"},
		{"no git suffix", api.h, "/site/info/refs?service=git-upload-pack", "Bearer " + ro, 200, "application/x-git-upload-pack-advertisement"},
		{"no service", api.h, "/site.git/info/refs", "Bearer " + ro, 403, ""},
		{"archive", api.h, "/site.git/info/refs?service=git-upload-archive", "Bearer " + ro, 403, ""},
		{"push without token", anon, receive, "", 401, ""},
		{"push read-only", api.h, receive, "Bearer " + ro, 403, ""},
		{"push read-write", api.h, receive, "Bearer " + rw, 200, "application/x-git-receive-pack-advertisement"},
		{"push other repo", api.h, receive, "Bearer " + limited, 404, ""},
		{"missing upload", api.h, "/nosuch.git/info/refs?service=git-upload-pack", "Bearer " + rw, 404, ""},
		{"bad name", api.h, "/Bad.git/info/refs?service=git-upload-pack", "Bearer " + rw, 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := api.callHandler(tc.h, http.MethodGet, tc.path, tc.auth)
			if rec.Code != tc.status {
				t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
			}
			if tc.status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != auth.Challenge {
				t.Fatalf("challenge %q", rec.Header().Get("WWW-Authenticate"))
			}
			if tc.status != http.StatusOK {
				return
			}
			if rec.Header().Get("Content-Type") != tc.ctype {
				t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get("Cache-Control") != "no-cache, max-age=0, must-revalidate" ||
				rec.Header().Get("Pragma") != "no-cache" ||
				rec.Header().Get("Expires") != "Fri, 01 Jan 1980 00:00:00 GMT" {
				t.Fatalf("cache headers %#v", rec.Header())
			}
			service := "git-upload-pack"
			if strings.Contains(tc.ctype, "receive") {
				service = "git-receive-pack"
			}
			line := fmt.Sprintf("%04x# service=%s\n", len("# service="+service+"\n")+4, service)
			if !strings.HasPrefix(rec.Body.String(), line) {
				t.Fatalf("body %q", rec.Body.String())
			}
		})
	}

	rec := api.callHandler(auto, http.MethodGet, "/nosuch.git/info/refs?service=git-receive-pack", "Bearer "+rw)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-create %d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "capabilities^{}") {
		t.Fatalf("expected an empty advertisement %q", rec.Body.String())
	}
	conn, err := pgx.Connect(ctx, api.raw)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int
	if err := conn.QueryRow(ctx, `select count(*) from repositories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("repositories %d", n)
	}
	rec = api.callHandler(auto, http.MethodGet, "/nosuch.git/info/refs?service=git-receive-pack", "Bearer "+limited)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("limited auto-create %d", rec.Code)
	}

	logs := api.logs.String()
	for _, secret := range []string{rw, ro, limited, "lkv_wrong"} {
		if strings.Contains(logs, secret) {
			t.Fatal("log contains a token")
		}
	}
}

func mustStoreID(t *testing.T) [16]byte {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
