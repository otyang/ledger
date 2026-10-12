package ledger_test

// Regression tests for the findings of the code review: each test reproduces
// the failure scenario that was reported.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/otyang/ledger"
	"github.com/otyang/ledger/ledgertest"
)

// lockRow holds a row lock on account id from another session until the
// returned release func is called.
func lockRow(t *testing.T, db *bun.DB, id ledger.AccountID) (release func()) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(context.Background(), "SELECT 1 FROM ledger.accounts WHERE id = ? FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback() }
}

// waitForLockWaiters blocks until at least n sessions are waiting on a lock.
func waitForLockWaiters(t *testing.T, db *bun.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := db.NewRaw(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(context.Background(), &waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d lock waiters", n)
}

func rr(t *testing.T, db *bun.DB) bun.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	// Take the snapshot now.
	if _, err := tx.ExecContext(context.Background(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// Finding 1: a *Tx call must honour its ctx, and must never report an error
// for a posting that then commits with the caller's transaction.
func TestTxCallHonoursContext(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)
	db := f.svc.DB()
	release := lockRow(t, db, alice.ID)
	defer release()

	caller, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer caller.Rollback()
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = f.svc.PostTx(tctx, caller, ledger.Transfer("ghost", "USD", f.world.ID, alice.ID, ledger.NewAmount(1)))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("PostTx ignored its deadline: took %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	release()

	// Whatever the caller does next, the failed posting must not exist.
	_ = caller.Commit()
	if _, err := f.svc.GetTransactionByReference(ctx, f.ledger.ID, "ghost"); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("PostTx returned %v but its posting committed (lookup err = %v)", ctx.Err(), err)
	}
}

// Finding 11: the ledger's balance check must stay deferred to COMMIT even if
// the caller made constraints immediate.
func TestTxCallWithImmediateConstraints(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)
	err := f.svc.DB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			return err
		}
		_, err := f.svc.PostTx(ctx, tx, ledger.Transfer("imm", "USD", f.world.ID, alice.ID, ledger.NewAmount(5)))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := f.balance(t, alice.ID); b != 5 {
		t.Errorf("alice = %d, want 5", b)
	}
}

// Finding 12: reads have Tx variants that see the caller's own uncommitted
// postings.
func TestReadsInsideCallerTx(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)
	err := f.svc.DB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		txn, err := f.svc.PostTx(ctx, tx, ledger.Transfer("own", "USD", f.world.ID, alice.ID, ledger.NewAmount(7)))
		if err != nil {
			return err
		}
		if _, err := f.svc.GetTransactionTx(ctx, tx, txn.ID); err != nil {
			t.Errorf("GetTransactionTx: %v", err)
		}
		a, err := f.svc.GetAccountTx(ctx, tx, alice.ID)
		if err != nil || a.Balance.String() != "7" {
			t.Errorf("GetAccountTx = %+v, %v; want balance 7", a, err)
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
}

// Finding 2: an entry posted after waiting on a lock must still come last in
// time, so BalanceAt(now) equals the current balance.
func TestBalanceAtAfterLockWait(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	b := f.account(t)
	target := f.account(t)
	if b.ID > target.ID {
		b, target = target, b
	}
	f.fund(t, b.ID, 100)
	db := f.svc.DB()

	release := lockRow(t, db, b.ID)
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Post(ctx, ledger.Transfer("slow", "USD", b.ID, target.ID, ledger.NewAmount(10)))
		done <- err
	}()
	waitForLockWaiters(t, db, 1)
	if _, err := f.svc.Post(ctx, ledger.Transfer("fast", "USD", f.world.ID, target.ID, ledger.NewAmount(5))); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.BalanceAt(ctx, target.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := f.balance(t, target.ID); i64(t, got) != want {
		t.Errorf("BalanceAt(now) = %s, current balance %d", got, want)
	}
	entries, err := f.svc.ListEntries(ctx, target.ID, ledger.ListEntriesParams{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].CreatedAt.Before(entries[i-1].CreatedAt) {
			t.Errorf("entry %d is timestamped before entry %d", i, i-1)
		}
	}
}

// Finding 3: concurrent Migrate calls wait for each other instead of failing.
func TestConcurrentMigrate(t *testing.T) {
	ctx := context.Background()
	db := ledgertest.NewDB(t)
	if _, err := db.ExecContext(ctx, "DROP SCHEMA ledger CASCADE"); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- ledger.New(db).Migrate(ctx) }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
}

// Down migrations revert cleanly, and the schema can be rebuilt afterwards.
func TestMigrateDownAndUp(t *testing.T) {
	ctx := context.Background()
	db := ledgertest.NewDB(t)
	svc := ledger.New(db)
	for range 6 {
		if err := svc.MigrateDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var tables int
	if err := db.NewRaw(`SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'ledger' AND table_name <> 'schema_migrations'`).Scan(ctx, &tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("%d ledger tables left after migrating down", tables)
	}
	if err := svc.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateLedger(ctx, ledger.CreateLedgerParams{Name: "again"}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateDownRefusesUntrackedData(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, true, false)
	for range 4 { // migrations 6, 5, 4 and 3 revert fine
		if err := f.svc.MigrateDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	err := f.svc.MigrateDown(ctx) // migration 2 can't hold untracked accounts
	if err == nil || !strings.Contains(err.Error(), "untracked accounts exist") {
		t.Errorf("err = %v, want refusal", err)
	}
}

// Migrating amounts back to BIGINT refuses values that don't fit, and turns
// hold leg amounts back into JSON numbers.
func TestMigrateDownAmounts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, false, false)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	h := f.hold(t, "h", alice.ID, bob.ID, 60)

	db := f.svc.DB()
	var kind string
	if err := db.NewRaw(`SELECT jsonb_typeof(legs -> 0 -> 'amount') FROM ledger.holds`).Scan(ctx, &kind); err != nil || kind != "string" {
		t.Fatalf("hold amount stored as %q, %v; want string", kind, err)
	}
	for range 3 { // migrations 6, 5 and 4
		if err := f.svc.MigrateDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.NewRaw(`SELECT jsonb_typeof(legs -> 0 -> 'amount') FROM ledger.holds`).Scan(ctx, &kind); err != nil || kind != "number" {
		t.Errorf("after downgrade hold amount is %q, %v; want number", kind, err)
	}
	// Upgrading again keeps old data usable, including holds whose legs
	// were written as numbers.
	if err := f.svc.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{}); err != nil {
		t.Fatal(err)
	}
	if b := f.balance(t, alice.ID); b != 40 {
		t.Errorf("alice balance = %d after upgrade and commit, want 40", b)
	}

	// 2^63 wei is about 9.2 ETH.
	tooBig, _ := ledger.ParseAmount("9223372036854775808")
	if _, err := f.svc.Post(ctx, ledger.Transfer("big", "USD", f.world.ID, alice.ID, tooBig)); err != nil {
		t.Fatal(err)
	}
	for range 2 { // migrations 6 and 5
		if err := f.svc.MigrateDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.MigrateDown(ctx); err == nil || !strings.Contains(err.Error(), "exceed BIGINT") {
		t.Errorf("err = %v, want refusal", err)
	}
}

// Registered decimals give stored amounts their meaning, so migration 5 can't
// be reverted once any are set.
func TestMigrateDownRefusesCurrencyDecimals(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if err := f.svc.MigrateDown(ctx); err != nil { // migration 6
		t.Fatal(err)
	}
	if err := f.svc.MigrateDown(ctx); err == nil || !strings.Contains(err.Error(), "registered decimals") {
		t.Errorf("err = %v, want refusal", err)
	}
	if c, err := f.svc.GetCurrency(ctx, "USD"); err != nil || *c.Decimals != 2 {
		t.Errorf("USD after refused downgrade = %+v, %v; want 2 decimals", c, err)
	}
}

// Finding 4: no user reference can block a hold's commit, and references are
// scoped per ledger.
func TestCommitCannotBeBlockedByReferences(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	h := f.hold(t, "h", alice.ID, bob.ID, 50)

	if _, err := f.svc.Post(ctx, ledger.Transfer(string(h.ID)+":commit", "USD", f.world.ID, bob.ID, ledger.NewAmount(1))); err != nil {
		t.Fatal(err)
	}
	txn, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{})
	if err != nil {
		t.Fatalf("CommitHold: %v", err)
	}
	if txn.Reference != "" || txn.HoldID == nil || *txn.HoldID != h.ID {
		t.Errorf("commit transaction = %+v; want no reference and hold_id %s", txn, h.ID)
	}
}

func TestReferencesArePerLedger(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)
	l2, err := f.svc.CreateLedger(ctx, ledger.CreateLedgerParams{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	w2, err := f.svc.EnsureIndicatorAccount(ctx, ledger.CreateAccountParams{LedgerID: l2.ID, Currency: "USD", Indicator: "@world", AllowNegative: true})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: l2.ID, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}

	t1, err := f.svc.Post(ctx, ledger.Transfer("order-1", "USD", f.world.ID, alice.ID, ledger.NewAmount(1)))
	if err != nil {
		t.Fatal(err)
	}
	t2, err := f.svc.Post(ctx, ledger.Transfer("order-1", "USD", w2.ID, a2.ID, ledger.NewAmount(2)))
	if err != nil {
		t.Fatalf("same reference in another ledger: %v", err)
	}
	if t1.ID == t2.ID || t1.LedgerID != f.ledger.ID || t2.LedgerID != l2.ID {
		t.Errorf("transactions %+v and %+v should be separate, one per ledger", t1, t2)
	}
}

// Finding 5: a posting or hold may not span ledgers, in Go or in the database.
func TestCrossLedgerRejected(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)
	l2, err := f.svc.CreateLedger(ctx, ledger.CreateLedgerParams{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: l2.ID, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Post(ctx, ledger.Transfer("x", "USD", f.world.ID, other.ID, ledger.NewAmount(7))); !errors.Is(err, ledger.ErrLedgerMismatch) {
		t.Errorf("cross-ledger Post: err = %v, want ErrLedgerMismatch", err)
	}
	if _, err := f.svc.CreateHold(ctx, ledger.HoldParams{Reference: "x", Currency: "USD", Legs: legs(f.world.ID, other.ID, 7)}); !errors.Is(err, ledger.ErrLedgerMismatch) {
		t.Errorf("cross-ledger hold: err = %v, want ErrLedgerMismatch", err)
	}

	// Raw SQL can't do it either.
	err = f.svc.DB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		id := ledger.NewTransactionID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger.transactions (id, ledger_id, reference, status, currency, request_hash, created_at)
			VALUES (?, ?, 'raw', 'posted', 'USD', '', now())`, id, f.ledger.ID); err != nil {
			return err
		}
		for _, leg := range []struct {
			account ledger.AccountID
			dir     string
		}{{alice.ID, "debit"}, {other.ID, "credit"}} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ledger.entries (id, transaction_id, account_id, direction, amount, created_at)
				VALUES (?, ?, ?, ?, 5, now())`, ledger.NewEntryID(), id, leg.account, leg.dir); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "does not match transaction") {
		t.Errorf("raw cross-ledger entry: err = %v, want trigger rejection", err)
	}
}

// Finding 6: paging an untracked account never skips an entry from a
// transaction that commits after the cursor was handed out.
func TestUntrackedPaginationWithInflightTransaction(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)
	alice, bob := f.account(t), f.account(t)
	db := f.svc.DB()

	slow, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PostTx(ctx, slow, ledger.Transfer("slow", "USD", f.world.ID, alice.ID, ledger.NewAmount(1))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Post(ctx, ledger.Transfer("fast", "USD", f.world.ID, bob.ID, ledger.NewAmount(1))); err != nil {
		t.Fatal(err)
	}

	page, err := f.svc.ListEntries(ctx, f.world.ID, ledger.ListEntriesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Fatalf("got %d entries while an older transaction is in flight, want them withheld", len(page))
	}
	if err := slow.Commit(); err != nil {
		t.Fatal(err)
	}

	page, err = f.svc.ListEntries(ctx, f.world.ID, ledger.ListEntriesParams{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	rest, err := f.svc.ListEntries(ctx, f.world.ID, ledger.ListEntriesParams{After: page[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page)+len(rest) != 2 {
		t.Errorf("paged %d + %d entries, want 2", len(page), len(rest))
	}
}

// Finding 12: a cursor from another account is an error, not an empty page.
func TestListEntriesForeignCursor(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 1)
	f.fund(t, bob.ID, 1)
	bobs, err := f.svc.ListEntries(ctx, bob.ID, ledger.ListEntriesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ListEntries(ctx, alice.ID, ledger.ListEntriesParams{After: bobs[0].ID}); !errors.Is(err, ledger.ErrInvalidParams) {
		t.Errorf("foreign cursor: err = %v, want ErrInvalidParams", err)
	}
}

// Finding 7: concurrent refunds can't exceed the original under REPEATABLE
// READ, even when every leg is on an untracked account.
func TestRepeatableReadRefunds(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)
	fees, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD", AllowNegative: true, Untracked: true})
	if err != nil {
		t.Fatal(err)
	}
	orig, err := f.svc.Post(ctx, ledger.Transfer("o", "USD", f.world.ID, fees.ID, ledger.NewAmount(100)))
	if err != nil {
		t.Fatal(err)
	}
	db := f.svc.DB()
	t1, t2 := rr(t, db), rr(t, db)

	if _, err := f.svc.RefundTx(ctx, t1, orig.ID, ledger.RefundParams{Reference: "r1"}); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() {
		_, err := f.svc.RefundTx(ctx, t2, orig.ID, ledger.RefundParams{Reference: "r2"})
		res <- err
	}()
	waitForLockWaiters(t, db, 1)
	if err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-res; !errors.Is(err, ledger.ErrRetryable) {
		t.Errorf("second refund: err = %v, want ErrRetryable", err)
	}
	_ = t2.Commit()

	left, err := f.svc.Refundable(ctx, orig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("refundable after one full refund = %+v, want nothing", left)
	}
}

// Finding 8: an idempotency race under REPEATABLE READ is retryable, not
// "not found".
func TestRepeatableReadSameReference(t *testing.T) {
	ctx := context.Background()
	f := setupWorld(t, true)
	fees, err := f.svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: f.ledger.ID, Currency: "USD", AllowNegative: true, Untracked: true})
	if err != nil {
		t.Fatal(err)
	}
	db := f.svc.DB()
	t1, t2 := rr(t, db), rr(t, db)
	p := ledger.Transfer("same", "USD", f.world.ID, fees.ID, ledger.NewAmount(5))

	if _, err := f.svc.PostTx(ctx, t1, p); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { _, err := f.svc.PostTx(ctx, t2, p); res <- err }()
	waitForLockWaiters(t, db, 1)
	if err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-res; !errors.Is(err, ledger.ErrRetryable) {
		t.Errorf("err = %v, want ErrRetryable", err)
	}

	// A plain retry outside the stale transaction returns the original.
	txn, err := f.svc.Post(ctx, p)
	if err != nil || txn.Reference != "same" {
		t.Errorf("retry = %+v, %v", txn, err)
	}
}

// Finding 12: request hashes don't depend on leg order.
func TestRetryWithReorderedLegs(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice := f.account(t)
	p := ledger.Transfer("dep", "USD", f.world.ID, alice.ID, ledger.NewAmount(10))
	first, err := f.svc.Post(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	p.Legs[0], p.Legs[1] = p.Legs[1], p.Legs[0]
	again, err := f.svc.Post(ctx, p)
	if err != nil || again.ID != first.ID {
		t.Errorf("retry with reordered legs = %v, %v; want %s", again, err, first.ID)
	}
}

// Finding 11: holds and entries are protected in the database.
func TestReviewSchemaGuards(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	alice, bob := f.account(t), f.account(t)
	f.fund(t, alice.ID, 100)
	h := f.hold(t, "h", alice.ID, bob.ID, 10)
	txn, err := f.svc.CommitHold(ctx, h.ID, ledger.CommitHoldParams{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.svc.Post(ctx, ledger.Transfer("other", "USD", alice.ID, bob.ID, ledger.NewAmount(1)))
	if err != nil {
		t.Fatal(err)
	}
	db := f.svc.DB()

	if _, err := db.ExecContext(ctx, `UPDATE ledger.holds SET transaction_id = ? WHERE id = ?`, other.ID, h.ID); err == nil {
		t.Error("a committed hold must not be repointed")
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE ledger.holds CASCADE`); err == nil {
		t.Error("holds must not be truncatable")
	}
	_, err = db.ExecContext(ctx, `INSERT INTO ledger.entries (id, transaction_id, account_id, direction, amount, created_at)
		VALUES (?, ?, ?, 'credit', 1, now())`, ledger.NewEntryID(), txn.ID, bob.ID)
	if err == nil || !strings.Contains(err.Error(), "only be added in the database transaction") {
		t.Errorf("entry added to an old transaction: err = %v", err)
	}
}
