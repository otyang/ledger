package ledger

import (
	"context"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
)

// RefundParams selects how much of a transaction to refund. Leave Amount,
// Value and Legs empty to refund everything not yet refunded; an explicit
// zero amount is an error.
type RefundParams struct {
	// Reference is the refund's idempotency key. Each partial refund needs
	// its own reference.
	Reference string `json:"reference"`
	// Amount refunds part of a two-leg transaction. Value is the same as a
	// decimal, e.g. "12.50"; set one or the other.
	Amount Amount `json:"amount,omitzero"`
	Value  string `json:"value,omitempty"`
	// Legs refunds part of a multi-leg transaction, deciding which accounts
	// give money back (e.g. the merchant but not the fee account). Each leg
	// must reverse a leg of the original, within what is left to refund.
	Legs        []Leg          `json:"legs,omitempty"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Refund posts a transaction reversing all or part of transaction id. Refunds
// can never add up to more than the original, even when requested
// concurrently: the original transaction is locked while a refund is posted.
func (s *Service) Refund(ctx context.Context, id TransactionID, p RefundParams) (*Transaction, error) {
	return s.RefundTx(ctx, s.db, id, p)
}

func (s *Service) RefundTx(ctx context.Context, db bun.IDB, id TransactionID, p RefundParams) (*Transaction, error) {
	if err := parseID(prefixTransaction, string(id)); err != nil {
		return nil, err
	}
	if err := validateReference(p.Reference); err != nil {
		return nil, err
	}
	if p.Value != "" || hasValues(p.Legs) {
		var currency string
		err := db.NewSelect().Model((*Transaction)(nil)).Column("currency").Where("t.id = ?", id).Scan(ctx, &currency)
		if err != nil {
			return nil, classify(ctx, notFound(err, "transaction %s", id))
		}
		if p.Amount, err = s.resolveAmount(ctx, db, currency, p.Amount, p.Value); err != nil {
			return nil, classify(ctx, err)
		}
		if p.Legs, err = s.resolveLegs(ctx, db, currency, p.Legs); err != nil {
			return nil, classify(ctx, err)
		}
		p.Value = ""
	}
	if len(p.Legs) > 0 {
		if err := validateLegs(p.Legs); err != nil {
			return nil, err
		}
	}
	return s.refund(ctx, db, id, p, requestHash("refund", string(id), p.Amount.String(), legsKey(p.Legs)), "")
}

// ReverseParams undoes a transaction. Unlike a refund, a reversal always
// takes back everything not yet refunded, and must say why.
type ReverseParams struct {
	// Reference is the reversal's idempotency key.
	Reference string `json:"reference"`
	// Reason is required and stored on the reversal, e.g. "posted to the
	// wrong account".
	Reason      string         `json:"reason"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Reverse posts a transaction undoing everything left of transaction id, to
// correct a mistake. It is a refund of the remainder with a reason: refunds
// and reversals together never exceed the original, and afterwards nothing is
// left to refund.
func (s *Service) Reverse(ctx context.Context, id TransactionID, p ReverseParams) (*Transaction, error) {
	return s.ReverseTx(ctx, s.db, id, p)
}

func (s *Service) ReverseTx(ctx context.Context, db bun.IDB, id TransactionID, p ReverseParams) (*Transaction, error) {
	if err := parseID(prefixTransaction, string(id)); err != nil {
		return nil, err
	}
	if err := validateReference(p.Reference); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Reason) == "" {
		return nil, fmt.Errorf("%w: a reversal needs a reason", ErrInvalidParams)
	}
	rp := RefundParams{Reference: p.Reference, Description: p.Description, Metadata: p.Metadata}
	return s.refund(ctx, db, id, rp, requestHash("reverse", string(id)), p.Reason)
}

// refund posts the refund described by p (already resolved and validated).
// A non-empty reason makes it a reversal.
func (s *Service) refund(ctx context.Context, db bun.IDB, id TransactionID, p RefundParams, hash, reason string) (*Transaction, error) {
	find := func(ctx context.Context, db bun.IDB) (*Transaction, error) {
		txn, err := getTransaction(ctx, db,
			"t.reference = ? AND t.ledger_id = (SELECT ledger_id FROM ledger.transactions WHERE id = ?)",
			p.Reference, id)
		return matchRequest(txn, err, hash, fmt.Errorf("%w: %q", ErrReferenceConflict, p.Reference))
	}
	return once(ctx, db, find, func(ctx context.Context, tx bun.Tx) (*Transaction, error) {
		orig, err := lockForRefund(ctx, tx, id)
		if err != nil {
			return nil, err
		}

		open, err := refundable(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		legs, err := pickLegs(open, p.Amount, p.Legs, ErrRefundExceeded)
		if err != nil {
			return nil, err
		}

		ps, err := lockAccounts(ctx, tx, accountIDs(legs), orig.LedgerID)
		if err != nil {
			return nil, err
		}
		txn := &Transaction{
			ID:             NewTransactionID(),
			LedgerID:       orig.LedgerID,
			Reference:      p.Reference,
			Status:         StatusPosted,
			Currency:       orig.Currency,
			Description:    p.Description,
			Metadata:       p.Metadata,
			RefundOf:       &orig.ID,
			ReversalReason: reason,
			RequestHash:    hash,
			CreatedAt:      ps.at,
		}
		if err := ps.post(txn, legs); err != nil {
			return nil, err
		}
		return txn, ps.save(ctx, tx, txn)
	})
}

// lockForRefund locks the original transaction and increments its
// refund_count. Concurrent refunds of the same transaction therefore
// serialize, and because each one writes the row, a refund running under
// REPEATABLE READ or SERIALIZABLE fails with a serialization error
// (ErrRetryable) instead of computing what is refundable from a stale
// snapshot. The lock is taken before any account, as everywhere.
func lockForRefund(ctx context.Context, tx bun.Tx, id TransactionID) (*Transaction, error) {
	orig := new(Transaction)
	if err := tx.NewSelect().Model(orig).Where("t.id = ?", id).For("NO KEY UPDATE").Scan(ctx); err != nil {
		return nil, notFound(err, "transaction %s", id)
	}
	if orig.RefundOf != nil {
		return nil, fmt.Errorf("%w: %s is itself a refund", ErrInvalidParams, id)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE ledger.transactions SET refund_count = refund_count + 1 WHERE id = ?", id); err != nil {
		return nil, fmt.Errorf("ledger: lock transaction %s: %w", id, err)
	}
	return orig, nil
}

// Refundable returns what is left to refund of transaction id, as the legs a
// full refund would post now.
func (s *Service) Refundable(ctx context.Context, id TransactionID) ([]Leg, error) {
	return s.RefundableTx(ctx, s.db, id)
}

func (s *Service) RefundableTx(ctx context.Context, db bun.IDB, id TransactionID) ([]Leg, error) {
	if _, err := s.GetTransactionTx(ctx, db, id); err != nil {
		return nil, err
	}
	legs, err := refundable(ctx, db, id)
	return legs, classify(ctx, err)
}

// refundable returns the original entries with directions reversed, minus
// what earlier refunds already posted, per (account, direction).
func refundable(ctx context.Context, db bun.IDB, id TransactionID) ([]Leg, error) {
	var entries []*Entry
	if err := db.NewSelect().Model(&entries).Where("e.transaction_id = ?", id).Order("e.id").Scan(ctx); err != nil {
		return nil, fmt.Errorf("ledger: load entries: %w", err)
	}
	var refunded []Leg
	err := db.NewSelect().
		TableExpr("ledger.entries AS e").
		Join("JOIN ledger.transactions AS t ON t.id = e.transaction_id").
		ColumnExpr("e.account_id, e.direction, sum(e.amount) AS amount").
		Where("t.refund_of = ?", id).
		GroupExpr("e.account_id, e.direction").
		Scan(ctx, &refunded)
	if err != nil {
		return nil, fmt.Errorf("ledger: load refunds: %w", err)
	}

	done := make(map[legKey]Amount, len(refunded))
	for _, leg := range refunded {
		done[legKey{leg.AccountID, leg.Direction}] = leg.Amount
	}
	reversed := make([]Leg, 0, len(entries))
	for _, e := range entries {
		reversed = append(reversed, Leg{AccountID: e.AccountID, Direction: e.Direction.opposite(), Amount: e.Amount})
	}
	open := make([]Leg, 0, len(reversed))
	for _, leg := range aggregate(reversed) {
		leg.Amount = leg.Amount.Sub(done[legKey{leg.AccountID, leg.Direction}])
		if leg.Amount.Sign() > 0 {
			open = append(open, leg)
		}
	}
	return open, nil
}
