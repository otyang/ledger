package ledger

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/uptrace/bun"
)

// maxBatchSize bounds how many transactions one batch may post.
const maxBatchSize = 1000

var rateRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type BatchParams struct {
	// Reference is the batch's idempotency key, unique per ledger.
	Reference   string         `json:"reference"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	// Transactions are posted together: all of them or none. A member's
	// Reference is optional; if set it must be unique like any other.
	// Funds are checked once, on the combined effect of all members.
	Transactions []PostParams `json:"transactions"`
}

// PostBatch posts several transactions atomically, e.g. a payroll run or
// marketplace payouts. All accounts must be in one ledger; members may use
// different currencies. Posting the same reference again returns the
// original batch.
func (s *Service) PostBatch(ctx context.Context, p BatchParams) (*Batch, error) {
	return s.PostBatchTx(ctx, s.db, p)
}

func (s *Service) PostBatchTx(ctx context.Context, db bun.IDB, p BatchParams) (*Batch, error) {
	return s.postBatch(ctx, db, BatchPlain, "", p)
}

// ExchangeSide is one currency's half of an exchange: Amount (or Value) moves
// From one account To another, in Currency.
type ExchangeSide struct {
	Currency string    `json:"currency"`
	From     AccountID `json:"from"`
	To       AccountID `json:"to"`
	Amount   Amount    `json:"amount,omitzero"`
	Value    string    `json:"value,omitempty"`
}

type ExchangeParams struct {
	Reference string `json:"reference"`
	// Source is what is sold, typically from the customer's account to your
	// liquidity account in that currency; Target is what is bought, from
	// your liquidity account in the other currency to the customer's.
	Source ExchangeSide `json:"source"`
	Target ExchangeSide `json:"target"`
	// Rate is recorded with the exchange, as a decimal such as "0.92". It is
	// informational: the ledger doesn't derive one amount from the other.
	Rate        string         `json:"rate,omitempty"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Exchange converts between currencies: it posts the Source and Target
// transfers together as a batch of kind "exchange", with the rate recorded.
func (s *Service) Exchange(ctx context.Context, p ExchangeParams) (*Batch, error) {
	return s.ExchangeTx(ctx, s.db, p)
}

func (s *Service) ExchangeTx(ctx context.Context, db bun.IDB, p ExchangeParams) (*Batch, error) {
	if p.Source.Currency == p.Target.Currency {
		return nil, fmt.Errorf("%w: an exchange needs two different currencies", ErrInvalidParams)
	}
	if p.Rate != "" && (!rateRE.MatchString(p.Rate) || strings.Trim(p.Rate, "0.") == "") {
		return nil, fmt.Errorf("%w: rate %q must be a positive decimal such as 0.92", ErrInvalidParams, p.Rate)
	}
	p.Rate = canonicalRate(p.Rate)
	side := func(sd ExchangeSide) PostParams {
		return PostParams{Currency: sd.Currency, Legs: []Leg{
			{AccountID: sd.From, Direction: Debit, Amount: sd.Amount, Value: sd.Value},
			{AccountID: sd.To, Direction: Credit, Amount: sd.Amount, Value: sd.Value},
		}}
	}
	return s.postBatch(ctx, db, BatchExchange, p.Rate, BatchParams{
		Reference:    p.Reference,
		Description:  p.Description,
		Metadata:     p.Metadata,
		Transactions: []PostParams{side(p.Source), side(p.Target)},
	})
}

func (s *Service) postBatch(ctx context.Context, db bun.IDB, kind BatchKind, rate string, p BatchParams) (*Batch, error) {
	if err := validateReference(p.Reference); err != nil {
		return nil, err
	}
	if len(p.Transactions) == 0 || len(p.Transactions) > maxBatchSize {
		return nil, fmt.Errorf("%w: a batch needs 1-%d transactions", ErrInvalidParams, maxBatchSize)
	}
	members := make([]PostParams, len(p.Transactions))
	refs := map[string]bool{}
	var allIDs []AccountID
	parts := []string{"batch", string(kind), rate}
	for i, m := range p.Transactions {
		if m.Reference != "" {
			if err := validateReference(m.Reference); err != nil {
				return nil, fmt.Errorf("transaction %d: %w", i, err)
			}
			if refs[m.Reference] {
				return nil, fmt.Errorf("%w: transaction %d: reference %q is used twice in the batch", ErrInvalidParams, i, m.Reference)
			}
			refs[m.Reference] = true
		}
		if err := validateCurrency(m.Currency); err != nil {
			return nil, fmt.Errorf("transaction %d: %w", i, err)
		}
		var err error
		if m.Legs, err = s.resolveLegs(ctx, db, m.Currency, m.Legs); err != nil {
			return nil, classify(ctx, fmt.Errorf("transaction %d: %w", i, err))
		}
		if err := validateLegs(m.Legs); err != nil {
			return nil, fmt.Errorf("transaction %d: %w", i, err)
		}
		members[i] = m
		allIDs = append(allIDs, accountIDs(m.Legs)...)
		parts = append(parts, m.Reference, m.Currency, legsKey(m.Legs))
	}
	hash := requestHash(parts...)

	find := func(ctx context.Context, db bun.IDB) (*Batch, error) {
		b, err := getBatch(ctx, db,
			"b.reference = ? AND b.ledger_id = (SELECT ledger_id FROM ledger.accounts WHERE id = ?)",
			p.Reference, members[0].Legs[0].AccountID)
		if err != nil {
			return nil, err
		}
		if b.RequestHash != hash {
			return nil, fmt.Errorf("%w: %q", ErrReferenceConflict, p.Reference)
		}
		return b, nil
	}
	return once(ctx, db, find, func(ctx context.Context, tx bun.Tx) (*Batch, error) {
		ps, err := lockAccounts(ctx, tx, uniqueSorted(allIDs), "")
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			var taken []string
			err := tx.NewSelect().Model((*Transaction)(nil)).Column("reference").
				Where("t.ledger_id = ? AND t.reference IN (?)", ps.ledgerID, bun.In(keys(refs))).
				Scan(ctx, &taken)
			if err != nil {
				return nil, fmt.Errorf("ledger: check references: %w", err)
			}
			if len(taken) > 0 {
				return nil, fmt.Errorf("%w: %q is already used by another transaction", ErrReferenceConflict, taken[0])
			}
		}
		b := &Batch{
			ID:          NewBatchID(),
			LedgerID:    ps.ledgerID,
			Reference:   p.Reference,
			Kind:        kind,
			Rate:        rate,
			Description: p.Description,
			Metadata:    p.Metadata,
			RequestHash: hash,
			CreatedAt:   ps.at,
		}
		for _, m := range members {
			txn := &Transaction{
				ID:          NewTransactionID(),
				LedgerID:    ps.ledgerID,
				Reference:   m.Reference,
				Status:      StatusPosted,
				Currency:    m.Currency,
				Description: m.Description,
				Metadata:    m.Metadata,
				BatchID:     &b.ID,
				RequestHash: hash,
				CreatedAt:   ps.at,
			}
			if err := ps.apply(txn, m.Legs); err != nil {
				return nil, err
			}
			b.Transactions = append(b.Transactions, txn)
		}
		if err := ps.checkFunds(); err != nil {
			return nil, err
		}
		if _, err := tx.NewInsert().Model(b).Exec(ctx); err != nil {
			return nil, fmt.Errorf("ledger: insert batch: %w", err)
		}
		for _, txn := range b.Transactions {
			if err := insertTransaction(ctx, tx, txn); err != nil {
				return nil, err
			}
		}
		if err := ps.saveAccounts(ctx, tx); err != nil {
			return nil, err
		}
		return b, emit(ctx, tx, EventBatchPosted, &b.LedgerID, string(b.ID), b, b.CreatedAt)
	})
}

// GetBatch returns a batch with its transactions and their entries.
func (s *Service) GetBatch(ctx context.Context, id BatchID) (*Batch, error) {
	if err := parseID(prefixBatch, string(id)); err != nil {
		return nil, err
	}
	b, err := getBatch(ctx, s.db, "b.id = ?", id)
	return b, classify(ctx, err)
}

func getBatch(ctx context.Context, db bun.IDB, where string, args ...any) (*Batch, error) {
	b := new(Batch)
	err := db.NewSelect().Model(b).
		Relation("Transactions", func(q *bun.SelectQuery) *bun.SelectQuery { return q.Order("t.id") }).
		Relation("Transactions.Entries", func(q *bun.SelectQuery) *bun.SelectQuery { return q.Order("e.id") }).
		Where(where, args...).
		Scan(ctx)
	if err != nil {
		return nil, notFound(err, "batch %v", args)
	}
	return b, nil
}

func uniqueSorted(ids []AccountID) []AccountID {
	legs := make([]Leg, len(ids))
	for i, id := range ids {
		legs[i].AccountID = id
	}
	return accountIDs(legs)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// canonicalRate drops insignificant zeros: "000.9200" becomes "0.92".
func canonicalRate(r string) string {
	if r == "" {
		return r
	}
	whole, frac, _ := strings.Cut(r, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	if frac = strings.TrimRight(frac, "0"); frac != "" {
		return whole + "." + frac
	}
	return whole
}
