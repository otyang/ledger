package ledger_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/otyang/ledger"
)

func (f *fixture) hold(t *testing.T, ref string, from, to ledger.AccountID, amount int64) *ledger.Hold {
	t.Helper()
	h, err := f.svc.CreateHold(context.Background(), ledger.HoldParams{Reference: ref, Currency: "USD", Legs: legs(from, to, amount)})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *fixture) get(t *testing.T, id ledger.AccountID) *ledger.Account {
	t.Helper()
	a, err := f.svc.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestHoldReservesFunds(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)

	h := f.hold(t, "auth-1", alice.ID, bob.ID, 600)
	if !strings.HasPrefix(string(h.ID), "hld_") || h.Status != ledger.HoldPending {
		t.Fatalf("unexpected hold %+v", h)
	}

	a := f.get(t, alice.ID)
	if a.Balance.String() != "1000" || a.PendingDebit.String() != "600" || a.Available().String() != "400" {
		t.Errorf("alice balance %v pending %v available %v; want 1000, 600, 400", a.Balance, a.PendingDebit, a.Available())
	}
	if b := f.get(t, bob.ID); b.Balance.String() != "0" || b.PendingCredit.String() != "600" {
		t.Errorf("bob balance %v pending credit %v; want 0, 600", b.Balance, b.PendingCredit)
	}

	// Reserved funds can't be spent, by a transfer or by another hold.
	if _, err := f.svc.Post(ctx, ledger.Transfer("spend", "USD", alice.ID, bob.ID, ledger.NewAmount(500))); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("spending held funds: err = %v, want ErrInsufficientFunds", err)
	}
	if _, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "auth-2", Currency: "USD", Legs: legs(alice.ID, bob.ID, 401)}); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("over-reserving: err = %v, want ErrInsufficientFunds", err)
	}

	// Same reference and legs returns the same hold; different legs conflict.
	again, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "auth-1", Currency: "USD", Legs: legs(alice.ID, bob.ID, 600)})
	if err != nil || again.ID != h.ID {
		t.Errorf("hold retry = %v, %v; want %s", again, err, h.ID)
	}
	if _, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "auth-1", Currency: "USD", Legs: legs(alice.ID, bob.ID, 1)}); !errors.Is(err, ledger.ErrReferenceConflict) {
		t.Errorf("hold reference reuse: err = %v, want ErrReferenceConflict", err)
	}
}

func TestCommitHoldPartial(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)
	h := f.hold(t, "auth", alice.ID, bob.ID, 600)

	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(601)}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("committing more than held: err = %v, want ErrInvalidParams", err)
	}

	txn, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(400)})
	if err != nil {
		t.Fatal(err)
	}
	if txn.HoldID == nil || *txn.HoldID != h.ID || len(txn.Entries) != 2 || txn.Entries[0].Amount.String() != "400" {
		t.Errorf("commit transaction = %+v", txn)
	}

	// The whole reservation is released; only 400 moved.
	a := f.get(t, alice.ID)
	if a.Balance.String() != "600" || a.PendingDebit.String() != "0" {
		t.Errorf("alice balance %v pending %v; want 600, 0", a.Balance, a.PendingDebit)
	}
	if b := f.get(t, bob.ID); b.Balance.String() != "400" || b.PendingCredit.String() != "0" {
		t.Errorf("bob balance %v pending %v; want 400, 0", b.Balance, b.PendingCredit)
	}

	got, err := f.svc.GetHold(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ledger.HoldCommitted || got.TransactionID == nil || *got.TransactionID != txn.ID || got.ResolvedAt == nil {
		t.Errorf("hold after commit = %+v", got)
	}

	// Retrying the commit is idempotent; changing it or voiding is not allowed.
	again, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(400)})
	if err != nil || again.ID != txn.ID {
		t.Errorf("commit retry = %v, %v; want %s", again, err, txn.ID)
	}
	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{}); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Errorf("second commit with other params: err = %v, want ErrHoldNotPending", err)
	}
	if _, err := f.svc.VoidHold(ctx, h.ID); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Errorf("void after commit: err = %v, want ErrHoldNotPending", err)
	}
}

func TestCommitHoldFullMultiLeg(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	buyer, merchant, fees := f.account(t), f.account(t), f.account(t)
	f.fund(t, buyer.ID, 1000)

	h, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "order", Currency: "USD", Legs: []ledger.Leg{
		{AccountID: buyer.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(1000)},
		{AccountID: merchant.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(970)},
		{AccountID: fees.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(30)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(500)}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("amount on a multi-leg hold: err = %v, want ErrInvalidParams", err)
	}

	// Partial commit of a multi-leg hold via explicit legs: the fee is kept, the rest shrinks.
	_, err = f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Legs: []ledger.Leg{
		{AccountID: buyer.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(800)},
		{AccountID: merchant.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(770)},
		{AccountID: fees.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(30)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[ledger.AccountID]int64{buyer.ID: 200, merchant.ID: 770, fees.ID: 30} {
		if a := f.get(t, id); i64(t, a.Balance) != want || a.PendingDebit.String() != "0" || a.PendingCredit.String() != "0" {
			t.Errorf("account %s balance %v pending %v/%v; want %v, 0/0", id, a.Balance, a.PendingDebit, a.PendingCredit, want)
		}
	}
}

func TestVoidHold(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)
	h := f.hold(t, "auth", alice.ID, bob.ID, 600)

	voided, err := f.svc.VoidHold(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if voided.Status != ledger.HoldVoided || voided.ResolvedAt == nil {
		t.Errorf("voided hold = %+v", voided)
	}
	if a := f.get(t, alice.ID); a.Balance.String() != "1000" || a.PendingDebit.String() != "0" {
		t.Errorf("alice balance %v pending %v; want 1000, 0", a.Balance, a.PendingDebit)
	}
	if again, err := f.svc.VoidHold(ctx, h.ID); err != nil || again.Status != ledger.HoldVoided {
		t.Errorf("void retry = %v, %v; want voided hold", again, err)
	}
	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{}); !errors.Is(err, ledger.ErrHoldNotPending) {
		t.Errorf("commit after void: err = %v, want ErrHoldNotPending", err)
	}
	if _, err := f.svc.VoidHold(ctx, ledger.NewHoldID()); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("void unknown hold: err = %v, want ErrNotFound", err)
	}
}

// TestHoldResolvesOnce races commits against voids: exactly one kind wins and
// the funds move (or are released) exactly once.
func TestHoldResolvesOnce(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)
	h := f.hold(t, "auth", alice.ID, bob.ID, 600)

	var mu sync.Mutex
	var commits, voids, rejected int
	txnIDs := map[ledger.TransactionID]bool{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				var txn *ledger.Transaction
				if txn, err = f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{}); err == nil {
					mu.Lock()
					commits++
					txnIDs[txn.ID] = true
					mu.Unlock()
				}
			} else if _, err = f.svc.VoidHold(ctx, h.ID); err == nil {
				mu.Lock()
				voids++
				mu.Unlock()
			}
			if errors.Is(err, ledger.ErrHoldNotPending) {
				mu.Lock()
				rejected++
				mu.Unlock()
			} else if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if (commits > 0) == (voids > 0) || rejected != 10 || len(txnIDs) > 1 {
		t.Fatalf("commits %d voids %d rejected %d txns %d: want exactly one side to win", commits, voids, rejected, len(txnIDs))
	}
	want := int64(1000)
	if commits > 0 {
		want = 400
	}
	if a := f.get(t, alice.ID); i64(t, a.Balance) != want || a.PendingDebit.String() != "0" {
		t.Errorf("alice balance %v pending %v; want %v, 0", a.Balance, a.PendingDebit, want)
	}
}

func TestHoldGuards(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	h := f.hold(t, "auth", alice.ID, bob.ID, 50)
	if _, err := f.svc.VoidHold(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	db := f.svc.DB()
	if _, err := db.ExecContext(ctx, `UPDATE ledger.holds SET status = 'pending', resolved_at = NULL WHERE id = ?`, h.ID); err == nil {
		t.Error("a resolved hold must not be reopened")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM ledger.holds WHERE id = ?`, h.ID); err == nil {
		t.Error("holds must not be deletable")
	}
	if _, err := db.ExecContext(ctx, `UPDATE ledger.accounts SET pending_debit = 101 WHERE id = ?`, alice.ID); err == nil {
		t.Error("pending debits beyond the balance must violate the overdraft check")
	}
}
