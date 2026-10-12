package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/otyang/ledger"
)

func TestRefundPartialThenRest(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)
	pay, err := f.svc.Post(ctx, ledger.Transfer("pay", "USD", alice.ID, bob.ID, ledger.NewAmount(1000)))
	if err != nil {
		t.Fatal(err)
	}

	r1, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r1", Amount: ledger.NewAmount(300)})
	if err != nil {
		t.Fatal(err)
	}
	if r1.RefundOf == nil || *r1.RefundOf != pay.ID {
		t.Errorf("refund not linked: %+v", r1)
	}
	// Refund legs mirror the original's order: credit alice, debit bob.
	if e := r1.Entries[1]; e.AccountID != bob.ID || e.Direction != ledger.Debit || e.Amount.String() != "300" {
		t.Errorf("refund should debit bob 300, got %+v", e)
	}

	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r2", Amount: ledger.NewAmount(701)}); !errors.Is(err, ledger.ErrRefundExceeded) {
		t.Errorf("refunding past the original: err = %v, want ErrRefundExceeded", err)
	}

	left, err := f.svc.Refundable(ctx, pay.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0].Amount.String() != "700" {
		t.Errorf("refundable = %+v, want 700 on both legs", left)
	}

	// No amount refunds whatever is left.
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r3"}); err != nil {
		t.Fatal(err)
	}
	if a, b := f.balance(t, alice.ID), f.balance(t, bob.ID); a != 1000 || b != 0 {
		t.Errorf("after full refund alice %d bob %d; want 1000, 0", a, b)
	}
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r4"}); !errors.Is(err, ledger.ErrRefundExceeded) {
		t.Errorf("refunding a fully refunded transaction: err = %v, want ErrRefundExceeded", err)
	}

	// A retry of an earlier refund still returns it.
	again, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r1", Amount: ledger.NewAmount(300)})
	if err != nil || again.ID != r1.ID {
		t.Errorf("refund retry = %v, %v; want %s", again, err, r1.ID)
	}
	if _, err := f.svc.Refund(ctx, r1.ID, ledger.RefundParams{Reference: "r5"}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("refunding a refund: err = %v, want ErrInvalidParams", err)
	}
}

func TestRefundMultiLeg(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	buyer, merchant, fees := f.account(t), f.account(t), f.account(t)
	f.fund(t, buyer.ID, 1000)
	order, err := f.svc.Post(ctx, ledger.PostParams{Reference: "order", Currency: "USD", Legs: []ledger.Leg{
		{AccountID: buyer.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(1000)},
		{AccountID: merchant.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(970)},
		{AccountID: fees.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(30)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Refund(ctx, order.ID, ledger.RefundParams{Reference: "r1", Amount: ledger.NewAmount(500)}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("amount on a multi-leg transaction: err = %v, want ErrInvalidParams", err)
	}

	// The merchant gives back 500; the fee is not refunded.
	_, err = f.svc.Refund(ctx, order.ID, ledger.RefundParams{Reference: "r2", Legs: []ledger.Leg{
		{AccountID: merchant.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(500)},
		{AccountID: buyer.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(500)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		legs []ledger.Leg
	}{
		{"more than the fee", []ledger.Leg{
			{AccountID: fees.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(31)},
			{AccountID: buyer.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(31)},
		}},
		{"more than the merchant has left to give", []ledger.Leg{
			{AccountID: merchant.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(471)},
			{AccountID: buyer.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(471)},
		}},
		{"not a reversal of the original", []ledger.Leg{
			{AccountID: buyer.ID, Direction: ledger.Debit, Amount: ledger.NewAmount(10)},
			{AccountID: merchant.ID, Direction: ledger.Credit, Amount: ledger.NewAmount(10)},
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Refund(ctx, order.ID, ledger.RefundParams{Reference: fmt.Sprintf("bad-%d", i), Legs: tc.legs})
			if !errors.Is(err, ledger.ErrRefundExceeded) {
				t.Errorf("err = %v, want ErrRefundExceeded", err)
			}
		})
	}

	// Full refund of the rest: merchant 470 and the 30 fee.
	if _, err := f.svc.Refund(ctx, order.ID, ledger.RefundParams{Reference: "r3"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[ledger.AccountID]int64{buyer.ID: 1000, merchant.ID: 0, fees.ID: 0} {
		if got := f.balance(t, id); got != want {
			t.Errorf("account %s = %d, want %d", id, got, want)
		}
	}
}

func TestRefundNeedsFunds(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob, carol := f.account(t), f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	pay, err := f.svc.Post(ctx, ledger.Transfer("pay", "USD", alice.ID, bob.ID, ledger.NewAmount(100)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Post(ctx, ledger.Transfer("spent", "USD", bob.ID, carol.ID, ledger.NewAmount(80))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r"}); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("refund from an account that spent the money: err = %v, want ErrInsufficientFunds", err)
	}
	if _, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: "r-part", Amount: ledger.NewAmount(20)}); err != nil {
		t.Errorf("partial refund within remaining funds: %v", err)
	}
}

// TestConcurrentRefundsNeverExceedOriginal fires many partial refunds at once;
// the total refunded must stop exactly at the original amount.
func TestConcurrentRefundsNeverExceedOriginal(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1000)
	pay, err := f.svc.Post(ctx, ledger.Transfer("pay", "USD", alice.ID, bob.ID, ledger.NewAmount(1000)))
	if err != nil {
		t.Fatal(err)
	}

	var ok, exceeded atomic.Int32
	var wg sync.WaitGroup
	for i := range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Refund(ctx, pay.ID, ledger.RefundParams{Reference: fmt.Sprintf("r%d", i), Amount: ledger.NewAmount(100)})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrRefundExceeded):
				exceeded.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != 10 || exceeded.Load() != 15 {
		t.Errorf("refunds succeeded %d, exceeded %d; want 10 and 15", ok.Load(), exceeded.Load())
	}
	if a := f.balance(t, alice.ID); a != 1000 {
		t.Errorf("alice = %d, want 1000", a)
	}
}

func TestRefundCommittedHold(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 500)
	h := f.hold(t, "auth", alice.ID, bob.ID, 500)
	txn, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(300)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Refund(ctx, txn.ID, ledger.RefundParams{Reference: "r", Amount: ledger.NewAmount(301)}); !errors.Is(err, ledger.ErrRefundExceeded) {
		t.Errorf("refund past the committed amount: err = %v, want ErrRefundExceeded", err)
	}
	if _, err := f.svc.Refund(ctx, txn.ID, ledger.RefundParams{Reference: "r"}); err != nil {
		t.Fatal(err)
	}
	if a := f.balance(t, alice.ID); a != 500 {
		t.Errorf("alice = %d, want 500", a)
	}
}
