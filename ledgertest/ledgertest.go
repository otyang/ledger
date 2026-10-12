// Package ledgertest provides throwaway, migrated Postgres databases for tests,
// without Docker.
//
// Wire it into a test package with TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(ledgertest.Run(m)) }
//
// then call ledgertest.NewDB(t) in each test.
//
// By default Run starts an embedded Postgres server (the binary is downloaded
// once and cached in ~/.embedded-postgres-go). Set LEDGER_TEST_DATABASE_URL to
// use an existing server instead, e.g. a local Homebrew Postgres:
//
//	LEDGER_TEST_DATABASE_URL=postgres://localhost:5432/postgres?sslmode=disable
//
// Either way each NewDB call creates its own database, dropped when the test ends.
package ledgertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/uptrace/bun"

	"github.com/otyang/ledger"
)

const EnvDatabaseURL = "LEDGER_TEST_DATABASE_URL"

// adminDSN points at a maintenance database used to create and drop the
// per-test databases. It is set by Run.
var adminDSN string

// Run starts the test Postgres server if needed, runs the tests, and stops the
// server. It returns the exit code for os.Exit.
func Run(m *testing.M) int {
	if dsn := os.Getenv(EnvDatabaseURL); dsn != "" {
		adminDSN = dsn
		return m.Run()
	}

	stop, err := startEmbedded()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledgertest: %v\n", err)
		return 1
	}
	defer stop()
	return m.Run()
}

func startEmbedded() (stop func(), err error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "ledgertest-pg-*")
	if err != nil {
		return nil, err
	}

	var logs bytes.Buffer
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		Logger(&logs))
	if err := pg.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start embedded postgres: %w\n%s", err, logs.String())
	}

	adminDSN = fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	return func() {
		_ = pg.Stop()
		_ = os.RemoveAll(dir)
	}, nil
}

// NewDB returns a connection to a fresh database with the ledger schema
// migrated. The database is dropped when the test finishes.
func NewDB(t testing.TB) *bun.DB {
	t.Helper()
	if adminDSN == "" {
		t.Fatal("ledgertest: call ledgertest.Run from TestMain first")
	}
	ctx := context.Background()

	admin := ledger.OpenDB(adminDSN)
	t.Cleanup(func() { _ = admin.Close() })

	name := "ledgertest_" + randomHex(8)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("ledgertest: create database: %v", err)
	}

	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		t.Fatalf("ledgertest: %v", err)
	}
	db := ledger.OpenDB(dsn)
	t.Cleanup(func() {
		_ = db.Close()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("ledgertest: drop database: %v", err)
		}
	})

	if err := ledger.New(db).Migrate(ctx); err != nil {
		t.Fatalf("ledgertest: %v", err)
	}
	return db
}

func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", EnvDatabaseURL, err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
