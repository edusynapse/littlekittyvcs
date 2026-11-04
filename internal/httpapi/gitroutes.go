package httpapi

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/edusynapse/littlekittyvcs/internal/fetch"
	"github.com/edusynapse/littlekittyvcs/internal/gitproto"
	"github.com/edusynapse/littlekittyvcs/internal/push"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// gitTarget is the repository one Git request may use.
// repo nil means it does not exist and AUTO_CREATE allows the receive path to create it.
type gitTarget struct {
	name  string
	repo  *store.Repo
	refs  []store.Ref
	packs []store.Pack
}

// authorizeGit is the access decision shared by info/refs and both POST routes.
// name is the repository name with one trailing ".git" already removed.
func (h *handler) authorizeGit(w http.ResponseWriter, r *http.Request, name string, svc gitproto.Service) (gitTarget, bool) {
	c, ok := h.authenticate(w, r, false)
	if !ok {
		return gitTarget{}, false
	}
	if c.anonymous && (svc == gitproto.ReceivePack || !h.cfg.AnonymousRead) {
		writeUnauthorized(w)
		return gitTarget{}, false
	}
	repo, refs, packs, err := h.st.ReadRepo(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		if svc == gitproto.ReceivePack && h.cfg.AutoCreate && !c.anonymous && c.token.Write && c.token.RepoID == nil {
			return gitTarget{name: name}, true
		}
		http.NotFound(w, r)
		return gitTarget{}, false
	}
	if err != nil {
		h.log.Error("repository read", "err", err)
		writeInternal(w, false)
		return gitTarget{}, false
	}
	if !c.anonymous && !c.token.Allows(repo.ID, false) {
		http.NotFound(w, r)
		return gitTarget{}, false
	}
	if svc == gitproto.ReceivePack && (c.anonymous || !c.token.Allows(repo.ID, true)) {
		writeText(w, http.StatusForbidden, "this token is read-only\n")
		return gitTarget{}, false
	}
	repoCopy := repo
	return gitTarget{name: name, repo: &repoCopy, refs: refs, packs: packs}, true
}

func (h *handler) uploadPack(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("repo"), ".git")
	if !store.ValidRepoName(name) {
		http.NotFound(w, r)
		return
	}
	t, ok := h.authorizeGit(w, r, name, gitproto.UploadPack)
	if !ok {
		return
	}
	fetch.Handle(fetch.Deps{Bucket: h.bucket, Keyring: h.kr, Index: h.index, Log: h.log, ChunkCache: h.chunks}, w, r, fetch.Target{
		Repo:  *t.repo,
		Refs:  t.refs,
		Packs: t.packs,
	})
}

func (h *handler) receivePack(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("repo"), ".git")
	if !store.ValidRepoName(name) {
		http.NotFound(w, r)
		return
	}
	t, ok := h.authorizeGit(w, r, name, gitproto.ReceivePack)
	if !ok {
		return
	}
	push.Handle(push.Deps{
		Store:              h.st,
		Bucket:             h.bucket,
		Keyring:            h.kr,
		Index:              h.index,
		SpoolDir:           os.TempDir(),
		DenyNonFastForward: h.cfg.DenyNonFastForward,
		Log:                h.log,
		ChunkCache:         h.chunks,
		BeforeCommit:       h.pushHook,
	}, w, r, push.Target{
		Name:  t.name,
		Repo:  t.repo,
		Refs:  t.refs,
		Packs: t.packs,
	})
}
