// Package gc deletes bucket objects nothing needs any more.
package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// Grace is how old an object must be before gc deletes it. A push uploads its
// objects before its transaction inserts the pack row. Without this, gc running
// in that window deletes a pack a branch is about to point at.
const Grace = 24 * time.Hour

// Options selects the clock, whether to delete, and where lines are written.
type Options struct {
	Now    func() time.Time // time.Now in production; tests move it
	DryRun bool             // print what would be deleted, delete nothing
	Out    io.Writer
}

// Result counts what the run did. Refused repositories were left untouched.
type Result struct {
	Deleted  int
	Refused  int
	Failures int
}

type objKey struct {
	info blob.Info
	id   [16]byte
	kind snapshot.KeyKind
}

type readSnap struct {
	key  string
	snap snapshot.Snapshot
}

// Run deletes packs left by a failed push and the objects of a deleted
// repository, once they are older than Grace. err stops the whole run: the
// listing failed, a database error, or the context ended. One key's problem
// is counted and the run goes on.
func Run(ctx context.Context, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring, o Options) (Result, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	cutoff := now.Add(-Grace)

	repos := map[[16]byte][]objKey{}
	var unexpected int
	err := bucket.List(ctx, "repos/", func(info blob.Info) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parsed, _, ok := snapshot.ParseKey(info.Key)
		if !ok {
			unexpected++
			return nil
		}
		repos[parsed.RepoID] = append(repos[parsed.RepoID], objKey{
			info: info, id: parsed.ObjID, kind: parsed.Kind,
		})
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	ids := make([][16]byte, 0, len(repos))
	for id := range repos {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return store.FormatID(ids[i]) < store.FormatID(ids[j])
	})

	var res Result
	for _, repoID := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := gcRepo(ctx, st, bucket, kr, o, cutoff, repoID, repos[repoID], &res); err != nil {
			return res, err
		}
	}
	if unexpected > 0 {
		if err := writeLine(o.Out, "unexpected keys=%d", unexpected); err != nil {
			return res, err
		}
	}
	if err := writeLine(o.Out, "deleted=%d refused=%d failures=%d", res.Deleted, res.Refused, res.Failures); err != nil {
		return res, err
	}
	return res, nil
}

func gcRepo(ctx context.Context, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring, o Options, cutoff time.Time, repoID [16]byte, keys []objKey, res *Result) error {
	sort.Slice(keys, func(i, j int) bool { return keys[i].info.Key < keys[j].info.Key })
	idText := store.FormatID(repoID)
	_, err := st.RepoByID(ctx, repoID)
	if err == nil {
		ids, err := st.PackIDs(ctx, repoID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if k.kind == snapshot.KeySnap {
				continue
			}
			if _, ok := ids[k.id]; ok {
				continue
			}
			if !k.info.LastModified.Before(cutoff) {
				continue
			}
			if _, err := remove(ctx, bucket, o, k.info.Key, res); err != nil {
				return err
			}
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	var snaps []objKey
	for _, k := range keys {
		if k.kind == snapshot.KeySnap {
			snaps = append(snaps, k)
		}
	}
	if len(snaps) == 0 {
		for _, k := range keys {
			if k.kind == snapshot.KeySnap || !k.info.LastModified.Before(cutoff) {
				continue
			}
			if _, err := remove(ctx, bucket, o, k.info.Key, res); err != nil {
				return err
			}
		}
		return nil
	}

	var readable []readSnap
	for _, k := range snaps {
		got, err := snapshot.Read(ctx, bucket, kr, repoID, k.id, k.info.Size)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		readable = append(readable, readSnap{key: k.info.Key, snap: got})
	}
	if len(readable) == 0 {
		res.Refused++
		return writeLine(o.Out, "refused %s snapshots cannot be read; check MASTER_KEY", idText)
	}
	sort.Slice(readable, func(i, j int) bool {
		if readable[i].snap.PushSeq != readable[j].snap.PushSeq {
			return readable[i].snap.PushSeq > readable[j].snap.PushSeq
		}
		return readable[i].key > readable[j].key
	})
	top := readable[0].snap.PushSeq
	var normalName string
	var markerKey string
	for _, s := range readable {
		if s.snap.PushSeq != top {
			break
		}
		if !s.snap.Deleted {
			normalName = s.snap.Name
			continue
		}
		if markerKey == "" || s.key > markerKey {
			markerKey = s.key
		}
	}
	if normalName != "" {
		res.Refused++
		return writeLine(o.Out, "refused %s %s has snapshots and no database row; run littlekittyvcs restore", idText, normalName)
	}

	othersGone := true
	for _, k := range keys {
		if k.info.Key == markerKey {
			continue
		}
		if !k.info.LastModified.Before(cutoff) {
			othersGone = false
			continue
		}
		ok, err := remove(ctx, bucket, o, k.info.Key, res)
		if err != nil {
			return err
		}
		if !ok {
			othersGone = false
		}
	}
	var marker objKey
	var found bool
	for _, k := range keys {
		if k.info.Key == markerKey {
			marker = k
			found = true
			break
		}
	}
	if found && othersGone && marker.info.LastModified.Before(cutoff) {
		if _, err := remove(ctx, bucket, o, marker.info.Key, res); err != nil {
			return err
		}
	}
	return nil
}

// remove deletes one key. The bool is false when the delete failed.
// A dry run counts the key as deleted and does not call Delete.
func remove(ctx context.Context, bucket blob.Store, o Options, key string, res *Result) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if o.DryRun {
		if err := writeLine(o.Out, "would delete %s", key); err != nil {
			return false, err
		}
		res.Deleted++
		return true, nil
	}
	if err := bucket.Delete(ctx, key); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		res.Failures++
		slog.Default().Error("gc", "err", err, "key", key)
		if werr := writeLine(o.Out, "failed %s", key); werr != nil {
			return false, werr
		}
		return false, nil
	}
	if err := writeLine(o.Out, "deleted %s", key); err != nil {
		return false, err
	}
	res.Deleted++
	return true, nil
}

func writeLine(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format+"\n", args...)
	return err
}
