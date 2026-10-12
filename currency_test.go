package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/otyang/ledger"
)

func currency(code string, decimals int) *ledger.Currency {
	return &ledger.Currency{Code: code, Decimals: &decimals}
}

func TestCurrencyParseFormat(t *testing.T) {
	usd, jpy, eth := currency("USD", 2), currency("JPY", 0), currency("ETH", 18)
	for _, tc := range []struct {
		c      *ledger.Currency
		in     string
		amount string
		out    string // Format of the parsed amount
	}{
		{usd, "5403.23", "540323", "5403.23"},
		{usd, "5403.2", "540320", "5403.20"},
		{usd, "5403", "540300", "5403.00"},
		{usd, "5403.230", "540323", "5403.23"}, // extra zeros are exact
		{usd, "0.05", "5", "0.05"},
		{usd, "0", "0", "0.00"},
		{usd, "-12.5", "-1250", "-12.50"},
		{usd, "0007.10", "710", "7.10"},
		{jpy, "5403", "5403", "5403"},
		{jpy, "5403.000", "5403", "5403"},
		{eth, "1.5", "1500000000000000000", "1.500000000000000000"},
		{eth, "0.000000000000000001", "1", "0.000000000000000001"},
	} {
		got, err := tc.c.Parse(tc.in)
		if err != nil || got.String() != tc.amount {
			t.Errorf("%s Parse(%q) = %s, %v; want %s", tc.c.Code, tc.in, got, err, tc.amount)
			continue
		}
		if s, err := tc.c.Format(got); err != nil || s != tc.out {
			t.Errorf("%s Format(%s) = %q, %v; want %q", tc.c.Code, got, s, err, tc.out)
		}
	}

	for _, tc := range []struct {
		c  *ledger.Currency
		in string
	}{
		{usd, "5403.234"}, // never rounded
		{usd, "5403.001"},
		{jpy, "5403.5"},
		{usd, ""}, {usd, "."}, {usd, ".5"}, {usd, "5."}, {usd, "5,403.23"}, {usd, "$5403.23"},
		{usd, "1e3"}, {usd, "+5"}, {usd, " 5"}, {usd, "5.2.3"}, {usd, "--5"},
	} {
		if got, err := tc.c.Parse(tc.in); !errors.Is(err, ledger.ErrInvalidParams) {
			t.Errorf("%s Parse(%q) = %s, %v; want ErrInvalidParams", tc.c.Code, tc.in, got, err)
		}
	}
	if _, err := eth.Parse(strings.Repeat("9", 61)); !errors.Is(err, ledger.ErrAmountOverflow) {
		t.Errorf("79-digit amount: err = %v, want ErrAmountOverflow", err)
	}

	unset := &ledger.Currency{Code: "OLD"}
	if _, err := unset.Parse("1"); !errors.Is(err, ledger.ErrUnknownCurrency) {
		t.Errorf("Parse without decimals: err = %v, want ErrUnknownCurrency", err)
	}
	if _, err := unset.Format(ledger.NewAmount(1)); !errors.Is(err, ledger.ErrUnknownCurrency) {
		t.Errorf("Format without decimals: err = %v, want ErrUnknownCurrency", err)
	}

	// A hand-built Currency with impossible decimals is an error, not a panic.
	for _, d := range []int{-1, 37} {
		bad := currency("BAD", d)
		if _, err := bad.Parse("1"); !errors.Is(err, ledger.ErrInvalidParams) {
			t.Errorf("Parse with %d decimals: err = %v, want ErrInvalidParams", d, err)
		}
		if _, err := bad.Format(ledger.NewAmount(1)); !errors.Is(err, ledger.ErrInvalidParams) {
			t.Errorf("Format with %d decimals: err = %v, want ErrInvalidParams", d, err)
		}
	}
}

// A zero amount or value must never mean "commit or refund everything".
func TestZeroIsNotEverything(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 10000)
	pay, err := f.svc.Post(ctx, ledger.Transfer("pay", "USD", alice.ID, bob.ID, ledger.NewAmount(5000)))
	if err != nil {
		t.Fatal(err)
	}
	h := f.hold(t, "h", alice.ID, bob.ID, 3000)

	for _, p := range []ledger.RefundParams{
		{Reference: "v0", Value: "0"}, {Reference: "v00", Value: "0.00"}, {Reference: "vneg0", Value: "-0.00"},
		{Reference: "a0", Amount: ledger.NewAmount(0)}, {Reference: "vneg", Value: "-1.00"},
	} {
		if _, err := f.svc.Refund(ctx, pay.ID, p); !errors.Is(err, ledger.ErrInvalidParams) {
			t.Errorf("Refund(%+v): err = %v, want ErrInvalidParams", p, err)
		}
	}
	for _, p := range []ledger.CommitHoldParams{{Value: "0"}, {Value: "-0"}, {Amount: ledger.NewAmount(0)}} {
		if _, err := f.svc.CommitHold(ctx, h.ID, p); !errors.Is(err, ledger.ErrInvalidParams) {
			t.Errorf("CommitHold(%+v): err = %v, want ErrInvalidParams", p, err)
		}
	}
	if a, b := f.balance(t, alice.ID), f.balance(t, bob.ID); a != 5000 || b != 5000 {
		t.Errorf("balances = %d, %d after rejected zero requests; want 5000, 5000", a, b)
	}

	// Leaving everything out still means everything.
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{}); err != nil {
		t.Fatal(err)
	}
	if a, b := f.balance(t, alice.ID), f.balance(t, bob.ID); a != 7000 || b != 3000 {
		t.Errorf("balances = %d, %d after full refund and commit; want 7000, 3000", a, b)
	}
}

// Currencies handed out are copies: changing one can't change how the
// service reads values.
func TestCurrencyCacheIsNotShared(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)

	reg, err := f.svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "USD", Decimals: 2})
	if err != nil {
		t.Fatal(err)
	}
	*reg.Decimals = 0
	got, err := f.svc.GetCurrency(ctx, "USD")
	if err != nil {
		t.Fatal(err)
	}
	*got.Decimals = 0
	if again, _ := f.svc.GetCurrency(ctx, "USD"); *again.Decimals != 2 {
		t.Fatalf("cached USD decimals = %d after callers changed their copies, want 2", *again.Decimals)
	}

	txn, err := f.svc.Post(ctx, ledger.PostParams{Reference: "p", Currency: "USD", Legs: []ledger.Leg{
		{AccountID: alice.ID, Direction: ledger.Debit, Value: "5"},
		{AccountID: bob.ID, Direction: ledger.Credit, Value: "5"},
	}})
	if err != nil || txn.Entries[0].Amount.String() != "500" {
		t.Errorf("5 USD = %v, %v; want amount 500", txn, err)
	}
}

// A timeout while looking up a currency is reported as a timeout.
func TestCurrencyLookupTimeout(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)

	// Block reads of the currencies table from another session.
	locker, err := f.svc.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback()
	if _, err := locker.ExecContext(ctx, "LOCK TABLE ledger.currencies IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	svc := ledger.New(f.svc.DB()) // nothing cached
	tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err = svc.Post(tctx, ledger.PostParams{Reference: "slow", Currency: "USD", Legs: []ledger.Leg{
		{AccountID: alice.ID, Direction: ledger.Debit, Value: "1"},
		{AccountID: bob.ID, Direction: ledger.Credit, Value: "1"},
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRegisterCurrency(t *testing.T) {
	ctx := context.Background()
	f := setup(t)

	c, err := f.svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "JPY", Decimals: 0})
	if err != nil || c.Code != "JPY" || c.Decimals == nil || *c.Decimals != 0 {
		t.Fatalf("register JPY = %+v, %v", c, err)
	}
	again, err := f.svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "JPY", Decimals: 0})
	if err != nil || !again.CreatedAt.Equal(c.CreatedAt) {
		t.Errorf("re-register = %+v, %v; want the original", again, err)
	}
	if _, err := f.svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "JPY", Decimals: 2}); !errors.Is(err, ledger.ErrCurrencyConflict) {
		t.Errorf("different decimals: err = %v, want ErrCurrencyConflict", err)
	}
	for _, p := range []ledger.CurrencyParams{{Code: "usd", Decimals: 2}, {Code: "X", Decimals: 2}, {Code: "ABC", Decimals: -1}, {Code: "ABC", Decimals: 37}} {
		if _, err := f.svc.RegisterCurrency(ctx, p); !errors.Is(err, ledger.ErrInvalidParams) {
			t.Errorf("RegisterCurrency(%+v): err = %v, want ErrInvalidParams", p, err)
		}
	}

	if _, err := f.svc.GetCurrency(ctx, "GBP"); !errors.Is(err, ledger.ErrUnknownCurrency) {
		t.Errorf("GetCurrency(GBP): err = %v, want ErrUnknownCurrency", err)
	}
	if _, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "GBP"}); !errors.Is(err, ledger.ErrUnknownCurrency) {
		t.Errorf("account in GBP: err = %v, want ErrUnknownCurrency", err)
	}

	// The database refuses changes the Go code would never make.
	db := f.svc.DB()
	if _, err := db.ExecContext(ctx, `UPDATE ledger.currencies SET decimals = 3 WHERE code = 'USD'`); err == nil {
		t.Error("changing decimals must fail")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM ledger.currencies WHERE code = 'JPY'`); err == nil {
		t.Error("deleting a currency must fail")
	}
}

// TestPostWithValues debits 5403.23 USD written as a decimal, through
// transfers, holds and refunds.
func TestPostWithValues(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	usd := func(value string) []ledger.Leg {
		return []ledger.Leg{
			{AccountID: alice.ID, Direction: ledger.Debit, Value: value},
			{AccountID: bob.ID, Direction: ledger.Credit, Value: value},
		}
	}
	if _, err := f.svc.Post(ctx, ledger.PostParams{Reference: "dep", Currency: "USD", Legs: []ledger.Leg{
		{AccountID: f.world.ID, Direction: ledger.Debit, Value: "6000"},
		{AccountID: alice.ID, Direction: ledger.Credit, Value: "6000.00"},
	}}); err != nil {
		t.Fatal(err)
	}

	legs := usd("5403.23")
	pay, err := f.svc.Post(ctx, ledger.PostParams{Reference: "pay", Currency: "USD", Legs: legs})
	if err != nil {
		t.Fatal(err)
	}
	if e := pay.Entries[0]; e.Amount.String() != "540323" || e.BalanceAfter.String() != "59677" {
		t.Errorf("entry = %s, balance after %s; want 540323, 59677", e.Amount, e.BalanceAfter)
	}
	if legs[0].Value != "5403.23" || !legs[0].Amount.IsZero() {
		t.Errorf("Post modified the caller's legs: %+v", legs[0])
	}
	if f.balance(t, alice.ID) != 59677 || f.balance(t, bob.ID) != 540323 {
		t.Errorf("balances = %d, %d; want 59677, 540323", f.balance(t, alice.ID), f.balance(t, bob.ID))
	}

	// Value and amount are the same request.
	again, err := f.svc.Post(ctx, ledger.Transfer("pay", "USD", alice.ID, bob.ID, ledger.NewAmount(540323)))
	if err != nil || again.ID != pay.ID {
		t.Errorf("replay with amount = %v, %v; want %s", again, err, pay.ID)
	}

	for _, tc := range []struct {
		name string
		legs []ledger.Leg
		want error
	}{
		{"too many decimals", usd("1.234"), ledger.ErrInvalidParams},
		{"not a number", usd("1,00"), ledger.ErrInvalidParams},
		{"negative", usd("-1"), ledger.ErrInvalidParams},
		{"zero", usd("0.00"), ledger.ErrInvalidParams},
		{"amount and value disagree", []ledger.Leg{
			{AccountID: alice.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(100), Value: "1.01"},
			{AccountID: bob.ID, Direction: ledger.Credit, Value: "1.01"},
		}, ledger.ErrInvalidParams},
	} {
		if _, err := f.svc.Post(ctx, ledger.PostParams{Reference: tc.name, Currency: "USD", Legs: tc.legs}); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	both := []ledger.Leg{
		{AccountID: alice.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(101), Value: "1.01"},
		{AccountID: bob.ID, Direction: ledger.Credit, Value: "1.01"},
	}
	if _, err := f.svc.Post(ctx, ledger.PostParams{Reference: "both", Currency: "USD", Legs: both}); err != nil {
		t.Errorf("amount and value that agree: %v", err)
	}
	if _, err := f.svc.Post(ctx, ledger.PostParams{Reference: "gbp", Currency: "GBP", Legs: usd("1")}); !errors.Is(err, ledger.ErrUnknownCurrency) {
		t.Errorf("unregistered currency: err = %v, want ErrUnknownCurrency", err)
	}

	// Holds store amounts only, and commit by value.
	h, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "h", Currency: "USD", Legs: usd("200")})
	if err != nil {
		t.Fatal(err)
	}
	if h, err = f.svc.GetHold(ctx, h.ID); err != nil || h.Legs[0].Amount.String() != "20000" || h.Legs[0].Value != "" {
		t.Errorf("stored hold legs = %+v, %v; want amount 20000 and no value", h.Legs, err)
	}
	commit, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Value: "150.50"})
	if err != nil || commit.Entries[0].Amount.String() != "15050" {
		t.Fatalf("commit 150.50 = %+v, %v", commit, err)
	}
	if again, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(15050)}); err != nil || again.ID != commit.ID {
		t.Errorf("commit replay with amount = %v, %v; want %s", again, err, commit.ID)
	}

	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r", Value: "0.23"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r2", Value: "0.001"}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("refund of 0.001 USD: err = %v, want ErrInvalidParams", err)
	}
	left, err := f.svc.Refundable(ctx, pay.ID)
	if err != nil || left[0].Amount.String() != "540300" {
		t.Errorf("refundable = %+v, %v; want 540300", left, err)
	}
	if _, err := f.svc.CommitHold(ctx, ledger.NewHoldID(), ledger.CommitHoldParams{Value: "1"}); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("commit unknown hold by value: err = %v, want ErrNotFound", err)
	}
}

// Currencies in use before migration 5 are registered without decimals.
// Amounts keep working; values need the decimals registered first.
func TestCurrencyInUseBeforeRegistration(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, false, false)
	alice := f.account(t)
	// Re-run migration 5 over existing accounts (6 depends on it).
	for range 2 {
		if err := f.svc.MigrateDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := f.svc

	c, err := svc.GetCurrency(ctx, "USD")
	if err != nil || c.Decimals != nil {
		t.Fatalf("USD = %+v, %v; want no decimals", c, err)
	}
	if _, err := svc.GetCurrency(ctx, "ETH"); !errors.Is(err, ledger.ErrUnknownCurrency) {
		t.Errorf("ETH had no accounts, so it isn't registered: err = %v", err)
	}
	value := ledger.PostParams{Reference: "v", Currency: "USD", Legs: []ledger.Leg{
		{AccountID: f.world.ID, Direction: ledger.Debit, Value: "1.50"},
		{AccountID: alice.ID, Direction: ledger.Credit, Value: "1.50"},
	}}
	if _, err := svc.Post(ctx, value); !errors.Is(err, ledger.ErrUnknownCurrency) {
		t.Errorf("value without decimals: err = %v, want ErrUnknownCurrency", err)
	}
	if _, err := svc.Post(ctx, ledger.Transfer("a", "USD", f.world.ID, alice.ID, ledger.NewAmount(150))); err != nil {
		t.Errorf("amount without decimals: %v", err)
	}

	if _, err := svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "USD", Decimals: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Post(ctx, value); err != nil {
		t.Errorf("value after registering decimals: %v", err)
	}
	if _, err := svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "USD", Decimals: 3}); !errors.Is(err, ledger.ErrCurrencyConflict) {
		t.Errorf("changing decimals: err = %v, want ErrCurrencyConflict", err)
	}
	if b := f.balance(t, alice.ID); b != 300 {
		t.Errorf("alice balance = %d, want 300", b)
	}
}
