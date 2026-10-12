package webhook_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otyang/ledger"
	"github.com/otyang/ledger/ledgertest"
	"github.com/otyang/ledger/webhook"
)

func TestMain(m *testing.M) {
	os.Exit(ledgertest.Run(m))
}

func TestDispatcher(t *testing.T) {
	ctx := context.Background()
	svc := ledger.New(ledgertest.NewDB(t))
	if _, err := svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "USD", Decimals: 2}); err != nil {
		t.Fatal(err)
	}
	l, err := svc.CreateLedger(ctx, ledger.CreateLedgerParams{Name: "main"})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	failures := 2 // the first deliveries fail, to exercise retries
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, sig, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("X-Ledger-Signature"), "t="), ",v1=")
		if sig != webhook.Sign("s3cret", ts, body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			http.Error(w, "try later", http.StatusServiceUnavailable)
			return
		}
		var ev ledger.Event
		_ = json.Unmarshal(body, &ev)
		if ev.ID != ledger.EventID(r.Header.Get("X-Ledger-Event-Id")) || ev.Type != r.Header.Get("X-Ledger-Event-Type") {
			http.Error(w, "header mismatch", http.StatusBadRequest)
			return
		}
		got = append(got, ev.Type)
	}))
	defer srv.Close()

	d, err := webhook.New(svc, webhook.Config{URL: srv.URL, Secret: "s3cret", Interval: 50 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- d.Run(runCtx) }()

	if _, err := svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: l.ID, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"currency.registered", "ledger.created", "account.created"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("delivered %v, want %v (in order, once each)", got, want)
	}
	cursor, err := svc.EventCursor(ctx, "webhook")
	if err != nil || cursor == "" {
		t.Errorf("cursor = %q, %v; want the last delivered event", cursor, err)
	}
	if _, err := webhook.New(svc, webhook.Config{URL: srv.URL}); err == nil {
		t.Error("a dispatcher without a secret must be refused")
	}
}

// A redirect is a failed delivery: following it would turn the POST into a
// body-less GET whose 2xx would wrongly count as delivered.
func TestRedirectIsNotDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := ledger.New(ledgertest.NewDB(t))
	if _, err := svc.CreateLedger(ctx, ledger.CreateLedgerParams{Name: "main"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		mu.Lock()
		gets++
		mu.Unlock()
	}))
	defer srv.Close()
	d, _ := webhook.New(svc, webhook.Config{URL: srv.URL + "/old", Secret: "s", Interval: 50 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	go d.Run(ctx)
	time.Sleep(1500 * time.Millisecond)
	if c, _ := svc.EventCursor(ctx, "webhook"); c != "" {
		t.Errorf("cursor advanced to %s after a redirect", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets != 0 {
		t.Errorf("the redirect was followed %d times", gets)
	}
}

// A dispatcher whose database session (and with it the lock) is lost stops
// delivering, so it never delivers alongside the dispatcher that takes over.
func TestLockLossStopsDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := ledgertest.NewDB(t)
	svc := ledger.New(db)
	var mu sync.Mutex
	who := map[string]map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		id := r.Header.Get("X-Ledger-Event-Id")
		if who[id] == nil {
			who[id] = map[string]bool{}
		}
		who[id][r.URL.Query().Get("who")] = true
	}))
	defer srv.Close()
	start := func(name string) {
		d, _ := webhook.New(svc, webhook.Config{URL: srv.URL + "/?who=" + name, Secret: "s", Interval: 50 * time.Millisecond,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		go d.Run(ctx)
	}
	start("A")
	time.Sleep(500 * time.Millisecond)
	var killed []bool
	if err := db.NewRaw(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted`).Scan(ctx, &killed); err != nil {
		t.Fatal(err)
	}
	start("B")
	time.Sleep(500 * time.Millisecond)
	for range 20 {
		if _, err := svc.CreateLedger(ctx, ledger.CreateLedgerParams{Name: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(4 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	both := 0
	for _, m := range who {
		if len(m) > 1 {
			both++
		}
	}
	// One event may be in flight at the moment of takeover; none beyond.
	if both > 1 || len(who) < 20 {
		t.Errorf("%d of %d events were delivered by both dispatchers", both, len(who))
	}
}
