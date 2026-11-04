// Package push is the git-receive-pack HTTP handler.
package push

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// snapshotTimeout bounds the ref snapshot write. The request context is
// detached first, so a client that hangs up after the commit cannot cancel it.
const snapshotTimeout = 30 * time.Second

// Deps are the process resources a push uses.
type Deps struct {
	Store              *store.Store
	Bucket             blob.Store
	Keyring            *objcrypt.Keyring
	Index              *objindex.Index
	SpoolDir           string
	DenyNonFastForward bool
	Log                *slog.Logger
	ChunkCache         *objcrypt.ChunkCache
	// BeforeCommit runs after the pack and index are uploaded and before
	// CommitPush. Tests use it to stop or hold a push there. Nil in production.
	BeforeCommit func(ctx context.Context) error
}

// Target is the repository the request may push to.
// Repo nil means the repository is absent and AUTO_CREATE allows creating it.
type Target struct {
	Name  string
	Repo  *store.Repo
	Refs  []store.Ref
	Packs []store.Pack
}

type command struct {
	name   string
	old    plumbing.Hash
	new    plumbing.Hash
	peeled *[20]byte
}

// Handle serves POST /{repo}/git-receive-pack.
func Handle(d Deps, w http.ResponseWriter, r *http.Request, t Target) {
	if mediaType(r.Header.Get("Content-Type")) != "application/x-git-receive-pack-request" {
		reject(w, http.StatusUnsupportedMediaType, "unsupported media type\n")
		return
	}
	if !identityEncoding(r.Header.Get("Content-Encoding")) {
		reject(w, http.StatusUnsupportedMediaType, "unsupported content encoding\n")
		return
	}
	// git 2.34.1 posts the four bytes "0000" before a receive-pack body that
	// does not fit in http.postBuffer, and sends the pack only when this
	// answer is HTTP 200. Those four bytes are not a push.
	body, probe, err := peelProbe(r.Body)
	if err != nil {
		reject(w, http.StatusBadRequest, "invalid request\n")
		return
	}
	if probe {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return
	}
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(body); err != nil {
		reject(w, http.StatusBadRequest, "invalid request\n")
		return
	}
	if req.Shallow != nil {
		reject(w, http.StatusBadRequest, "shallow updates are not supported\n")
		return
	}
	cmds, ok := commandsOf(req)
	if !ok {
		reject(w, http.StatusBadRequest, "invalid request\n")
		return
	}
	report := req.Capabilities.Supports(capability.ReportStatus)
	side := req.Capabilities.Supports(capability.Sideband64k)

	repoID, createName, err := repositoryID(t)
	if err != nil {
		d.log(err, repoID)
		writeResult(w, report, side, "ok", commandStatuses(cmds, "storage error"))
		return
	}

	var spooled *packstore.Spooled
	var parsed *packstore.Parsed
	var fresh *packstore.Pack
	var packID [16]byte
	if allDeletes(cmds) {
		switch packTail(req.Packfile) {
		case tailData:
			reject(w, http.StatusBadRequest, "unexpected pack\n")
			return
		case tailError:
			reject(w, http.StatusBadRequest, "invalid request\n")
			return
		}
	} else {
		packID, err = store.NewID()
		if err != nil {
			d.log(err, repoID)
			writeResult(w, report, side, "ok", commandStatuses(cmds, "storage error"))
			return
		}
		binding := objcrypt.Binding{RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindPack}
		spooled, err = packstore.Spool(r.Context(), req.Packfile, d.SpoolDir, d.Keyring, binding)
		if err != nil {
			d.unpackFail(w, report, side, repoID, err, cmds)
			return
		}
		defer os.Remove(spooled.Path)
		parsed, fresh, err = packstore.Parse(r.Context(), spooled, d.Keyring, binding)
		if err != nil {
			d.unpackFail(w, report, side, repoID, err, cmds)
			return
		}
		defer fresh.Close()
	}

	if err := d.Index.Refresh(r.Context(), d.Bucket, d.Keyring, repoID, t.Packs); err != nil {
		d.log(err, repoID)
		writeResult(w, report, side, "ok", commandStatuses(cmds, "storage error"))
		return
	}
	objs := packstore.NewObjects(r.Context(), d.Bucket, d.Keyring, repoID, func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) {
		return d.Index.Lookup(repoID, id)
	}, fresh, d.ChunkCache)
	defer objs.Close()
	reason, err := d.check(repoID, cmds, parsed, fresh, objs)
	if reason != "" {
		if err != nil {
			d.log(err, repoID)
		}
		writeResult(w, report, side, "ok", commandStatuses(cmds, reason))
		return
	}
	if alreadyApplied(cmds, t.Refs) {
		writeResult(w, report, side, "ok", commandStatuses(cmds, "ok"))
		return
	}
	if err := fillPeeled(cmds, parsed, objs); err != nil {
		d.log(err, repoID)
		writeResult(w, report, side, "ok", commandStatuses(cmds, "storage error"))
		return
	}

	var row *store.NewPack
	var objects uint32
	var packBytes, idxBytes int64
	if spooled != nil && spooled.ObjectCount > 0 {
		objects = spooled.ObjectCount
		packBytes = spooled.Size
		row, idxBytes, err = d.storePack(r.Context(), repoID, packID, spooled, parsed)
		if err != nil {
			d.log(err, repoID)
			writeResult(w, report, side, "ok", commandStatuses(cmds, "storage error"))
			return
		}
	}
	if d.BeforeCommit != nil {
		if err := d.BeforeCommit(r.Context()); err != nil {
			d.log(err, repoID)
			writeResult(w, report, side, "ok", commandStatuses(cmds, "storage error"))
			return
		}
	}
	updates := make([]store.RefUpdate, len(cmds))
	for i, c := range cmds {
		updates[i] = store.RefUpdate{Name: c.name, Old: hashPtr(c.old), New: hashPtr(c.new), Peeled: c.peeled}
	}
	committed, err := d.Store.CommitPush(r.Context(), store.Push{
		RepoID:     repoID,
		CreateName: createName,
		Pack:       row,
		Updates:    updates,
	})
	if err != nil {
		if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrExists) {
			writeResult(w, report, side, "ok", commandStatuses(cmds, "failed to update ref"))
			return
		}
		d.log(err, repoID)
		writeResult(w, report, side, "ok", commandStatuses(cmds, "storage error"))
		return
	}
	if row != nil && parsed != nil {
		d.Index.Add(repoID, store.Pack{
			ID:       row.ID,
			PackKey:  row.PackKey,
			IdxKey:   row.IdxKey,
			Bytes:    row.Bytes,
			IdxBytes: row.IdxBytes,
		}, parsed.Index)
	}
	name := t.Name
	if t.Repo != nil {
		name = t.Repo.Name
	}
	snapOK := d.writeSnapshot(r.Context(), repoID, name, committed)
	d.pushed(repoID, packID, row != nil, objects, packBytes, idxBytes, len(cmds), snapOK, committed.PushSeq)
	writeResult(w, report, side, "ok", commandStatuses(cmds, "ok"))
}

func (d Deps) writeSnapshot(reqCtx context.Context, repoID [16]byte, name string, committed store.Committed) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), snapshotTimeout)
	defer cancel()
	_, err := snapshot.Write(ctx, d.Bucket, d.Keyring, snapshot.Snapshot{
		RepoID:  repoID,
		Name:    name,
		PushSeq: committed.PushSeq,
		Refs:    committed.Refs,
	})
	if err != nil {
		if d.Log != nil {
			d.Log.Error("ref snapshot", "err", err, "repository", store.FormatID(repoID))
		}
		return false
	}
	return true
}

func (d Deps) unpackFail(w http.ResponseWriter, report, side bool, repoID [16]byte, err error, cmds []command) {
	unpack, status := unpackReason(err)
	if status == "storage error" {
		d.log(err, repoID)
	}
	writeResult(w, report, side, unpack, commandStatuses(cmds, status))
}

func unpackReason(err error) (string, string) {
	switch {
	case errors.Is(err, packstore.ErrTooLarge),
		errors.Is(err, packstore.ErrTooSmall),
		errors.Is(err, packstore.ErrBadHeader),
		errors.Is(err, packstore.ErrBadVersion),
		errors.Is(err, packstore.ErrChecksum),
		errors.Is(err, packstore.ErrThinPack),
		errors.Is(err, packstore.ErrTruncated),
		errors.Is(err, packstore.ErrBadPack),
		errors.Is(err, packstore.ErrTrailing):
		return err.Error(), "unpacker error"
	default:
		return "storage error", "storage error"
	}
}

func (d Deps) log(err error, repoID [16]byte) {
	if d.Log == nil || err == nil {
		return
	}
	d.Log.Error("push", "err", err, "repository", store.FormatID(repoID))
}

func (d Deps) pushed(repoID, packID [16]byte, havePack bool, objects uint32, packBytes, idxBytes int64, refs int, snapshotOK bool, pushSeq int64) {
	if d.Log == nil {
		return
	}
	args := []any{
		"repository", store.FormatID(repoID),
		"objects", objects,
		"pack_bytes", packBytes,
		"idx_bytes", idxBytes,
		"refs", refs,
		"snapshot", snapshotOK,
		"push_seq", pushSeq,
	}
	if havePack {
		args = append(args, "pack", store.FormatID(packID))
	}
	d.Log.Info("push", args...)
}

func repositoryID(t Target) ([16]byte, string, error) {
	if t.Repo != nil {
		return t.Repo.ID, "", nil
	}
	id, err := store.NewID()
	return id, t.Name, err
}

func commandsOf(req *packp.ReferenceUpdateRequest) ([]command, bool) {
	if len(req.Commands) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]command, 0, len(req.Commands))
	for _, c := range req.Commands {
		name := c.Name.String()
		if !strings.HasPrefix(name, "refs/") || c.Name.Validate() != nil {
			return nil, false
		}
		if seen[name] || (c.Old == plumbing.ZeroHash && c.New == plumbing.ZeroHash) {
			return nil, false
		}
		seen[name] = true
		out = append(out, command{name: name, old: c.Old, new: c.New})
	}
	return out, true
}

func allDeletes(cmds []command) bool {
	for _, c := range cmds {
		if c.new != plumbing.ZeroHash {
			return false
		}
	}
	return true
}

// peelProbe reports whether r is git's four-byte flush probe.
// Anything else is returned with those leading bytes still readable.
func peelProbe(r io.Reader) (io.Reader, bool, error) {
	if r == nil {
		return nil, false, io.ErrUnexpectedEOF
	}
	var peek [5]byte
	n, err := io.ReadFull(r, peek[:])
	if n == 4 && bytes.Equal(peek[:4], []byte("0000")) && errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, true, nil
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, false, err
	}
	return io.MultiReader(bytes.NewReader(peek[:n]), r), false, nil
}

const (
	tailEmpty = iota
	tailData
	tailError
)

// packTail reads the body once. A delete-only push has nothing after the flush.
func packTail(r io.Reader) int {
	if r == nil {
		return tailError
	}
	var one [1]byte
	n, err := r.Read(one[:])
	if n > 0 {
		return tailData
	}
	if err == io.EOF {
		return tailEmpty
	}
	if err != nil {
		return tailError
	}
	n, err = r.Read(one[:])
	if n > 0 {
		return tailData
	}
	if err == io.EOF {
		return tailEmpty
	}
	return tailError
}

func alreadyApplied(cmds []command, refs []store.Ref) bool {
	cur := map[string][20]byte{}
	for _, ref := range refs {
		if ref.ObjectID != nil {
			cur[ref.Name] = *ref.ObjectID
		}
	}
	for _, c := range cmds {
		have, ok := cur[c.name]
		if c.new == plumbing.ZeroHash {
			if ok {
				return false
			}
			continue
		}
		if !ok || have != [20]byte(c.new) {
			return false
		}
	}
	return true
}

func (d Deps) storePack(ctx context.Context, repoID, packID [16]byte, sp *packstore.Spooled, parsed *packstore.Parsed) (*store.NewPack, int64, error) {
	packKey := packstore.PackKey(repoID, packID)
	idxKey := packstore.IdxKey(repoID, packID)
	f, err := os.Open(sp.Path)
	if err != nil {
		return nil, 0, err
	}
	putErr := d.Bucket.Put(ctx, packKey, f, sp.Size)
	_ = f.Close()
	if putErr != nil {
		return nil, 0, putErr
	}
	var buf bytes.Buffer
	iw, err := objcrypt.NewWriter(&buf, d.Keyring, objcrypt.Binding{
		RepoID: repoID, ObjectID: packID, Kind: objcrypt.KindIdx,
	}, objcrypt.WriterOptions{})
	if err != nil {
		return nil, 0, err
	}
	if _, err := iw.Write(parsed.IdxBytes); err != nil {
		return nil, 0, err
	}
	if err := iw.Close(); err != nil {
		return nil, 0, err
	}
	idxBytes := iw.CiphertextSize()
	if err := d.Bucket.Put(ctx, idxKey, bytes.NewReader(buf.Bytes()), idxBytes); err != nil {
		return nil, 0, err
	}
	return &store.NewPack{
		ID: packID, PackKey: packKey, IdxKey: idxKey, Bytes: sp.Size, IdxBytes: idxBytes,
	}, idxBytes, nil
}

func (d Deps) check(repoID [16]byte, cmds []command, parsed *packstore.Parsed, fresh *packstore.Pack, objs *packstore.Objects) (string, error) {
	if parsed != nil {
		for id, typ := range parsed.Objects {
			if typ != plumbing.CommitObject && typ != plumbing.TreeObject && typ != plumbing.TagObject {
				continue
			}
			got, content, err := fresh.Get(id)
			if err != nil {
				return "storage error", err
			}
			refs, err := packstore.Refs(got, content)
			if err != nil {
				return "storage error", err
			}
			for _, ref := range refs {
				if _, ok := parsed.Objects[ref]; ok {
					continue
				}
				if _, _, ok := d.Index.Lookup(repoID, ref); ok {
					continue
				}
				return "missing object", nil
			}
		}
	}
	for _, c := range cmds {
		if c.new == plumbing.ZeroHash {
			continue
		}
		id := [20]byte(c.new)
		var typ plumbing.ObjectType
		var known bool
		if parsed != nil {
			typ, known = parsed.Objects[id]
		}
		if !known {
			if _, _, ok := d.Index.Lookup(repoID, id); !ok {
				return "missing object", nil
			}
		}
		if !strings.HasPrefix(c.name, "refs/heads/") {
			continue
		}
		if !known {
			var err error
			typ, _, err = objs.Get(id)
			if err != nil {
				return "storage error", err
			}
		}
		if typ != plumbing.CommitObject {
			return "not a commit", nil
		}
	}
	if !d.DenyNonFastForward {
		return "", nil
	}
	for _, c := range cmds {
		if !strings.HasPrefix(c.name, "refs/heads/") || c.old == plumbing.ZeroHash || c.new == plumbing.ZeroHash || c.old == c.new {
			continue
		}
		err := fastForward(objs, [20]byte(c.new), [20]byte(c.old))
		if errors.Is(err, errNonFastForward) {
			return "non-fast-forward", nil
		}
		if err != nil {
			return "storage error", err
		}
	}
	return "", nil
}

var errNonFastForward = errors.New("non-fast-forward")

func fastForward(objs *packstore.Objects, tip, old [20]byte) error {
	queue := [][20]byte{tip}
	seen := map[[20]byte]struct{}{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == old {
			return nil
		}
		if _, ok := seen[cur]; ok {
			continue
		}
		seen[cur] = struct{}{}
		typ, content, err := objs.Get(cur)
		if err != nil {
			return err
		}
		if typ != plumbing.CommitObject {
			return errors.New("push: history object is not a commit")
		}
		parents, err := commitParents(content)
		if err != nil {
			return err
		}
		queue = append(queue, parents...)
	}
	return errNonFastForward
}

func commitParents(content []byte) ([][20]byte, error) {
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.CommitObject)
	if _, err := mo.Write(content); err != nil {
		return nil, err
	}
	var c object.Commit
	if err := c.Decode(mo); err != nil {
		return nil, err
	}
	out := make([][20]byte, len(c.ParentHashes))
	for i, parent := range c.ParentHashes {
		out[i] = parent
	}
	return out, nil
}

// fillPeeled sets command.peeled when the new object is an annotated tag.
// A read failure is reported as "push: object read" so a hash in a library
// error is not logged.
func fillPeeled(cmds []command, parsed *packstore.Parsed, objs *packstore.Objects) error {
	for i := range cmds {
		if cmds[i].new == plumbing.ZeroHash {
			continue
		}
		id := [20]byte(cmds[i].new)
		var typ plumbing.ObjectType
		known := false
		if parsed != nil {
			typ, known = parsed.Objects[id]
		}
		if !known {
			var err error
			typ, _, err = objs.Get(id)
			if err != nil {
				return errors.New("push: object read")
			}
		}
		if typ != plumbing.TagObject {
			continue
		}
		peeled, err := packstore.Peel(objs, id)
		if err != nil {
			return errors.New("push: object read")
		}
		cmds[i].peeled = peeled
	}
	return nil
}
