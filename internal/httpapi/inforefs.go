package httpapi

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/edusynapse/littlekittyvcs/internal/gitproto"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

func (h *handler) infoRefs(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("repo"), ".git")
	if !store.ValidRepoName(name) {
		http.NotFound(w, r)
		return
	}
	svc, ok := gitproto.ParseService(r.URL.Query().Get("service"))
	if !ok {
		writeText(w, http.StatusForbidden, "only smart HTTP is supported\n")
		return
	}
	t, ok := h.authorizeGit(w, r, name, svc)
	if !ok {
		return
	}
	head, direct := directRefs(t.refs, svc == gitproto.UploadPack)
	h.writeAdvertisement(w, svc, head, direct)
}

func directRefs(refs []store.Ref, upload bool) (string, []gitproto.Ref) {
	var head string
	var direct []gitproto.Ref
	for _, ref := range refs {
		if ref.Name == "HEAD" && ref.ObjectID == nil {
			head = ref.Symbolic
			continue
		}
		if ref.ObjectID == nil {
			continue
		}
		item := gitproto.Ref{Name: ref.Name, Hash: *ref.ObjectID}
		if upload {
			item.Peeled = ref.Peeled
		}
		direct = append(direct, item)
	}
	return head, direct
}

func (h *handler) writeAdvertisement(w http.ResponseWriter, svc gitproto.Service, head string, refs []gitproto.Ref) {
	var buf bytes.Buffer
	if err := gitproto.WriteAdvertisement(&buf, svc, head, refs); err != nil {
		h.log.Error("advertisement", "err", err)
		writeInternal(w, false)
		return
	}
	w.Header().Set("Content-Type", svc.ContentType())
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
