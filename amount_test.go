package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/otyang/ledger"
)

func TestParseAmount(t *testing.T) {
	nines := strings.Repeat("9", 78)
	for in, want := range map[string]string{
		"0":                               "0",
		"5000":                            "5000",
		"-12":                             "-12",
		"007":                             "7",
		nines:                             nines,
		strings.Repeat("0", 100) + "5":    "5",
		"1000000000000000000000000000000": "1000000000000000000000000000000",
	} {
		got, err := ledger.ParseAmount(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseAmount(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "1.5", "1e3", " 1", "+1", "0x10", "1_000", "--1"} {
		if _, err := ledger.ParseAmount(in); !errors.Is(err, ledger.ErrInvalidParams) {
			t.Errorf("ParseAmount(%q): err = %v, want ErrInvalidParams", in, err)
		}
	}
	if _, err := ledger.ParseAmount("1" + nines); !errors.Is(err, ledger.ErrAmountOverflow) {
		t.Errorf("79 digits: err = %v, want ErrAmountOverflow", err)
	}
}

func TestAmountJSON(t *testing.T) {
	b, err := json.Marshal(ledger.Leg{Amount: ledger.NewAmount(5000)})
	if err != nil || !strings.Contains(string(b), `"amount":"5000"`) {
		t.Errorf("marshal = %s, %v; want the amount as a string", b, err)
	}
	if b, _ := json.Marshal(ledger.CommitHoldParams{}); string(b) != "{}" {
		t.Errorf("zero amount should be omitted, got %s", b)
	}

	for _, in := range []string{`{"amount":"123456789012345678901234567890"}`, `{"amount":123456789012345678901234567890}`} {
		var leg ledger.Leg
		if err := json.Unmarshal([]byte(in), &leg); err != nil || leg.Amount.String() != "123456789012345678901234567890" {
			t.Errorf("unmarshal %s = %s, %v; want every digit kept", in, leg.Amount, err)
		}
	}
	for _, in := range []string{`{"amount":1.5}`, `{"amount":"1e3"}`, `{"amount":1e3}`, `{"amount":true}`} {
		var leg ledger.Leg
		if err := json.Unmarshal([]byte(in), &leg); err == nil {
			t.Errorf("unmarshal %s = %s, want an error", in, leg.Amount)
		}
	}
}

// TestCryptoAmounts posts ETH in wei (18 decimals), far beyond what an int64
// can hold, through transfers, holds and refunds.
func TestCryptoAmounts(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	world, err := f.svc.EnsureIndicatorAccount(ctx, ledger.CreateAccountParams{
		LedgerID: f.ledger.ID, Currency: "ETH", Indicator: "@world", AllowNegative: true, Untracked: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	account := func() *ledger.Account {
		a, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "ETH"})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	alice, bob := account(), account()
	eth := func(n string) ledger.Amount {
		a, err := ledger.ParseAmount(n + strings.Repeat("0", 18))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	post := func(p ledger.PostParams) *ledger.Transaction {
		t.Helper()
		txn, err := f.svc.Post(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return txn
	}
	check := func(id ledger.AccountID, balance, pending string) {
		t.Helper()
		a, err := f.svc.GetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.Balance.String() != balance || a.PendingDebit.String() != pending {
			t.Errorf("account %s balance %s pending %s; want %s, %s", id, a.Balance, a.PendingDebit, balance, pending)
		}
	}

	dep := post(ledger.Transfer("dep", "ETH", world.ID, alice.ID, eth("1000")))
	if got := dep.Entries[1].BalanceAfter.String(); got != "1000000000000000000000" {
		t.Errorf("balance_after = %s, want 1000 ETH", got)
	}

	// 250 ETH and 1 wei.
	pay := post(ledger.Transfer("pay", "ETH", alice.ID, bob.ID, eth("250").Add(ledger.NewAmount(1))))
	check(alice.ID, "749999999999999999999", "0")
	check(bob.ID, "250000000000000000001", "0")

	// Replays match on the exact amount.
	if again := post(ledger.Transfer("pay", "ETH", alice.ID, bob.ID, eth("250").Add(ledger.NewAmount(1)))); again.ID != pay.ID {
		t.Errorf("replay = %s, want %s", again.ID, pay.ID)
	}
	if _, err := f.svc.Post(ctx, ledger.Transfer("pay", "ETH", alice.ID, bob.ID, eth("250").Add(ledger.NewAmount(2)))); !errors.Is(err, ledger.ErrReferenceConflict) {
		t.Errorf("reuse with 1 wei more: err = %v, want ErrReferenceConflict", err)
	}

	h, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "hold", Currency: "ETH",
		Legs: ledger.Transfer("", "", alice.ID, bob.ID, eth("100")).Legs})
	if err != nil {
		t.Fatal(err)
	}
	check(alice.ID, "749999999999999999999", "100000000000000000000")
	if h, err = f.svc.GetHold(ctx, h.ID); err != nil || h.Legs[0].Amount.String() != "100000000000000000000" {
		t.Errorf("hold legs read back = %+v, %v; want 100 ETH", h, err)
	}
	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: eth("40")}); err != nil {
		t.Fatal(err)
	}
	check(alice.ID, "709999999999999999999", "0")
	check(bob.ID, "290000000000000000001", "0")

	// Refund the odd wei.
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "wei", Amount: ledger.NewAmount(1)}); err != nil {
		t.Fatal(err)
	}
	check(alice.ID, "710000000000000000000", "0")
	check(bob.ID, "290000000000000000000", "0")
	left, err := f.svc.Refundable(ctx, pay.ID)
	if err != nil || len(left) != 2 || left[0].Amount.Cmp(eth("250")) != 0 {
		t.Errorf("refundable = %+v, %v; want 250 ETH", left, err)
	}

	check(world.ID, "-1000000000000000000000", "0")
	if b, err := f.svc.BalanceAt(ctx, alice.ID, dep.CreatedAt); err != nil || b.String() != "1000000000000000000000" {
		t.Errorf("BalanceAt(deposit) = %s, %v; want 1000 ETH", b, err)
	}
	entries, err := f.svc.ListEntries(ctx, alice.ID, ledger.ListEntriesParams{})
	if err != nil || len(entries) != 4 || entries[3].BalanceAfter.String() != "710000000000000000000" {
		t.Errorf("alice entries = %d, %v; want 4 ending at 710 ETH", len(entries), err)
	}

	// An account's totals hold up to 78 digits.
	carol := account()
	nines, _ := ledger.ParseAmount(strings.Repeat("9", 78))
	post(ledger.Transfer("nines", "ETH", world.ID, carol.ID, nines))
	check(carol.ID, nines.String(), "0")
	if _, err := f.svc.Post(ctx, ledger.Transfer("nines+1", "ETH", world.ID, carol.ID, ledger.NewAmount(1))); !errors.Is(err, ledger.ErrAmountOverflow) {
		t.Errorf("past the maximum: err = %v, want ErrAmountOverflow", err)
	}
}
