package ledger

import (
	"time"

	"github.com/uptrace/bun"
)

// Direction is the side of an entry. A credit increases an account's balance
// and a debit decreases it (balance = credit_total - debit_total).
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

func (d Direction) opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

type TransactionStatus string

const StatusPosted TransactionStatus = "posted"

type HoldStatus string

const (
	HoldPending   HoldStatus = "pending"
	HoldCommitted HoldStatus = "committed"
	HoldVoided    HoldStatus = "voided"
	// HoldExpired holds passed their ExpiresAt and were released.
	HoldExpired HoldStatus = "expired"
)

// AccountStatus controls what may move through an account.
type AccountStatus string

const (
	AccountActive AccountStatus = "active"
	// AccountFrozen accounts can receive money but not send it: debits and
	// holds that debit them are refused.
	AccountFrozen AccountStatus = "frozen"
	// AccountClosed accounts are empty and accept nothing. Closing is final.
	AccountClosed AccountStatus = "closed"
)

// AccountType classifies an account for financial statements.
type AccountType string

const (
	AccountAsset     AccountType = "asset"
	AccountLiability AccountType = "liability"
	AccountEquity    AccountType = "equity"
	AccountRevenue   AccountType = "revenue"
	AccountExpense   AccountType = "expense"
)

type Ledger struct {
	bun.BaseModel `bun:"table:ledger.ledgers,alias:l"`

	ID       LedgerID       `bun:",pk" json:"id"`
	Name     string         `bun:",notnull" json:"name"`
	Metadata map[string]any `bun:",type:jsonb,nullzero" json:"metadata,omitempty"`
	// ClosedThrough is the end of the last closed period: nothing can be
	// posted at or before it.
	ClosedThrough *time.Time `bun:",nullzero" json:"closed_through,omitempty"`
	CreatedAt     time.Time  `bun:",notnull" json:"created_at"`
}

// Account holds a running balance in a single currency. Amounts are whole
// numbers of the currency's minor unit (e.g. cents, or wei for ETH).
//
// Funds reserved by pending holds are in PendingDebit and PendingCredit; they
// are not part of Balance until the hold is committed. Version counts the
// entries posted to the account.
//
// An Untracked account keeps no cached totals and takes no lock when posted
// to, so it never becomes a bottleneck. Reads compute its totals from entries.
// It suits high-traffic system accounts such as "@world", and must allow
// negative balances since its funds cannot be checked.
type Account struct {
	bun.BaseModel `bun:"table:ledger.accounts,alias:a"`

	ID            AccountID `bun:",pk" json:"id"`
	LedgerID      LedgerID  `bun:",notnull" json:"ledger_id"`
	Indicator     *string   `bun:",nullzero" json:"indicator,omitempty"`
	Currency      string    `bun:",notnull" json:"currency"`
	AllowNegative bool      `bun:",notnull" json:"allow_negative"`
	// OverdraftLimit lets an account without AllowNegative go this far below
	// zero. Nil means no overdraft.
	OverdraftLimit *Amount        `json:"overdraft_limit,omitempty"`
	Untracked      bool           `bun:",notnull" json:"untracked"`
	Status         AccountStatus  `bun:",notnull,default:'active'" json:"status"`
	StatusReason   string         `bun:",nullzero" json:"status_reason,omitempty"`
	Type           AccountType    `bun:",nullzero" json:"type,omitempty"`
	CreditTotal    Amount         `bun:",notnull" json:"credit_total"`
	DebitTotal     Amount         `bun:",notnull" json:"debit_total"`
	Balance        Amount         `bun:",notnull" json:"balance"`
	PendingDebit   Amount         `bun:",notnull" json:"pending_debit"`
	PendingCredit  Amount         `bun:",notnull" json:"pending_credit"`
	Version        int64          `bun:",notnull" json:"version"`
	Metadata       map[string]any `bun:",type:jsonb,nullzero" json:"metadata,omitempty"`
	CreatedAt      time.Time      `bun:",notnull" json:"created_at"`
	UpdatedAt      time.Time      `bun:",notnull" json:"updated_at"`
}

// Available is the balance that can still be spent: Balance minus funds
// reserved by pending holds. With an overdraft limit, the account can spend
// down to minus that limit.
func (a *Account) Available() Amount {
	return a.Balance.Sub(a.PendingDebit)
}

// floor is the lowest Available may go: 0, or minus the overdraft limit.
func (a *Account) floor() Amount {
	if a.OverdraftLimit != nil {
		return a.OverdraftLimit.Neg()
	}
	return NewAmount(0)
}

type Transaction struct {
	bun.BaseModel `bun:"table:ledger.transactions,alias:t"`

	ID       TransactionID `bun:",pk" json:"id"`
	LedgerID LedgerID      `bun:",notnull" json:"ledger_id"`
	// Reference is the idempotency key, unique per ledger. It is empty on
	// the transaction that commits a hold, which is identified by HoldID.
	Reference   string            `bun:",nullzero" json:"reference,omitempty"`
	Status      TransactionStatus `bun:",notnull" json:"status"`
	Currency    string            `bun:",notnull" json:"currency"`
	Description string            `bun:",nullzero" json:"description,omitempty"`
	Metadata    map[string]any    `bun:",type:jsonb,nullzero" json:"metadata,omitempty"`
	// RefundOf is set on refunds and points at the refunded transaction.
	RefundOf *TransactionID `bun:",nullzero" json:"refund_of,omitempty"`
	// HoldID is set on the transaction that committed a hold.
	HoldID *HoldID `bun:",nullzero" json:"hold_id,omitempty"`
	// BatchID is set on transactions posted as part of a batch or exchange.
	BatchID *BatchID `bun:",nullzero" json:"batch_id,omitempty"`
	// ReversalReason is set on reversals: refunds of everything that was left,
	// made to correct a mistake.
	ReversalReason string    `bun:",nullzero" json:"reversal_reason,omitempty"`
	RequestHash    string    `bun:",notnull" json:"-"`
	CreatedAt      time.Time `bun:",notnull" json:"created_at"`

	Entries []*Entry `bun:"rel:has-many,join:id=transaction_id" json:"entries,omitempty"`
}

// Entry is one immutable leg of a transaction against one account.
//
// For tracked accounts, AccountVersion is the account's version after this
// entry was applied: gap-free per account, it gives a total order for the
// account's history, and BalanceAfter is the balance at that point. Both are
// nil for entries on untracked accounts.
type Entry struct {
	bun.BaseModel `bun:"table:ledger.entries,alias:e"`

	ID             EntryID       `bun:",pk" json:"id"`
	TransactionID  TransactionID `bun:",notnull" json:"transaction_id"`
	AccountID      AccountID     `bun:",notnull" json:"account_id"`
	Direction      Direction     `bun:",notnull" json:"direction"`
	Amount         Amount        `bun:",notnull" json:"amount"`
	BalanceAfter   *Amount       `json:"balance_after,omitempty"`
	AccountVersion *int64        `json:"account_version,omitempty"`
	CreatedAt      time.Time     `bun:",notnull" json:"created_at"`
}

// Hold reserves funds without moving them. It stays pending until it is
// committed (posting a transaction for all or part of it), voided (releasing
// the funds), or, if it has an ExpiresAt, expired (released by
// ExpireHolds). Holds without ExpiresAt never expire.
type Hold struct {
	bun.BaseModel `bun:"table:ledger.holds,alias:h"`

	ID          HoldID         `bun:",pk" json:"id"`
	LedgerID    LedgerID       `bun:",notnull" json:"ledger_id"`
	Reference   string         `bun:",notnull" json:"reference"`
	Status      HoldStatus     `bun:",notnull" json:"status"`
	Currency    string         `bun:",notnull" json:"currency"`
	Description string         `bun:",nullzero" json:"description,omitempty"`
	Metadata    map[string]any `bun:",type:jsonb,nullzero" json:"metadata,omitempty"`
	Legs        []Leg          `bun:",type:jsonb,notnull" json:"legs"`
	RequestHash string         `bun:",notnull" json:"-"`
	// TransactionID is the transaction that committed the hold.
	TransactionID *TransactionID `bun:",nullzero" json:"transaction_id,omitempty"`
	ExpiresAt     *time.Time     `bun:",nullzero" json:"expires_at,omitempty"`
	CreatedAt     time.Time      `bun:",notnull" json:"created_at"`
	ResolvedAt    *time.Time     `bun:",nullzero" json:"resolved_at,omitempty"`
}

// Batch groups transactions posted together, all or nothing. An exchange is a
// batch of two transactions in different currencies with the rate recorded.
type Batch struct {
	bun.BaseModel `bun:"table:ledger.batches,alias:b"`

	ID          BatchID        `bun:",pk" json:"id"`
	LedgerID    LedgerID       `bun:",notnull" json:"ledger_id"`
	Reference   string         `bun:",notnull" json:"reference"`
	Kind        BatchKind      `bun:",notnull" json:"kind"`
	Rate        string         `bun:",nullzero" json:"rate,omitempty"`
	Description string         `bun:",nullzero" json:"description,omitempty"`
	Metadata    map[string]any `bun:",type:jsonb,nullzero" json:"metadata,omitempty"`
	RequestHash string         `bun:",notnull" json:"-"`
	CreatedAt   time.Time      `bun:",notnull" json:"created_at"`

	Transactions []*Transaction `bun:"rel:has-many,join:id=batch_id" json:"transactions"`
}

type BatchKind string

const (
	BatchPlain    BatchKind = "batch"
	BatchExchange BatchKind = "exchange"
)
