package ledger

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Every ID is "<prefix>_<uuidv7>", e.g. "txn_01926f3a-7c2e-7b4a-9f1d-3e8c5a2b1d40".
// UUIDv7 is time-ordered, which keeps primary-key indexes append-friendly.
// Distinct types make it a compile error to pass an AccountID where a
// TransactionID is expected.
type (
	LedgerID      string
	AccountID     string
	TransactionID string
	EntryID       string
	HoldID        string
	BatchID       string
	EventID       string
)

const (
	prefixLedger      = "ldg"
	prefixAccount     = "acc"
	prefixTransaction = "txn"
	prefixEntry       = "ent"
	prefixHold        = "hld"
	prefixBatch       = "bat"
	prefixEvent       = "evt"
)

func newID(prefix string) string {
	return prefix + "_" + uuid.Must(uuid.NewV7()).String()
}

func NewLedgerID() LedgerID           { return LedgerID(newID(prefixLedger)) }
func NewAccountID() AccountID         { return AccountID(newID(prefixAccount)) }
func NewTransactionID() TransactionID { return TransactionID(newID(prefixTransaction)) }
func NewEntryID() EntryID             { return EntryID(newID(prefixEntry)) }
func NewHoldID() HoldID               { return HoldID(newID(prefixHold)) }
func NewBatchID() BatchID             { return BatchID(newID(prefixBatch)) }
func NewEventID() EventID             { return EventID(newID(prefixEvent)) }

// parseID checks that s is "<prefix>_<uuidv7>" in canonical lowercase form,
// matching the CHECK constraints in the schema.
func parseID(prefix, s string) error {
	p, rest, ok := strings.Cut(s, "_")
	if !ok || p != prefix {
		return fmt.Errorf("%w: %q must start with %s_", ErrInvalidID, s, prefix)
	}
	u, err := uuid.Parse(rest)
	if err != nil || u.String() != rest || u.Version() != 7 || u.Variant() != uuid.RFC4122 {
		return fmt.Errorf("%w: %q is not a %s_<uuidv7>", ErrInvalidID, s, prefix)
	}
	return nil
}

func ParseLedgerID(s string) (LedgerID, error) {
	return LedgerID(s), parseID(prefixLedger, s)
}

func ParseAccountID(s string) (AccountID, error) {
	return AccountID(s), parseID(prefixAccount, s)
}

func ParseTransactionID(s string) (TransactionID, error) {
	return TransactionID(s), parseID(prefixTransaction, s)
}

func ParseEntryID(s string) (EntryID, error) {
	return EntryID(s), parseID(prefixEntry, s)
}

func ParseHoldID(s string) (HoldID, error) {
	return HoldID(s), parseID(prefixHold, s)
}

func ParseBatchID(s string) (BatchID, error) {
	return BatchID(s), parseID(prefixBatch, s)
}

func ParseEventID(s string) (EventID, error) {
	return EventID(s), parseID(prefixEvent, s)
}
