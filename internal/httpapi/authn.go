package httpapi

import (
	"errors"
	"net/http"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

type caller struct {
	anonymous bool
	token     auth.Token
}

func (h *handler) authenticate(w http.ResponseWriter, r *http.Request, asJSON bool) (caller, bool) {
	if r.Header.Get("Authorization") == "" {
		return caller{anonymous: true}, true
	}
	plain, ok := auth.FromRequest(r)
	if !ok {
		writeUnauthorized(w)
		return caller{}, false
	}
	row, err := h.st.TokenByHash(r.Context(), auth.Hash(plain))
	if errors.Is(err, store.ErrNotFound) {
		writeUnauthorized(w)
		return caller{}, false
	}
	if err != nil {
		h.log.Error("token lookup", "err", err)
		writeInternal(w, asJSON)
		return caller{}, false
	}
	return caller{token: auth.Token{
		ID:     row.ID,
		Write:  row.Write,
		Admin:  row.Admin,
		RepoID: row.RepoID,
	}}, true
}

func (h *handler) requireAdmin(w http.ResponseWriter, r *http.Request) (auth.Token, bool) {
	w.Header().Set("Cache-Control", "no-store")
	c, ok := h.authenticate(w, r, true)
	if !ok {
		return auth.Token{}, false
	}
	if c.anonymous {
		writeUnauthorized(w)
		return auth.Token{}, false
	}
	if !c.token.Admin {
		writeJSONError(w, http.StatusForbidden, "admin token required")
		return auth.Token{}, false
	}
	return c.token, true
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", auth.Challenge)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("authentication required\n"))
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func writeInternal(w http.ResponseWriter, asJSON bool) {
	if asJSON {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeText(w, http.StatusInternalServerError, "internal error\n")
}
