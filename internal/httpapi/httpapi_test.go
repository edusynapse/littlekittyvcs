package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Deps{}, Config{})
	get := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("get %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
	}

	head := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, head)
	if rec.Code != http.StatusOK {
		t.Fatalf("head %d", rec.Code)
	}

	post := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, post)
	if rec.Code == http.StatusOK {
		t.Fatal("post returned 200")
	}

	missing := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing %d", rec.Code)
	}
}

func TestAccessLogFlushesThroughWrapper(t *testing.T) {
	h := logAccess(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); ok {
			t.Fatal("wrapper itself is a Flusher; streaming must go through Unwrap")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("stream\n"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if !rec.Flushed {
		t.Fatal("underlying writer was not flushed")
	}
	if rec.Body.String() != "stream\n" {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestAccessLogOmitsAuthorization(t *testing.T) {
	var buf bytes.Buffer
	h := New(slog.New(slog.NewJSONHandler(&buf, nil)), Deps{}, Config{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer lkv_secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(buf.String(), "lkv_secret") {
		t.Fatalf("log contains the token: %s", buf.String())
	}
}

func TestRoutesBuild(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Deps{}, Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz %d", rec.Code)
	}
}
