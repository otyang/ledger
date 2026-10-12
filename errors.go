package ledger

import (
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun/driver/pgdriver"
)

// Sentinel errors. Returned errors wrap these with detail, so match them with
// errors.Is.
var (
	ErrNotFound          = errors.New("ledger: not found")
	ErrAlreadyExists     = errors.New("ledger: already exists")
	ErrInvalidID         = errors.New("ledger: invalid id")
	ErrInvalidParams     = errors.New("ledger: invalid params")
	ErrUnbalanced        = errors.New("ledger: debits and credits do not balance")
	ErrCurrencyMismatch  = errors.New("ledger: currency mismatch")
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	ErrAmountOverflow    = errors.New("ledger: amount overflow")
	ErrReferenceConflict = errors.New("ledger: reference already used for a different transaction")
	ErrHoldNotPending    = errors.New("ledger: hold is not pending")
	ErrRefundExceeded    = errors.New("ledger: refund exceeds the refundable amount")
	ErrLedgerMismatch    = errors.New("ledger: accounts belong to different ledgers")
	ErrUnknownCurrency   = errors.New("ledger: unknown currency")
	ErrCurrencyConflict  = errors.New("ledger: currency already registered with different decimals")
	ErrAccountFrozen     = errors.New("ledger: account is frozen")
	ErrAccountClosed     = errors.New("ledger: account is closed")
	ErrHoldExpired       = errors.New("ledger: hold has expired")
	ErrPeriodClosed      = errors.New("ledger: period is closed")

	// ErrRetryable marks failures caused by concurrency (serialization
	// failures, deadlocks, lock timeouts, or a concurrent request with the
	// same idempotency key that the current snapshot can't see yet). Retrying
	// the whole operation, or the caller's whole transaction, is safe.
	ErrRetryable = errors.New("ledger: concurrent update, retry")

	// ErrTxAborted means a *Tx call could not roll back its own savepoint, so
	// the state of the caller's transaction is unknown. The caller must roll
	// it back.
	ErrTxAborted = errors.New("ledger: caller's transaction must be rolled back")
)

const (
	pgRaiseException       = "P0001"
	pgUniqueViolation      = "23505"
	pgForeignKeyViolation  = "23503"
	pgSerializationFailure = "40001"
	pgDeadlockDetected     = "40P01"
	pgLockNotAvailable     = "55P03"
)

// pgErrorIs reports whether err is a Postgres error with the given SQLSTATE
// code, optionally restricted to a named constraint.
func pgErrorIs(err error, code, constraint string) bool {
	var pgErr pgdriver.Error
	if !errors.As(err, &pgErr) || pgErr.Field('C') != code {
		return false
	}
	return constraint == "" || pgErr.Field('n') == constraint
}

// mapTriggerError turns the database's own refusals into sentinel errors.
// The Go code checks the same rules first; these catch races, such as an
// untracked (unlocked) account being frozen while a posting is in flight.
func mapTriggerError(err error) error {
	var pgErr pgdriver.Error
	if !errors.As(err, &pgErr) || pgErr.Field('C') != pgRaiseException {
		return err
	}
	switch msg := pgErr.Field('M'); {
	case strings.Contains(msg, "is closed through"):
		return fmt.Errorf("%w: %w", ErrPeriodClosed, err)
	case strings.HasSuffix(msg, " is frozen"):
		return fmt.Errorf("%w: %w", ErrAccountFrozen, err)
	case strings.HasSuffix(msg, " is closed"):
		return fmt.Errorf("%w: %w", ErrAccountClosed, err)
	}
	return err
}
