package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/go-git/go-git/v5/plumbing/format/idxfile"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// Options selects what Restore prints and which repository it inserts.
type Options struct {
	OnlyName string    // non-empty: restore only the repository with this name
	Out      io.Writer // one line per repository uuid; counts and names only
}

type sized struct {
	key  string
	size int64
}

type snapObj struct {
	id   [16]byte
	size int64
}

type repoObjects struct {
	packs      map[[16]byte]sized
	idxs       map[[16]byte]sized
	snaps      []snapObj
	unexpected int
}

// Restore rebuilds every repository in the bucket whose id is not a row yet.
// failed counts repositories that could not be restored.
// err is a listing failure or a cancelled context. One repository's problem
// is a failed line, and the run goes on to the next.
func Restore(ctx context.Context, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring, o Options) (int, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	repos := map[[16]byte]*repoObjects{}
	var stray int
	err := bucket.List(ctx, "repos/", func(info blob.Info) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parsed, hasRepo, ok := ParseKey(info.Key)
		if !hasRepo {
			stray++
			return nil
		}
		repoID := parsed.RepoID
		slot := repos[repoID]
		if slot == nil {
			slot = &repoObjects{
				packs: map[[16]byte]sized{},
				idxs:  map[[16]byte]sized{},
			}
			repos[repoID] = slot
		}
		if !ok {
			slot.unexpected++
			return nil
		}
		switch parsed.Kind {
		case KeyPack:
			slot.packs[parsed.ObjID] = sized{key: info.Key, size: info.Size}
		case KeyIdx:
			slot.idxs[parsed.ObjID] = sized{key: info.Key, size: info.Size}
		case KeySnap:
			slot.snaps = append(slot.snaps, snapObj{id: parsed.ObjID, size: info.Size})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	ids := make([][16]byte, 0, len(repos))
	for id := range repos {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return store.FormatID(ids[i]) < store.FormatID(ids[j])
	})

	var failed int
	var nameSeen bool
	for _, repoID := range ids {
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		n, seen, err := restoreOne(ctx, st, bucket, kr, o, repoID, repos[repoID])
		failed += n
		if seen {
			nameSeen = true
		}
		if err != nil {
			return failed, err
		}
	}
	if o.OnlyName != "" && !nameSeen {
		if err := writeLine(o.Out, "failed no repository named %s in the bucket", o.OnlyName); err != nil {
			return failed, err
		}
		failed++
	}
	if stray > 0 {
		if err := writeLine(o.Out, "unexpected keys=%d", stray); err != nil {
			return failed, err
		}
	}
	return failed, nil
}

// restoreOne handles one repository uuid.
// When OnlyName is set, "no readable snapshot" and "snapshots name different
// repositories" are printed and not counted: both happen before the name is
// known, and a damaged unrelated repository must not fail a named restore.
// A skipped database row whose name equals OnlyName counts as the name being
// found, so a second named restore does not also report the name missing.
func restoreOne(ctx context.Context, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring, o Options, repoID [16]byte, objs *repoObjects) (int, bool, error) {
	idText := store.FormatID(repoID)
	row, err := st.RepoByID(ctx, repoID)
	if err == nil {
		if err := writeLine(o.Out, "skipped %s already in database", idText); err != nil {
			return 0, false, err
		}
		return 0, o.OnlyName != "" && row.Name == o.OnlyName, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return 0, false, err
	}

	var readable []Snapshot
	var unreadable int
	for _, snap := range objs.snaps {
		got, err := Read(ctx, bucket, kr, repoID, snap.id, snap.size)
		if err != nil {
			if ctx.Err() != nil {
				return 0, false, ctx.Err()
			}
			unreadable++
			continue
		}
		readable = append(readable, got)
	}
	if len(objs.snaps) == 0 {
		if err := writeLine(o.Out, "skipped %s no snapshot", idText); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	// Failures 4 and 5 are before the name is known.
	if len(readable) == 0 {
		if err := writeLine(o.Out, "failed %s no readable snapshot", idText); err != nil {
			return 0, false, err
		}
		if o.OnlyName != "" {
			return 0, false, nil
		}
		return 1, false, nil
	}
	name := readable[0].Name
	for _, s := range readable[1:] {
		if s.Name != name {
			if err := writeLine(o.Out, "failed %s snapshots name different repositories", idText); err != nil {
				return 0, false, err
			}
			if o.OnlyName != "" {
				return 0, false, nil
			}
			return 1, false, nil
		}
	}
	sort.Slice(readable, func(i, j int) bool { return readable[i].PushSeq > readable[j].PushSeq })
	// A named restore still prints this skip, including for a different name,
	// the same way it prints "already in database". The name counts as seen
	// only when it is the one that was asked for.
	if o.OnlyName != "" && name != o.OnlyName {
		if readable[0].Deleted {
			if err := writeLine(o.Out, "skipped %s deleted", idText); err != nil {
				return 0, false, err
			}
		}
		return 0, false, nil
	}
	seen := o.OnlyName != "" && name == o.OnlyName

	if dup, ok := duplicatedSeq(readable); ok {
		if err := writeLine(o.Out, "failed %s two snapshots with push_seq %d", idText, dup); err != nil {
			return 0, seen, err
		}
		return 1, seen, nil
	}

	if readable[0].Deleted {
		if err := writeLine(o.Out, "skipped %s deleted", idText); err != nil {
			return 0, seen, err
		}
		return 0, seen, nil
	}

	good, incomplete, bad, err := goodPacks(ctx, bucket, kr, repoID, objs)
	if err != nil {
		return 0, seen, err
	}
	idx := objindex.New()
	if len(good) > 0 {
		if err := idx.Refresh(ctx, bucket, kr, repoID, good); err != nil {
			if ctx.Err() != nil {
				return 0, seen, ctx.Err()
			}
			slog.Default().Error("restore", "err", err, "repository", idText)
			if err := writeLine(o.Out, "failed %s storage error", idText); err != nil {
				return 0, seen, err
			}
			return 1, seen, nil
		}
	}

	highest := readable[0].PushSeq
	chosen := -1
	for i := range readable {
		if readable[i].Deleted {
			continue
		}
		if objectsPresent(idx, repoID, readable[i]) {
			chosen = i
			break
		}
	}
	if chosen < 0 {
		if err := writeLine(o.Out, "failed %s no snapshot whose objects are all stored", idText); err != nil {
			return 0, seen, err
		}
		return 1, seen, nil
	}
	pick := readable[chosen]
	rows := make([]store.RestoredPack, len(good))
	for i, p := range good {
		rows[i] = store.RestoredPack{
			ID: p.ID, PackKey: p.PackKey, IdxKey: p.IdxKey, Bytes: p.Bytes, IdxBytes: p.IdxBytes,
		}
	}
	reader := packstore.NewObjects(ctx, bucket, kr, repoID, func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) {
		return idx.Lookup(repoID, id)
	}, nil, nil)
	for i := range pick.Refs {
		if pick.Refs[i].ObjectID == nil {
			continue
		}
		peeled, peelErr := packstore.Peel(reader, *pick.Refs[i].ObjectID)
		if peelErr != nil {
			reader.Close()
			if ctx.Err() != nil {
				return 0, seen, ctx.Err()
			}
			slog.Default().Error("restore", "err", errors.New("restore: object read"), "repository", idText)
			if err := writeLine(o.Out, "failed %s storage error", idText); err != nil {
				return 0, seen, err
			}
			return 1, seen, nil
		}
		pick.Refs[i].Peeled = peeled
	}
	reader.Close()
	err = st.RestoreRepo(ctx, store.Restore{
		ID: repoID, Name: pick.Name, PushSeq: highest, Refs: pick.Refs, Packs: rows,
	})
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, store.ErrExists) {
			return 0, seen, ctx.Err()
		}
		if errors.Is(err, store.ErrExists) {
			if err := writeLine(o.Out, "failed %s name or id already taken", idText); err != nil {
				return 0, seen, err
			}
			return 1, seen, nil
		}
		slog.Default().Error("restore", "err", err, "repository", idText)
		if err := writeLine(o.Out, "failed %s storage error", idText); err != nil {
			return 0, seen, err
		}
		return 1, seen, nil
	}

	line := fmt.Sprintf("restored %s %s push_seq=%d refs=%d packs=%d", pick.Name, idText, highest, len(pick.Refs), len(good))
	if pick.PushSeq != highest {
		line += " used_older_snapshot=true"
	}
	if unreadable > 0 {
		line += fmt.Sprintf(" unreadable=%d", unreadable)
	}
	if incomplete > 0 {
		line += fmt.Sprintf(" incomplete=%d", incomplete)
	}
	if bad > 0 {
		line += fmt.Sprintf(" bad=%d", bad)
	}
	if objs.unexpected > 0 {
		line += fmt.Sprintf(" unexpected=%d", objs.unexpected)
	}
	if err := writeLine(o.Out, "%s", line); err != nil {
		return 0, seen, err
	}
	return 0, seen, nil
}

func duplicatedSeq(snaps []Snapshot) (int64, bool) {
	counts := map[int64]int{}
	for _, s := range snaps {
		counts[s.PushSeq]++
	}
	var dup int64
	var found bool
	for seq, n := range counts {
		if n >= 2 && (!found || seq < dup) {
			dup = seq
			found = true
		}
	}
	return dup, found
}

func goodPacks(ctx context.Context, bucket blob.Store, kr *objcrypt.Keyring, repoID [16]byte, objs *repoObjects) ([]store.Pack, int, int, error) {
	ids := map[[16]byte]struct{}{}
	for id := range objs.packs {
		ids[id] = struct{}{}
	}
	for id := range objs.idxs {
		ids[id] = struct{}{}
	}
	order := make([][16]byte, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool {
		return store.FormatID(order[i]) < store.FormatID(order[j])
	})
	var incomplete, bad int
	var good []store.Pack
	for _, id := range order {
		pack, hasPack := objs.packs[id]
		idx, hasIdx := objs.idxs[id]
		if !hasPack || !hasIdx {
			incomplete++
			continue
		}
		row := store.Pack{
			ID: id, PackKey: pack.key, IdxKey: idx.key, Bytes: pack.size, IdxBytes: idx.size,
		}
		_, err := objcrypt.Open(ctx, bucket, pack.key, pack.size, kr, objcrypt.Binding{
			RepoID: repoID, ObjectID: id, Kind: objcrypt.KindPack,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, 0, ctx.Err()
			}
			bad++
			continue
		}
		fresh := objindex.New()
		if err := fresh.Refresh(ctx, bucket, kr, repoID, []store.Pack{row}); err != nil {
			if ctx.Err() != nil {
				return nil, 0, 0, ctx.Err()
			}
			bad++
			continue
		}
		good = append(good, row)
	}
	return good, incomplete, bad, nil
}

func objectsPresent(idx *objindex.Index, repoID [16]byte, s Snapshot) bool {
	for _, ref := range s.Refs {
		if ref.ObjectID == nil {
			continue
		}
		if _, _, ok := idx.Lookup(repoID, *ref.ObjectID); !ok {
			return false
		}
	}
	return true
}

func writeLine(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format+"\n", args...)
	return err
}
