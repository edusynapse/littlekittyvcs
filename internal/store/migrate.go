package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateLock is the PostgreSQL transaction advisory lock that serializes
// schema migrations, so several processes starting at once apply each file once.
const migrateLock int64 = 0x6c6b760000000001

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies embedded SQL files that this database does not yet have.
// A database whose recorded version is newer than this binary is refused.
// Newly applied versions are logged on log.
func (s *Store) Migrate(ctx context.Context, log *slog.Logger) error {
	files, err := loadMigrations()
	if err != nil {
		return err
	}
	if err := s.withMigrateLock(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			create table if not exists schema_migrations (
				version int primary key,
				applied_at timestamptz not null default now()
			)`)
		return err
	}); err != nil {
		return err
	}
	for _, file := range files {
		applied, err := s.applyMigration(ctx, file)
		if err != nil {
			return err
		}
		if applied {
			log.Info("migration applied", "version", file.version)
		}
	}
	return s.withMigrateLock(ctx, func(tx pgx.Tx) error {
		var newer int
		err := tx.QueryRow(ctx, `select count(*) from schema_migrations where version > $1`, len(files)).Scan(&newer)
		if err != nil {
			return err
		}
		if newer > 0 {
			return errorsNewer()
		}
		return nil
	})
}

func errorsNewer() error {
	return fmt.Errorf("store: database schema is newer than this binary")
}

func (s *Store) applyMigration(ctx context.Context, file migration) (bool, error) {
	applied := false
	err := s.withMigrateLock(ctx, func(tx pgx.Tx) error {
		var exists bool
		err := tx.QueryRow(ctx, `select exists (select 1 from schema_migrations where version = $1)`, file.version).Scan(&exists)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		// The file is several statements. The extended protocol accepts one.
		// PgConn.Exec uses the simple protocol, inside the transaction already open.
		if _, err := tx.Conn().PgConn().Exec(ctx, file.sql).ReadAll(); err != nil {
			return fmt.Errorf("store: migration %d: %w", file.version, err)
		}
		if _, err := tx.Exec(ctx, `insert into schema_migrations (version) values ($1)`, file.version); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

func (s *Store) withMigrateLock(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, migrateLock); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	parsed, err := parseMigrationNames(names)
	if err != nil {
		return nil, err
	}
	for i := range parsed {
		body, err := migrationFS.ReadFile("migrations/" + parsed[i].name)
		if err != nil {
			return nil, err
		}
		parsed[i].sql = string(body)
	}
	return parsed, nil
}

func parseMigrationNames(names []string) ([]migration, error) {
	out := make([]migration, 0, len(names))
	seen := make(map[int]string, len(names))
	for _, name := range names {
		match := migrationName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("store: migration %s: name must match NNNN_words.sql", name)
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration %s: bad version", name)
		}
		if prev, ok := seen[version]; ok {
			return nil, fmt.Errorf("store: migration version %d is in %s and %s", version, prev, name)
		}
		seen[version] = name
		out = append(out, migration{version: version, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, file := range out {
		if file.version != i+1 {
			return nil, fmt.Errorf("store: migration versions must be 1..%d with no gap", len(out))
		}
	}
	return out, nil
}
