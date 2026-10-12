package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/otyang/ledger"
)

// Response bodies add a decimal "*_value" field next to every amount, e.g.
// "balance": "540323" and "balance_value": "5403.23" for USD. The value fields
// are left out for a currency whose decimals aren't registered yet, and, with
// a logged warning, if the currency can't be read (e.g. a timeout).

type accountView struct {
	*ledger.Account
	CreditTotalValue   string `json:"credit_total_value,omitempty"`
	DebitTotalValue    string `json:"debit_total_value,omitempty"`
	BalanceValue       string `json:"balance_value,omitempty"`
	PendingDebitValue  string `json:"pending_debit_value,omitempty"`
	PendingCreditValue string `json:"pending_credit_value,omitempty"`
	// OverdraftLimitValue is set when the account has an overdraft limit.
	OverdraftLimitValue string `json:"overdraft_limit_value,omitempty"`
}

type entryView struct {
	*ledger.Entry
	Value             string `json:"value,omitempty"`
	BalanceAfterValue string `json:"balance_after_value,omitempty"`
}

type transactionView struct {
	*ledger.Transaction
	Entries []entryView `json:"entries,omitempty"`
}

type holdView struct {
	*ledger.Hold
	Legs []ledger.Leg `json:"legs"`
}

// views builds response bodies for one request. It looks up each currency at
// most once.
type views struct {
	ctx        context.Context
	svc        *ledger.Service
	log        *slog.Logger
	currencies map[string]*ledger.Currency
}

func (h *handler) views(ctx context.Context) *views {
	return &views{ctx: ctx, svc: h.svc, log: h.cfg.Logger, currencies: map[string]*ledger.Currency{}}
}

// format returns a as a decimal in currency, or "" if that isn't known.
func (v *views) format(currency string, a ledger.Amount) string {
	if currency == "" {
		return ""
	}
	c, ok := v.currencies[currency]
	if !ok {
		var err error
		c, err = v.svc.GetCurrency(v.ctx, currency)
		if err != nil && !errors.Is(err, ledger.ErrUnknownCurrency) {
			v.log.WarnContext(v.ctx, "omitting *_value fields: currency lookup failed", "currency", currency, "err", err)
		}
		v.currencies[currency] = c
	}
	if c == nil {
		return ""
	}
	s, _ := c.Format(a)
	return s
}

func (v *views) account(a *ledger.Account) any {
	if a == nil {
		return nil
	}
	av := accountView{
		Account:            a,
		CreditTotalValue:   v.format(a.Currency, a.CreditTotal),
		DebitTotalValue:    v.format(a.Currency, a.DebitTotal),
		BalanceValue:       v.format(a.Currency, a.Balance),
		PendingDebitValue:  v.format(a.Currency, a.PendingDebit),
		PendingCreditValue: v.format(a.Currency, a.PendingCredit),
	}
	if a.OverdraftLimit != nil {
		av.OverdraftLimitValue = v.format(a.Currency, *a.OverdraftLimit)
	}
	return av
}

func (v *views) entries(currency string, entries []*ledger.Entry) []entryView {
	out := make([]entryView, len(entries))
	for i, e := range entries {
		out[i] = entryView{Entry: e, Value: v.format(currency, e.Amount)}
		if e.BalanceAfter != nil {
			out[i].BalanceAfterValue = v.format(currency, *e.BalanceAfter)
		}
	}
	return out
}

func (v *views) transaction(txn *ledger.Transaction) any {
	if txn == nil {
		return nil
	}
	return transactionView{Transaction: txn, Entries: v.entries(txn.Currency, txn.Entries)}
}

func (v *views) legs(currency string, legs []ledger.Leg) []ledger.Leg {
	out := make([]ledger.Leg, len(legs))
	for i, leg := range legs {
		leg.Value = v.format(currency, leg.Amount)
		out[i] = leg
	}
	return out
}

func (v *views) hold(h *ledger.Hold) any {
	if h == nil {
		return nil
	}
	return holdView{Hold: h, Legs: v.legs(h.Currency, h.Legs)}
}

type batchView struct {
	*ledger.Batch
	Transactions []any `json:"transactions"`
}

func (v *views) batch(b *ledger.Batch) any {
	if b == nil {
		return nil
	}
	out := batchView{Batch: b, Transactions: make([]any, len(b.Transactions))}
	for i, t := range b.Transactions {
		out.Transactions[i] = v.transaction(t)
	}
	return out
}

type trialBalanceLineView struct {
	ledger.TrialBalanceLine
	BalanceValue string `json:"balance_value,omitempty"`
}

type currencyTotalView struct {
	ledger.CurrencyTotal
	TotalValue string `json:"total_value,omitempty"`
}

type trialBalanceView struct {
	*ledger.TrialBalance
	Accounts []trialBalanceLineView `json:"accounts"`
	Totals   []currencyTotalView    `json:"totals"`
}

func (v *views) trialBalance(tb *ledger.TrialBalance) any {
	if tb == nil {
		return nil
	}
	out := trialBalanceView{TrialBalance: tb, Accounts: make([]trialBalanceLineView, len(tb.Accounts)), Totals: make([]currencyTotalView, len(tb.Totals))}
	for i, l := range tb.Accounts {
		out.Accounts[i] = trialBalanceLineView{l, v.format(l.Currency, l.Balance)}
	}
	for i, t := range tb.Totals {
		out.Totals[i] = currencyTotalView{t, v.format(t.Currency, t.Total)}
	}
	return out
}

type reportLineView struct {
	ledger.ReportLine
	AmountValue string `json:"amount_value,omitempty"`
}

type reportSectionView struct {
	Accounts   []reportLineView `json:"accounts"`
	Total      ledger.Amount    `json:"total"`
	TotalValue string           `json:"total_value,omitempty"`
}

func (v *views) section(currency string, s ledger.ReportSection) reportSectionView {
	out := reportSectionView{Accounts: make([]reportLineView, len(s.Accounts)), Total: s.Total, TotalValue: v.format(currency, s.Total)}
	for i, l := range s.Accounts {
		out.Accounts[i] = reportLineView{l, v.format(currency, l.Amount)}
	}
	return out
}

type balanceSheetView struct {
	LedgerID       ledger.LedgerID   `json:"ledger_id"`
	Currency       string            `json:"currency"`
	At             time.Time         `json:"at"`
	Assets         reportSectionView `json:"assets"`
	Liabilities    reportSectionView `json:"liabilities"`
	Equity         reportSectionView `json:"equity"`
	NetIncome      ledger.Amount     `json:"net_income"`
	NetIncomeValue string            `json:"net_income_value,omitempty"`
	Unclassified   reportSectionView `json:"unclassified"`
}

func (v *views) balanceSheet(bs *ledger.BalanceSheet) any {
	if bs == nil {
		return nil
	}
	c := bs.Currency
	return balanceSheetView{
		LedgerID: bs.LedgerID, Currency: c, At: bs.At,
		Assets: v.section(c, bs.Assets), Liabilities: v.section(c, bs.Liabilities), Equity: v.section(c, bs.Equity),
		NetIncome: bs.NetIncome, NetIncomeValue: v.format(c, bs.NetIncome),
		Unclassified: v.section(c, bs.Unclassified),
	}
}

type incomeStatementView struct {
	LedgerID       ledger.LedgerID   `json:"ledger_id"`
	Currency       string            `json:"currency"`
	From           time.Time         `json:"from"`
	To             time.Time         `json:"to"`
	Revenue        reportSectionView `json:"revenue"`
	Expenses       reportSectionView `json:"expenses"`
	NetIncome      ledger.Amount     `json:"net_income"`
	NetIncomeValue string            `json:"net_income_value,omitempty"`
}

func (v *views) incomeStatement(is *ledger.IncomeStatement) any {
	if is == nil {
		return nil
	}
	c := is.Currency
	return incomeStatementView{
		LedgerID: is.LedgerID, Currency: c, From: is.From, To: is.To,
		Revenue: v.section(c, is.Revenue), Expenses: v.section(c, is.Expenses),
		NetIncome: is.NetIncome, NetIncomeValue: v.format(c, is.NetIncome),
	}
}

type periodCloseView struct {
	LedgerID      ledger.LedgerID `json:"ledger_id"`
	ClosedThrough time.Time       `json:"closed_through"`
	ClosedAt      time.Time       `json:"closed_at"`
	TrialBalance  any             `json:"trial_balance"`
}

func (v *views) periodClose(pc *ledger.PeriodClose) any {
	return periodCloseView{pc.LedgerID, pc.ClosedThrough, pc.ClosedAt, v.trialBalance(pc.TrialBalance)}
}

type statementLineView struct {
	ledger.StatementLine
	Value        string `json:"value,omitempty"`
	BalanceValue string `json:"balance_value,omitempty"`
}

type statementView struct {
	Account             any                 `json:"account"`
	From                time.Time           `json:"from"`
	To                  time.Time           `json:"to"`
	OpeningBalance      ledger.Amount       `json:"opening_balance"`
	OpeningBalanceValue string              `json:"opening_balance_value,omitempty"`
	ClosingBalance      ledger.Amount       `json:"closing_balance"`
	ClosingBalanceValue string              `json:"closing_balance_value,omitempty"`
	Lines               []statementLineView `json:"lines"`
}

func (v *views) statement(st *ledger.Statement) any {
	if st == nil {
		return nil
	}
	c := st.Account.Currency
	lines := make([]statementLineView, len(st.Lines))
	for i, l := range st.Lines {
		lines[i] = statementLineView{l, v.format(c, l.Amount), v.format(c, l.Balance)}
	}
	return statementView{
		Account: v.account(st.Account), From: st.From, To: st.To,
		OpeningBalance: st.OpeningBalance, OpeningBalanceValue: v.format(c, st.OpeningBalance),
		ClosingBalance: st.ClosingBalance, ClosingBalanceValue: v.format(c, st.ClosingBalance),
		Lines: lines,
	}
}
