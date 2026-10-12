package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// maxStatementLines bounds one statement; ask for a shorter period beyond it.
const maxStatementLines = 100_000

// closeLag is how far in the past a period end must be. Postings take their
// timestamp when they start, so this leaves time for any in flight to commit.
const closeLag = time.Minute

type ListTransactionsParams struct {
	// LedgerID is required.
	LedgerID LedgerID
	// AccountID, if set, returns only transactions with an entry on it.
	AccountID AccountID
	// From and To, if set, bound created_at: From <= created_at < To.
	From, To time.Time
	// Metadata, if set, returns only transactions whose metadata contains
	// all of these keys and values, e.g. {"customer_id": "c_77"}.
	Metadata map[string]any
	// After is the pagination cursor: the ID of the last transaction from the
	// previous page.
	After TransactionID
	// Limit defaults to 100 and is capped at 1000.
	Limit int
}

// ListTransactions searches a ledger's transactions, oldest first, with their
// entries. It is meant for browsing and support tools; to follow new
// transactions as they happen, use ListEvents, which never skips any.
func (s *Service) ListTransactions(ctx context.Context, p ListTransactionsParams) ([]*Transaction, error) {
	txns, err := s.listTransactions(ctx, p)
	return txns, classify(ctx, err)
}

func (s *Service) listTransactions(ctx context.Context, p ListTransactionsParams) ([]*Transaction, error) {
	if err := parseID(prefixLedger, string(p.LedgerID)); err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)

	txns := make([]*Transaction, 0)
	q := s.db.NewSelect().Model(&txns).
		Relation("Entries", func(q *bun.SelectQuery) *bun.SelectQuery { return q.Order("e.id") }).
		Where("t.ledger_id = ?", p.LedgerID).
		Order("t.id").
		Limit(int64(limit))
	if p.AccountID != "" {
		if err := parseID(prefixAccount, string(p.AccountID)); err != nil {
			return nil, err
		}
		q = q.Where("EXISTS (SELECT 1 FROM ledger.entries x WHERE x.transaction_id = t.id AND x.account_id = ?)", p.AccountID)
	}
	if !p.From.IsZero() {
		q = q.Where("t.created_at >= ?", p.From)
	}
	if !p.To.IsZero() {
		q = q.Where("t.created_at < ?", p.To)
	}
	if len(p.Metadata) > 0 {
		b, err := json.Marshal(p.Metadata)
		if err != nil {
			return nil, fmt.Errorf("%w: metadata filter: %v", ErrInvalidParams, err)
		}
		q = q.Where("t.metadata @> ?::jsonb", string(b))
	}
	if p.After != "" {
		if err := parseID(prefixTransaction, string(p.After)); err != nil {
			return nil, err
		}
		q = q.Where("t.id > ?", p.After)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("ledger: list transactions: %w", err)
	}
	return txns, nil
}

// TrialBalanceLine is one account's balance in a trial balance.
type TrialBalanceLine struct {
	AccountID AccountID   `json:"account_id"`
	Indicator *string     `json:"indicator,omitempty"`
	Type      AccountType `json:"type,omitempty"`
	Currency  string      `json:"currency"`
	Balance   Amount      `json:"balance"`
}

// CurrencyTotal sums balances in one currency. In a trial balance it is
// always zero: every amount that left one account arrived in another.
type CurrencyTotal struct {
	Currency string `json:"currency"`
	Total    Amount `json:"total"`
}

type TrialBalance struct {
	LedgerID LedgerID           `json:"ledger_id"`
	At       time.Time          `json:"at"`
	Accounts []TrialBalanceLine `json:"accounts"`
	Totals   []CurrencyTotal    `json:"totals"`
}

// TrialBalance returns every account in the ledger with its posted balance
// as of at (holds are not included), and the total per currency.
func (s *Service) TrialBalance(ctx context.Context, ledgerID LedgerID, at time.Time) (*TrialBalance, error) {
	if err := parseID(prefixLedger, string(ledgerID)); err != nil {
		return nil, err
	}
	if _, err := s.GetLedger(ctx, ledgerID); err != nil {
		return nil, err
	}
	tb, err := trialBalance(ctx, s.db, ledgerID, at)
	return tb, classify(ctx, err)
}

func trialBalance(ctx context.Context, db bun.IDB, ledgerID LedgerID, at time.Time) (*TrialBalance, error) {
	lines := make([]TrialBalanceLine, 0)
	err := db.NewRaw(`
		SELECT a.id AS account_id, a.indicator, COALESCE(a.type, '') AS type, a.currency,
		       CASE WHEN a.untracked THEN
		           (SELECT COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)
		              FROM ledger.entries e WHERE e.account_id = a.id AND e.created_at <= ?0)
		       ELSE
		           COALESCE((SELECT e.balance_after FROM ledger.entries e
		                      WHERE e.account_id = a.id AND e.created_at <= ?0
		                      ORDER BY e.created_at DESC, e.account_version DESC LIMIT 1), 0)
		       END AS balance
		  FROM ledger.accounts a
		 WHERE a.ledger_id = ?1
		 ORDER BY a.currency, a.id`, at, ledgerID).Scan(ctx, &lines)
	if err != nil {
		return nil, fmt.Errorf("ledger: trial balance: %w", err)
	}
	tb := &TrialBalance{LedgerID: ledgerID, At: at.UTC(), Accounts: lines, Totals: []CurrencyTotal{}}
	for _, l := range lines {
		if n := len(tb.Totals); n == 0 || tb.Totals[n-1].Currency != l.Currency {
			tb.Totals = append(tb.Totals, CurrencyTotal{Currency: l.Currency, Total: NewAmount(0)})
		}
		t := &tb.Totals[len(tb.Totals)-1]
		t.Total = t.Total.Add(l.Balance)
	}
	return tb, nil
}

// StatementLine is one entry in an account statement.
type StatementLine struct {
	EntryID       EntryID       `json:"entry_id"`
	TransactionID TransactionID `json:"transaction_id"`
	Reference     string        `json:"reference,omitempty"`
	Description   string        `json:"description,omitempty"`
	Direction     Direction     `json:"direction"`
	Amount        Amount        `json:"amount"`
	// Balance is the running balance after this line.
	Balance   Amount    `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

type Statement struct {
	Account        *Account        `json:"account"`
	From           time.Time       `json:"from"`
	To             time.Time       `json:"to"`
	OpeningBalance Amount          `json:"opening_balance"`
	ClosingBalance Amount          `json:"closing_balance"`
	Lines          []StatementLine `json:"lines"`
}

// Statement returns an account's entries with from <= created_at < to, the
// balance before them and the running balance after each, like a bank
// statement. Periods with more than 100,000 entries are refused; split them.
func (s *Service) Statement(ctx context.Context, accountID AccountID, from, to time.Time) (*Statement, error) {
	st, err := s.statement(ctx, accountID, from, to)
	return st, classify(ctx, err)
}

func (s *Service) statement(ctx context.Context, accountID AccountID, from, to time.Time) (*Statement, error) {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, fmt.Errorf("%w: a statement needs from < to", ErrInvalidParams)
	}
	a, err := getAccountRow(ctx, s.db, accountID)
	if err != nil {
		return nil, err
	}
	if err := fillUntracked(ctx, s.db, a); err != nil {
		return nil, err
	}
	opening, err := s.balanceBefore(ctx, a, from)
	if err != nil {
		return nil, err
	}
	lines := make([]StatementLine, 0)
	err = s.db.NewRaw(`
		SELECT e.id AS entry_id, e.transaction_id, COALESCE(t.reference, '') AS reference,
		       COALESCE(t.description, '') AS description, e.direction, e.amount, e.created_at
		  FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id
		 WHERE e.account_id = ? AND e.created_at >= ? AND e.created_at < ?
		 ORDER BY e.created_at, e.account_version, e.id
		 LIMIT ?`, accountID, from, to, maxStatementLines+1).Scan(ctx, &lines)
	if err != nil {
		return nil, fmt.Errorf("ledger: statement: %w", err)
	}
	if len(lines) > maxStatementLines {
		return nil, fmt.Errorf("%w: more than %d entries in this period; request a shorter one", ErrInvalidParams, maxStatementLines)
	}
	running := opening
	for i := range lines {
		if lines[i].Direction == Credit {
			running = running.Add(lines[i].Amount)
		} else {
			running = running.Sub(lines[i].Amount)
		}
		lines[i].Balance = running
	}
	return &Statement{Account: a, From: from.UTC(), To: to.UTC(), OpeningBalance: opening, ClosingBalance: running, Lines: lines}, nil
}

// balanceBefore is the account's balance from entries strictly before t.
func (s *Service) balanceBefore(ctx context.Context, a *Account, t time.Time) (Amount, error) {
	q := s.db.NewSelect().Model((*Entry)(nil)).Where("e.account_id = ? AND e.created_at < ?", a.ID, t)
	if a.Untracked {
		q = q.ColumnExpr("COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)")
	} else {
		q = q.Column("e.balance_after").OrderExpr("e.created_at DESC, e.account_version DESC").Limit(1)
	}
	var b Amount
	err := q.Scan(ctx, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return NewAmount(0), nil
	}
	if err != nil {
		return Amount{}, fmt.Errorf("ledger: opening balance: %w", err)
	}
	if !b.isSet() {
		b = NewAmount(0)
	}
	return b, nil
}

// ReportLine is one account in a financial statement. Amount is shown the way
// accountants expect: positive in the account's normal direction (debit for
// assets and expenses, credit for the rest).
type ReportLine struct {
	AccountID AccountID `json:"account_id"`
	Indicator *string   `json:"indicator,omitempty"`
	Amount    Amount    `json:"amount"`
}

type ReportSection struct {
	Accounts []ReportLine `json:"accounts"`
	Total    Amount       `json:"total"`
}

func (r *ReportSection) add(l TrialBalanceLine, amount Amount) {
	r.Accounts = append(r.Accounts, ReportLine{AccountID: l.AccountID, Indicator: l.Indicator, Amount: amount})
	r.Total = r.Total.Add(amount)
}

func newSection() ReportSection { return ReportSection{Accounts: []ReportLine{}, Total: NewAmount(0)} }

// BalanceSheet groups one currency's accounts by type as of at. It always
// satisfies assets = liabilities + equity + net_income + unclassified, where
// net_income is revenue minus expenses so far and unclassified holds
// accounts without a type (at their credit-positive balance).
type BalanceSheet struct {
	LedgerID     LedgerID      `json:"ledger_id"`
	Currency     string        `json:"currency"`
	At           time.Time     `json:"at"`
	Assets       ReportSection `json:"assets"`
	Liabilities  ReportSection `json:"liabilities"`
	Equity       ReportSection `json:"equity"`
	NetIncome    Amount        `json:"net_income"`
	Unclassified ReportSection `json:"unclassified"`
}

func (s *Service) BalanceSheet(ctx context.Context, ledgerID LedgerID, currency string, at time.Time) (*BalanceSheet, error) {
	if err := validateCurrency(currency); err != nil {
		return nil, err
	}
	tb, err := s.TrialBalance(ctx, ledgerID, at)
	if err != nil {
		return nil, err
	}
	bs := &BalanceSheet{LedgerID: ledgerID, Currency: currency, At: tb.At,
		Assets: newSection(), Liabilities: newSection(), Equity: newSection(), Unclassified: newSection(), NetIncome: NewAmount(0)}
	for _, l := range tb.Accounts {
		if l.Currency != currency {
			continue
		}
		switch l.Type {
		case AccountAsset:
			bs.Assets.add(l, l.Balance.Neg())
		case AccountLiability:
			bs.Liabilities.add(l, l.Balance)
		case AccountEquity:
			bs.Equity.add(l, l.Balance)
		case AccountRevenue:
			bs.NetIncome = bs.NetIncome.Add(l.Balance)
		case AccountExpense:
			bs.NetIncome = bs.NetIncome.Add(l.Balance) // an expense's balance is negative
		default:
			bs.Unclassified.add(l, l.Balance)
		}
	}
	return bs, nil
}

// IncomeStatement shows one currency's revenue and expenses over a period:
// the change in each such account after from, up to and including to.
type IncomeStatement struct {
	LedgerID  LedgerID      `json:"ledger_id"`
	Currency  string        `json:"currency"`
	From      time.Time     `json:"from"`
	To        time.Time     `json:"to"`
	Revenue   ReportSection `json:"revenue"`
	Expenses  ReportSection `json:"expenses"`
	NetIncome Amount        `json:"net_income"`
}

func (s *Service) IncomeStatement(ctx context.Context, ledgerID LedgerID, currency string, from, to time.Time) (*IncomeStatement, error) {
	if err := validateCurrency(currency); err != nil {
		return nil, err
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("%w: an income statement needs from < to", ErrInvalidParams)
	}
	start, err := s.TrialBalance(ctx, ledgerID, from)
	if err != nil {
		return nil, err
	}
	end, err := s.TrialBalance(ctx, ledgerID, to)
	if err != nil {
		return nil, err
	}
	before := map[AccountID]Amount{}
	for _, l := range start.Accounts {
		before[l.AccountID] = l.Balance
	}
	is := &IncomeStatement{LedgerID: ledgerID, Currency: currency, From: start.At, To: end.At,
		Revenue: newSection(), Expenses: newSection()}
	for _, l := range end.Accounts {
		if l.Currency != currency {
			continue
		}
		change := l.Balance.Sub(before[l.AccountID])
		switch l.Type {
		case AccountRevenue:
			is.Revenue.add(l, change)
		case AccountExpense:
			is.Expenses.add(l, change.Neg())
		}
	}
	is.NetIncome = is.Revenue.Total.Sub(is.Expenses.Total)
	return is, nil
}

// PeriodClose records a closed accounting period and the trial balance it
// was closed with.
type PeriodClose struct {
	bun.BaseModel `bun:"table:ledger.period_closes,alias:pc"`

	LedgerID      LedgerID      `bun:",pk" json:"ledger_id"`
	ClosedThrough time.Time     `bun:",pk" json:"closed_through"`
	ClosedAt      time.Time     `bun:",notnull" json:"closed_at"`
	TrialBalance  *TrialBalance `bun:",type:jsonb,notnull" json:"trial_balance"`
}

// ClosePeriod closes a ledger's books through a moment: from then on nothing
// can be posted at or before it, which the database enforces. The trial
// balance at that moment is stored with the close. Periods can't be
// reopened; closing the same moment again returns the existing close.
// through must be at least a minute in the past.
func (s *Service) ClosePeriod(ctx context.Context, ledgerID LedgerID, through time.Time) (*PeriodClose, error) {
	if err := parseID(prefixLedger, string(ledgerID)); err != nil {
		return nil, err
	}
	through = through.UTC().Truncate(time.Microsecond)
	if through.IsZero() || through.After(now().Add(-closeLag)) {
		return nil, fmt.Errorf("%w: a period can only be closed through a moment at least %s ago", ErrInvalidParams, closeLag)
	}
	var pc *PeriodClose
	err := inTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		l := new(Ledger)
		if err := tx.NewSelect().Model(l).Where("l.id = ?", ledgerID).For("UPDATE").Scan(ctx); err != nil {
			return notFound(err, "ledger %s", ledgerID)
		}
		if l.ClosedThrough != nil && !through.After(*l.ClosedThrough) {
			pc = new(PeriodClose)
			err := tx.NewSelect().Model(pc).Where("pc.ledger_id = ? AND pc.closed_through = ?", ledgerID, through).Scan(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: ledger %s is already closed through %s", ErrPeriodClosed, ledgerID, l.ClosedThrough.Format(time.RFC3339Nano))
			}
			return err
		}
		tb, err := trialBalance(ctx, tx, ledgerID, through)
		if err != nil {
			return err
		}
		pc = &PeriodClose{LedgerID: ledgerID, ClosedThrough: through, ClosedAt: now(), TrialBalance: tb}
		if _, err := tx.NewInsert().Model(pc).Exec(ctx); err != nil {
			return fmt.Errorf("ledger: record period close: %w", err)
		}
		if _, err := tx.NewUpdate().Model(l).Set("closed_through = ?", through).WherePK().Exec(ctx); err != nil {
			return fmt.Errorf("ledger: close period: %w", err)
		}
		return emit(ctx, tx, EventPeriodClosed, &ledgerID, string(ledgerID), pc, pc.ClosedAt)
	})
	if err != nil {
		return nil, err
	}
	return pc, nil
}

// ListPeriodCloses returns a ledger's closed periods, oldest first.
func (s *Service) ListPeriodCloses(ctx context.Context, ledgerID LedgerID) ([]*PeriodClose, error) {
	if err := parseID(prefixLedger, string(ledgerID)); err != nil {
		return nil, err
	}
	closes := make([]*PeriodClose, 0)
	err := s.db.NewSelect().Model(&closes).Where("pc.ledger_id = ?", ledgerID).Order("pc.closed_through").Scan(ctx)
	if err != nil {
		return nil, classify(ctx, fmt.Errorf("ledger: list period closes: %w", err))
	}
	return closes, nil
}
