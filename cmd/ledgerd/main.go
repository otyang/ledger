// Command ledgerd runs the ledger as a standalone HTTP service.
//
// Environment:
//
//	LEDGER_DATABASE_URL      Postgres DSN (required), e.g. postgres://localhost:5432/ledger?sslmode=disable
//	LEDGER_ADDR              listen address (default :7710)
//	LEDGER_API_TOKEN         bearer token required on every request (required unless LEDGER_INSECURE_NO_AUTH=true)
//	LEDGER_INSECURE_NO_AUTH  set to "true" to run without authentication, e.g. behind an authenticating proxy
//	LEDGER_REQUEST_TIMEOUT   per-request timeout including lock waits (default 10s)
//	LEDGER_RATE_LIMIT        requests per second allowed per client IP (default 0: no limit)
//	LEDGER_RATE_BURST        burst size for LEDGER_RATE_LIMIT (default: the rate, rounded up)
//	LEDGER_HOLD_EXPIRY_INTERVAL  how often to release expired holds (default 1m; 0 disables)
//	LEDGER_SNAPSHOT_INTERVAL     how often to refresh untracked-account snapshots (default 5m; 0 disables)
//	LEDGER_WEBHOOK_URL       deliver every event to this URL (default: no webhooks)
//	LEDGER_WEBHOOK_SECRET    signs webhook requests (required with LEDGER_WEBHOOK_URL)
//	LEDGER_SHUTDOWN_DRAIN    on SIGTERM, keep serving this long with /readyz failing, so load balancers stop routing first (default 0)
//
// Flags:
//
//	-version  print the version and exit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/otyang/ledger"
	"github.com/otyang/ledger/httpapi"
	"github.com/otyang/ledger/webhook"
)

// defaultAddr is deliberately not 8080, which many local tools already use.
const defaultAddr = ":7710"

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = ""

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("ledgerd", buildVersion())
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("ledgerd stopped", "err", err)
		os.Exit(1)
	}
}

// buildVersion is the -ldflags version, or else the module version recorded
// by "go install", or else "dev".
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("LEDGER_DATABASE_URL")
	if dsn == "" {
		return errors.New("LEDGER_DATABASE_URL is required")
	}
	addr := os.Getenv("LEDGER_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	token := os.Getenv("LEDGER_API_TOKEN")
	if token == "" && os.Getenv("LEDGER_INSECURE_NO_AUTH") != "true" {
		return errors.New("LEDGER_API_TOKEN is required (or set LEDGER_INSECURE_NO_AUTH=true)")
	}
	timeout, err := durationEnv("LEDGER_REQUEST_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}
	expiryEvery, err := durationEnv("LEDGER_HOLD_EXPIRY_INTERVAL", time.Minute)
	if err != nil {
		return err
	}
	snapshotEvery, err := durationEnv("LEDGER_SNAPSHOT_INTERVAL", 5*time.Minute)
	if err != nil {
		return err
	}
	drain, err := durationEnv("LEDGER_SHUTDOWN_DRAIN", 0)
	if err != nil {
		return err
	}
	var rateLimit float64
	var rateBurst int
	if v := os.Getenv("LEDGER_RATE_LIMIT"); v != "" {
		if rateLimit, err = strconv.ParseFloat(v, 64); err != nil || rateLimit < 0 {
			return fmt.Errorf("LEDGER_RATE_LIMIT must be a non-negative number, not %q", v)
		}
	}
	if v := os.Getenv("LEDGER_RATE_BURST"); v != "" {
		if rateBurst, err = strconv.Atoi(v); err != nil || rateBurst < 0 {
			return fmt.Errorf("LEDGER_RATE_BURST must be a non-negative integer, not %q", v)
		}
	}
	webhookURL, webhookSecret := os.Getenv("LEDGER_WEBHOOK_URL"), os.Getenv("LEDGER_WEBHOOK_SECRET")
	if webhookURL != "" && webhookSecret == "" {
		return errors.New("LEDGER_WEBHOOK_SECRET is required with LEDGER_WEBHOOK_URL")
	}

	// Claim the port before touching the database, so a clash is reported
	// at once and "listening" is only logged once it is true.
	ln, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("address %s is already in use by another program; stop it or set LEDGER_ADDR to a free address, e.g. LEDGER_ADDR=:7711", addr)
	}
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer ln.Close()

	db := ledger.OpenDB(dsn)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	svc := ledger.New(db)
	if err := svc.Migrate(ctx); err != nil {
		return err
	}

	var ready atomic.Bool
	ready.Store(true)
	writeTimeout := time.Duration(0) // none, like the requests
	if timeout > 0 {
		writeTimeout = timeout + 5*time.Second
	}
	srv := &http.Server{
		Handler: httpapi.New(svc, httpapi.Config{
			Logger: log, Token: token, RequestTimeout: timeout,
			Ready: ready.Load, RateLimit: rateLimit, RateBurst: rateBurst,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       2 * time.Minute,
	}
	// Background jobs stop when jobsCtx is cancelled, after the server has
	// stopped accepting requests.
	jobsCtx, stopJobs := context.WithCancel(context.Background())
	var jobs sync.WaitGroup
	defer func() { stopJobs(); jobs.Wait() }()
	every(jobsCtx, &jobs, expiryEvery, log, "expire holds", func(ctx context.Context) (int, error) {
		return svc.ExpireHolds(ctx, 1000)
	})
	every(jobsCtx, &jobs, snapshotEvery, log, "refresh snapshots", svc.RefreshSnapshots)
	if webhookURL != "" {
		d, err := webhook.New(svc, webhook.Config{URL: webhookURL, Secret: webhookSecret, Logger: log})
		if err != nil {
			return err
		}
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			_ = d.Run(jobsCtx)
		}()
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("ledgerd listening", "addr", ln.Addr().String(), "version", buildVersion())
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	ready.Store(false)
	if drain > 0 {
		log.Info("draining: readiness fails while requests are still served", "for", drain.String())
		time.Sleep(drain)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a duration such as 30s or 5m, not %q", name, v)
	}
	return d, nil
}

// every runs job every interval until ctx is done. An interval of 0
// disables it.
func every(ctx context.Context, wg *sync.WaitGroup, interval time.Duration, log *slog.Logger, name string, job func(context.Context) (int, error)) {
	if interval <= 0 {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if n, err := job(ctx); err != nil && ctx.Err() == nil {
				log.Error("background job failed", "job", name, "err", err)
			} else if n > 0 {
				log.Info("background job done", "job", name, "count", n)
			}
		}
	}()
}
