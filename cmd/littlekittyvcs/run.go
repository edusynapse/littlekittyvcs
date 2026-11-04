package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/edusynapse/littlekittyvcs/internal/auth"
	"github.com/edusynapse/littlekittyvcs/internal/backup"
	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/config"
	"github.com/edusynapse/littlekittyvcs/internal/gc"
	"github.com/edusynapse/littlekittyvcs/internal/httpapi"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/snapshot"
	"github.com/edusynapse/littlekittyvcs/internal/store"
	"github.com/edusynapse/littlekittyvcs/internal/tokenbackup"
)

// chunkCacheBytes is the process-wide decrypted-chunk cache.
// 256 MiB is provisional for the 1 GiB instance. Build step 15 sets the real value.
const chunkCacheBytes = 256 << 20

// runServe checks settings, migrates, and serves. A nil listener binds 0.0.0.0.
// Cancellation shuts the server down and returns nil.
func runServe(ctx context.Context, getenv config.Getenv, ln net.Listener, log *slog.Logger) error {
	// A caller-supplied listener is closed when startup fails before Serve.
	// Serve itself closes the listener it accepts.
	serving := false
	if ln != nil {
		defer func() {
			if !serving {
				_ = ln.Close()
			}
		}()
	}
	cfg, err := config.LoadServe(getenv)
	if err != nil {
		return err
	}
	kr, err := objcrypt.NewKeyring(cfg.MasterKey)
	if err != nil {
		return err
	}
	// Building a client does not contact the bucket.
	bucket, err := blob.NewS3(ctx, cfg.S3)
	if err != nil {
		return err
	}
	if bucket == nil {
		return errors.New("littlekittyvcs: bucket client was not built")
	}
	var backupBucket blob.Store
	if cfg.Backup.Enabled {
		backupBucket, err = blob.NewS3(ctx, cfg.Backup.S3)
		if err != nil {
			return err
		}
	}
	st, err := store.Open(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}
	if ln == nil {
		ln, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", cfg.Port))
		if err != nil {
			return err
		}
	}
	fields := []any{
		"port", cfg.Port,
		"public_url", cfg.PublicURL,
		"anonymous_read", cfg.AnonymousRead,
		"deny_non_fast_forward", cfg.DenyNonFastForward,
		"auto_create", cfg.AutoCreate,
		"s3_endpoint", cfg.S3.Endpoint,
		"s3_region", cfg.S3.Region,
		"s3_bucket", cfg.S3.Bucket,
		"s3_path_style", cfg.S3.PathStyle,
		"backup", cfg.Backup.Enabled,
		"chunk_cache_bytes", chunkCacheBytes,
	}
	if cfg.Backup.Enabled {
		interval := ""
		if cfg.Backup.Interval > 0 {
			interval = cfg.Backup.Interval.String()
		}
		fields = append(fields,
			"backup_endpoint", cfg.Backup.S3.Endpoint,
			"backup_region", cfg.Backup.S3.Region,
			"backup_bucket", cfg.Backup.S3.Bucket,
			"backup_path_style", cfg.Backup.S3.PathStyle,
			"backup_interval", interval,
		)
	}
	log.Info("serving", fields...)
	if cfg.Backup.Enabled && cfg.Backup.Interval > 0 {
		backup.Schedule(ctx, cfg.Backup.Interval, st, bucket, backupBucket, kr, log)
	}
	chunks := objcrypt.NewChunkCache(chunkCacheBytes)
	srv := &http.Server{
		Handler: httpapi.New(log, httpapi.Deps{
			Store:        st,
			Bucket:       bucket,
			BackupBucket: backupBucket,
			Keyring:      kr,
			Index:        objindex.New(),
			ChunkCache:   chunks,
		}, httpapi.Config{
			AnonymousRead:      cfg.AnonymousRead,
			AutoCreate:         cfg.AutoCreate,
			DenyNonFastForward: cfg.DenyNonFastForward,
			PublicURL:          cfg.PublicURL,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serving = true
	errCh := make(chan error, 1)
	go func() {
		serveErr := srv.Serve(ln)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		errCh <- serveErr
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

// runAdminToken creates an admin token, prints it once, and stores only its hash.
func runAdminToken(ctx context.Context, getenv config.Getenv, args []string, stdout io.Writer, log *slog.Logger) error {
	fs := flag.NewFlagSet("admin-token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	label := fs.String("label", "", "name of the token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("admin-token: unexpected arguments")
	}
	if n := utf8.RuneCountInString(*label); n < 1 || n > 200 {
		return errors.New("admin-token: label must be 1 to 200 characters")
	}
	cfg, err := config.LoadStorage(getenv)
	if err != nil {
		return err
	}
	kr, err := objcrypt.NewKeyring(cfg.MasterKey)
	if err != nil {
		return err
	}
	bucket, backupBucket, err := openStorageBuckets(ctx, cfg)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}
	plaintext, hash, err := auth.Generate()
	if err != nil {
		return err
	}
	id, err := store.NewID()
	if err != nil {
		return err
	}
	err = st.InsertToken(ctx, hash, store.TokenRow{
		ID:     id,
		Label:  *label,
		Write:  true,
		Admin:  true,
		RepoID: nil,
	})
	if err != nil {
		return err
	}
	// The token is already committed. A failed backup is logged and the token
	// is still printed: this is the only time the operator sees it.
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, berr := tokenbackup.WriteCurrent(bctx, st, kr, bucket, backupBucket); berr != nil {
		log.Error("token backup", "err", berr)
	}
	if _, err := fmt.Fprintln(stdout, plaintext); err != nil {
		return err
	}
	log.Info("admin token created", "id", store.FormatID(id), "label", *label)
	return nil
}

// runRestore rebuilds repository, ref, and pack rows from the bucket.
// Lines go to stdout. A failed repository, or a run that cannot continue, is an error.
func runRestore(ctx context.Context, getenv config.Getenv, args []string, stdout io.Writer, log *slog.Logger) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "restore only this repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("restore: unexpected arguments")
	}
	if *repo != "" && !store.ValidRepoName(*repo) {
		return errors.New("restore: invalid repository name")
	}
	cfg, err := config.LoadStorage(getenv)
	if err != nil {
		return err
	}
	kr, err := objcrypt.NewKeyring(cfg.MasterKey)
	if err != nil {
		return err
	}
	bucket, err := blob.NewS3(ctx, cfg.S3)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}
	failed, err := snapshot.Restore(ctx, st, bucket, kr, snapshot.Options{
		OnlyName: *repo,
		Out:      stdout,
	})
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("restore: %d repositories failed", failed)
	}
	return nil
}

// runGC deletes bucket objects nothing needs. Lines go to stdout.
// A refused repository, a failed delete, or a run that cannot continue is an error.
func runGC(ctx context.Context, getenv config.Getenv, args []string, stdout io.Writer, log *slog.Logger) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "print what would be deleted")
	useBackup := fs.Bool("backup", false, "delete from the backup bucket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("gc: unexpected arguments")
	}
	cfg, err := config.LoadStorage(getenv)
	if err != nil {
		return err
	}
	if *useBackup && !cfg.Backup.Enabled {
		return errors.New("gc: BACKUP_ENABLED is required")
	}
	kr, err := objcrypt.NewKeyring(cfg.MasterKey)
	if err != nil {
		return err
	}
	s3cfg := cfg.S3
	if *useBackup {
		s3cfg = cfg.Backup.S3
	}
	bucket, err := blob.NewS3(ctx, s3cfg)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}
	result, err := gc.Run(ctx, st, bucket, kr, gc.Options{
		Now:    time.Now,
		DryRun: *dry,
		Out:    stdout,
	})
	if err != nil {
		return err
	}
	switch {
	case result.Refused > 0 && result.Failures > 0:
		return fmt.Errorf("gc: %d repositories refused; %d deletes failed", result.Refused, result.Failures)
	case result.Refused > 0:
		return fmt.Errorf("gc: %d repositories refused", result.Refused)
	case result.Failures > 0:
		return fmt.Errorf("gc: %d deletes failed", result.Failures)
	}
	return nil
}

// runBackup copies objects the backup bucket is missing. The summary line goes
// to stdout. A skipped run, because another process holds the lock, exits 0.
func runBackup(ctx context.Context, getenv config.Getenv, args []string, stdout io.Writer, log *slog.Logger) error {
	if len(args) != 0 {
		return errors.New("backup: unexpected arguments")
	}
	cfg, err := config.LoadStorage(getenv)
	if err != nil {
		return err
	}
	if !cfg.Backup.Enabled {
		return errors.New("backup: BACKUP_ENABLED is required")
	}
	kr, err := objcrypt.NewKeyring(cfg.MasterKey)
	if err != nil {
		return err
	}
	primary, dst, err := openStorageBuckets(ctx, cfg)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}
	_, err = backup.Run(ctx, st, primary, dst, kr, backup.Options{Log: log, Out: stdout})
	return err
}

// runRestoreTokens loads the newest token backup from the primary bucket.
func runRestoreTokens(ctx context.Context, getenv config.Getenv, args []string, stdout io.Writer, log *slog.Logger) error {
	if len(args) != 0 {
		return errors.New("restore-tokens: unexpected arguments")
	}
	cfg, err := config.LoadStorage(getenv)
	if err != nil {
		return err
	}
	kr, err := objcrypt.NewKeyring(cfg.MasterKey)
	if err != nil {
		return err
	}
	bucket, _, err := openStorageBuckets(ctx, cfg)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}
	res, err := tokenbackup.Restore(ctx, st, bucket, kr)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, tokenbackup.FormatSummary(res))
	return err
}

// testBuckets, when set by a test, replaces the S3 clients. Production leaves it nil.
var testBuckets func(context.Context, config.Storage) (blob.Store, blob.Store, error)

func openStorageBuckets(ctx context.Context, cfg config.Storage) (blob.Store, blob.Store, error) {
	if testBuckets != nil {
		return testBuckets(ctx, cfg)
	}
	primary, err := blob.NewS3(ctx, cfg.S3)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Backup.Enabled {
		return primary, nil, nil
	}
	extra, err := blob.NewS3(ctx, cfg.Backup.S3)
	if err != nil {
		return nil, nil, err
	}
	return primary, extra, nil
}
