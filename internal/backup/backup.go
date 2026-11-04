// Package backup copies objects the backup bucket does not yet have.
package backup

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
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/tokenbackup"
)

// LockKey is the PostgreSQL session advisory lock held for one backup run.
// It is not the migration lock (internal/store migrateLock, 0x6c6b760000000001).
const LockKey int64 = 0x6c6b760000000002

// ErrCopyFailed is returned when the run finished and at least one object failed.
var ErrCopyFailed = errors.New("backup: copy failed")

// Result counts one run. Ran is false when another run held the lock.
type Result struct {
	Ran        bool
	Copied     int
	Skipped    int
	Failed     int
	Bytes      int64
	FailedKeys []string
}

// Options selects where the summary is written. Out nil logs the summary.
// Log nil uses the default logger.
type Options struct {
	Log *slog.Logger
	Out io.Writer
}

// Run copies every repos/ and tokens/ object that is missing from dst, or
// present there at a different size. It never deletes from dst. A token backup
// is written to both buckets before the copy. Another run holding LockKey is
// skipped and counts as success.
func Run(ctx context.Context, st *store.Store, primary, dst blob.Store, kr *objcrypt.Keyring, opt Options) (Result, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	if primary == nil || dst == nil {
		return Result{}, errors.New("backup: bucket is missing")
	}
	var res Result
	ran, err := st.WithAdvisoryLock(ctx, LockKey, func(ctx context.Context) error {
		if _, err := tokenbackup.WriteCurrent(ctx, st, kr, primary, dst); err != nil {
			return fmt.Errorf("token backup: %w", err)
		}
		copied, err := copyMissing(ctx, primary, dst, log)
		res = copied
		res.Ran = true
		return err
	})
	if err != nil {
		if res.Ran {
			writeSummary(log, opt.Out, res)
		}
		return res, err
	}
	if !ran {
		log.Info("backup skipped another run in progress")
		return Result{}, nil
	}
	res.Ran = true
	writeSummary(log, opt.Out, res)
	if res.Failed > 0 {
		return res, ErrCopyFailed
	}
	return res, nil
}

// Schedule starts a copy every interval. The first run is one interval after
// the call, not at the call. A run still going when the next is due delays the
// next. Cancelling ctx cancels the run in progress and stops the schedule.
func Schedule(ctx context.Context, every time.Duration, st *store.Store, primary, dst blob.Store, kr *objcrypt.Keyring, log *slog.Logger) {
	if every <= 0 || dst == nil {
		return
	}
	go func() {
		timer := time.NewTimer(every)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return
			}
			_, err := Run(ctx, st, primary, dst, kr, Options{Log: log})
			if ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, ErrCopyFailed) {
				log.Error("backup", "err", err)
			}
			timer.Reset(every)
		}
	}()
}

func copyMissing(ctx context.Context, primary, dst blob.Store, log *slog.Logger) (Result, error) {
	have, err := listPrefixes(ctx, primary)
	if err != nil {
		return Result{}, err
	}
	there, err := listPrefixes(ctx, dst)
	if err != nil {
		return Result{}, err
	}
	keys := make([]string, 0, len(have))
	for key := range have {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var res Result
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		size := have[key]
		if got, ok := there[key]; ok && got == size {
			res.Skipped++
			continue
		}
		if err := blob.Copy(ctx, primary, dst, key, size); err != nil {
			res.Failed++
			res.FailedKeys = append(res.FailedKeys, key)
			log.Error("backup copy failed", "key", key, "err", err)
			continue
		}
		res.Copied++
		res.Bytes += size
	}
	return res, nil
}

func listPrefixes(ctx context.Context, bucket blob.Store) (map[string]int64, error) {
	out := map[string]int64{}
	for _, prefix := range []string{"repos/", "tokens/"} {
		err := bucket.List(ctx, prefix, func(info blob.Info) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			out[info.Key] = info.Size
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func writeSummary(log *slog.Logger, out io.Writer, res Result) {
	line := fmt.Sprintf("backup copied=%d skipped=%d failed=%d bytes=%d", res.Copied, res.Skipped, res.Failed, res.Bytes)
	if out != nil {
		_, _ = fmt.Fprintln(out, line)
		return
	}
	log.Info(line)
}
