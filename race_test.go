package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/otyang/ledger"
)

// inFlight runs first inside an open database transaction, starts second
// concurrently, commits the transaction, and returns second's error. It is
// how a client's retry races its own original request.
func inFlight(t *testing.T, f *fixture, first func(ctx context.Context, tx bun.IDB) error, second func(ctx context.Context) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := f.svc.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- second(ctx) }()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// A retry racing its own original request gets the original's result, even
// when the original made the retry's own checks fail (funds spent, nothing
// left to refund, member references taken).
func TestConcurrentRetriesReplay(t *testing.T) {
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)

	spendAll := ledger.Transfer("all", "USD", alice.ID, bob.ID, amt(100))
	var orig *ledger.Transaction
	err := inFlight(t, f,
		func(ctx context.Context, tx bun.IDB) (err error) { orig, err = f.svc.PostTx(ctx, tx, spendAll); return },
		func(ctx context.Context) error {
			txn, err := f.svc.Post(ctx, spendAll)
			if err == nil && txn.ID != orig.ID {
				t.Errorf("retry posted a second transaction %s", txn.ID)
			}
			return err
		})
	if err != nil {
		t.Errorf("post retry: %v, want the original", err)
	}

	hold := ledger.HoldParams{Reference: "h", Currency: "USD", Legs: legs(bob.ID, alice.ID, 100)}
	if err := inFlight(t, f,
		func(ctx context.Context, tx bun.IDB) error { _, err := f.svc.CreateHoldTx(ctx, tx, hold); return err },
		func(ctx context.Context) error { _, err := f.svc.CreateHold(ctx, hold); return err }); err != nil {
		t.Errorf("hold retry: %v, want the original", err)
	}
	if h, err := f.svc.GetHoldTx(context.Background(), f.svc.DB(), mustHold(t, f, "h")); err == nil {
		if _, err := f.svc.VoidHold(context.Background(), h.ID); err != nil {
			t.Fatal(err)
		}
	}

	rev := ledger.ReverseParams{Reference: "rev", Reason: "oops"}
	if err := inFlight(t, f,
		func(ctx context.Context, tx bun.IDB) error {
			_, err := f.svc.ReverseTx(ctx, tx, orig.ID, rev)
			return err
		},
		func(ctx context.Context) error { _, err := f.svc.Reverse(ctx, orig.ID, rev); return err }); err != nil {
		t.Errorf("reversal retry: %v, want the original", err)
	}

	f.fund(t, alice.ID, 50)
	batch := ledger.BatchParams{Reference: "b", Transactions: []ledger.PostParams{{Reference: "m1", Currency: "USD", Legs: legs(alice.ID, bob.ID, 10)}}}
	if err := inFlight(t, f,
		func(ctx context.Context, tx bun.IDB) error { _, err := f.svc.PostBatchTx(ctx, tx, batch); return err },
		func(ctx context.Context) error { _, err := f.svc.PostBatch(ctx, batch); return err }); err != nil {
		t.Errorf("batch retry: %v, want the original", err)
	}

	// A genuinely different request still fails as it should.
	if _, err := f.svc.Post(context.Background(), ledger.Transfer("other", "USD", alice.ID, bob.ID, amt(1000))); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("different request: err = %v, want ErrInsufficientFunds", err)
	}
	if b := f.balance(t, bob.ID); b != 10 {
		t.Errorf("bob = %d, want 10 (100 in, reversed, 10 from the batch)", b)
	}
}

// Freezing an untracked account waits for debits already in flight, and
// refuses those that come after.
func TestFreezeUntrackedWaitsForInFlightDebits(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)
	alice := f.account(t)
	tx, err := f.svc.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PostTx(ctx, tx, ledger.Transfer("inflight", "USD", f.world.ID, alice.ID, amt(10))); err != nil {
		t.Fatal(err)
	}
	froze := make(chan error, 1)
	go func() {
		_, err := f.svc.UpdateAccount(ctx, f.world.ID, ledger.UpdateAccountParams{Status: status(ledger.AccountFrozen)})
		froze <- err
	}()
	select {
	case err := <-froze:
		t.Fatalf("freeze returned (%v) while a debit was still in flight", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-froze; err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Post(ctx, ledger.Transfer("after", "USD", f.world.ID, alice.ID, amt(1))); !errors.Is(err, ledger.ErrAccountFrozen) {
		t.Errorf("debit after freeze: err = %v, want ErrAccountFrozen", err)
	}
}

// Retrying a hold request after its expiry passed returns the hold.
func TestHoldRetryAfterExpiry(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	exp := time.Now().Add(300 * time.Millisecond)
	p := ledger.HoldParams{Reference: "h1", Currency: "USD", Legs: legs(alice.ID, bob.ID, 10), ExpiresAt: &exp}
	h, err := f.svc.CreateHold(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if again, err := f.svc.CreateHold(ctx, p); err != nil || again.ID != h.ID {
		t.Errorf("retry after expiry = %v, %v; want the original hold", again, err)
	}
}

// ExpireHolds skips a hold whose accounts are busy instead of waiting.
func TestExpireHoldsSkipsBusyAccounts(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	exp := time.Now().Add(200 * time.Millisecond)
	if _, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "h1", Currency: "USD", Legs: legs(alice.ID, bob.ID, 10), ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	tx, _ := f.svc.DB().BeginTx(ctx, nil)
	if _, err := f.svc.PostTx(ctx, tx, ledger.Transfer("lock", "USD", f.world.ID, bob.ID, amt(1))); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n, err := f.svc.ExpireHolds(ctx, 10)
	if err != nil || n != 0 || time.Since(start) > 5*time.Second {
		t.Errorf("ExpireHolds with a busy account = %d, %v after %s; want 0, nil, quickly", n, err, time.Since(start))
	}
	_ = tx.Rollback()
	if n, err := f.svc.ExpireHolds(ctx, 10); err != nil || n != 1 {
		t.Errorf("ExpireHolds once free = %d, %v; want 1", n, err)
	}
}

// Event cursors never move backwards.
func TestEventCursorMonotonic(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	events, err := f.svc.ListEvents(ctx, ledger.ListEventsParams{})
	if err != nil || len(events) < 2 {
		t.Fatalf("events = %d, %v", len(events), err)
	}
	last, first := events[len(events)-1].ID, events[0].ID
	if err := f.svc.SetEventCursor(ctx, "c", last); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetEventCursor(ctx, "c", first); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.svc.EventCursor(ctx, "c"); c != last {
		t.Errorf("cursor moved back to %s, want %s", c, last)
	}
}

// Equivalent exchange rates are the same request.
func TestExchangeRateCanonical(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	eurWorld := f.create(t, ledger.CreateAccountParams{Currency: "EUR", Indicator: "@world", AllowNegative: true})
	usd, eur, fx := f.account(t), f.create(t, ledger.CreateAccountParams{Currency: "EUR"}), f.create(t, ledger.CreateAccountParams{Indicator: "@fx"})
	f.fund(t, usd.ID, 1000)
	p := ledger.ExchangeParams{Reference: "fx", Rate: "0.920",
		Source: ledger.ExchangeSide{Currency: "USD", From: usd.ID, To: fx.ID, Amount: amt(100)},
		Target: ledger.ExchangeSide{Currency: "EUR", From: eurWorld.ID, To: eur.ID, Amount: amt(92)}}
	b, err := f.svc.Exchange(ctx, p)
	if err != nil || b.Rate != "0.92" {
		t.Fatalf("exchange = %v, %v; want rate 0.92", b, err)
	}
	p.Rate = "00.92"
	if again, err := f.svc.Exchange(ctx, p); err != nil || again.ID != b.ID {
		t.Errorf("retry with 00.92 = %v, %v; want the original", again, err)
	}
}

func mustHold(t *testing.T, f *fixture, ref string) ledger.HoldID {
	t.Helper()
	var id ledger.HoldID
	if err := f.svc.DB().NewSelect().TableExpr("ledger.holds").Column("id").Where("reference = ?", ref).Scan(context.Background(), &id); err != nil {
		t.Fatal(err)
	}
	return id
}
