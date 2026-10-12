package ledger

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

type HoldParams struct {
	// Reference is the idempotency key for creating the hold, unique per ledger.
	Reference   string         `json:"reference"`
	Currency    string         `json:"currency"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	// Legs are the postings the hold reserves; they must balance.
	Legs []Leg `json:"legs"`
	// ExpiresAt, if set, is when the hold lapses: it can no longer be
	// committed, and ExpireHolds releases its funds. Not part of the
	// idempotency check, so a retry may compute it again.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CreateHold reserves funds for legs without moving them: debits count against
// the source accounts' available balance until the hold is committed or voided.
func (s *Service) CreateHold(ctx context.Context, p HoldParams) (*Hold, error) {
	return s.CreateHoldTx(ctx, s.db, p)
}

func (s *Service) CreateHoldTx(ctx context.Context, db bun.IDB, p HoldParams) (*Hold, error) {
	if err := validateReference(p.Reference); err != nil {
		return nil, err
	}
	if err := validateCurrency(p.Currency); err != nil {
		return nil, err
	}
	var err error
	if p.Legs, err = s.resolveLegs(ctx, db, p.Currency, p.Legs); err != nil {
		return nil, classify(ctx, err)
	}
	if err := validateLegs(p.Legs); err != nil {
		return nil, err
	}
	hash := requestHash("hold", p.Currency, legsKey(p.Legs))

	find := func(ctx context.Context, db bun.IDB) (*Hold, error) {
		h := new(Hold)
		err := db.NewSelect().Model(h).
			Where("h.reference = ?", p.Reference).
			Where("h.ledger_id = (SELECT ledger_id FROM ledger.accounts WHERE id = ?)", p.Legs[0].AccountID).
			Scan(ctx)
		if err != nil {
			return nil, notFound(err, "hold %q", p.Reference)
		}
		if h.RequestHash != hash {
			return nil, fmt.Errorf("%w: %q", ErrReferenceConflict, p.Reference)
		}
		return h, nil
	}
	return once(ctx, db, find, func(ctx context.Context, tx bun.Tx) (*Hold, error) {
		// Checked only for a new hold: a retry of one that has since
		// expired returns it rather than failing.
		if p.ExpiresAt != nil && !p.ExpiresAt.After(now()) {
			return nil, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidParams)
		}
		ps, err := lockAccounts(ctx, tx, accountIDs(p.Legs), "")
		if err != nil {
			return nil, err
		}
		if err := ps.checkCurrency(p.Currency, p.Legs); err != nil {
			return nil, err
		}
		if err := ps.reserve(p.Legs, 1); err != nil {
			return nil, err
		}
		h := &Hold{
			ID:          NewHoldID(),
			LedgerID:    ps.ledgerID,
			Reference:   p.Reference,
			Status:      HoldPending,
			Currency:    p.Currency,
			Description: p.Description,
			Metadata:    p.Metadata,
			Legs:        p.Legs,
			RequestHash: hash,
			CreatedAt:   ps.at,
		}
		if p.ExpiresAt != nil {
			h.ExpiresAt = ptr(p.ExpiresAt.UTC().Truncate(time.Microsecond))
		}
		if _, err := tx.NewInsert().Model(h).Exec(ctx); err != nil {
			return nil, fmt.Errorf("ledger: insert hold: %w", err)
		}
		if err := ps.save(ctx, tx, nil); err != nil {
			return nil, err
		}
		return h, emit(ctx, tx, EventHoldCreated, &h.LedgerID, string(h.ID), h, h.CreatedAt)
	})
}

func (s *Service) GetHold(ctx context.Context, id HoldID) (*Hold, error) {
	return s.GetHoldTx(ctx, s.db, id)
}

func (s *Service) GetHoldTx(ctx context.Context, db bun.IDB, id HoldID) (*Hold, error) {
	if err := parseID(prefixHold, string(id)); err != nil {
		return nil, err
	}
	h := new(Hold)
	if err := db.NewSelect().Model(h).Where("h.id = ?", id).Scan(ctx); err != nil {
		return nil, classify(ctx, notFound(err, "hold %s", id))
	}
	return h, nil
}

// CommitHoldParams selects how much of a hold to commit. Leave Amount, Value
// and Legs empty to commit the full hold; an explicit zero amount is an error.
// Whatever is not committed is released.
type CommitHoldParams struct {
	// Amount commits part of a two-leg hold. Value is the same as a decimal,
	// e.g. "12.50"; set one or the other.
	Amount Amount `json:"amount,omitzero"`
	Value  string `json:"value,omitempty"`
	// Legs commits part of a multi-leg hold; each (account, direction) total
	// must not exceed the held amount.
	Legs []Leg `json:"legs,omitempty"`
	// Description and Metadata default to the hold's.
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// CommitHold posts the hold (or part of it) as a transaction and releases the
// whole reservation. A hold commits at most once: retrying with the same
// params returns the same transaction; anything else fails with
// ErrHoldNotPending.
func (s *Service) CommitHold(ctx context.Context, id HoldID, p CommitHoldParams) (*Transaction, error) {
	return s.CommitHoldTx(ctx, s.db, id, p)
}

func (s *Service) CommitHoldTx(ctx context.Context, db bun.IDB, id HoldID, p CommitHoldParams) (*Transaction, error) {
	if err := parseID(prefixHold, string(id)); err != nil {
		return nil, err
	}
	if p.Value != "" || hasValues(p.Legs) {
		var currency string
		err := db.NewSelect().Model((*Hold)(nil)).Column("currency").Where("h.id = ?", id).Scan(ctx, &currency)
		if err != nil {
			return nil, classify(ctx, notFound(err, "hold %s", id))
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
	hash := requestHash("commit", string(id), p.Amount.String(), legsKey(p.Legs))

	// The commit transaction is identified by its hold, not by a reference,
	// so no user-chosen reference can block it.
	find := func(ctx context.Context, db bun.IDB) (*Transaction, error) {
		txn, err := getTransaction(ctx, db, "t.hold_id = ?", id)
		return matchRequest(txn, err, hash, fmt.Errorf("%w: hold %s was committed with different parameters", ErrHoldNotPending, id))
	}
	return once(ctx, db, find, func(ctx context.Context, tx bun.Tx) (*Transaction, error) {
		h, err := lockHold(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		switch h.Status {
		case HoldCommitted:
			// Committed concurrently; its transaction is visible now.
			return find(ctx, tx)
		case HoldVoided, HoldExpired:
			return nil, fmt.Errorf("%w: hold %s is %s", ErrHoldNotPending, id, h.Status)
		}
		if h.expired(now()) {
			return nil, fmt.Errorf("%w: %w: hold %s expired at %s", ErrHoldNotPending, ErrHoldExpired, id, h.ExpiresAt.Format(time.RFC3339))
		}

		legs, err := pickLegs(aggregate(h.Legs), p.Amount, p.Legs, fmt.Errorf("%w: commit exceeds hold", ErrInvalidParams))
		if err != nil {
			return nil, err
		}
		ps, err := lockAccounts(ctx, tx, accountIDs(h.Legs), h.LedgerID)
		if err != nil {
			return nil, err
		}
		if err := ps.reserve(h.Legs, -1); err != nil {
			return nil, err
		}

		txn := &Transaction{
			ID:          NewTransactionID(),
			LedgerID:    h.LedgerID,
			Status:      StatusPosted,
			Currency:    h.Currency,
			Description: cmp.Or(p.Description, h.Description),
			Metadata:    p.Metadata,
			HoldID:      &h.ID,
			RequestHash: hash,
			CreatedAt:   ps.at,
		}
		if txn.Metadata == nil {
			txn.Metadata = h.Metadata
		}
		if err := ps.post(txn, legs); err != nil {
			return nil, err
		}
		if err := ps.save(ctx, tx, txn); err != nil {
			return nil, err
		}

		h.Status, h.TransactionID, h.ResolvedAt = HoldCommitted, &txn.ID, &ps.at
		if err := resolveHold(ctx, tx, h); err != nil {
			return nil, err
		}
		return txn, emit(ctx, tx, EventHoldCommitted, &h.LedgerID, string(h.ID), h, ps.at)
	})
}

// VoidHold releases a pending hold's reserved funds. Voiding a voided hold
// returns it unchanged; voiding a committed hold fails with ErrHoldNotPending.
func (s *Service) VoidHold(ctx context.Context, id HoldID) (*Hold, error) {
	return s.VoidHoldTx(ctx, s.db, id)
}

func (s *Service) VoidHoldTx(ctx context.Context, db bun.IDB, id HoldID) (*Hold, error) {
	if err := parseID(prefixHold, string(id)); err != nil {
		return nil, err
	}
	var h *Hold
	err := inTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if h, err = lockHold(ctx, tx, id); err != nil {
			return err
		}
		switch h.Status {
		case HoldVoided:
			return nil
		case HoldCommitted, HoldExpired:
			return fmt.Errorf("%w: hold %s is %s", ErrHoldNotPending, id, h.Status)
		}
		return release(ctx, tx, h, HoldVoided, EventHoldVoided)
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// lockHold loads a hold and locks it, serializing commit and void. The hold is
// always locked before its accounts, matching the parent-then-accounts lock
// order used everywhere. Under REPEATABLE READ or SERIALIZABLE, locking a hold
// that changed since the snapshot fails with a serialization error
// (ErrRetryable) rather than acting on a stale status.
func lockHold(ctx context.Context, tx bun.Tx, id HoldID) (*Hold, error) {
	h := new(Hold)
	if err := tx.NewSelect().Model(h).Where("h.id = ?", id).For("NO KEY UPDATE").Scan(ctx); err != nil {
		return nil, notFound(err, "hold %s", id)
	}
	return h, nil
}

// release returns a pending hold's reserved funds and marks it voided or
// expired.
func release(ctx context.Context, tx bun.Tx, h *Hold, status HoldStatus, event string) error {
	ps, err := lockAccounts(ctx, tx, accountIDs(h.Legs), h.LedgerID)
	if err != nil {
		return err
	}
	if err := ps.reserve(h.Legs, -1); err != nil {
		return err
	}
	if err := ps.save(ctx, tx, nil); err != nil {
		return err
	}
	h.Status, h.ResolvedAt = status, &ps.at
	if err := resolveHold(ctx, tx, h); err != nil {
		return err
	}
	return emit(ctx, tx, event, &h.LedgerID, string(h.ID), h, ps.at)
}

// expired reports whether a pending hold is past its expiry at t.
func (h *Hold) expired(t time.Time) bool {
	return h.ExpiresAt != nil && !t.Before(*h.ExpiresAt)
}

// ExpireHolds releases up to limit pending holds whose ExpiresAt has passed,
// marking them expired, and returns how many it released. Each hold is
// released in its own database transaction. Holds being committed or voided
// at the same moment, or whose accounts stay locked by other transactions for
// more than a couple of seconds, are skipped and picked up by a later call.
// Run it periodically; ledgerd does.
func (s *Service) ExpireHolds(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	var ids []HoldID
	err := s.db.NewSelect().Model((*Hold)(nil)).Column("id").
		Where("h.status = 'pending' AND h.expires_at <= ?", now()).
		OrderExpr("h.expires_at").Limit(int64(limit)).
		Scan(ctx, &ids)
	if err != nil {
		return 0, classify(ctx, fmt.Errorf("ledger: find expired holds: %w", err))
	}
	n := 0
	for _, id := range ids {
		released := false
		err := inTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '2s'"); err != nil {
				return err
			}
			h := new(Hold)
			err := tx.NewSelect().Model(h).Where("h.id = ?", id).For("NO KEY UPDATE SKIP LOCKED").Scan(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				return nil // locked by a concurrent commit or void
			}
			if err != nil {
				return err
			}
			if h.Status != HoldPending || !h.expired(now()) {
				return nil
			}
			released = true
			return release(ctx, tx, h, HoldExpired, EventHoldExpired)
		})
		if errors.Is(err, ErrRetryable) {
			continue // its accounts are busy; next time
		}
		if err != nil {
			return n, err
		}
		if released {
			n++
		}
	}
	return n, nil
}

func resolveHold(ctx context.Context, tx bun.Tx, h *Hold) error {
	_, err := tx.NewUpdate().Model(h).Column("status", "transaction_id", "resolved_at").WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("ledger: resolve hold %s: %w", h.ID, err)
	}
	return nil
}
