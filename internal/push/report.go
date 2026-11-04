package push

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
)

func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func identityEncoding(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "" || v == "identity"
}

func reject(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeResult(w http.ResponseWriter, report, side bool, unpack string, commands []packp.CommandStatus) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if !report {
		return
	}
	rs := &packp.ReportStatus{UnpackStatus: unpack}
	for i := range commands {
		rs.CommandStatuses = append(rs.CommandStatuses, &commands[i])
	}
	var dst io.Writer = w
	if side {
		dst = sideband.NewMuxer(sideband.Sideband64k, w)
	}
	if err := rs.Encode(dst); err != nil {
		return
	}
	if side {
		_ = pktline.NewEncoder(w).Flush()
	}
}

func commandStatuses(cmds []command, status string) []packp.CommandStatus {
	out := make([]packp.CommandStatus, len(cmds))
	for i, c := range cmds {
		out[i] = packp.CommandStatus{
			ReferenceName: plumbing.ReferenceName(c.name),
			Status:        status,
		}
	}
	return out
}

func hashPtr(h plumbing.Hash) *[20]byte {
	if h == plumbing.ZeroHash {
		return nil
	}
	id := [20]byte(h)
	return &id
}
