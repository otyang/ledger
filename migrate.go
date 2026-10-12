package ledger

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/uptrace/bun"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is the pg_advisory_lock key that serializes migrations
// across processes ("ledger" in ASCII).
const migrationLockKey int64 = 0x6c6564676572

type migration struct {
	version string // file name prefix, e.g. "20261012000001"
	up      string
	down    string
}

// Migrate applies all pending schema migrations. It is safe to call on every
// startup and from several processes at once: a Postgres advisory lock makes
// concurrent callers wait their turn, and is released automatically if a
// process dies. Each migration and its bookkeeping row commit in one
// transaction, so a crash can never leave a migration half-recorded.
func (s *Service) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	return s.withMigrationLock(ctx, func(conn bun.Conn, applied []string) error {
		for _, m := range migrations {
			if slices.Contains(applied, m.version) {
				continue
			}
			err := conn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
				// Run the file through database/sql directly: Bun would treat
				// any "?" in the SQL as a placeholder.
				if _, err := tx.Tx.ExecContext(ctx, m.up); err != nil {
					return err
				}
				_, err := tx.Tx.ExecContext(ctx, `INSERT INTO ledger.schema_migrations (version) VALUES ($1)`, m.version)
				return err
			})
			if err != nil {
				return fmt.Errorf("ledger: migration %s: %w", m.version, err)
			}
		}
		return nil
	})
}

// MigrateDown reverts the most recently applied migration. It is meant for
// operators and tests; applications normally only call Migrate.
func (s *Service) MigrateDown(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	return s.withMigrationLock(ctx, func(conn bun.Conn, applied []string) error {
		if len(applied) == 0 {
			return nil
		}
		last := applied[len(applied)-1]
		i := slices.IndexFunc(migrations, func(m migration) bool { return m.version == last })
		if i < 0 || migrations[i].down == "" {
			return fmt.Errorf("ledger: no down migration for %s", last)
		}
		return conn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.Tx.ExecContext(ctx, migrations[i].down); err != nil {
				return fmt.Errorf("ledger: migration %s down: %w", last, err)
			}
			_, err := tx.Tx.ExecContext(ctx, `DELETE FROM ledger.schema_migrations WHERE version = $1`, last)
			return err
		})
	})
}

// withMigrationLock pins one connection, takes the migration advisory lock on
// it, ensures the bookkeeping table exists, and calls fn with the applied
// versions in order.
func (s *Service) withMigrationLock(ctx context.Context, fn func(bun.Conn, []string) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("ledger: migrate: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(?)", migrationLockKey); err != nil {
		return fmt.Errorf("ledger: migration lock: %w", err)
	}
	// Unlock even if ctx is done; if this fails, closing the session releases it.
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(?)", migrationLockKey) //nolint:errcheck

	_, err = conn.ExecContext(ctx, `
		CREATE SCHEMA IF NOT EXISTS ledger;
		CREATE TABLE IF NOT EXISTS ledger.schema_migrations (
			version    TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("ledger: migrate: %w", err)
	}

	var applied []string
	if err := conn.NewSelect().
		TableExpr("ledger.schema_migrations").
		Column("version").
		Order("version").
		Scan(ctx, &applied); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("ledger: read applied migrations: %w", err)
	}
	return fn(conn, applied)
}

// loadMigrations reads the embedded <version>_<name>.{up,down}.sql files in
// version order.
func loadMigrations() ([]migration, error) {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	byVersion := map[string]*migration{}
	for _, f := range files {
		base := strings.TrimPrefix(f, "migrations/")
		version, _, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("ledger: bad migration file name %q", base)
		}
		body, err := fs.ReadFile(migrationFS, f)
		if err != nil {
			return nil, err
		}
		m := byVersion[version]
		if m == nil {
			m = &migration{version: version}
			byVersion[version] = m
		}
		switch {
		case strings.HasSuffix(base, ".up.sql"):
			m.up = string(body)
		case strings.HasSuffix(base, ".down.sql"):
			m.down = string(body)
		default:
			return nil, fmt.Errorf("ledger: bad migration file name %q", base)
		}
	}

	out := make([]migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.up == "" {
			return nil, fmt.Errorf("ledger: migration %s has no up file", m.version)
		}
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b migration) int { return strings.Compare(a.version, b.version) })
	return out, nil
}

// CheckSchema reports an error unless the database is reachable and every
// migration this binary knows about has been applied. Health checks use it.
func (s *Service) CheckSchema(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	var applied []string
	if err := s.db.NewSelect().TableExpr("ledger.schema_migrations").Column("version").Scan(ctx, &applied); err != nil {
		return classify(ctx, fmt.Errorf("ledger: read applied migrations: %w", err))
	}
	for _, m := range migrations {
		if !slices.Contains(applied, m.version) {
			return fmt.Errorf("ledger: migration %s is not applied", m.version)
		}
	}
	return nil
}
