// Package gitproto writes the Git smart HTTP ref advertisement.
package gitproto

import (
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
)

// Service is a smart HTTP service name.
type Service string

const (
	UploadPack  Service = "git-upload-pack"
	ReceivePack Service = "git-receive-pack"
)

const (
	uploadCaps  = "multi_ack_detailed side-band-64k ofs-delta agent=littlekittyvcs"
	receiveCaps = "report-status delete-refs atomic ofs-delta side-band-64k no-thin agent=littlekittyvcs"
)

// ParseService returns ok=false for anything but the two service names.
func ParseService(s string) (Service, bool) {
	switch Service(s) {
	case UploadPack, ReceivePack:
		return Service(s), true
	default:
		return "", false
	}
}

// ContentType is the smart HTTP advertisement content type.
func (s Service) ContentType() string {
	return "application/x-" + string(s) + "-advertisement"
}

// Ref is one direct ref: a name and a 20-byte SHA-1 object id.
// Peeled is set for an annotated tag. Upload-pack writes it on the next line.
// Receive-pack ignores it.
type Ref struct {
	Name   string
	Hash   [20]byte
	Peeled *[20]byte
}

// WriteAdvertisement writes the smart HTTP info/refs body for svc.
// head is the symbolic target of HEAD ("refs/heads/main"), or "" when there is no HEAD row.
// refs are the direct refs, in any order. The slice is not modified.
func WriteAdvertisement(w io.Writer, svc Service, head string, refs []Ref) error {
	if _, ok := ParseService(string(svc)); !ok {
		return fmt.Errorf("gitproto: unknown service %q", svc)
	}
	ordered := make([]Ref, len(refs))
	copy(ordered, refs)
	for _, ref := range ordered {
		if !strings.HasPrefix(ref.Name, "refs/") {
			return fmt.Errorf("gitproto: ref %q must start with refs/", ref.Name)
		}
		if err := plumbing.ReferenceName(ref.Name).Validate(); err != nil {
			return fmt.Errorf("gitproto: ref %q: %w", ref.Name, err)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	var headHash [20]byte
	headFound := false
	if svc == UploadPack && head != "" {
		for _, ref := range ordered {
			if ref.Name == head {
				headHash = ref.Hash
				headFound = true
				break
			}
		}
	}
	caps := receiveCaps
	if svc == UploadPack {
		caps = uploadCaps
		if headFound {
			caps += " symref=HEAD:" + head
		}
	}

	enc := pktline.NewEncoder(w)
	if err := enc.EncodeString("# service=" + string(svc) + "\n"); err != nil {
		return err
	}
	if err := enc.Flush(); err != nil {
		return err
	}
	if len(ordered) == 0 {
		line := "0000000000000000000000000000000000000000 capabilities^{}\x00" + caps + "\n"
		if err := enc.EncodeString(line); err != nil {
			return err
		}
		return enc.Flush()
	}
	if headFound {
		if err := enc.EncodeString(refLine(headHash, "HEAD", caps)); err != nil {
			return err
		}
		for _, ref := range ordered {
			if err := writeRefLine(enc, svc, ref, ""); err != nil {
				return err
			}
		}
		return enc.Flush()
	}
	if err := writeRefLine(enc, svc, ordered[0], caps); err != nil {
		return err
	}
	for _, ref := range ordered[1:] {
		if err := writeRefLine(enc, svc, ref, ""); err != nil {
			return err
		}
	}
	return enc.Flush()
}

func writeRefLine(enc *pktline.Encoder, svc Service, ref Ref, caps string) error {
	if err := enc.EncodeString(refLine(ref.Hash, ref.Name, caps)); err != nil {
		return err
	}
	if svc != UploadPack || ref.Peeled == nil {
		return nil
	}
	return enc.EncodeString(hex.EncodeToString(ref.Peeled[:]) + " " + ref.Name + "^{}\n")
}

func refLine(id [20]byte, name, caps string) string {
	s := hex.EncodeToString(id[:]) + " " + name
	if caps != "" {
		s += "\x00" + caps
	}
	return s + "\n"
}
