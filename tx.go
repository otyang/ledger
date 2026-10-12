package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/uptrace/bun"
)

var savepointSeq atomic.Uint64

// inTx runs fn in a new database transaction, or in a savepoint when db is
// already a transaction (the *Tx variants of the API).
//
// Bun's own Tx.RunInTx is not used for savepoints: it hands fn the outer
// transaction's context, so the caller's deadline would be ignored, and if the
// deadline then expired before RELEASE SAVEPOINT the call would return an
// error while its writes stayed in the caller's transaction. Here fn gets the
// caller's ctx, and the savepoint is always released or rolled back with a
// context that can't be cancelled, so the result we return always matches
// what the caller's transaction will commit.
func inTx(ctx context.Context, db bun.IDB, fn func(context.Context, bun.Tx) error) error {
	var err error
	switch tx := db.(type) {
	case bun.Tx:
		err = savepoint(ctx, tx, fn)
	case *bun.Tx:
		err = savepoint(ctx, *tx, fn)
	default:
		err = db.RunInTx(ctx, nil, fn)
	}
	return classify(ctx, err)
}

func savepoint(ctx context.Context, tx bun.Tx, fn func(context.Context, bun.Tx) error) error {
	name := fmt.Sprintf("ledger_sp_%d", savepointSeq.Add(1))
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return err
	}
	err := func() error {
		// The balance check must run at COMMIT, after all entries exist, even
		// if the caller ran SET CONSTRAINTS ALL IMMEDIATE.
		if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ledger.transactions_balanced DEFERRED"); err != nil {
			return err
		}
		return fn(ctx, tx)
	}()

	bg := context.WithoutCancel(ctx)
	if err == nil {
		if _, err = tx.ExecContext(bg, "RELEASE SAVEPOINT "+name); err == nil {
			return nil
		}
	}
	if _, rbErr := tx.ExecContext(bg, "ROLLBACK TO SAVEPOINT "+name); rbErr != nil {
		return fmt.Errorf("%w: %w (rolling back savepoint: %v)", ErrTxAborted, err, rbErr)
	}
	_, _ = tx.ExecContext(bg, "RELEASE SAVEPOINT "+name)
	return err
}

// classify makes concurrency failures match ErrRetryable and context
// cancellation match ctx.Err(), keeping the original error wrapped.
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if pgErrorIs(err, pgSerializationFailure, "") || pgErrorIs(err, pgDeadlockDetected, "") || pgErrorIs(err, pgLockNotAvailable, "") {
		return fmt.Errorf("%w: %w", ErrRetryable, err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w: %w", ctxErr, err)
	}
	return err
}

// once runs create at most once per idempotency key. find looks up an
// existing result for the key; it returns ErrNotFound if there is none, and
// a conflict error if the key was used for a different request. Both run in
// the same transaction (or savepoint), so a failure never leaks into the
// caller's transaction.
func once[T any](ctx context.Context, db bun.IDB, find func(context.Context, bun.IDB) (T, error), create func(context.Context, bun.Tx) (T, error)) (T, error) {
	var out T
	err := inTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if out, err = find(ctx, tx); !errors.Is(err, ErrNotFound) {
			return err
		}
		out, err = create(ctx, tx)
		return err
	})
	if isIdempotencyConflict(err) {
		// A concurrent request with the same key committed first.
		err = inTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
			var err error
			out, err = find(ctx, tx)
			return err
		})
		if errors.Is(err, ErrNotFound) {
			// Our snapshot predates the winner (REPEATABLE READ or
			// SERIALIZABLE): only a retry of the whole transaction can see it.
			err = fmt.Errorf("%w: a concurrent request used the same idempotency key", ErrRetryable)
		}
	} else if err != nil && !errors.Is(err, ErrTxAborted) && ctx.Err() == nil {
		// An identical request may have committed while this one waited for
		// its locks, and then made create fail (the funds are spent, nothing
		// is left to refund). That request's result is the answer.
		var prior T
		ferr := inTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
			var err error
			prior, err = find(ctx, tx)
			return err
		})
		if ferr == nil {
			return prior, nil
		}
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

func isIdempotencyConflict(err error) bool {
	for _, c := range []string{"transactions_reference_key", "transactions_hold_id_key", "holds_reference_key", "batches_reference_key"} {
		if pgErrorIs(err, pgUniqueViolation, c) {
			return true
		}
	}
	return false
}
