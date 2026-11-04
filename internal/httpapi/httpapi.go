// Package httpapi is the HTTP server: health, the Git smart HTTP routes, and the admin API.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// Deps are the process resources a request uses.
type Deps struct {
	Store        *store.Store
	Bucket       blob.Store
	BackupBucket blob.Store // nil when the backup bucket is off
	Keyring      *objcrypt.Keyring
	Index        *objindex.Index
	ChunkCache   *objcrypt.ChunkCache
	PushHook     func(ctx context.Context) error // tests only; passed to push.Deps.BeforeCommit
}

// Config is the settings a request uses. The rest of the process settings stay in the command.
type Config struct {
	AnonymousRead      bool
	AutoCreate         bool
	DenyNonFastForward bool
	PublicURL          string
}

type handler struct {
	log          *slog.Logger
	st           *store.Store
	bucket       blob.Store
	backupBucket blob.Store
	kr           *objcrypt.Keyring
	index        *objindex.Index
	chunks       *objcrypt.ChunkCache
	cfg          Config
	// pushHook is passed to the push path. Nil in production.
	pushHook func(context.Context) error
	// beforeDeleteRow runs after the delete marker is written and before
	// DeleteRepo. Tests in this package set it. Nil in production.
	beforeDeleteRow func()
}

// New returns the HTTP handler. A zero Deps is enough for a test that calls /healthz.
func New(log *slog.Logger, d Deps, cfg Config) http.Handler {
	h := &handler{
		log: log, st: d.Store, bucket: d.Bucket, backupBucket: d.BackupBucket,
		kr: d.Keyring, index: d.Index, chunks: d.ChunkCache, cfg: cfg,
		pushHook: d.PushHook,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /{repo}/info/refs", h.infoRefs)
	mux.HandleFunc("POST /{repo}/git-upload-pack", h.uploadPack)
	mux.HandleFunc("POST /{repo}/git-receive-pack", h.receivePack)
	mux.HandleFunc("POST /admin/repos", h.createRepo)
	mux.HandleFunc("DELETE /admin/repos/{name}", h.deleteRepo)
	mux.HandleFunc("POST /admin/tokens", h.createToken)
	mux.HandleFunc("GET /admin/tokens", h.listTokens)
	mux.HandleFunc("DELETE /admin/tokens/{id}", h.deleteToken)
	return logAccess(log, mux)
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func logAccess(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration", time.Since(start),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.NewResponseController reach the writer underneath, so a
// handler can flush a streamed body.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
