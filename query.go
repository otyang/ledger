package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

func (s *Service) GetTransaction(ctx context.Context, id TransactionID) (*Transaction, error) {
	return s.GetTransactionTx(ctx, s.db, id)
}

// GetTransactionTx reads through db, so inside a caller's transaction it sees
// that transaction's own uncommitted postings.
func (s *Service) GetTransactionTx(ctx context.Context, db bun.IDB, id TransactionID) (*Transaction, error) {
	if err := parseID(prefixTransaction, string(id)); err != nil {
		return nil, err
	}
	txn, err := getTransaction(ctx, db, "t.id = ?", id)
	return txn, classify(ctx, err)
}

// GetTransactionByReference finds a transaction by its reference, which is
// unique per ledger.
func (s *Service) GetTransactionByReference(ctx context.Context, ledgerID LedgerID, reference string) (*Transaction, error) {
	txn, err := getTransaction(ctx, s.db, "t.ledger_id = ? AND t.reference = ?", ledgerID, reference)
	return txn, classify(ctx, err)
}

// UpdateTransactionMetadata merges patch into a transaction's metadata (keys
// set to nil are removed) and returns the transaction. Metadata is the only
// part of a transaction that can ever change.
func (s *Service) UpdateTransactionMetadata(ctx context.Context, id TransactionID, patch map[string]any) (*Transaction, error) {
	if err := parseID(prefixTransaction, string(id)); err != nil {
		return nil, err
	}
	var txn *Transaction
	err := inTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur := new(Transaction)
		if err := tx.NewSelect().Model(cur).Where("t.id = ?", id).For("NO KEY UPDATE").Scan(ctx); err != nil {
			return notFound(err, "transaction %s", id)
		}
		cur.Metadata = mergeMetadata(cur.Metadata, patch)
		if _, err := tx.NewUpdate().Model(cur).Column("metadata").WherePK().Exec(ctx); err != nil {
			return fmt.Errorf("ledger: update transaction %s: %w", id, err)
		}
		var err error
		if txn, err = getTransaction(ctx, tx, "t.id = ?", id); err != nil {
			return err
		}
		return emit(ctx, tx, EventTransactionUpdated, &txn.LedgerID, string(txn.ID), txn, now())
	})
	if err != nil {
		return nil, err
	}
	return txn, nil
}

func getTransaction(ctx context.Context, db bun.IDB, where string, args ...any) (*Transaction, error) {
	txn := new(Transaction)
	err := db.NewSelect().Model(txn).
		Relation("Entries", func(q *bun.SelectQuery) *bun.SelectQuery {
			// Entry IDs are UUIDv7 generated in leg order, so this is leg order.
			return q.Order("e.id")
		}).
		Where(where, args...).
		Scan(ctx)
	if err != nil {
		return nil, notFound(err, "transaction %v", args)
	}
	return txn, nil
}

type ListEntriesParams struct {
	// After is the pagination cursor: pass the ID of the last entry from the
	// previous page. Empty starts from the beginning.
	After EntryID
	// Limit defaults to 100 and is capped at 1000.
	Limit int
}

// ListEntries returns an account's entries in a stable order that is safe to
// tail: an entry never appears behind a cursor that was already handed out.
//
// Tracked accounts are ordered by AccountVersion, which is gap-free and
// assigned under the account lock, so every committed page is final.
//
// Untracked accounts take no lock, so their entries are ordered by the ID of
// the database transaction that wrote them, and only entries from database
// transactions older than every transaction still in progress are returned.
// An entry from a long-running transaction therefore shows up once that
// transaction ends, never behind a cursor that has already moved past it. The
// cost is that any long-running transaction on the database delays new
// entries on untracked accounts.
func (s *Service) ListEntries(ctx context.Context, accountID AccountID, p ListEntriesParams) ([]*Entry, error) {
	entries, err := s.listEntries(ctx, accountID, p)
	return entries, classify(ctx, err)
}

func (s *Service) listEntries(ctx context.Context, accountID AccountID, p ListEntriesParams) ([]*Entry, error) {
	a, err := getAccountRow(ctx, s.db, accountID)
	if err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)

	var cursorTxID int64
	var cursorVersion sql.NullInt64
	if p.After != "" {
		if err := parseID(prefixEntry, string(p.After)); err != nil {
			return nil, err
		}
		err := s.db.NewSelect().
			TableExpr("ledger.entries AS e").
			ColumnExpr("e.txid, e.account_version").
			Where("e.id = ? AND e.account_id = ?", p.After, accountID).
			Scan(ctx, &cursorTxID, &cursorVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: cursor %s is not an entry of account %s", ErrInvalidParams, p.After, accountID)
		}
		if err != nil {
			return nil, fmt.Errorf("ledger: list entries: %w", err)
		}
	}

	entries := make([]*Entry, 0)
	q := s.db.NewSelect().Model(&entries).
		Where("e.account_id = ?", accountID).
		Limit(int64(limit))
	if a.Untracked {
		q = q.Where("e.txid < pg_snapshot_xmin(pg_current_snapshot())::text::bigint").
			OrderExpr("e.txid, e.id")
		if p.After != "" {
			q = q.Where("(e.txid, e.id) > (?, ?)", cursorTxID, p.After)
		}
	} else {
		q = q.Order("e.account_version")
		if p.After != "" {
			q = q.Where("e.account_version > ?", cursorVersion.Int64)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("ledger: list entries: %w", err)
	}
	return entries, nil
}

// BalanceAt returns the account's balance as of time at. For tracked accounts
// it reads balance_after on the latest entry at or before at (entry
// timestamps never go backwards within an account, so the latest timestamp is
// also the latest version). For untracked ones it sums the entries up to at.
func (s *Service) BalanceAt(ctx context.Context, accountID AccountID, at time.Time) (Amount, error) {
	balance, err := s.balanceAt(ctx, accountID, at)
	return balance, classify(ctx, err)
}

func (s *Service) balanceAt(ctx context.Context, accountID AccountID, at time.Time) (Amount, error) {
	a, err := getAccountRow(ctx, s.db, accountID)
	if err != nil {
		return Amount{}, err
	}
	q := s.db.NewSelect().
		Model((*Entry)(nil)).
		Where("e.account_id = ?", accountID).
		Where("e.created_at <= ?", at)
	if a.Untracked {
		q = q.ColumnExpr("COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)")
	} else {
		q = q.Column("e.balance_after").
			OrderExpr("e.created_at DESC, e.account_version DESC").
			Limit(1)
	}

	var balance Amount
	err = q.Scan(ctx, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return Amount{}, nil
	}
	if err != nil {
		return Amount{}, fmt.Errorf("ledger: balance at: %w", err)
	}
	return balance, nil
}
