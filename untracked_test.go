package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/otyang/ledger"
)

func TestUntrackedAccount(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)
	alice, bob := f.account(t), f.account(t)

	// Many concurrent deposits from @world.
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			to := alice.ID
			if i%2 == 1 {
				to = bob.ID
			}
			if _, err := f.svc.Post(ctx, ledger.Transfer(fmt.Sprintf("dep-%d", i), "USD", f.world.ID, to, ledger.NewAmount(10))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	w := f.get(t, f.world.ID)
	if !w.Untracked || w.Balance.String() != "-300" || w.DebitTotal.String() != "300" || w.Version != 30 {
		t.Errorf("@world = %+v; want untracked, balance -300, debit 300, 30 entries", w)
	}
	if a := f.balance(t, alice.ID); a != 150 {
		t.Errorf("alice = %d, want 150", a)
	}

	// Entries on untracked accounts have no running balance.
	page, err := f.svc.ListEntries(ctx, f.world.ID, ledger.ListEntriesParams{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 20 || page[0].BalanceAfter != nil || page[0].AccountVersion != nil {
		t.Fatalf("world entries page = %d entries, first %+v", len(page), page[0])
	}
	rest, err := f.svc.ListEntries(ctx, f.world.ID, ledger.ListEntriesParams{After: page[19].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 10 {
		t.Errorf("second page = %d entries, want 10", len(rest))
	}

	if bal, err := f.svc.BalanceAt(ctx, f.world.ID, time.Now()); err != nil || bal.String() != "-300" {
		t.Errorf("BalanceAt(world) = %s, %v; want -300", bal, err)
	}

	// The cached columns of an untracked account stay zero.
	var raw ledger.Amount
	if err := f.svc.DB().NewRaw(`SELECT debit_total FROM ledger.accounts WHERE id = ?`, f.world.ID).Scan(ctx, &raw); err != nil || raw.Sign() != 0 {
		t.Errorf("stored debit_total = %s, %v; want 0", raw, err)
	}

	// Holds and refunds work with untracked accounts too.
	h, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "payout", Currency: "USD", Legs: legs(alice.ID, f.world.ID, 100)})
	if err != nil {
		t.Fatal(err)
	}
	txn, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Refund(ctx, txn.ID, ledger.RefundParams{Reference: "payout-r", Amount: ledger.NewAmount(40)}); err != nil {
		t.Fatal(err)
	}
	if a, w := f.balance(t, alice.ID), f.balance(t, f.world.ID); a != 90 || w != -240 {
		t.Errorf("alice %d world %d; want 90, -240", a, w)
	}
}

func TestUntrackedRules(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)

	_, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD", Untracked: true})
	if !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("untracked without allow_negative: err = %v, want ErrInvalidParams", err)
	}
	if _, err := f.svc.DB().ExecContext(ctx, `UPDATE ledger.accounts SET untracked = false WHERE id = ?`, f.world.ID); err == nil {
		t.Error("the untracked flag must be immutable")
	}
}
