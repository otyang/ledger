package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/otyang/ledger"
	"github.com/otyang/ledger/ledgertest"
)

func TestMain(m *testing.M) {
	os.Exit(ledgertest.Run(m))
}

type fixture struct {
	svc    *ledger.Service
	ledger *ledger.Ledger
	world  *ledger.Account
}

// setup creates a ledger with a tracked "@world" account.
func setup(t *testing.T) *fixture {
	return setupWorld(t, false)
}

func setupWorld(t *testing.T, untracked bool) *fixture {
	t.Helper()
	return newFixture(t, untracked, true)
}

// newFixture creates a ledger with a "@world" account. Without decimals, the
// currencies are registered the way migration 5 registers currencies already
// in use: with unknown decimals. Amounts still work, and MigrateDown can still
// revert migration 5.
func newFixture(t *testing.T, untracked, decimals bool) *fixture {
	t.Helper()
	svc := ledger.New(ledgertest.NewDB(t))
	l, err := svc.CreateLedger(context.Background(), ledger.CreateLedgerParams{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for code, d := range map[string]int{"USD": 2, "EUR": 2, "ETH": 18} {
		if decimals {
			_, err = svc.RegisterCurrency(context.Background(), ledger.CurrencyParams{Code: code, Decimals: d})
		} else {
			_, err = svc.DB().ExecContext(context.Background(), `INSERT INTO ledger.currencies (code, created_at) VALUES (?, now())`, code)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	world, err := svc.EnsureIndicatorAccount(context.Background(), ledger.CreateAccountParams{
		LedgerID: l.ID, Currency: "USD", Indicator: "@world", AllowNegative: true, Untracked: untracked,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{svc: svc, ledger: l, world: world}
}

func (f *fixture) account(t *testing.T) *ledger.Account {
	t.Helper()
	a, err := f.svc.CreateAccount(context.Background(), ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) fund(t *testing.T, to ledger.AccountID, amount int64) {
	t.Helper()
	ref := "fund-" + string(to) + fmt.Sprint(time.Now().UnixNano())
	if _, err := f.svc.Post(context.Background(), ledger.Transfer(ref, "USD", f.world.ID, to, ledger.NewAmount(amount))); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) balance(t *testing.T, id ledger.AccountID) int64 {
	t.Helper()
	a, err := f.svc.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return i64(t, a.Balance)
}

// i64 returns a as an int64, failing the test if it doesn't fit.
func i64(t *testing.T, a ledger.Amount) int64 {
	t.Helper()
	n, ok := a.Int64()
	if !ok {
		t.Fatalf("amount %s does not fit in an int64", a)
	}
	return n
}

func TestIDs(t *testing.T) {
	id := ledger.NewTransactionID()
	if !strings.HasPrefix(string(id), "txn_") {
		t.Fatalf("id %q lacks txn_ prefix", id)
	}
	if _, err := ledger.ParseTransactionID(string(id)); err != nil {
		t.Fatalf("parse own id: %v", err)
	}
	for _, bad := range []string{
		string(ledger.NewAccountID()),                  // wrong prefix
		"txn_not-a-uuid",                               // not a uuid
		"txn_6ba7b810-9dad-11d1-80b4-00c04fd430c8",     // uuid v1
		"txn_01926f3a-7c2e-7b4a-cf1d-3e8c5a2b1d40",     // wrong variant
		strings.ToUpper(string(id)),                    // non-canonical
		"txn" + strings.TrimPrefix(string(id), "txn_"), // missing separator
	} {
		if _, err := ledger.ParseTransactionID(bad); !errors.Is(err, ledger.ErrInvalidID) {
			t.Errorf("ParseTransactionID(%q) = %v, want ErrInvalidID", bad, err)
		}
	}
}

func TestPostTransfer(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)

	f.fund(t, alice.ID, 1000)
	txn, err := f.svc.Post(ctx, ledger.Transfer("pay-1", "USD", alice.ID, bob.ID, ledger.NewAmount(300)))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(string(txn.ID), "txn_") || len(txn.Entries) != 2 {
		t.Fatalf("unexpected transaction %+v", txn)
	}
	for _, e := range txn.Entries {
		if !strings.HasPrefix(string(e.ID), "ent_") {
			t.Errorf("entry id %q lacks ent_ prefix", e.ID)
		}
	}
	if got := f.balance(t, alice.ID); got != 700 {
		t.Errorf("alice balance = %d, want 700", got)
	}
	if got := f.balance(t, bob.ID); got != 300 {
		t.Errorf("bob balance = %d, want 300", got)
	}
	if got := f.balance(t, f.world.ID); got != -1000 {
		t.Errorf("world balance = %d, want -1000", got)
	}

	entries, err := f.svc.ListEntries(ctx, alice.ID, ledger.ListEntriesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 ||
		*entries[0].AccountVersion != 1 || entries[0].BalanceAfter.String() != "1000" ||
		*entries[1].AccountVersion != 2 || entries[1].BalanceAfter.String() != "700" {
		t.Errorf("alice history wrong: %+v %+v", entries[0], entries[1])
	}
	page, err := f.svc.ListEntries(ctx, alice.ID, ledger.ListEntriesParams{After: entries[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != entries[1].ID {
		t.Errorf("page after first entry = %+v, want only the second entry", page)
	}

	got, err := f.svc.GetTransaction(ctx, txn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference != "pay-1" || len(got.Entries) != 2 || got.Entries[0].Direction != ledger.Debit {
		t.Errorf("GetTransaction = %+v", got)
	}
}

func TestPostMultiLeg(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	buyer, merchant, fees := f.account(t), f.account(t), f.account(t)
	f.fund(t, buyer.ID, 1000)

	_, err := f.svc.Post(ctx, ledger.PostParams{
		Reference: "order-1",
		Currency:  "USD",
		Legs: []ledger.Leg{
			{AccountID: buyer.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(1000)},
			{AccountID: merchant.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(970)},
			{AccountID: fees.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(30)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := f.balance(t, merchant.ID); b != 970 {
		t.Errorf("merchant = %d, want 970", b)
	}
	if b := f.balance(t, fees.ID); b != 30 {
		t.Errorf("fees = %d, want 30", b)
	}
}

func TestPostRejects(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)

	eur, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "EUR"})
	if err != nil {
		t.Fatal(err)
	}
	// 10^78 has 79 digits, one more than an amount can hold.
	tooBig := ledger.AmountFromBig(new(big.Int).Exp(big.NewInt(10), big.NewInt(78), nil))

	cases := []struct {
		name string
		p    ledger.PostParams
		want error
	}{
		{"insufficient funds", ledger.Transfer("r1", "USD", alice.ID, bob.ID, ledger.NewAmount(101)), ledger.ErrInsufficientFunds},
		{"unbalanced", ledger.PostParams{Reference: "r2", Currency: "USD", Legs: []ledger.Leg{
			{AccountID: alice.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(10)},
			{AccountID: bob.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(9)},
		}}, ledger.ErrUnbalanced},
		{"currency mismatch", ledger.Transfer("r3", "USD", alice.ID, eur.ID, ledger.NewAmount(10)), ledger.ErrCurrencyMismatch},
		{"unknown account", ledger.Transfer("r4", "USD", alice.ID, ledger.NewAccountID(), ledger.NewAmount(10)), ledger.ErrNotFound},
		{"bad account id", ledger.Transfer("r5", "USD", alice.ID, "bob", ledger.NewAmount(10)), ledger.ErrInvalidID},
		{"zero amount", ledger.Transfer("r6", "USD", alice.ID, bob.ID, ledger.NewAmount(0)), ledger.ErrInvalidParams},
		{"same account", ledger.Transfer("r7", "USD", alice.ID, alice.ID, ledger.NewAmount(10)), ledger.ErrInvalidParams},
		{"no reference", ledger.Transfer("", "USD", alice.ID, bob.ID, ledger.NewAmount(10)), ledger.ErrInvalidParams},
		{"bad currency", ledger.Transfer("r8", "usd", alice.ID, bob.ID, ledger.NewAmount(10)), ledger.ErrInvalidParams},
		{"overflow", ledger.Transfer("r9", "USD", f.world.ID, alice.ID, tooBig), ledger.ErrAmountOverflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.svc.Post(ctx, tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	if b := f.balance(t, alice.ID); b != 100 {
		t.Errorf("rejected postings changed alice's balance to %d", b)
	}
}

func TestIdempotency(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)

	first, err := f.svc.Post(ctx, ledger.Transfer("same-ref", "USD", alice.ID, bob.ID, ledger.NewAmount(40)))
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.Post(ctx, ledger.Transfer("same-ref", "USD", alice.ID, bob.ID, ledger.NewAmount(40)))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Errorf("retry created %s, want original %s", again.ID, first.ID)
	}
	if b := f.balance(t, alice.ID); b != 60 {
		t.Errorf("alice = %d after retry, want 60", b)
	}

	_, err = f.svc.Post(ctx, ledger.Transfer("same-ref", "USD", alice.ID, bob.ID, ledger.NewAmount(41)))
	if !errors.Is(err, ledger.ErrReferenceConflict) {
		t.Errorf("reusing reference with a different amount: err = %v, want ErrReferenceConflict", err)
	}
}

func TestConcurrentSameReference(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)

	const n = 20
	ids := make([]ledger.TransactionID, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			txn, err := f.svc.Post(ctx, ledger.Transfer("race", "USD", alice.ID, bob.ID, ledger.NewAmount(10)))
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = txn.ID
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent retries produced different transactions: %v", ids)
		}
	}
	if b := f.balance(t, alice.ID); b != 90 {
		t.Errorf("alice = %d, want 90 (applied exactly once)", b)
	}
}

func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)

	var ok, insufficient atomic.Int32
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Post(ctx, ledger.Transfer(fmt.Sprintf("debit-%d", i), "USD", alice.ID, bob.ID, ledger.NewAmount(100)))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrInsufficientFunds):
				insufficient.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != 10 || insufficient.Load() != 40 {
		t.Errorf("succeeded %d, insufficient %d; want 10 and 40", ok.Load(), insufficient.Load())
	}
	if b := f.balance(t, alice.ID); b != 0 {
		t.Errorf("alice = %d, want 0", b)
	}
}

// TestInvariants hammers random transfers, holds and refunds concurrently,
// then checks that the books balance and every cached balance matches its
// entry history. It runs with both a tracked and an untracked @world.
func TestInvariants(t *testing.T) {
	for _, untracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("untracked_world=%v", untracked), func(t *testing.T) {
			testInvariants(t, setupWorld(t, untracked))
		})
	}
}

func testInvariants(t *testing.T, f *fixture) {
	ctx := context.Background()

	accounts := make([]*ledger.Account, 8)
	for i := range accounts {
		accounts[i] = f.account(t)
		f.fund(t, accounts[i].ID, 500)
	}

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				from := accounts[rand.IntN(len(accounts))].ID
				to := accounts[rand.IntN(len(accounts))].ID
				if from == to {
					continue
				}
				ref := fmt.Sprintf("w%d-%d", w, i)
				n := rand.Int64N(200) + 1
				amount := ledger.NewAmount(n)
				var err error
				switch rand.IntN(4) {
				case 0: // plain transfer
					_, err = f.svc.Post(ctx, ledger.Transfer(ref, "USD", from, to, amount))
				case 1: // transfer, then partial refund
					var txn *ledger.Transaction
					if txn, err = f.svc.Post(ctx, ledger.Transfer(ref, "USD", from, to, amount)); err == nil {
						_, err = f.svc.Refund(ctx, txn.ID, ledger.RefundParams{Reference: ref + "-r", Amount: ledger.NewAmount(n/2 + 1)})
					}
				case 2: // hold, then partial commit
					var h *ledger.Hold
					if h, err = f.svc.CreateHold(ctx, ledger.HoldParams{Reference: ref, Currency: "USD", Legs: legs(from, to, n)}); err == nil {
						_, err = f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(n/2 + 1)})
					}
				case 3: // hold, then void
					var h *ledger.Hold
					if h, err = f.svc.CreateHold(ctx, ledger.HoldParams{Reference: ref, Currency: "USD", Legs: legs(from, to, n)}); err == nil {
						_, err = f.svc.VoidHold(ctx, h.ID)
					}
				}
				if err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	var total int64
	for _, a := range append(accounts, f.world) {
		total += f.balance(t, a.ID)
	}
	if total != 0 {
		t.Errorf("sum of all balances = %d, want 0", total)
	}

	db := f.svc.DB()
	var drift int
	err := db.NewRaw(`
		SELECT count(*) FROM ledger.accounts a
		WHERE NOT a.untracked AND (
		      a.balance <> COALESCE((
		          SELECT sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END)
		          FROM ledger.entries e WHERE e.account_id = a.id), 0)
		   OR a.version <> (SELECT count(*) FROM ledger.entries e WHERE e.account_id = a.id)
		   OR a.pending_debit <> 0 OR a.pending_credit <> 0)`).
		Scan(ctx, &drift)
	if err != nil {
		t.Fatal(err)
	}
	if drift != 0 {
		t.Errorf("%d accounts disagree with their entries", drift)
	}
}

func TestPostTxJoinsCallerTransaction(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)

	errRollback := errors.New("caller rolls back")
	err := f.svc.DB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := f.svc.PostTx(ctx, tx, ledger.Transfer("in-tx", "USD", alice.ID, bob.ID, ledger.NewAmount(50))); err != nil {
			return err
		}
		// A failed posting must not poison the caller's transaction.
		if _, err := f.svc.PostTx(ctx, tx, ledger.Transfer("too-much", "USD", alice.ID, bob.ID, ledger.NewAmount(1000))); !errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Errorf("err = %v, want ErrInsufficientFunds", err)
		}
		var n int
		if err := tx.NewSelect().Model((*ledger.Transaction)(nil)).ColumnExpr("count(*)").Scan(ctx, &n); err != nil {
			t.Errorf("caller tx unusable after failed posting: %v", err)
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}

	if b := f.balance(t, alice.ID); b != 100 {
		t.Errorf("alice = %d, want 100: posting should roll back with the caller", b)
	}
	if _, err := f.svc.GetTransactionByReference(ctx, f.ledger.ID, "in-tx"); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("transaction survived caller rollback: %v", err)
	}
}

func TestBalanceAt(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)

	before := time.Now()
	f.fund(t, alice.ID, 100)
	mid := time.Now()
	f.fund(t, alice.ID, 50)

	for _, tc := range []struct {
		at   time.Time
		want int64
	}{
		{before.Add(-time.Second), 0},
		{mid, 100},
		{time.Now(), 150},
	} {
		got, err := f.svc.BalanceAt(ctx, alice.ID, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		if i64(t, got) != tc.want {
			t.Errorf("BalanceAt(%v) = %v, want %v", tc.at, got, tc.want)
		}
	}
}

func TestDatabaseGuards(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	txn, err := f.svc.Post(ctx, ledger.Transfer("guarded", "USD", alice.ID, bob.ID, ledger.NewAmount(10)))
	if err != nil {
		t.Fatal(err)
	}
	db := f.svc.DB()

	if _, err := db.ExecContext(ctx, `UPDATE ledger.entries SET amount = 1 WHERE transaction_id = ?`, txn.ID); err == nil {
		t.Error("entries must be immutable")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM ledger.transactions WHERE id = ?`, txn.ID); err == nil {
		t.Error("transactions must not be deletable")
	}
	if _, err := db.ExecContext(ctx, `UPDATE ledger.transactions SET reference = 'x' WHERE id = ?`, txn.ID); err == nil {
		t.Error("transaction reference must be immutable")
	}
	if _, err := db.ExecContext(ctx, `UPDATE ledger.transactions SET metadata = '{"note":"ok"}' WHERE id = ?`, txn.ID); err != nil {
		t.Errorf("metadata should be updatable: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE ledger.accounts SET balance = 5 WHERE id = ?`, alice.ID); err == nil {
		t.Error("balance must equal credit_total - debit_total")
	}

	// Bypass the Go code: an unbalanced transaction must fail at COMMIT.
	err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		id := ledger.NewTransactionID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger.transactions (id, ledger_id, reference, status, currency, request_hash, created_at)
			VALUES (?, ?, 'raw', 'posted', 'USD', '', now())`, id, f.ledger.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ledger.entries (id, transaction_id, account_id, direction, amount, balance_after, account_version, created_at)
			VALUES (?, ?, ?, 'credit', 5, 0, 999, now())`, ledger.NewEntryID(), id, bob.ID)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "entries, need at least 2") {
		t.Errorf("unbalanced raw insert: err = %v, want deferred trigger failure", err)
	}
}

func TestEnsureIndicatorAccount(t *testing.T) {
	ctx := context.Background()
	f := setup(t)

	again, err := f.svc.EnsureIndicatorAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD", Indicator: "@world"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != f.world.ID || !again.AllowNegative {
		t.Errorf("got %+v, want existing @world %s", again, f.world.ID)
	}

	_, err = f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD", Indicator: "@world"})
	if !errors.Is(err, ledger.ErrAlreadyExists) {
		t.Errorf("duplicate indicator: err = %v, want ErrAlreadyExists", err)
	}
	_, err = f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: ledger.NewLedgerID(), Currency: "USD"})
	if !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("unknown ledger: err = %v, want ErrNotFound", err)
	}
}

func legs(from, to ledger.AccountID, amount int64) []ledger.Leg {
	return ledger.Transfer("", "", from, to, ledger.NewAmount(amount)).Legs
}
