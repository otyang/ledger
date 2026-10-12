// Package webhook delivers ledger events to an HTTP endpoint.
//
// Each event is POSTed as JSON, in order, at least once: a delivery that
// fails is retried (with backoff) until the endpoint answers 2xx, and only
// then does the dispatcher move on. Receivers should therefore deduplicate
// on the event ID. Progress is stored in the database, so a restart resumes
// where it left off, and a Postgres advisory lock ensures only one process
// delivers at a time even when several replicas run a dispatcher.
//
// Requests carry these headers:
//
//	X-Ledger-Event-Id    the event ID (evt_…)
//	X-Ledger-Event-Type  e.g. transaction.posted
//	X-Ledger-Signature   t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">
//
// Verify the signature with the shared secret, and reject timestamps that are
// too old, to block forged or replayed requests.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/otyang/ledger"
)

type Config struct {
	// URL receives the events. Required.
	URL string
	// Secret signs each request. Required.
	Secret string
	// Name identifies this dispatcher's progress. Use different names to
	// deliver the same events to several endpoints. Default "webhook".
	Name string
	// Interval is how often to look for new events when idle. Default 1s.
	Interval time.Duration
	// Client sends the requests. Default: a client with a 10s timeout that
	// doesn't follow redirects (a redirected POST would turn into a GET, and
	// its 2xx would wrongly count as delivered). A custom client should do
	// the same.
	Client *http.Client
	// Logger receives delivery failures. Default slog.Default().
	Logger *slog.Logger
}

type Dispatcher struct {
	svc *ledger.Service
	cfg Config
}

func New(svc *ledger.Service, cfg Config) (*Dispatcher, error) {
	if cfg.URL == "" || cfg.Secret == "" {
		return nil, errors.New("webhook: URL and Secret are required")
	}
	if cfg.Name == "" {
		cfg.Name = "webhook"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Dispatcher{svc: svc, cfg: cfg}, nil
}

// Run delivers events until ctx is done, then returns ctx.Err().
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		err := d.runLocked(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.cfg.Logger.WarnContext(ctx, "webhook dispatcher restarting", "err", err)
		if !sleep(ctx, d.cfg.Interval) {
			return ctx.Err()
		}
	}
}

// runLocked waits for the dispatcher's advisory lock, then delivers while
// holding it. The lock belongs to one database session, so it is released
// if this process dies, or if the session is lost (a network blip, a
// database restart). The cursor is read and written on that same session,
// and the lock is confirmed before every delivery, so a dispatcher that lost
// its lock stops instead of delivering alongside the new holder. Session
// locks don't survive a transaction-mode pooler such as PgBouncer; connect
// the dispatcher to Postgres directly.
func (d *Dispatcher) runLocked(ctx context.Context) error {
	conn, err := d.svc.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	h := fnv.New64a()
	h.Write([]byte("ledger-webhook:" + d.cfg.Name))
	key := int64(h.Sum64())
	for {
		var locked bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(?)", key).Scan(&locked); err != nil {
			return err
		}
		if locked {
			break
		}
		if !sleep(ctx, d.cfg.Interval) {
			return ctx.Err()
		}
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(?)", key) //nolint:errcheck

	// pg_locks shows a bigint advisory key as its high and low 32 bits.
	hi, lo := int64(uint64(key)>>32), int64(uint64(key)&0xffffffff)
	stillLocked := func() error {
		var held bool
		err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()
			  AND classid::bigint = ? AND objid::bigint = ? AND objsubid = 1)`, hi, lo).Scan(&held)
		if err == nil && !held {
			err = errors.New("webhook: lost the delivery lock")
		}
		return err
	}

	for {
		cursor, err := d.svc.EventCursorTx(ctx, conn, d.cfg.Name)
		if err != nil {
			return err
		}
		events, err := d.svc.ListEvents(ctx, ledger.ListEventsParams{After: cursor, Limit: 100})
		if err != nil {
			return err
		}
		if len(events) == 0 {
			if !sleep(ctx, d.cfg.Interval) {
				return ctx.Err()
			}
			continue
		}
		for _, ev := range events {
			if err := stillLocked(); err != nil {
				return err
			}
			if err := d.deliverWithRetry(ctx, ev, stillLocked); err != nil {
				return err
			}
			if err := d.svc.SetEventCursorTx(ctx, conn, d.cfg.Name, ev.ID); err != nil {
				return err
			}
		}
	}
}

func (d *Dispatcher) deliverWithRetry(ctx context.Context, ev *ledger.Event, stillLocked func() error) error {
	backoff := time.Second
	for {
		err := d.deliver(ctx, ev)
		if err == nil {
			return nil
		}
		if lockErr := stillLocked(); lockErr != nil {
			return lockErr
		}
		d.cfg.Logger.WarnContext(ctx, "webhook delivery failed; retrying", "event", ev.ID, "type", ev.Type, "retry_in", backoff, "err", err)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(2*backoff, time.Minute)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, ev *ledger.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ledgerd-webhook")
	req.Header.Set("X-Ledger-Event-Id", string(ev.ID))
	req.Header.Set("X-Ledger-Event-Type", ev.Type)
	req.Header.Set("X-Ledger-Signature", "t="+ts+",v1="+Sign(d.cfg.Secret, ts, body))
	resp, err := d.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if loc := resp.Header.Get("Location"); loc != "" {
			return fmt.Errorf("endpoint answered %s to %s; redirects aren't followed, update LEDGER_WEBHOOK_URL", resp.Status, loc)
		}
		return fmt.Errorf("endpoint answered %s", resp.Status)
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 of "<timestamp>.<body>" with secret, as
// sent in X-Ledger-Signature. Receivers can use it to verify requests.
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
