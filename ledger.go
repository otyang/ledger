// Package ledger is an embeddable double-entry ledger on PostgreSQL, built on
// the Bun ORM.
//
// The caller owns the database connection:
//
//	db := ledger.OpenDB(os.Getenv("DATABASE_URL"))
//	svc := ledger.New(db)
//	if err := svc.Migrate(ctx); err != nil { ... }
//
// All tables live in the "ledger" Postgres schema, so they do not collide with
// the host application's tables.
package ledger

import (
	"database/sql"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

type Service struct {
	db *bun.DB
	// currencies caches registered currencies by code, as *Currency values
	// that are never handed out (callers get copies). Only currencies with
	// decimals are cached: their decimals never change, and migration 5
	// refuses to revert while any are set, so entries can't go stale.
	currencies sync.Map
}

func New(db *bun.DB) *Service {
	return &Service{db: db}
}

// DB returns the underlying database handle.
func (s *Service) DB() *bun.DB {
	return s.db
}

// OpenDB is a convenience for opening a Postgres-backed *bun.DB from a DSN such
// as "postgres://user:pass@localhost:5432/app?sslmode=disable". Sessions use
// UTC so timestamps read back match the UTC values written.
func OpenDB(dsn string) *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dsn),
		pgdriver.WithConnParams(map[string]any{"TimeZone": "UTC"}),
	))
	return bun.NewDB(sqldb, pgdialect.New())
}

// now is truncated to microseconds, the resolution Postgres stores, so values
// returned from writes match what a later read returns.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// later returns the later of two times.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
