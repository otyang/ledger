package httpapi_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otyang/ledger"
	"github.com/otyang/ledger/httpapi"
	"github.com/otyang/ledger/ledgertest"
)

type client struct {
	t   *testing.T
	url string
}

func newClient(t *testing.T, cfg httpapi.Config) (*client, *ledger.Service) {
	t.Helper()
	svc := ledger.New(ledgertest.NewDB(t))
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.Token = "secret"
	srv := httptest.NewServer(httpapi.New(svc, cfg))
	t.Cleanup(srv.Close)
	return &client{t: t, url: srv.URL}, svc
}

func (c *client) raw(method, path string, body any, auth bool) (*http.Response, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.url+path, rd)
	if auth {
		req.Header.Set("Authorization", "Bearer secret")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (c *client) call(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	resp, b := c.raw(method, path, body, true)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d %s, want %d", method, path, resp.StatusCode, b, want)
	}
	return out
}

func leg(account any, dir, value string) map[string]any {
	return map[string]any{"account_id": account, "direction": dir, "value": value}
}

func TestBusinessEndpoints(t *testing.T) {
	c, _ := newClient(t, httpapi.Config{})
	c.call("POST", "/currencies", map[string]any{"code": "USD", "decimals": 2}, 201)
	c.call("POST", "/currencies", map[string]any{"code": "EUR", "decimals": 2}, 201)
	ldg := c.call("POST", "/ledgers", map[string]any{"name": "main"}, 201)
	L := ldg["id"].(string)
	acct := func(body map[string]any) string {
		body["ledger_id"] = L
		if body["currency"] == nil {
			body["currency"] = "USD"
		}
		return c.call("POST", "/accounts", body, 201)["id"].(string)
	}
	world := acct(map[string]any{"indicator": "@world", "allow_negative": true, "untracked": true})
	alice := acct(map[string]any{"overdraft_limit_value": "10.00", "type": "liability"})
	bob := acct(map[string]any{"type": "liability"})

	pay := c.call("POST", "/transactions", map[string]any{"reference": "dep", "currency": "USD",
		"metadata": map[string]any{"customer_id": "c_1"},
		"legs":     []any{leg(world, "debit", "100.00"), leg(alice, "credit", "100.00")}}, 201)

	// Account controls.
	a := c.call("PATCH", "/accounts/"+alice, map[string]any{"status": "frozen", "status_reason": "review"}, 200)
	if a["status"] != "frozen" || a["overdraft_limit_value"] != "10.00" || a["type"] != "liability" {
		t.Errorf("patched account = %v", a)
	}
	errBody := c.call("POST", "/transactions", map[string]any{"reference": "out", "currency": "USD",
		"legs": []any{leg(alice, "debit", "1"), leg(bob, "credit", "1")}}, 422)
	if code := errBody["error"].(map[string]any)["code"]; code != "account_frozen" {
		t.Errorf("frozen debit code = %v", code)
	}
	c.call("PATCH", "/accounts/"+alice, map[string]any{"status": "active"}, 200)

	// Metadata, reversal, search.
	txnID := pay["id"].(string)
	upd := c.call("PATCH", "/transactions/"+txnID, map[string]any{"metadata": map[string]any{"invoice": "inv-1"}}, 200)
	if md := upd["metadata"].(map[string]any); md["invoice"] != "inv-1" || md["customer_id"] != "c_1" {
		t.Errorf("metadata = %v", md)
	}
	found := c.call("GET", "/ledgers/"+L+"/transactions?metadata.customer_id=c_1", nil, 200)["transactions"].([]any)
	if len(found) != 1 || found[0].(map[string]any)["id"] != txnID {
		t.Errorf("search = %v", found)
	}
	c.call("POST", "/transactions/"+txnID+"/reversals", map[string]any{"reference": "rev"}, 400)
	rev := c.call("POST", "/transactions/"+txnID+"/reversals", map[string]any{"reference": "rev", "reason": "test deposit"}, 201)
	if rev["reversal_reason"] != "test deposit" || rev["refund_of"] != txnID {
		t.Errorf("reversal = %v", rev)
	}

	// Batch and exchange.
	batch := c.call("POST", "/batches", map[string]any{"reference": "b1", "transactions": []any{
		map[string]any{"currency": "USD", "legs": []any{leg(world, "debit", "50"), leg(alice, "credit", "50")}},
		map[string]any{"currency": "USD", "legs": []any{leg(alice, "debit", "20"), leg(bob, "credit", "20")}},
	}}, 201)
	if txns := batch["transactions"].([]any); len(txns) != 2 || batch["kind"] != "batch" {
		t.Errorf("batch = %v", batch)
	}
	if got := c.call("GET", "/batches/"+batch["id"].(string), nil, 200); got["reference"] != "b1" {
		t.Errorf("get batch = %v", got)
	}
	eurWorld := acct(map[string]any{"currency": "EUR", "indicator": "@world", "allow_negative": true})
	aliceEUR := acct(map[string]any{"currency": "EUR"})
	fxUSD := acct(map[string]any{"indicator": "@fx"})
	ex := c.call("POST", "/exchanges", map[string]any{"reference": "fx", "rate": "0.9",
		"source": map[string]any{"currency": "USD", "from": alice, "to": fxUSD, "value": "10.00"},
		"target": map[string]any{"currency": "EUR", "from": eurWorld, "to": aliceEUR, "value": "9.00"}}, 201)
	if ex["kind"] != "exchange" || ex["rate"] != "0.9" {
		t.Errorf("exchange = %v", ex)
	}

	// Reports.
	tb := c.call("GET", "/ledgers/"+L+"/trial-balance", nil, 200)
	for _, tot := range tb["totals"].([]any) {
		if tot.(map[string]any)["total"] != "0" {
			t.Errorf("trial balance total = %v", tot)
		}
	}
	bs := c.call("GET", "/ledgers/"+L+"/balance-sheet?currency=USD", nil, 200)
	if bs["liabilities"].(map[string]any)["total_value"] != "40.00" {
		t.Errorf("balance sheet liabilities = %v", bs["liabilities"])
	}
	c.call("GET", "/ledgers/"+L+"/income-statement?currency=USD&from=2020-01-01T00:00:00Z", nil, 200)
	c.call("GET", "/ledgers/"+L+"/balance-sheet", nil, 400)

	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	st := c.call("GET", "/accounts/"+alice+"/statement?from="+from, nil, 200)
	if st["closing_balance_value"] != "20.00" || st["opening_balance_value"] != "0.00" {
		t.Errorf("statement = opening %v closing %v", st["opening_balance_value"], st["closing_balance_value"])
	}
	resp, body := c.raw("GET", "/accounts/"+alice+"/statement.csv?from="+from, nil, true)
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" || len(rows) != 8 || rows[0][0] != "created_at" || rows[7][9] != "20.00" {
		t.Errorf("csv = %d rows, %v, %q", len(rows), err, body)
	}

	// Period close and events.
	through := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	c.call("POST", "/ledgers/"+L+"/periods", map[string]any{"through": through}, 201)
	if periods := c.call("GET", "/ledgers/"+L+"/periods", nil, 200)["periods"].([]any); len(periods) != 1 {
		t.Errorf("periods = %v", periods)
	}
	events := c.call("GET", "/events?ledger_id="+L, nil, 200)["events"].([]any)
	if len(events) < 10 || events[len(events)-1].(map[string]any)["type"] != "period.closed" {
		t.Errorf("events = %d, last %v", len(events), events[len(events)-1])
	}
}

func TestHoldExpiryOverHTTP(t *testing.T) {
	c, svc := newClient(t, httpapi.Config{})
	c.call("POST", "/currencies", map[string]any{"code": "USD", "decimals": 2}, 201)
	L := c.call("POST", "/ledgers", map[string]any{"name": "main"}, 201)["id"].(string)
	world := c.call("POST", "/accounts", map[string]any{"ledger_id": L, "currency": "USD", "allow_negative": true}, 201)["id"]
	alice := c.call("POST", "/accounts", map[string]any{"ledger_id": L, "currency": "USD"}, 201)["id"]
	c.call("POST", "/transactions", map[string]any{"reference": "f", "currency": "USD", "legs": []any{leg(world, "debit", "10"), leg(alice, "credit", "10")}}, 201)
	h := c.call("POST", "/holds", map[string]any{"reference": "h", "currency": "USD", "expires_at": time.Now().Add(200 * time.Millisecond).Format(time.RFC3339Nano),
		"legs": []any{leg(alice, "debit", "5"), leg(world, "credit", "5")}}, 201)
	if h["expires_at"] == nil {
		t.Errorf("hold = %v", h)
	}
	time.Sleep(300 * time.Millisecond)
	errBody := c.call("POST", "/holds/"+h["id"].(string)+"/commit", nil, 409)
	if code := errBody["error"].(map[string]any)["code"]; code != "hold_expired" {
		t.Errorf("code = %v, want hold_expired", code)
	}
	if n, err := svc.ExpireHolds(t.Context(), 10); err != nil || n != 1 {
		t.Errorf("ExpireHolds = %d, %v", n, err)
	}
	if got := c.call("GET", "/holds/"+h["id"].(string), nil, 200); got["status"] != "expired" {
		t.Errorf("status = %v", got["status"])
	}
}

func TestOperationalEndpoints(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	c, _ := newClient(t, httpapi.Config{Ready: ready.Load, RateLimit: 5, RateBurst: 3})

	// Health checks need no token and aren't rate limited.
	for range 10 {
		if resp, _ := c.raw("GET", "/healthz", nil, false); resp.StatusCode != 200 {
			t.Fatalf("healthz = %d", resp.StatusCode)
		}
	}
	if resp, b := c.raw("GET", "/readyz", nil, false); resp.StatusCode != 200 {
		t.Errorf("readyz = %d %s", resp.StatusCode, b)
	}
	ready.Store(false)
	if resp, _ := c.raw("GET", "/readyz", nil, false); resp.StatusCode != 503 {
		t.Errorf("readyz while shutting down = %d, want 503", resp.StatusCode)
	}

	// Rate limiting: a burst of 3, then 429 with Retry-After.
	codes := map[int]int{}
	for range 6 {
		resp, _ := c.raw("GET", "/currencies/USD", nil, true)
		codes[resp.StatusCode]++
		if resp.StatusCode == 429 && resp.Header.Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}
	if codes[429] == 0 || codes[404] == 0 {
		t.Errorf("status codes = %v, want some 404 and some 429", codes)
	}

	time.Sleep(time.Second)
	if resp, _ := c.raw("GET", "/metrics", nil, false); resp.StatusCode != 401 {
		t.Errorf("metrics without token = %d, want 401", resp.StatusCode)
	}
	resp, body := c.raw("GET", "/metrics", nil, true)
	m := string(body)
	if resp.StatusCode != 200 || !strings.Contains(m, `ledger_http_requests_total{route="GET /currencies/{code}",code="404"}`) ||
		!strings.Contains(m, "ledger_http_request_duration_seconds_bucket") || !strings.Contains(m, "ledger_db_connections") {
		t.Errorf("metrics = %d\n%s", resp.StatusCode, m)
	}
}
