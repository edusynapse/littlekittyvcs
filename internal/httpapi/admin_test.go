package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func TestAdminAuth(t *testing.T) {
	api := newAPI(t, Config{})
	rw := api.insertToken("rw", true, false, nil)
	routes := []struct{ method, path string }{
		{http.MethodPost, "/admin/repos"},
		{http.MethodPost, "/admin/tokens"},
		{http.MethodGet, "/admin/tokens"},
		{http.MethodDelete, "/admin/tokens/00000000-0000-4000-8000-000000000001"},
		{http.MethodDelete, "/admin/repos/missing"},
	}
	for _, route := range routes {
		rec := api.call(route.method, route.path, "", "")
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != auth.Challenge {
			t.Fatalf("%s %s %d %q", route.method, route.path, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
		rec = api.call(route.method, route.path, rw, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s %d", route.method, route.path, rec.Code)
		}
	}
}

func TestAdminRepos(t *testing.T) {
	api := newAPI(t, Config{PublicURL: "https://git.example.com/"})
	admin := api.insertToken("admin", true, true, nil)
	rec := api.call(http.MethodPost, "/admin/repos", admin, `{"name":"site"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ParseID(created.ID); err != nil || created.Name != "site" || created.URL != "https://git.example.com/site.git" {
		t.Fatalf("%+v %v", created, err)
	}
	plain := New(api.log, Deps{Store: api.st}, Config{})
	req := httptest.NewRequest(http.MethodPost, "/admin/repos", strings.NewReader(`{"name":"other"}`))
	req.Header.Set("Authorization", "Bearer "+admin)
	rec = httptest.NewRecorder()
	plain.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["url"]; ok {
		t.Fatalf("url key present: %s", rec.Body.String())
	}
	rec = api.call(http.MethodPost, "/admin/repos", admin, `{"name":"site"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("taken %d", rec.Code)
	}
	for _, body := range []string{
		`{"name":"site.git"}`,
		`{"name":"Bad"}`,
		`{"name":"x","extra":1}`,
		`{"name":"z"}{"name":"y"}`,
		strings.Repeat("a", 65*1024),
	} {
		rec = api.call(http.MethodPost, "/admin/repos", admin, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q status %d %s", trim(body), rec.Code, rec.Body.String())
		}
	}
}

func TestAdminCreateToken(t *testing.T) {
	api := newAPI(t, Config{})
	if _, err := api.st.CreateRepo(context.Background(), mustStoreID(t), "site"); err != nil {
		t.Fatal(err)
	}
	admin := api.insertToken("admin", true, true, nil)
	rec := api.call(http.MethodPost, "/admin/tokens", admin, `{"label":"ci","write":false,"repo":"site"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
		Repo  string `json:"repo"`
		Write bool   `json:"write"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Token, "lkv_") || created.Repo != "site" || created.Write {
		t.Fatalf("%+v", created)
	}
	upload := api.call(http.MethodGet, "/site.git/info/refs?service=git-upload-pack", created.Token, "")
	if upload.Code != http.StatusOK {
		t.Fatalf("upload %d", upload.Code)
	}
	push := api.call(http.MethodGet, "/site.git/info/refs?service=git-receive-pack", created.Token, "")
	if push.Code != http.StatusForbidden {
		t.Fatalf("push %d", push.Code)
	}
	for _, body := range []string{
		`{"label":"x","admin":true}`,
		`{"label":"x","admin":true,"write":true,"repo":"site"}`,
		`{"label":""}`,
		`{"label":"` + strings.Repeat("a", 201) + `"}`,
	} {
		rec = api.call(http.MethodPost, "/admin/tokens", admin, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d %s", trim(body), rec.Code, rec.Body.String())
		}
	}
	rec = api.call(http.MethodPost, "/admin/tokens", admin, `{"label":"x","repo":"missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing repo %d", rec.Code)
	}
	if strings.Contains(api.logs.String(), created.Token) {
		t.Fatal("log contains the created token")
	}
}

func TestAdminListTokens(t *testing.T) {
	api := newAPI(t, Config{})
	admin := api.insertToken("admin", true, true, nil)
	time.Sleep(20 * time.Millisecond)
	var plains []string
	for _, body := range []string{
		`{"label":"one","write":true}`,
		`{"label":"two","write":false}`,
	} {
		rec := api.call(http.MethodPost, "/admin/tokens", admin, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var created struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		plains = append(plains, created.Token)
		time.Sleep(20 * time.Millisecond)
	}
	rec := api.call(http.MethodGet, "/admin/tokens", admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Tokens []struct {
			Label     string `json:"label"`
			CreatedAt string `json:"created_at"`
			Repo      string `json:"repo"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tokens) != 3 || listed.Tokens[0].Label != "admin" || listed.Tokens[1].Label != "one" || listed.Tokens[2].Label != "two" {
		t.Fatalf("%+v", listed.Tokens)
	}
	body := rec.Body.String()
	secrets := append([]string{admin}, plains...)
	for _, plain := range secrets {
		if strings.Contains(body, plain) {
			t.Fatal("list contains a plaintext token")
		}
		sum := auth.Hash(plain)
		encodings := []string{
			hex.EncodeToString(sum[:]),
			base64.StdEncoding.EncodeToString(sum[:]),
			base64.RawStdEncoding.EncodeToString(sum[:]),
			base64.URLEncoding.EncodeToString(sum[:]),
			base64.RawURLEncoding.EncodeToString(sum[:]),
		}
		for _, enc := range encodings {
			if strings.Contains(body, enc) {
				t.Fatal("list contains a hash")
			}
		}
	}
	for _, row := range listed.Tokens {
		when, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
		if err != nil || when.Location() != time.UTC {
			t.Fatalf("created_at %q %v", row.CreatedAt, err)
		}
	}
}

func TestAdminDeleteToken(t *testing.T) {
	api := newAPI(t, Config{})
	if _, err := api.st.CreateRepo(context.Background(), mustStoreID(t), "site"); err != nil {
		t.Fatal(err)
	}
	admin := api.insertToken("admin", true, true, nil)
	rec := api.call(http.MethodPost, "/admin/tokens", admin, `{"label":"ro","repo":"site"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var ro struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ro); err != nil {
		t.Fatal(err)
	}
	rec = api.call(http.MethodDelete, "/admin/tokens/"+ro.ID, admin, "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete %d %q", rec.Code, rec.Body.String())
	}
	rec = api.call(http.MethodGet, "/site.git/info/refs?service=git-upload-pack", ro.Token, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked %d", rec.Code)
	}
	rec = api.call(http.MethodDelete, "/admin/tokens/"+ro.ID, admin, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete %d", rec.Code)
	}
	rec = api.call(http.MethodDelete, "/admin/tokens/not-a-uuid", admin, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id %d", rec.Code)
	}
	rec = api.call(http.MethodPost, "/admin/tokens", admin, `{"label":"self","write":true,"admin":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("self %d %s", rec.Code, rec.Body.String())
	}
	var self struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	rec = api.call(http.MethodDelete, "/admin/tokens/"+self.ID, self.Token, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("self delete %d %s", rec.Code, rec.Body.String())
	}
	rec = api.call(http.MethodGet, "/admin/tokens", self.Token, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("self next %d", rec.Code)
	}
}

func trim(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}
