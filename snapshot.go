package ledger

import (
	"context"
	"fmt"
)

// RefreshSnapshots updates the running totals of every untracked account, so
// reading their balance only has to add up entries written since. It changes
// no balance, only how fast it is computed. Run it periodically; ledgerd
// does. It returns how many accounts it refreshed.
//
// A snapshot covers exactly the entries written by database transactions
// older than every transaction still running (txid < xmin): those can no
// longer change, and every later entry has a txid at or above the bound.
func (s *Service) RefreshSnapshots(ctx context.Context) (int, error) {
	var ids []AccountID
	if err := s.db.NewSelect().Model((*Account)(nil)).Column("id").Where("a.untracked").Scan(ctx, &ids); err != nil {
		return 0, classify(ctx, fmt.Errorf("ledger: list untracked accounts: %w", err))
	}
	for i, id := range ids {
		// One statement: the bound, the previous snapshot and the new
		// entries all come from the same database snapshot. Concurrent
		// refreshes are safe: each result is correct on its own, and a
		// snapshot never moves backwards.
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO ledger.account_snapshots AS s
			       (account_id, txid_bound, credit_total, debit_total, entry_count, refreshed_at)
			SELECT ?0, b.bound,
			       COALESCE(old.credit_total, 0) + COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'credit'), 0),
			       COALESCE(old.debit_total, 0) + COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'debit'), 0),
			       COALESCE(old.entry_count, 0) + count(e.id),
			       ?1
			  FROM (SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint AS bound) b
			  LEFT JOIN ledger.account_snapshots old ON old.account_id = ?0
			  LEFT JOIN ledger.entries e ON e.account_id = ?0
			        AND e.txid >= COALESCE(old.txid_bound, 0) AND e.txid < b.bound
			 GROUP BY b.bound, old.credit_total, old.debit_total, old.entry_count
			ON CONFLICT (account_id) DO UPDATE
			   SET txid_bound = EXCLUDED.txid_bound, credit_total = EXCLUDED.credit_total,
			       debit_total = EXCLUDED.debit_total, entry_count = EXCLUDED.entry_count,
			       refreshed_at = EXCLUDED.refreshed_at
			 WHERE s.txid_bound <= EXCLUDED.txid_bound`, id, now())
		if err != nil {
			return i, classify(ctx, fmt.Errorf("ledger: refresh snapshot of %s: %w", id, err))
		}
	}
	return len(ids), nil
}
