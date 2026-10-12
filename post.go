package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

const maxReferenceLen = 255

// Leg moves an amount into or out of one account.
type Leg struct {
	AccountID AccountID `json:"account_id"`
	Direction Direction `json:"direction"`
	// Amount is in minor units: 540323 for 5403.23 USD.
	Amount Amount `json:"amount"`
	// Value is the same amount as a decimal, "5403.23", converted using the
	// currency's registered decimals. Set Amount or Value; if both are set
	// they must agree. The ledger stores and returns only Amount.
	Value string `json:"value,omitempty"`
}

type PostParams struct {
	// Reference is the idempotency key. Posting the same reference again
	// returns the original transaction; reusing it with different currency or
	// legs fails with ErrReferenceConflict.
	Reference   string         `json:"reference"`
	Currency    string         `json:"currency"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	// Legs must contain at least two entries whose debits equal their credits.
	Legs []Leg `json:"legs"`
}

// Transfer is shorthand for the common two-leg posting: debit from, credit to.
func Transfer(reference, currency string, from, to AccountID, amount Amount) PostParams {
	return PostParams{
		Reference: reference,
		Currency:  currency,
		Legs:      transferLegs(from, to, amount),
	}
}

func transferLegs(from, to AccountID, amount Amount) []Leg {
	return []Leg{
		{AccountID: from, Direction: Debit, Amount: amount},
		{AccountID: to, Direction: Credit, Amount: amount},
	}
}

// Post records a balanced transaction in its own database transaction.
func (s *Service) Post(ctx context.Context, p PostParams) (*Transaction, error) {
	return s.PostTx(ctx, s.db, p)
}

// PostTx records a balanced transaction using db, which may be the caller's
// bun.Tx. That lets the caller commit ledger postings atomically with its own
// writes. Inside a caller's transaction the work runs in a savepoint, so a
// failed posting does not abort the caller's transaction.
func (s *Service) PostTx(ctx context.Context, db bun.IDB, p PostParams) (*Transaction, error) {
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
	hash := requestHash("post", p.Currency, legsKey(p.Legs))

	find := func(ctx context.Context, db bun.IDB) (*Transaction, error) {
		// References are unique per ledger; the ledger is the first account's.
		txn, err := getTransaction(ctx, db,
			"t.reference = ? AND t.ledger_id = (SELECT ledger_id FROM ledger.accounts WHERE id = ?)",
			p.Reference, p.Legs[0].AccountID)
		return matchRequest(txn, err, hash, fmt.Errorf("%w: %q", ErrReferenceConflict, p.Reference))
	}
	return once(ctx, db, find, func(ctx context.Context, tx bun.Tx) (*Transaction, error) {
		ps, err := lockAccounts(ctx, tx, accountIDs(p.Legs), "")
		if err != nil {
			return nil, err
		}
		txn := &Transaction{
			ID:          NewTransactionID(),
			LedgerID:    ps.ledgerID,
			Reference:   p.Reference,
			Status:      StatusPosted,
			Currency:    p.Currency,
			Description: p.Description,
			Metadata:    p.Metadata,
			RequestHash: hash,
			CreatedAt:   ps.at,
		}
		if err := ps.post(txn, p.Legs); err != nil {
			return nil, err
		}
		return txn, ps.save(ctx, tx, txn)
	})
}

// matchRequest returns txn if it was created by the same request (same hash),
// and conflict otherwise.
func matchRequest(txn *Transaction, err error, hash string, conflict error) (*Transaction, error) {
	if err != nil {
		return nil, err
	}
	if txn.RequestHash != hash {
		return nil, conflict
	}
	return txn, nil
}

// posting is an in-progress change to a set of accounts within one database
// transaction. Tracked accounts are locked and written back by save; untracked
// ones are only read.
type posting struct {
	// at is the timestamp for everything this posting writes. It is taken
	// after the locks and is never earlier than any locked account's last
	// update, so entry timestamps never go backwards within an account, even
	// across servers with skewed clocks.
	at       time.Time
	ledgerID LedgerID
	accounts map[AccountID]*Account
	locked   []*Account // tracked accounts in lock order
}

// lockAccounts loads the accounts, locking tracked ones in a fixed (sorted)
// order so concurrent postings cannot deadlock. NO KEY UPDATE is the lock an
// UPDATE of non-key columns takes; unlike FOR UPDATE it doesn't block
// foreign-key checks, so inserting entries for an untracked account never
// waits on anything.
//
// All accounts must belong to one ledger: ledgerID if it is set, otherwise
// the same ledger as each other.
func lockAccounts(ctx context.Context, tx bun.Tx, ids []AccountID, ledgerID LedgerID) (*posting, error) {
	ps := &posting{ledgerID: ledgerID, accounts: make(map[AccountID]*Account, len(ids))}
	err := tx.NewSelect().Model(&ps.locked).
		Where("a.id IN (?)", bun.In(ids)).
		Where("NOT a.untracked").
		OrderExpr("a.id").
		For("NO KEY UPDATE").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	for _, a := range ps.locked {
		ps.accounts[a.ID] = a
	}

	if len(ps.accounts) < len(ids) {
		var untracked []*Account
		err := tx.NewSelect().Model(&untracked).
			Where("a.id IN (?)", bun.In(ids)).
			Where("a.untracked").
			Scan(ctx)
		if err != nil {
			return nil, fmt.Errorf("ledger: load accounts: %w", err)
		}
		for _, a := range untracked {
			ps.accounts[a.ID] = a
		}
	}
	for _, id := range ids {
		a := ps.accounts[id]
		if a == nil {
			return nil, fmt.Errorf("%w: account %s", ErrNotFound, id)
		}
		if ps.ledgerID == "" {
			ps.ledgerID = a.LedgerID
		}
		if a.LedgerID != ps.ledgerID {
			return nil, fmt.Errorf("%w: account %s is in ledger %s, not %s", ErrLedgerMismatch, a.ID, a.LedgerID, ps.ledgerID)
		}
	}

	ps.at = now()
	for _, a := range ps.locked {
		if a.UpdatedAt.After(ps.at) {
			ps.at = a.UpdatedAt
		}
	}
	return ps, nil
}

func (ps *posting) checkCurrency(currency string, legs []Leg) error {
	for _, leg := range legs {
		if a := ps.accounts[leg.AccountID]; a.Currency != currency {
			return fmt.Errorf("%w: account %s is %s, transaction is %s", ErrCurrencyMismatch, a.ID, a.Currency, currency)
		}
	}
	return nil
}

// checkStatus refuses legs that a frozen account (debits) or a closed account
// (anything) may not take.
func (ps *posting) checkStatus(legs []Leg) error {
	for _, leg := range legs {
		switch a := ps.accounts[leg.AccountID]; {
		case a.Status == AccountClosed:
			return fmt.Errorf("%w: account %s", ErrAccountClosed, a.ID)
		case a.Status == AccountFrozen && leg.Direction == Debit:
			return fmt.Errorf("%w: account %s can't send money", ErrAccountFrozen, a.ID)
		}
	}
	return nil
}

// post applies legs to the accounts in memory, fills txn.Entries, and checks
// funds on the net effect, so an account may be debited and credited within one
// transaction.
func (ps *posting) post(txn *Transaction, legs []Leg) error {
	if err := ps.apply(txn, legs); err != nil {
		return err
	}
	return ps.checkFunds()
}

// apply is post without the funds check, so a batch can apply several
// transactions and check funds once, on their combined effect.
func (ps *posting) apply(txn *Transaction, legs []Leg) error {
	if err := ps.checkCurrency(txn.Currency, legs); err != nil {
		return err
	}
	if err := ps.checkStatus(legs); err != nil {
		return err
	}
	txn.Entries = make([]*Entry, 0, len(legs))
	for _, leg := range legs {
		a := ps.accounts[leg.AccountID]
		e := &Entry{
			ID:            NewEntryID(),
			TransactionID: txn.ID,
			AccountID:     a.ID,
			Direction:     leg.Direction,
			Amount:        leg.Amount,
			CreatedAt:     ps.at,
		}
		if !a.Untracked {
			if err := a.apply(leg.Direction, leg.Amount, ps.at); err != nil {
				return err
			}
			e.BalanceAfter, e.AccountVersion = ptr(a.Balance), ptr(a.Version)
		}
		txn.Entries = append(txn.Entries, e)
	}
	return nil
}

// reserve adds (sign 1) or releases (sign -1) the legs as pending amounts on
// tracked accounts. Untracked accounts don't track pending funds. Releasing
// is always allowed; reserving checks account status like a posting would.
func (ps *posting) reserve(legs []Leg, sign int64) error {
	if sign > 0 {
		if err := ps.checkStatus(legs); err != nil {
			return err
		}
	}
	for _, leg := range legs {
		a := ps.accounts[leg.AccountID]
		if a.Untracked {
			continue
		}
		pending := &a.PendingCredit
		if leg.Direction == Debit {
			pending = &a.PendingDebit
		}
		delta := leg.Amount
		if sign < 0 {
			delta = delta.Neg()
		}
		v := pending.Add(delta)
		if !v.inRange() {
			return fmt.Errorf("%w: pending amount on account %s", ErrAmountOverflow, a.ID)
		}
		if v.Sign() < 0 {
			return fmt.Errorf("ledger: releasing %s %s from account %s exceeds its pending amount", leg.Amount, leg.Direction, a.ID)
		}
		*pending = v
		a.UpdatedAt = ps.at
	}
	return ps.checkFunds()
}

func (ps *posting) checkFunds() error {
	for _, a := range ps.locked {
		if !a.AllowNegative && a.Available().Cmp(a.floor()) < 0 {
			return fmt.Errorf("%w: account %s", ErrInsufficientFunds, a.ID)
		}
	}
	return nil
}

// save inserts txn and its entries (if txn is non-nil), writes back the
// locked accounts, and records a transaction.posted event.
func (ps *posting) save(ctx context.Context, tx bun.Tx, txn *Transaction) error {
	if txn != nil {
		if err := insertTransaction(ctx, tx, txn); err != nil {
			return err
		}
	}
	return ps.saveAccounts(ctx, tx)
}

func insertTransaction(ctx context.Context, tx bun.Tx, txn *Transaction) error {
	if _, err := tx.NewInsert().Model(txn).Exec(ctx); err != nil {
		return fmt.Errorf("ledger: insert transaction: %w", err)
	}
	if _, err := tx.NewInsert().Model(&txn.Entries).Exec(ctx); err != nil {
		return mapTriggerError(fmt.Errorf("ledger: insert entries: %w", err))
	}
	return emit(ctx, tx, EventTransactionPosted, &txn.LedgerID, string(txn.ID), txn, txn.CreatedAt)
}

func (ps *posting) saveAccounts(ctx context.Context, tx bun.Tx) error {
	for _, a := range ps.locked {
		_, err := tx.NewUpdate().Model(a).
			Column("credit_total", "debit_total", "balance", "pending_debit", "pending_credit", "version", "updated_at").
			WherePK().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("ledger: update account %s: %w", a.ID, err)
		}
	}
	return nil
}

// apply adds one leg to the account in memory.
func (a *Account) apply(d Direction, amount Amount, at time.Time) error {
	total := &a.CreditTotal
	if d == Debit {
		total = &a.DebitTotal
	}
	v := total.Add(amount)
	if !v.inRange() {
		return fmt.Errorf("%w: account %s totals", ErrAmountOverflow, a.ID)
	}
	*total = v
	// Both totals are in [0, 10^78), so the balance fits its column too.
	a.Balance = a.CreditTotal.Sub(a.DebitTotal)
	a.Version++
	a.UpdatedAt = at
	return nil
}

func validateReference(ref string) error {
	if ref == "" || len(ref) > maxReferenceLen {
		return fmt.Errorf("%w: reference must be 1-%d characters", ErrInvalidParams, maxReferenceLen)
	}
	return nil
}

// validateLegs checks that legs are well formed and balanced.
func validateLegs(legs []Leg) error {
	if len(legs) < 2 {
		return fmt.Errorf("%w: need at least 2 legs", ErrInvalidParams)
	}
	var debits, credits Amount
	for i, leg := range legs {
		if err := parseID(prefixAccount, string(leg.AccountID)); err != nil {
			return fmt.Errorf("leg %d: %w", i, err)
		}
		if leg.Amount.Sign() <= 0 {
			return fmt.Errorf("%w: leg %d: amount must be positive", ErrInvalidParams, i)
		}
		if !leg.Amount.inRange() {
			return fmt.Errorf("%w: leg %d: amount has more than %d digits", ErrAmountOverflow, i, maxAmountDigits)
		}
		switch leg.Direction {
		case Debit:
			debits = debits.Add(leg.Amount)
		case Credit:
			credits = credits.Add(leg.Amount)
		default:
			return fmt.Errorf("%w: leg %d: direction must be %q or %q", ErrInvalidParams, i, Debit, Credit)
		}
	}
	if debits.Cmp(credits) != 0 {
		return fmt.Errorf("%w: debits %s, credits %s", ErrUnbalanced, debits, credits)
	}
	if len(accountIDs(legs)) < 2 {
		return fmt.Errorf("%w: legs must involve at least 2 accounts", ErrInvalidParams)
	}
	return nil
}

// accountIDs returns the distinct account IDs in sorted order (the lock order).
func accountIDs(legs []Leg) []AccountID {
	ids := make([]AccountID, 0, len(legs))
	for _, leg := range legs {
		ids = append(ids, leg.AccountID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// requestHash fingerprints a request, so a retry with the same reference can
// be told apart from a conflicting reuse.
func requestHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// legsKey is a canonical form of legs: the same movements in a different
// order produce the same key.
func legsKey(legs []Leg) string {
	legs = aggregate(legs)
	slices.SortFunc(legs, func(a, b Leg) int {
		return cmp.Or(strings.Compare(string(a.AccountID), string(b.AccountID)), strings.Compare(string(a.Direction), string(b.Direction)))
	})
	var b []byte
	for _, leg := range legs {
		b = append(b, leg.AccountID...)
		b = append(b, '|')
		b = append(b, leg.Direction...)
		b = append(b, '|')
		b = append(b, leg.Amount.String()...)
		b = append(b, ';')
	}
	return string(b)
}

func ptr[T any](v T) *T { return &v }
