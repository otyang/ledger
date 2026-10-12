package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/otyang/ledger"
	"github.com/otyang/ledger/httpapi"
	"github.com/otyang/ledger/ledgertest"
)

func TestMain(m *testing.M) {
	os.Exit(ledgertest.Run(m))
}

func TestFlow(t *testing.T) {
	svc := ledger.New(ledgertest.NewDB(t))
	srv := httptest.NewServer(httpapi.New(svc, httpapi.Config{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Token:          "secret",
		RequestTimeout: 5 * time.Second,
	}))
	t.Cleanup(srv.Close)

	call := func(method, path string, body any, wantStatus int) map[string]any {
		t.Helper()
		var rd io.Reader
		switch b := body.(type) {
		case nil:
		case string: // raw body
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s = %d %v, want %d", method, path, resp.StatusCode, out, wantStatus)
		}
		return out
	}

	// Authentication is required.
	resp, err := http.Post(srv.URL+"/ledgers", "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request = %d, want 401", resp.StatusCode)
	}

	call("POST", "/currencies", map[string]any{"code": "USD", "decimals": 2}, 201)
	ldg := call("POST", "/ledgers", map[string]any{"name": "main"}, 201)
	world := call("POST", "/accounts", map[string]any{"ledger_id": ldg["id"], "currency": "USD", "indicator": "@world", "allow_negative": true}, 201)
	alice := call("POST", "/accounts", map[string]any{"ledger_id": ldg["id"], "currency": "USD"}, 201)

	deposit := map[string]any{
		"reference": "dep-1",
		"currency":  "USD",
		"legs": []map[string]any{
			{"account_id": world["id"], "direction": "debit", "amount": 2500},
			{"account_id": alice["id"], "direction": "credit", "amount": 2500},
		},
	}
	txn := call("POST", "/transactions", deposit, 201)
	replay := call("POST", "/transactions", deposit, 201)
	if replay["id"] != txn["id"] {
		t.Errorf("replay id %v, want %v", replay["id"], txn["id"])
	}

	got := call("GET", "/accounts/"+alice["id"].(string), nil, 200)
	if got["balance"] != "2500" {
		t.Errorf("alice balance = %v, want 2500", got["balance"])
	}
	bal := call("GET", "/accounts/"+alice["id"].(string)+"/balance", nil, 200)
	if bal["balance"] != "2500" {
		t.Errorf("balance endpoint = %v, want 2500", bal["balance"])
	}
	entries := call("GET", "/accounts/"+alice["id"].(string)+"/entries", nil, 200)
	if n := len(entries["entries"].([]any)); n != 1 {
		t.Errorf("entries = %d, want 1", n)
	}
	call("GET", "/transactions/"+txn["id"].(string), nil, 200)

	// Error mapping.
	overdraw := map[string]any{
		"reference": "too-much",
		"currency":  "USD",
		"legs": []map[string]any{
			{"account_id": alice["id"], "direction": "debit", "amount": 9999},
			{"account_id": world["id"], "direction": "credit", "amount": 9999},
		},
	}
	errBody := call("POST", "/transactions", overdraw, 422)
	if code := errBody["error"].(map[string]any)["code"]; code != "insufficient_funds" {
		t.Errorf("error code = %v", code)
	}
	// Holds: reserve, partial commit with a body, void with an empty body.
	bob := call("POST", "/accounts", map[string]any{"ledger_id": ldg["id"], "currency": "USD"}, 201)
	holdBody := func(ref string, amount int) map[string]any {
		return map[string]any{
			"reference": ref,
			"currency":  "USD",
			"legs": []map[string]any{
				{"account_id": alice["id"], "direction": "debit", "amount": amount},
				{"account_id": bob["id"], "direction": "credit", "amount": amount},
			},
		}
	}
	hold := call("POST", "/holds", holdBody("auth-1", 1000), 201)
	if got := call("GET", "/accounts/"+alice["id"].(string), nil, 200); got["pending_debit"] != "1000" {
		t.Errorf("pending_debit = %v, want 1000", got["pending_debit"])
	}
	commit := call("POST", "/holds/"+hold["id"].(string)+"/commit", map[string]any{"amount": 600}, 201)
	if commit["hold_id"] != hold["id"] {
		t.Errorf("commit hold_id = %v, want %v", commit["hold_id"], hold["id"])
	}
	call("POST", "/holds/"+hold["id"].(string)+"/void", nil, 409)

	hold2 := call("POST", "/holds", holdBody("auth-2", 100), 201)
	if v := call("POST", "/holds/"+hold2["id"].(string)+"/void", nil, 200); v["status"] != "voided" {
		t.Errorf("void status = %v", v["status"])
	}
	call("POST", "/holds/"+hold2["id"].(string)+"/commit", nil, 409)

	// Refunds: partial, then over the remaining amount.
	refund := call("POST", "/transactions/"+commit["id"].(string)+"/refunds", map[string]any{"reference": "ref-1", "amount": 200}, 201)
	if refund["refund_of"] != commit["id"] {
		t.Errorf("refund_of = %v, want %v", refund["refund_of"], commit["id"])
	}
	left := call("GET", "/transactions/"+commit["id"].(string)+"/refundable", nil, 200)
	if amt := left["legs"].([]any)[0].(map[string]any)["amount"]; amt != "400" {
		t.Errorf("refundable = %v, want 400", amt)
	}
	errBody = call("POST", "/transactions/"+commit["id"].(string)+"/refunds", map[string]any{"reference": "ref-2", "amount": 401}, 422)
	if code := errBody["error"].(map[string]any)["code"]; code != "refund_exceeded" {
		t.Errorf("error code = %v", code)
	}
	if got := call("GET", "/accounts/"+alice["id"].(string), nil, 200); got["balance"] != "2100" || got["pending_debit"] != "0" {
		t.Errorf("alice = %v, want balance 2100 and no pending", got)
	}

	// Unknown accounts named in a body are 422; unknown URL resources are 404.
	call("POST", "/transactions", map[string]any{
		"reference": "ghost", "currency": "USD",
		"legs": []map[string]any{
			{"account_id": alice["id"], "direction": "debit", "amount": 1},
			{"account_id": string(ledger.NewAccountID()), "direction": "credit", "amount": 1},
		},
	}, 422)
	call("POST", "/holds/"+string(ledger.NewHoldID())+"/void", nil, 404)
	call("POST", "/ledgers", `{"name":"a"} {"name":"b"}`, 400)

	call("GET", "/transactions/txn_nope", nil, 400)
	call("GET", "/transactions/"+string(ledger.NewTransactionID()), nil, 404)
	call("POST", "/accounts", map[string]any{"ledger_id": ldg["id"], "currency": "USD", "bogus": 1}, 400)

	// Amounts beyond 2^53 keep every digit, in and out.
	const huge = "123456789012345678901234567890"
	hugeTxn := call("POST", "/transactions", map[string]any{
		"reference": "huge", "currency": "USD",
		"legs": []map[string]any{
			{"account_id": world["id"], "direction": "debit", "amount": huge},
			{"account_id": alice["id"], "direction": "credit", "amount": huge},
		},
	}, 201)
	if amt := hugeTxn["entries"].([]any)[1].(map[string]any)["amount"]; amt != huge {
		t.Errorf("entry amount = %v, want %s", amt, huge)
	}
	call("POST", "/transactions", map[string]any{
		"reference": "fraction", "currency": "USD",
		"legs": []map[string]any{
			{"account_id": world["id"], "direction": "debit", "amount": "1.5"},
			{"account_id": alice["id"], "direction": "credit", "amount": "1.5"},
		},
	}, 400)

	// Currencies: registering again is a no-op, decimals can't change, and
	// they must be given explicitly.
	call("POST", "/currencies", map[string]any{"code": "USD", "decimals": 2}, 201)
	call("POST", "/currencies", map[string]any{"code": "USD", "decimals": 3}, 409)
	call("POST", "/currencies", map[string]any{"code": "EUR"}, 400)
	if c := call("GET", "/currencies/USD", nil, 200); c["decimals"] != float64(2) {
		t.Errorf("USD = %v, want 2 decimals", c)
	}
	call("GET", "/currencies/GBP", nil, 404)
	call("POST", "/accounts", map[string]any{"ledger_id": ldg["id"], "currency": "GBP"}, 422)

	// Decimal values go in as "value"; every amount comes out with a
	// "*_value" twin.
	payer := call("POST", "/accounts", map[string]any{"ledger_id": ldg["id"], "currency": "USD"}, 201)
	payee := call("POST", "/accounts", map[string]any{"ledger_id": ldg["id"], "currency": "USD"}, 201)
	if payer["balance_value"] != "0.00" {
		t.Errorf("new account balance_value = %v, want 0.00", payer["balance_value"])
	}
	call("POST", "/transactions", map[string]any{
		"reference": "fund-payer", "currency": "USD",
		"legs": []map[string]any{
			{"account_id": world["id"], "direction": "debit", "value": "6000"},
			{"account_id": payer["id"], "direction": "credit", "value": "6000"},
		},
	}, 201)
	pay := call("POST", "/transactions", map[string]any{
		"reference": "pay-5403", "currency": "USD",
		"legs": []map[string]any{
			{"account_id": payer["id"], "direction": "debit", "value": "5403.23"},
			{"account_id": payee["id"], "direction": "credit", "amount": "540323"},
		},
	}, 201)
	if e := pay["entries"].([]any)[0].(map[string]any); e["amount"] != "540323" || e["value"] != "5403.23" || e["balance_after_value"] != "596.77" {
		t.Errorf("debit entry = %v, want 540323 / 5403.23, balance after 596.77", e)
	}
	payerID := payer["id"].(string)
	if a := call("GET", "/accounts/"+payerID, nil, 200); a["balance_value"] != "596.77" || a["debit_total_value"] != "5403.23" {
		t.Errorf("payer = %v, want balance_value 596.77, debit_total_value 5403.23", a)
	}
	if b := call("GET", "/accounts/"+payerID+"/balance", nil, 200); b["balance"] != "59677" || b["balance_value"] != "596.77" {
		t.Errorf("balance endpoint = %v, want 59677 / 596.77", b)
	}
	list := call("GET", "/accounts/"+payerID+"/entries", nil, 200)["entries"].([]any)
	if e := list[1].(map[string]any); e["value"] != "5403.23" {
		t.Errorf("listed entry = %v, want value 5403.23", e)
	}
	call("POST", "/transactions", map[string]any{
		"reference": "too-precise", "currency": "USD",
		"legs": []map[string]any{
			{"account_id": payer["id"], "direction": "debit", "value": "1.234"},
			{"account_id": payee["id"], "direction": "credit", "value": "1.234"},
		},
	}, 400)

	hold = call("POST", "/holds", map[string]any{
		"reference": "hold-value", "currency": "USD",
		"legs": []map[string]any{
			{"account_id": payer["id"], "direction": "debit", "value": "100"},
			{"account_id": payee["id"], "direction": "credit", "value": "100"},
		},
	}, 201)
	if leg := hold["legs"].([]any)[0].(map[string]any); leg["amount"] != "10000" || leg["value"] != "100.00" {
		t.Errorf("hold leg = %v, want 10000 / 100.00", leg)
	}
	commit = call("POST", "/holds/"+hold["id"].(string)+"/commit", map[string]any{"value": "40.10"}, 201)
	if e := commit["entries"].([]any)[0].(map[string]any); e["amount"] != "4010" {
		t.Errorf("commit entry = %v, want 4010", e)
	}
	// Zero is never "everything".
	call("POST", "/transactions/"+pay["id"].(string)+"/refunds", map[string]any{"reference": "zero", "value": "0.00"}, 400)
	call("POST", "/transactions/"+pay["id"].(string)+"/refunds", map[string]any{"reference": "zero-amount", "amount": "0"}, 400)
	left = call("GET", "/transactions/"+pay["id"].(string)+"/refundable", nil, 200)
	if leg := left["legs"].([]any)[0].(map[string]any); leg["value"] != "5403.23" {
		t.Errorf("refundable leg = %v, want value 5403.23", leg)
	}
}
