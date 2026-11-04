package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/tokenbackup"
)

// markerTimeout bounds the marker write and the row delete after it. Both use
// a context detached from the request, so a client that hangs up cannot
// cancel either one.
const markerTimeout = 30 * time.Second

func (h *handler) createRepo(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !store.ValidRepoName(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid repository name")
		return
	}
	id, err := store.NewID()
	if err != nil {
		h.log.Error("new id", "err", err)
		writeInternal(w, true)
		return
	}
	repo, err := h.st.CreateRepo(r.Context(), id, req.Name)
	if errors.Is(err, store.ErrExists) {
		writeJSONError(w, http.StatusConflict, "repository already exists")
		return
	}
	if err != nil {
		h.log.Error("create repository", "err", err)
		writeInternal(w, true)
		return
	}
	resp := struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url,omitempty"`
	}{ID: store.FormatID(repo.ID), Name: repo.Name}
	if url, ok := repoURL(h.cfg.PublicURL, repo.Name); ok {
		resp.URL = url
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (h *handler) deleteRepo(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	if !store.ValidRepoName(name) {
		writeJSONError(w, http.StatusNotFound, "repository not found")
		return
	}
	repo, err := h.st.RepoByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "repository not found")
		return
	}
	if err != nil {
		h.log.Error("repository lookup", "err", err)
		writeInternal(w, true)
		return
	}
	seq, err := h.st.BumpPushSeq(r.Context(), repo.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "repository not found")
		return
	}
	if err != nil {
		h.log.Error("bump push seq", "err", err)
		writeInternal(w, true)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), markerTimeout)
	defer cancel()
	key, err := snapshot.Write(ctx, h.bucket, h.kr, snapshot.Snapshot{
		RepoID:  repo.ID,
		Name:    repo.Name,
		PushSeq: seq,
		Deleted: true,
	})
	if err != nil {
		h.log.Error("delete marker", "err", err, "repository", store.FormatID(repo.ID))
		writeInternal(w, true)
		return
	}
	if h.beforeDeleteRow != nil {
		h.beforeDeleteRow()
	}
	err = h.st.DeleteRepo(ctx, repo.ID, seq)
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "repository not found")
		return
	}
	if err != nil {
		// The row delete failed. A marker left on a live repository would make
		// a later restore skip it. Drop the marker only when the row remains.
		_, lookErr := h.st.RepoByID(ctx, repo.ID)
		switch {
		case errors.Is(lookErr, store.ErrNotFound):
			// The row is already gone. Keep the marker.
		case lookErr != nil:
			h.log.Error("delete marker", "err", lookErr, "key", key)
		default:
			if delErr := h.bucket.Delete(ctx, key); delErr != nil {
				h.log.Error("delete marker", "err", delErr, "key", key)
			}
		}
		if errors.Is(err, store.ErrStale) {
			writeJSONError(w, http.StatusConflict, "repository changed during delete; try again")
			return
		}
		h.log.Error("delete repository", "err", err, "repository", store.FormatID(repo.ID))
		writeInternal(w, true)
		return
	}
	h.index.Drop(repo.ID)
	h.writeTokenBackup(r.Context())
	h.log.Info("repository deleted", "repository", store.FormatID(repo.ID), "name", repo.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) createToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	var req struct {
		Label string `json:"label"`
		Write bool   `json:"write"`
		Admin bool   `json:"admin"`
		Repo  string `json:"repo"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if n := utf8.RuneCountInString(req.Label); n < 1 || n > 200 {
		writeJSONError(w, http.StatusBadRequest, "label must be 1 to 200 characters")
		return
	}
	if req.Admin && (!req.Write || req.Repo != "") {
		writeJSONError(w, http.StatusBadRequest, "an admin token must be able to write every repository")
		return
	}
	var repoID *[16]byte
	if req.Repo != "" {
		repo, err := h.st.RepoByName(r.Context(), req.Repo)
		if errors.Is(err, store.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "repository not found")
			return
		}
		if err != nil {
			h.log.Error("repository lookup", "err", err)
			writeInternal(w, true)
			return
		}
		repoID = &repo.ID
	}
	plaintext, hash, err := auth.Generate()
	if err != nil {
		h.log.Error("new token", "err", err)
		writeInternal(w, true)
		return
	}
	id, err := store.NewID()
	if err != nil {
		h.log.Error("new id", "err", err)
		writeInternal(w, true)
		return
	}
	err = h.st.InsertToken(r.Context(), hash, store.TokenRow{
		ID:     id,
		Label:  req.Label,
		Write:  req.Write,
		Admin:  req.Admin,
		RepoID: repoID,
	})
	if err != nil {
		h.log.Error("create token", "err", err)
		writeInternal(w, true)
		return
	}
	h.writeTokenBackup(r.Context())
	h.log.Info("admin token created", "id", store.FormatID(id), "label", req.Label)
	resp := struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Label string `json:"label"`
		Write bool   `json:"write"`
		Admin bool   `json:"admin"`
		Repo  string `json:"repo,omitempty"`
	}{
		ID:    store.FormatID(id),
		Token: plaintext,
		Label: req.Label,
		Write: req.Write,
		Admin: req.Admin,
		Repo:  req.Repo,
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (h *handler) listTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	rows, err := h.st.ListTokens(r.Context())
	if err != nil {
		h.log.Error("list tokens", "err", err)
		writeInternal(w, true)
		return
	}
	type item struct {
		ID        string    `json:"id"`
		Label     string    `json:"label"`
		Write     bool      `json:"write"`
		Admin     bool      `json:"admin"`
		Repo      string    `json:"repo,omitempty"`
		CreatedAt time.Time `json:"created_at"`
	}
	out := make([]item, 0, len(rows))
	for _, row := range rows {
		out = append(out, item{
			ID:        store.FormatID(row.ID),
			Label:     row.Label,
			Write:     row.Write,
			Admin:     row.Admin,
			Repo:      row.RepoName,
			CreatedAt: row.CreatedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Tokens []item `json:"tokens"`
	}{Tokens: out})
}

func (h *handler) deleteToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	id, err := store.ParseID(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid token id")
		return
	}
	err = h.st.DeleteToken(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		h.log.Error("delete token", "err", err)
		writeInternal(w, true)
		return
	}
	h.writeTokenBackup(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// writeTokenBackup stores the tokens table after a committed change.
// A failed write is logged and does not change the response: the row is already committed.
// The context is detached from the request and bounded, so a client hang-up does not cancel it.
func (h *handler) writeTokenBackup(parent context.Context) {
	if h.bucket == nil || h.kr == nil || h.st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), markerTimeout)
	defer cancel()
	if _, err := tokenbackup.WriteCurrent(ctx, h.st, h.kr, h.bucket, h.backupBucket); err != nil && h.log != nil {
		h.log.Error("token backup", "err", err)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func repoURL(public, name string) (string, bool) {
	if public == "" {
		return "", false
	}
	return strings.TrimRight(public, "/") + "/" + name + ".git", true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}
