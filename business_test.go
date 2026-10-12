package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/otyang/ledger"
)

func amt(n int64) ledger.Amount { return ledger.NewAmount(n) }

func (f *fixture) create(t *testing.T, p ledger.CreateAccountParams) *ledger.Account {
	t.Helper()
	p.LedgerID = f.ledger.ID
	if p.Currency == "" {
		p.Currency = "USD"
	}
	a, err := f.svc.CreateAccount(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) update(t *testing.T, id ledger.AccountID, p ledger.UpdateAccountParams) *ledger.Account {
	t.Helper()
	a, err := f.svc.UpdateAccount(context.Background(), id, p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func status(s ledger.AccountStatus) *ledger.AccountStatus { return &s }

func TestAccountFreezeAndClose(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)

	a := f.update(t, alice.ID, ledger.UpdateAccountParams{Status: status(ledger.AccountFrozen), StatusReason: "fraud review"})
	if a.Status != ledger.AccountFrozen || a.StatusReason != "fraud review" {
		t.Fatalf("frozen account = %s %q", a.Status, a.StatusReason)
	}
	// Money can't leave a frozen account, in any form.
	if _, err := f.svc.Post(ctx, ledger.Transfer("out", "USD", alice.ID, bob.ID, amt(10))); !errors.Is(err, ledger.ErrAccountFrozen) {
		t.Errorf("debit frozen: err = %v, want ErrAccountFrozen", err)
	}
	if _, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "h", Currency: "USD", Legs: legs(alice.ID, bob.ID, 10)}); !errors.Is(err, ledger.ErrAccountFrozen) {
		t.Errorf("hold on frozen: err = %v, want ErrAccountFrozen", err)
	}
	// It can still receive.
	f.fund(t, alice.ID, 5)

	f.update(t, alice.ID, ledger.UpdateAccountParams{Status: status(ledger.AccountActive)})
	if _, err := f.svc.Post(ctx, ledger.Transfer("out", "USD", alice.ID, bob.ID, amt(1005))); err != nil {
		t.Fatalf("debit after unfreeze: %v", err)
	}

	// Only an empty account can be closed.
	if _, err := f.svc.UpdateAccount(ctx, bob.ID, ledger.UpdateAccountParams{Status: status(ledger.AccountClosed)}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("close non-empty: err = %v, want ErrInvalidParams", err)
	}
	f.update(t, alice.ID, ledger.UpdateAccountParams{Status: status(ledger.AccountClosed), StatusReason: "customer left"})
	if _, err := f.svc.Post(ctx, ledger.Transfer("in", "USD", bob.ID, alice.ID, amt(1))); !errors.Is(err, ledger.ErrAccountClosed) {
		t.Errorf("credit closed: err = %v, want ErrAccountClosed", err)
	}
	if _, err := f.svc.UpdateAccount(ctx, alice.ID, ledger.UpdateAccountParams{Status: status(ledger.AccountActive)}); !errors.Is(err, ledger.ErrAccountClosed) {
		t.Errorf("reopen: err = %v, want ErrAccountClosed", err)
	}

	// The database refuses the same on its own.
	db := f.svc.DB()
	if _, err := db.ExecContext(ctx, `UPDATE ledger.accounts SET status = 'active' WHERE id = ?`, alice.ID); err == nil {
		t.Error("the database must refuse reopening a closed account")
	}
	if _, err := db.ExecContext(ctx, `UPDATE ledger.accounts SET status = 'closed' WHERE id = ?`, bob.ID); err == nil {
		t.Error("the database must refuse closing a non-empty account")
	}
}

func TestOverdraftLimit(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	bob := f.account(t)
	alice := f.create(t, ledger.CreateAccountParams{OverdraftLimitValue: "5.00"})
	if alice.OverdraftLimit == nil || alice.OverdraftLimit.String() != "500" {
		t.Fatalf("overdraft limit = %v, want 500", alice.OverdraftLimit)
	}
	if _, err := f.svc.Post(ctx, ledger.Transfer("a", "USD", alice.ID, bob.ID, amt(500))); err != nil {
		t.Fatalf("spend down to -limit: %v", err)
	}
	if _, err := f.svc.Post(ctx, ledger.Transfer("b", "USD", alice.ID, bob.ID, amt(1))); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("past the limit: err = %v, want ErrInsufficientFunds", err)
	}
	if b := f.balance(t, alice.ID); b != -500 {
		t.Errorf("balance = %d, want -500", b)
	}
	// Can't lower the limit below what is owed, or remove it.
	if _, err := f.svc.UpdateAccount(ctx, alice.ID, ledger.UpdateAccountParams{OverdraftLimit: ptrAmt(100)}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("lower limit below debt: err = %v, want ErrInvalidParams", err)
	}
	if _, err := f.svc.UpdateAccount(ctx, alice.ID, ledger.UpdateAccountParams{OverdraftLimit: ptrAmt(0)}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("remove limit while owing: err = %v, want ErrInvalidParams", err)
	}
	a := f.update(t, alice.ID, ledger.UpdateAccountParams{OverdraftLimit: ptrAmt(1000)})
	if a.OverdraftLimit.String() != "1000" {
		t.Errorf("raised limit = %s", a.OverdraftLimit)
	}
	if _, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD", AllowNegative: true, OverdraftLimit: ptrAmt(5)}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("limit with allow_negative: err = %v, want ErrInvalidParams", err)
	}
	if _, err := f.svc.DB().ExecContext(ctx, `UPDATE ledger.accounts SET overdraft_limit = 1 WHERE id = ?`, alice.ID); err == nil {
		t.Error("the database must refuse a limit below what is owed")
	}
}

func ptrAmt(n int64) *ledger.Amount { a := amt(n); return &a }

func TestAccountTypeAndMetadata(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.create(t, ledger.CreateAccountParams{Metadata: map[string]any{"owner": "alice", "tier": "gold"}})
	typ := ledger.AccountLiability
	a = f.update(t, a.ID, ledger.UpdateAccountParams{Type: &typ, Metadata: map[string]any{"tier": nil, "kyc": "done"}})
	if a.Type != ledger.AccountLiability || a.Metadata["owner"] != "alice" || a.Metadata["kyc"] != "done" || a.Metadata["tier"] != nil {
		t.Errorf("account = type %s metadata %v", a.Type, a.Metadata)
	}
	other := ledger.AccountAsset
	if _, err := f.svc.UpdateAccount(ctx, a.ID, ledger.UpdateAccountParams{Type: &other}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("change type: err = %v, want ErrInvalidParams", err)
	}
	bad := ledger.AccountType("income")
	if _, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD", Type: bad}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("bad type: err = %v, want ErrInvalidParams", err)
	}
}

func TestHoldExpiry(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)

	past := time.Now().Add(-time.Second)
	if _, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "old", Currency: "USD", Legs: legs(alice.ID, bob.ID, 1), ExpiresAt: &past}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("expiry in the past: err = %v, want ErrInvalidParams", err)
	}
	soon := time.Now().Add(300 * time.Millisecond)
	h, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "soon", Currency: "USD", Legs: legs(alice.ID, bob.ID, 400), ExpiresAt: &soon})
	if err != nil {
		t.Fatal(err)
	}
	keep := f.hold(t, "forever", alice.ID, bob.ID, 100)
	if n, err := f.svc.ExpireHolds(ctx, 10); err != nil || n != 0 {
		t.Fatalf("ExpireHolds before expiry = %d, %v", n, err)
	}
	time.Sleep(400 * time.Millisecond)

	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{}); !errors.Is(err, ledger.ErrHoldExpired) || !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Errorf("commit after expiry: err = %v, want ErrHoldExpired", err)
	}
	if n, err := f.svc.ExpireHolds(ctx, 10); err != nil || n != 1 {
		t.Fatalf("ExpireHolds = %d, %v; want 1", n, err)
	}
	got, _ := f.svc.GetHold(ctx, h.ID)
	if got.Status != ledger.HoldExpired || got.ResolvedAt == nil {
		t.Errorf("hold = %s, resolved %v; want expired", got.Status, got.ResolvedAt)
	}
	if a := f.get(t, alice.ID); a.PendingDebit.String() != "100" {
		t.Errorf("pending debit = %s, want 100 (only the hold without expiry)", a.PendingDebit)
	}
	if _, err := f.svc.VoidHold(ctx, h.ID); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Errorf("void expired: err = %v, want ErrHoldNotPending", err)
	}
	if k, _ := f.svc.GetHold(ctx, keep.ID); k.Status != ledger.HoldPending {
		t.Errorf("hold without expiry = %s, want pending", k.Status)
	}
}

func TestReverse(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)
	pay, err := f.svc.Post(ctx, ledger.Transfer("pay", "USD", alice.ID, bob.ID, amt(600)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r", Amount: amt(100)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Reverse(ctx, pay.ID, ledger.ReverseParams{Reference: "rev"}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("reverse without reason: err = %v, want ErrInvalidParams", err)
	}
	rev, err := f.svc.Reverse(ctx, pay.ID, ledger.ReverseParams{Reference: "rev", Reason: "wrong recipient"})
	if err != nil {
		t.Fatal(err)
	}
	if rev.ReversalReason != "wrong recipient" || rev.RefundOf == nil || rev.Entries[0].Amount.String() != "500" {
		t.Errorf("reversal = reason %q refund_of %v amount %s", rev.ReversalReason, rev.RefundOf, rev.Entries[0].Amount)
	}
	if again, err := f.svc.Reverse(ctx, pay.ID, ledger.ReverseParams{Reference: "rev", Reason: "said differently"}); err != nil || again.ID != rev.ID {
		t.Errorf("replay = %v, %v; want the same reversal", again, err)
	}
	if _, err := f.svc.Reverse(ctx, pay.ID, ledger.ReverseParams{Reference: "rev2", Reason: "again"}); !errors.Is(err, ledger.ErrRefundExceeded) {
		t.Errorf("second reversal: err = %v, want ErrRefundExceeded", err)
	}
	if a, b := f.balance(t, alice.ID), f.balance(t, bob.ID); a != 1000 || b != 0 {
		t.Errorf("balances = %d, %d; want 1000, 0", a, b)
	}
}

func TestUpdateTransactionMetadata(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	p := ledger.Transfer("pay", "USD", alice.ID, bob.ID, amt(10))
	p.Metadata = map[string]any{"order": "1", "note": "x"}
	txn, err := f.svc.Post(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.UpdateTransactionMetadata(ctx, txn.ID, map[string]any{"note": nil, "invoice": "inv-9"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["order"] != "1" || got.Metadata["invoice"] != "inv-9" || got.Metadata["note"] != nil || len(got.Entries) != 2 {
		t.Errorf("metadata = %v, entries %d", got.Metadata, len(got.Entries))
	}
	// Still idempotent: metadata isn't part of the request.
	if again, err := f.svc.Post(ctx, p); err != nil || again.ID != txn.ID {
		t.Errorf("replay after metadata change = %v, %v", again, err)
	}
}

func TestBatch(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	company, alice, bob := f.account(t), f.account(t), f.account(t)

	// Funds are checked on the combined effect: the first member funds the
	// company, the others pay out of it.
	p := ledger.BatchParams{Reference: "payroll-oct", Description: "October payroll", Transactions: []ledger.PostParams{
		{Currency: "USD", Legs: legs(f.world.ID, company.ID, 1000)},
		{Reference: "salary-alice", Currency: "USD", Legs: legs(company.ID, alice.ID, 600)},
		{Currency: "USD", Legs: []ledger.Leg{
			{AccountID: company.ID, Direction: ledger.Debit, Value: "4.00"},
			{AccountID: bob.ID, Direction: ledger.Credit, Value: "4.00"},
		}},
	}}
	b, err := f.svc.PostBatch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != ledger.BatchPlain || len(b.Transactions) != 3 || *b.Transactions[1].BatchID != b.ID || b.Transactions[1].Reference != "salary-alice" {
		t.Fatalf("batch = %+v", b)
	}
	if c, a, bb := f.balance(t, company.ID), f.balance(t, alice.ID), f.balance(t, bob.ID); c != 0 || a != 600 || bb != 400 {
		t.Errorf("balances = %d %d %d; want 0 600 400", c, a, bb)
	}
	if again, err := f.svc.PostBatch(ctx, p); err != nil || again.ID != b.ID || len(again.Transactions) != 3 {
		t.Errorf("replay = %v, %v", again, err)
	}
	if got, err := f.svc.GetBatch(ctx, b.ID); err != nil || len(got.Transactions[0].Entries) != 2 {
		t.Errorf("GetBatch = %v, %v", got, err)
	}

	// All or nothing: one failing member posts nothing.
	bad := ledger.BatchParams{Reference: "bad", Transactions: []ledger.PostParams{
		{Currency: "USD", Legs: legs(alice.ID, bob.ID, 100)},
		{Currency: "USD", Legs: legs(alice.ID, bob.ID, 600)},
	}}
	if _, err := f.svc.PostBatch(ctx, bad); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("overdrawn batch: err = %v, want ErrInsufficientFunds", err)
	}
	if a := f.balance(t, alice.ID); a != 600 {
		t.Errorf("alice = %d after failed batch, want 600", a)
	}
	// Member references are unique like any other.
	dup := ledger.BatchParams{Reference: "dup", Transactions: []ledger.PostParams{{Reference: "salary-alice", Currency: "USD", Legs: legs(alice.ID, bob.ID, 1)}}}
	if _, err := f.svc.PostBatch(ctx, dup); !errors.Is(err, ledger.ErrReferenceConflict) {
		t.Errorf("reused member reference: err = %v, want ErrReferenceConflict", err)
	}
	p.Description = "changed"
	p.Transactions[0].Legs = legs(f.world.ID, company.ID, 999)
	if _, err := f.svc.PostBatch(ctx, p); !errors.Is(err, ledger.ErrReferenceConflict) {
		t.Errorf("different batch, same reference: err = %v, want ErrReferenceConflict", err)
	}
}

func TestExchange(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	eurWorld := f.create(t, ledger.CreateAccountParams{Currency: "EUR", Indicator: "@world", AllowNegative: true})
	custUSD, custEUR := f.account(t), f.create(t, ledger.CreateAccountParams{Currency: "EUR"})
	poolUSD := f.create(t, ledger.CreateAccountParams{Indicator: "@fx"})
	poolEUR := f.create(t, ledger.CreateAccountParams{Currency: "EUR", Indicator: "@fx"})
	f.fund(t, custUSD.ID, 10000)
	if _, err := f.svc.Post(ctx, ledger.Transfer("eur-liquidity", "EUR", eurWorld.ID, poolEUR.ID, amt(100000))); err != nil {
		t.Fatal(err)
	}

	b, err := f.svc.Exchange(ctx, ledger.ExchangeParams{
		Reference: "fx-1",
		Source:    ledger.ExchangeSide{Currency: "USD", From: custUSD.ID, To: poolUSD.ID, Value: "100.00"},
		Target:    ledger.ExchangeSide{Currency: "EUR", From: poolEUR.ID, To: custEUR.ID, Value: "92.00"},
		Rate:      "0.92",
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != ledger.BatchExchange || b.Rate != "0.92" || b.Transactions[0].Currency != "USD" || b.Transactions[1].Currency != "EUR" {
		t.Errorf("exchange = %+v", b)
	}
	if u, e := f.balance(t, custUSD.ID), f.balance(t, custEUR.ID); u != 0 || e != 9200 {
		t.Errorf("customer = %d USD, %d EUR; want 0, 9200", u, e)
	}
	same := ledger.ExchangeParams{Reference: "fx-2", Source: ledger.ExchangeSide{Currency: "USD"}, Target: ledger.ExchangeSide{Currency: "USD"}}
	if _, err := f.svc.Exchange(ctx, same); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("same currency: err = %v, want ErrInvalidParams", err)
	}
	badRate := ledger.ExchangeParams{Reference: "fx-3", Rate: "-1",
		Source: ledger.ExchangeSide{Currency: "USD"}, Target: ledger.ExchangeSide{Currency: "EUR"}}
	if _, err := f.svc.Exchange(ctx, badRate); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("bad rate: err = %v, want ErrInvalidParams", err)
	}
}

func TestListTransactions(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)
	start := time.Now()
	for i, cust := range []string{"c1", "c2", "c1"} {
		p := ledger.Transfer("pay-"+string(rune('a'+i)), "USD", alice.ID, bob.ID, amt(10))
		p.Metadata = map[string]any{"customer_id": cust}
		if _, err := f.svc.Post(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	list := func(p ledger.ListTransactionsParams) []*ledger.Transaction {
		t.Helper()
		p.LedgerID = f.ledger.ID
		txns, err := f.svc.ListTransactions(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return txns
	}
	if n := len(list(ledger.ListTransactionsParams{})); n != 4 {
		t.Errorf("all = %d, want 4", n)
	}
	if n := len(list(ledger.ListTransactionsParams{AccountID: bob.ID})); n != 3 {
		t.Errorf("bob's = %d, want 3", n)
	}
	c1 := list(ledger.ListTransactionsParams{Metadata: map[string]any{"customer_id": "c1"}})
	if len(c1) != 2 || c1[0].Reference != "pay-a" || len(c1[0].Entries) != 2 {
		t.Errorf("customer c1 = %d", len(c1))
	}
	if n := len(list(ledger.ListTransactionsParams{From: start})); n != 3 {
		t.Errorf("since start = %d, want 3", n)
	}
	page := list(ledger.ListTransactionsParams{Limit: 2})
	rest := list(ledger.ListTransactionsParams{After: page[1].ID})
	if len(page) != 2 || len(rest) != 2 || rest[0].ID == page[1].ID {
		t.Errorf("pages = %d + %d", len(page), len(rest))
	}
}

func TestTrialBalanceAndStatements(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)
	asset := ledger.AccountAsset
	cash := f.create(t, ledger.CreateAccountParams{Type: asset, AllowNegative: true, Indicator: "@cash"})
	customer := f.create(t, ledger.CreateAccountParams{Type: ledger.AccountLiability})
	revenue := f.create(t, ledger.CreateAccountParams{Type: ledger.AccountRevenue})
	expense := f.create(t, ledger.CreateAccountParams{Type: ledger.AccountExpense, AllowNegative: true})
	equity := f.create(t, ledger.CreateAccountParams{Type: ledger.AccountEquity})

	post := func(ref string, from, to ledger.AccountID, n int64) {
		t.Helper()
		if _, err := f.svc.Post(ctx, ledger.Transfer(ref, "USD", from, to, amt(n))); err != nil {
			t.Fatal(err)
		}
	}
	post("capital", cash.ID, equity.ID, 5000) // owner invests: cash up, equity up
	before := time.Now()
	post("deposit", cash.ID, customer.ID, 1000) // customer deposits
	post("fee", customer.ID, revenue.ID, 30)    // we charge a fee
	post("rent", expense.ID, cash.ID, 200)      // we pay rent
	f.fund(t, customer.ID, 70)                  // untyped @world moves money too

	tb, err := f.svc.TrialBalance(ctx, f.ledger.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.Totals) != 1 || !tb.Totals[0].Total.IsZero() || len(tb.Accounts) != 6 {
		t.Errorf("trial balance totals = %+v, accounts %d", tb.Totals, len(tb.Accounts))
	}
	old, _ := f.svc.TrialBalance(ctx, f.ledger.ID, before)
	for _, l := range old.Accounts {
		if l.AccountID == customer.ID && !l.Balance.IsZero() {
			t.Errorf("customer balance before deposit = %s, want 0", l.Balance)
		}
	}

	bs, err := f.svc.BalanceSheet(ctx, f.ledger.ID, "USD", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// assets = liabilities + equity + net income + unclassified
	rhs := bs.Liabilities.Total.Add(bs.Equity.Total).Add(bs.NetIncome).Add(bs.Unclassified.Total)
	if bs.Assets.Total.String() != "5800" || bs.Liabilities.Total.String() != "1040" || bs.NetIncome.String() != "-170" || bs.Assets.Total.Cmp(rhs) != 0 {
		t.Errorf("balance sheet: assets %s liabilities %s equity %s net income %s unclassified %s",
			bs.Assets.Total, bs.Liabilities.Total, bs.Equity.Total, bs.NetIncome, bs.Unclassified.Total)
	}
	is, err := f.svc.IncomeStatement(ctx, f.ledger.ID, "USD", before, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if is.Revenue.Total.String() != "30" || is.Expenses.Total.String() != "200" || is.NetIncome.String() != "-170" {
		t.Errorf("income statement: revenue %s expenses %s net %s", is.Revenue.Total, is.Expenses.Total, is.NetIncome)
	}

	st, err := f.svc.Statement(ctx, customer.ID, before, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !st.OpeningBalance.IsZero() || st.ClosingBalance.String() != "1040" || len(st.Lines) != 3 || st.Lines[1].Balance.String() != "970" || st.Lines[1].Reference != "fee" {
		t.Errorf("statement: opening %s closing %s lines %+v", st.OpeningBalance, st.ClosingBalance, st.Lines)
	}
	ws, err := f.svc.Statement(ctx, f.world.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Second))
	if err != nil || ws.ClosingBalance.String() != "-70" {
		t.Errorf("untracked statement closing = %v, %v; want -70", ws, err)
	}
}

func TestClosePeriod(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)

	if _, err := f.svc.ClosePeriod(ctx, f.ledger.ID, time.Now()); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("close through now: err = %v, want ErrInvalidParams", err)
	}
	through := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Microsecond)
	pc, err := f.svc.ClosePeriod(ctx, f.ledger.ID, through)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.ClosedThrough.Equal(through) || pc.TrialBalance == nil || len(pc.TrialBalance.Accounts) != 2 {
		t.Errorf("close = %+v", pc)
	}
	if again, err := f.svc.ClosePeriod(ctx, f.ledger.ID, through); err != nil || !again.ClosedAt.Equal(pc.ClosedAt) {
		t.Errorf("close again = %v, %v; want the same close", again, err)
	}
	if _, err := f.svc.ClosePeriod(ctx, f.ledger.ID, through.Add(-time.Minute)); !errors.Is(err, ledger.ErrPeriodClosed) {
		t.Errorf("close earlier: err = %v, want ErrPeriodClosed", err)
	}
	if l, _ := f.svc.GetLedger(ctx, f.ledger.ID); l.ClosedThrough == nil || !l.ClosedThrough.Equal(through) {
		t.Errorf("ledger closed_through = %v", l.ClosedThrough)
	}
	// Posting now is still fine.
	f.fund(t, alice.ID, 10)

	// The database refuses entries dated inside the closed period.
	db := f.svc.DB()
	err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		id := ledger.NewTransactionID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger.transactions (id, ledger_id, reference, status, currency, request_hash, created_at)
			VALUES (?, ?, 'backdated', 'posted', 'USD', '', ?)`, id, f.ledger.ID, through); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ledger.entries (id, transaction_id, account_id, direction, amount, created_at)
			VALUES (?, ?, ?, 'credit', 5, ?)`, ledger.NewEntryID(), id, f.world.ID, through)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "is closed through") {
		t.Errorf("backdated entry: err = %v, want refusal", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE ledger.ledgers SET closed_through = NULL WHERE id = ?`, f.ledger.ID); err == nil {
		t.Error("the database must refuse reopening a period")
	}
	closes, err := f.svc.ListPeriodCloses(ctx, f.ledger.ID)
	if err != nil || len(closes) != 1 {
		t.Errorf("ListPeriodCloses = %d, %v", len(closes), err)
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	h := f.hold(t, "h", alice.ID, bob.ID, 10)
	if _, err := f.svc.VoidHold(ctx, h.ID); err != nil {
		t.Fatal(err)
	}

	events, err := f.svc.ListEvents(ctx, ledger.ListEventsParams{LedgerID: f.ledger.ID})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	want := "ledger.created account.created account.created account.created transaction.posted hold.created hold.voided"
	if got := strings.Join(types, " "); got != want {
		t.Errorf("events =\n%s\nwant\n%s", got, want)
	}
	var posted ledger.Transaction
	if err := json.Unmarshal(events[4].Data, &posted); err != nil || !strings.HasPrefix(posted.Reference, "fund-") || len(posted.Entries) != 2 {
		t.Errorf("transaction event data = %s (%v)", events[4].Data, err)
	}
	all, _ := f.svc.ListEvents(ctx, ledger.ListEventsParams{})
	registered := 0
	for _, ev := range all {
		if ev.Type == ledger.EventCurrencyRegistered && ev.LedgerID == nil {
			registered++
		}
	}
	if registered != 3 {
		t.Errorf("currency.registered events = %d, want 3 (USD, EUR, ETH)", registered)
	}
	rest, err := f.svc.ListEvents(ctx, ledger.ListEventsParams{After: events[4].ID, LedgerID: f.ledger.ID})
	if err != nil || len(rest) != 2 || rest[0].Type != ledger.EventHoldCreated {
		t.Errorf("after cursor = %d, %v", len(rest), err)
	}

	if c, err := f.svc.EventCursor(ctx, "test"); err != nil || c != "" {
		t.Errorf("new cursor = %q, %v", c, err)
	}
	if err := f.svc.SetEventCursor(ctx, "test", events[2].ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.svc.EventCursor(ctx, "test"); c != events[2].ID {
		t.Errorf("cursor = %q, want %q", c, events[2].ID)
	}
	if _, err := f.svc.DB().ExecContext(ctx, `UPDATE ledger.events SET type = 'x'`); err == nil {
		t.Error("events must be immutable")
	}
}

func TestSnapshots(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)
	accounts := []*ledger.Account{f.account(t), f.account(t)}
	world := func() *ledger.Account { return f.get(t, f.world.ID) }

	for i := range 5 {
		f.fund(t, accounts[i%2].ID, int64(10+i))
	}
	before := world()
	if n, err := f.svc.RefreshSnapshots(ctx); err != nil || n != 1 {
		t.Fatalf("RefreshSnapshots = %d, %v", n, err)
	}
	after := world()
	if before.Balance.Cmp(after.Balance) != 0 || before.Version != after.Version || after.Version != 5 {
		t.Errorf("snapshot changed the balance: %s v%d → %s v%d", before.Balance, before.Version, after.Balance, after.Version)
	}

	// Postings during refreshes are never lost or counted twice.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.fund(t, accounts[i%2].ID, 1)
		}()
		if i%5 == 0 {
			if _, err := f.svc.RefreshSnapshots(ctx); err != nil {
				t.Error(err)
			}
		}
	}
	wg.Wait()
	if _, err := f.svc.RefreshSnapshots(ctx); err != nil {
		t.Fatal(err)
	}
	w := world()
	if w.Balance.String() != "-80" || w.Version != 25 || w.DebitTotal.String() != "80" {
		t.Errorf("world after concurrent postings = balance %s version %d debits %s; want -80, 25, 80", w.Balance, w.Version, w.DebitTotal)
	}
}

func TestMigrateDownRefusesBusinessFeatures(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a := f.account(t)
	f.update(t, a.ID, ledger.UpdateAccountParams{Status: status(ledger.AccountFrozen)})
	if err := f.svc.MigrateDown(ctx); err == nil || !strings.Contains(err.Error(), "cannot downgrade") {
		t.Errorf("err = %v, want refusal", err)
	}
	if err := f.svc.CheckSchema(ctx); err != nil {
		t.Errorf("CheckSchema = %v", err)
	}
}
