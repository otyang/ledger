package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/uptrace/bun"
)

var (
	currencyRE  = regexp.MustCompile(`^[A-Z][A-Z0-9]{2,9}$`)
	indicatorRE = regexp.MustCompile(`^@[A-Za-z0-9_.:-]+$`)
)

func validateCurrency(c string) error {
	if !currencyRE.MatchString(c) {
		return fmt.Errorf("%w: currency %q must be 3-10 uppercase letters or digits", ErrInvalidParams, c)
	}
	return nil
}

func validateIndicator(ind string) error {
	if !indicatorRE.MatchString(ind) {
		return fmt.Errorf("%w: indicator %q must look like @name", ErrInvalidParams, ind)
	}
	return nil
}

type CreateLedgerParams struct {
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (s *Service) CreateLedger(ctx context.Context, p CreateLedgerParams) (*Ledger, error) {
	if p.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidParams)
	}
	l := &Ledger{
		ID:        NewLedgerID(),
		Name:      p.Name,
		Metadata:  p.Metadata,
		CreatedAt: now(),
	}
	err := inTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(l).Exec(ctx); err != nil {
			return fmt.Errorf("ledger: create ledger: %w", err)
		}
		return emit(ctx, tx, EventLedgerCreated, &l.ID, string(l.ID), l, l.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Service) GetLedger(ctx context.Context, id LedgerID) (*Ledger, error) {
	if err := parseID(prefixLedger, string(id)); err != nil {
		return nil, err
	}
	l := new(Ledger)
	if err := s.db.NewSelect().Model(l).Where("l.id = ?", id).Scan(ctx); err != nil {
		return nil, classify(ctx, notFound(err, "ledger %s", id))
	}
	return l, nil
}

type CreateAccountParams struct {
	LedgerID LedgerID `json:"ledger_id"`
	Currency string   `json:"currency"`
	// Indicator optionally names the account, e.g. "@world". It is unique per
	// ledger and currency.
	Indicator     string `json:"indicator,omitempty"`
	AllowNegative bool   `json:"allow_negative"`
	// Untracked accounts take no lock when posted to and compute their
	// balance from entries on read. They require AllowNegative.
	Untracked bool `json:"untracked"`
	// OverdraftLimit (or OverdraftLimitValue, as a decimal) lets an account
	// without AllowNegative go this far below zero.
	OverdraftLimit      *Amount `json:"overdraft_limit,omitempty"`
	OverdraftLimitValue string  `json:"overdraft_limit_value,omitempty"`
	// Type classifies the account for financial statements. Optional; once
	// set it can't change.
	Type     AccountType    `json:"type,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func validateAccountType(t AccountType) error {
	switch t {
	case "", AccountAsset, AccountLiability, AccountEquity, AccountRevenue, AccountExpense:
		return nil
	}
	return fmt.Errorf("%w: type must be asset, liability, equity, revenue or expense, not %q", ErrInvalidParams, t)
}

// resolveLimit converts an overdraft limit given as an amount or a value.
// It returns nil when neither is set.
func (s *Service) resolveLimit(ctx context.Context, currency string, amount *Amount, value string) (*Amount, error) {
	if amount == nil && value == "" {
		return nil, nil
	}
	var a Amount
	if amount != nil {
		a = *amount
	}
	v, err := s.resolveAmount(ctx, s.db, currency, a, value)
	if err != nil {
		return nil, classify(ctx, err)
	}
	if v.Sign() < 0 || !v.inRange() {
		return nil, fmt.Errorf("%w: overdraft limit must be a positive amount", ErrInvalidParams)
	}
	return &v, nil
}

func (s *Service) CreateAccount(ctx context.Context, p CreateAccountParams) (*Account, error) {
	if err := parseID(prefixLedger, string(p.LedgerID)); err != nil {
		return nil, err
	}
	if err := validateCurrency(p.Currency); err != nil {
		return nil, err
	}
	var indicator *string
	if p.Indicator != "" {
		if err := validateIndicator(p.Indicator); err != nil {
			return nil, err
		}
		indicator = &p.Indicator
	}
	if p.Untracked && !p.AllowNegative {
		return nil, fmt.Errorf("%w: untracked accounts must allow negative balances", ErrInvalidParams)
	}
	if err := validateAccountType(p.Type); err != nil {
		return nil, err
	}
	limit, err := s.resolveLimit(ctx, p.Currency, p.OverdraftLimit, p.OverdraftLimitValue)
	if err != nil {
		return nil, err
	}
	if limit != nil && limit.IsZero() {
		limit = nil
	}
	if limit != nil && p.AllowNegative {
		return nil, fmt.Errorf("%w: an overdraft limit only applies to accounts without allow_negative", ErrInvalidParams)
	}

	at := now()
	a := &Account{
		ID:             NewAccountID(),
		LedgerID:       p.LedgerID,
		Indicator:      indicator,
		Currency:       p.Currency,
		AllowNegative:  p.AllowNegative,
		OverdraftLimit: limit,
		Untracked:      p.Untracked,
		Status:         AccountActive,
		Type:           p.Type,
		Metadata:       p.Metadata,
		CreatedAt:      at,
		UpdatedAt:      at,
	}
	err = inTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
			return err
		}
		return emit(ctx, tx, EventAccountCreated, &a.LedgerID, string(a.ID), a, at)
	})
	if err != nil {
		switch {
		case pgErrorIs(err, pgForeignKeyViolation, "accounts_currency_fkey"):
			return nil, fmt.Errorf("%w: %s; register it with RegisterCurrency", ErrUnknownCurrency, p.Currency)
		case pgErrorIs(err, pgForeignKeyViolation, ""):
			return nil, fmt.Errorf("%w: ledger %s", ErrNotFound, p.LedgerID)
		case pgErrorIs(err, pgUniqueViolation, "accounts_indicator_key"):
			return nil, fmt.Errorf("%w: account %s %s in ledger %s", ErrAlreadyExists, p.Indicator, p.Currency, p.LedgerID)
		}
		return nil, fmt.Errorf("ledger: create account: %w", err)
	}
	return a, nil
}

// UpdateAccountParams changes an account. Nil fields are left unchanged.
type UpdateAccountParams struct {
	// Status freezes (no money leaves), unfreezes or closes the account.
	// Only an empty account (zero balance, nothing pending) can be closed,
	// and closing is final.
	Status *AccountStatus `json:"status,omitempty"`
	// StatusReason records why, e.g. "fraud review". It is replaced on every
	// status change, and cleared when one is made without a reason.
	StatusReason string `json:"status_reason,omitempty"`
	// OverdraftLimit (or OverdraftLimitValue) sets the overdraft limit; 0
	// removes it. It can't be lowered below what the account already owes.
	OverdraftLimit      *Amount `json:"overdraft_limit,omitempty"`
	OverdraftLimitValue string  `json:"overdraft_limit_value,omitempty"`
	// Type sets the account's type, if it has none yet.
	Type *AccountType `json:"type,omitempty"`
	// Metadata is merged into the account's metadata; keys set to null are
	// removed.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// UpdateAccount applies p to an account and returns it. The account is locked
// FOR UPDATE while it changes. That conflicts with postings holding it (or,
// for untracked accounts, the key-share lock their entries take), so a status
// change waits for postings already in flight, and later ones see the new
// status.
func (s *Service) UpdateAccount(ctx context.Context, id AccountID, p UpdateAccountParams) (*Account, error) {
	if err := parseID(prefixAccount, string(id)); err != nil {
		return nil, err
	}
	if p.Status != nil {
		switch *p.Status {
		case AccountActive, AccountFrozen, AccountClosed:
		default:
			return nil, fmt.Errorf("%w: status must be active, frozen or closed, not %q", ErrInvalidParams, *p.Status)
		}
	}
	if p.Type != nil {
		if err := validateAccountType(*p.Type); err != nil {
			return nil, err
		}
	}
	var a *Account
	err := inTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		a = new(Account)
		if err := tx.NewSelect().Model(a).Where("a.id = ?", id).For("UPDATE").Scan(ctx); err != nil {
			return notFound(err, "account %s", id)
		}
		if p.OverdraftLimit != nil || p.OverdraftLimitValue != "" {
			limit, err := s.resolveLimit(ctx, a.Currency, p.OverdraftLimit, p.OverdraftLimitValue)
			if err != nil {
				return err
			}
			if limit.IsZero() {
				limit = nil
			}
			if limit != nil && a.AllowNegative {
				return fmt.Errorf("%w: an overdraft limit only applies to accounts without allow_negative", ErrInvalidParams)
			}
			a.OverdraftLimit = limit
			if !a.AllowNegative && a.Available().Cmp(a.floor()) < 0 {
				return fmt.Errorf("%w: account %s already owes more than that limit", ErrInvalidParams, id)
			}
		}
		if p.Type != nil {
			if a.Type != "" && a.Type != *p.Type {
				return fmt.Errorf("%w: account %s already has type %s", ErrInvalidParams, id, a.Type)
			}
			a.Type = *p.Type
		}
		if p.Status != nil && *p.Status != a.Status {
			if a.Status == AccountClosed {
				return fmt.Errorf("%w: account %s", ErrAccountClosed, id)
			}
			if *p.Status == AccountClosed {
				if err := fillUntracked(ctx, tx, a); err != nil {
					return err
				}
				if !a.Balance.IsZero() || !a.PendingDebit.IsZero() || !a.PendingCredit.IsZero() {
					return fmt.Errorf("%w: account %s must have a zero balance and nothing pending to close", ErrInvalidParams, id)
				}
			}
			a.Status, a.StatusReason = *p.Status, p.StatusReason
		}
		a.Metadata = mergeMetadata(a.Metadata, p.Metadata)
		a.UpdatedAt = later(now(), a.UpdatedAt)
		_, err := tx.NewUpdate().Model(a).
			Column("status", "status_reason", "overdraft_limit", "type", "metadata", "updated_at").
			WherePK().Exec(ctx)
		if err != nil {
			return fmt.Errorf("ledger: update account %s: %w", id, err)
		}
		if err := fillUntracked(ctx, tx, a); err != nil {
			return err
		}
		return emit(ctx, tx, EventAccountUpdated, &a.LedgerID, string(a.ID), a, a.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// mergeMetadata returns base with patch applied: keys set to nil are removed.
func mergeMetadata(base, patch map[string]any) map[string]any {
	if len(patch) == 0 {
		return base
	}
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EnsureIndicatorAccount returns the account with p's indicator in p's ledger
// and currency, creating it from p if it doesn't exist yet. An existing
// account is returned as is. System accounts such as "@world", which
// represent money entering or leaving the ledger, typically set
// AllowNegative and Untracked.
func (s *Service) EnsureIndicatorAccount(ctx context.Context, p CreateAccountParams) (*Account, error) {
	if p.Indicator == "" {
		return nil, fmt.Errorf("%w: indicator is required", ErrInvalidParams)
	}
	a, err := s.GetAccountByIndicator(ctx, p.LedgerID, p.Indicator, p.Currency)
	if !errors.Is(err, ErrNotFound) {
		return a, err
	}
	a, err = s.CreateAccount(ctx, p)
	if errors.Is(err, ErrAlreadyExists) {
		// Lost a creation race; the winner's row is committed now.
		return s.GetAccountByIndicator(ctx, p.LedgerID, p.Indicator, p.Currency)
	}
	return a, err
}

// GetAccount returns the account with its current totals. For an untracked
// account the totals are computed from all of its entries.
func (s *Service) GetAccount(ctx context.Context, id AccountID) (*Account, error) {
	return s.GetAccountTx(ctx, s.db, id)
}

// GetAccountTx reads through db, so inside a caller's transaction it sees that
// transaction's own uncommitted postings.
func (s *Service) GetAccountTx(ctx context.Context, db bun.IDB, id AccountID) (*Account, error) {
	a, err := getAccountRow(ctx, db, id)
	if err == nil {
		err = fillUntracked(ctx, db, a)
	}
	if err != nil {
		return nil, classify(ctx, err)
	}
	return a, nil
}

// getAccountRow loads the stored row without computing untracked totals.
func getAccountRow(ctx context.Context, db bun.IDB, id AccountID) (*Account, error) {
	if err := parseID(prefixAccount, string(id)); err != nil {
		return nil, err
	}
	a := new(Account)
	if err := db.NewSelect().Model(a).Where("a.id = ?", id).Scan(ctx); err != nil {
		return nil, notFound(err, "account %s", id)
	}
	return a, nil
}

func (s *Service) GetAccountByIndicator(ctx context.Context, ledgerID LedgerID, indicator, currency string) (*Account, error) {
	a := new(Account)
	err := s.db.NewSelect().Model(a).
		Where("a.ledger_id = ?", ledgerID).
		Where("a.indicator = ?", indicator).
		Where("a.currency = ?", currency).
		Scan(ctx)
	if err == nil {
		err = fillUntracked(ctx, s.db, a)
	}
	if err != nil {
		return nil, classify(ctx, notFound(err, "account %s %s in ledger %s", indicator, currency, ledgerID))
	}
	return a, nil
}

// fillUntracked computes an untracked account's totals from its entries:
// those since its latest snapshot (see RefreshSnapshots), or all of them.
func fillUntracked(ctx context.Context, db bun.IDB, a *Account) error {
	if !a.Untracked {
		return nil
	}
	// Start from the account's snapshot, if any, and add the entries written
	// since. One statement, so both are read from the same snapshot.
	err := db.NewRaw(`
		SELECT COALESCE(s.credit_total, 0) + COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'credit'), 0),
		       COALESCE(s.debit_total, 0) + COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'debit'), 0),
		       COALESCE(s.entry_count, 0) + count(e.id)
		  FROM (SELECT ?::text AS id) acc
		  LEFT JOIN ledger.account_snapshots s ON s.account_id = acc.id
		  LEFT JOIN ledger.entries e ON e.account_id = acc.id AND e.txid >= COALESCE(s.txid_bound, 0)
		 GROUP BY s.credit_total, s.debit_total, s.entry_count`, a.ID).
		Scan(ctx, &a.CreditTotal, &a.DebitTotal, &a.Version)
	if err != nil {
		return fmt.Errorf("ledger: compute untracked balance: %w", err)
	}
	a.Balance = a.CreditTotal.Sub(a.DebitTotal)
	return nil
}

// notFound maps sql.ErrNoRows to ErrNotFound and passes other errors through.
func notFound(err error, format string, args ...any) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: "+format, append([]any{ErrNotFound}, args...)...)
	}
	return err
}
