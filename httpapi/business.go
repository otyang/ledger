package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/otyang/ledger"
)

func (h *handler) updateAccount(w http.ResponseWriter, r *http.Request) {
	var p ledger.UpdateAccountParams
	if !h.decode(w, r, &p) {
		return
	}
	a, err := h.svc.UpdateAccount(r.Context(), ledger.AccountID(r.PathValue("id")), p)
	h.respond(w, r, http.StatusOK, h.views(r.Context()).account(a), err)
}

func (h *handler) updateTransaction(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Metadata map[string]any `json:"metadata"`
	}
	if !h.decode(w, r, &p) {
		return
	}
	txn, err := h.svc.UpdateTransactionMetadata(r.Context(), ledger.TransactionID(r.PathValue("id")), p.Metadata)
	h.respond(w, r, http.StatusOK, h.views(r.Context()).transaction(txn), err)
}

func (h *handler) reverse(w http.ResponseWriter, r *http.Request) {
	var p ledger.ReverseParams
	if !h.decode(w, r, &p) {
		return
	}
	txn, err := h.svc.Reverse(r.Context(), ledger.TransactionID(r.PathValue("id")), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).transaction(txn), err)
}

func (h *handler) postBatch(w http.ResponseWriter, r *http.Request) {
	var p ledger.BatchParams
	if !h.decode(w, r, &p) {
		return
	}
	b, err := h.svc.PostBatch(r.Context(), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).batch(b), referenced(err))
}

func (h *handler) getBatch(w http.ResponseWriter, r *http.Request) {
	b, err := h.svc.GetBatch(r.Context(), ledger.BatchID(r.PathValue("id")))
	h.respond(w, r, http.StatusOK, h.views(r.Context()).batch(b), err)
}

func (h *handler) exchange(w http.ResponseWriter, r *http.Request) {
	var p ledger.ExchangeParams
	if !h.decode(w, r, &p) {
		return
	}
	b, err := h.svc.Exchange(r.Context(), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).batch(b), referenced(err))
}

func (h *handler) listTransactions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := ledger.ListTransactionsParams{
		LedgerID:  ledger.LedgerID(r.PathValue("id")),
		AccountID: ledger.AccountID(q.Get("account_id")),
		After:     ledger.TransactionID(q.Get("after")),
	}
	var err error
	if p.Limit, err = intParam(q.Get("limit")); err == nil {
		if p.From, err = timeParam("from", q.Get("from"), time.Time{}); err == nil {
			p.To, err = timeParam("to", q.Get("to"), time.Time{})
		}
	}
	if err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	for key, vals := range q {
		if k, ok := strings.CutPrefix(key, "metadata."); ok && k != "" {
			if p.Metadata == nil {
				p.Metadata = map[string]any{}
			}
			p.Metadata[k] = vals[0]
		}
	}
	txns, err := h.svc.ListTransactions(r.Context(), p)
	var body any
	if err == nil {
		v := h.views(r.Context())
		out := make([]any, len(txns))
		for i, t := range txns {
			out[i] = v.transaction(t)
		}
		body = map[string]any{"transactions": out}
	}
	h.respond(w, r, http.StatusOK, body, err)
}

func (h *handler) trialBalance(w http.ResponseWriter, r *http.Request) {
	at, err := timeParam("at", r.URL.Query().Get("at"), time.Now())
	if err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	tb, err := h.svc.TrialBalance(r.Context(), ledger.LedgerID(r.PathValue("id")), at)
	h.respond(w, r, http.StatusOK, h.views(r.Context()).trialBalance(tb), err)
}

func (h *handler) balanceSheet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	at, err := timeParam("at", q.Get("at"), time.Now())
	if err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	bs, err := h.svc.BalanceSheet(r.Context(), ledger.LedgerID(r.PathValue("id")), q.Get("currency"), at)
	h.respond(w, r, http.StatusOK, h.views(r.Context()).balanceSheet(bs), err)
}

func (h *handler) incomeStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := timeParam("from", q.Get("from"), time.Time{})
	var to time.Time
	if err == nil {
		to, err = timeParam("to", q.Get("to"), time.Now())
	}
	if err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	is, err := h.svc.IncomeStatement(r.Context(), ledger.LedgerID(r.PathValue("id")), q.Get("currency"), from, to)
	h.respond(w, r, http.StatusOK, h.views(r.Context()).incomeStatement(is), err)
}

func (h *handler) closePeriod(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Through time.Time `json:"through"`
	}
	if !h.decode(w, r, &p) {
		return
	}
	pc, err := h.svc.ClosePeriod(r.Context(), ledger.LedgerID(r.PathValue("id")), p.Through)
	var body any
	if err == nil {
		body = h.views(r.Context()).periodClose(pc)
	}
	h.respond(w, r, http.StatusCreated, body, err)
}

func (h *handler) listPeriods(w http.ResponseWriter, r *http.Request) {
	closes, err := h.svc.ListPeriodCloses(r.Context(), ledger.LedgerID(r.PathValue("id")))
	var body any
	if err == nil {
		v := h.views(r.Context())
		out := make([]any, len(closes))
		for i, pc := range closes {
			out[i] = v.periodClose(pc)
		}
		body = map[string]any{"periods": out}
	}
	h.respond(w, r, http.StatusOK, body, err)
}

func (h *handler) statementParams(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	from, err := timeParam("from", q.Get("from"), time.Time{})
	if err != nil {
		return from, from, err
	}
	to, err := timeParam("to", q.Get("to"), time.Now())
	return from, to, err
}

func (h *handler) statement(w http.ResponseWriter, r *http.Request) {
	from, to, err := h.statementParams(r)
	if err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	st, err := h.svc.Statement(r.Context(), ledger.AccountID(r.PathValue("id")), from, to)
	h.respond(w, r, http.StatusOK, h.views(r.Context()).statement(st), err)
}

// statementCSV writes the same statement as CSV, for spreadsheets.
func (h *handler) statementCSV(w http.ResponseWriter, r *http.Request) {
	from, to, err := h.statementParams(r)
	if err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	id := ledger.AccountID(r.PathValue("id"))
	st, err := h.svc.Statement(r.Context(), id, from, to)
	if err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	v := h.views(r.Context())
	cur := st.Account.Currency
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="statement-%s.csv"`, id))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"created_at", "entry_id", "transaction_id", "reference", "description", "direction", "amount", "value", "balance", "balance_value"})
	_ = cw.Write([]string{st.From.Format(time.RFC3339Nano), "", "", "", "Opening balance", "", "", "", st.OpeningBalance.String(), v.format(cur, st.OpeningBalance)})
	for _, l := range st.Lines {
		_ = cw.Write([]string{
			l.CreatedAt.Format(time.RFC3339Nano), string(l.EntryID), string(l.TransactionID), csvSafe(l.Reference), csvSafe(l.Description),
			string(l.Direction), l.Amount.String(), v.format(cur, l.Amount), l.Balance.String(), v.format(cur, l.Balance),
		})
	}
	_ = cw.Write([]string{st.To.Format(time.RFC3339Nano), "", "", "", "Closing balance", "", "", "", st.ClosingBalance.String(), v.format(cur, st.ClosingBalance)})
	cw.Flush()
}

// csvSafe stops spreadsheets from treating user-supplied text as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := ledger.ListEventsParams{After: ledger.EventID(q.Get("after")), LedgerID: ledger.LedgerID(q.Get("ledger_id"))}
	var err error
	if p.Limit, err = intParam(q.Get("limit")); err != nil {
		h.respond(w, r, 0, nil, err)
		return
	}
	events, err := h.svc.ListEvents(r.Context(), p)
	var body any
	if err == nil {
		body = map[string]any{"events": events}
	}
	h.respond(w, r, http.StatusOK, body, err)
}

func intParam(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%w: limit must be an integer", ledger.ErrInvalidParams)
	}
	return n, nil
}

func timeParam(name, v string, def time.Time) (time.Time, error) {
	if v == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return t, fmt.Errorf("%w: %s must be RFC 3339, e.g. 2026-10-01T00:00:00Z", ledger.ErrInvalidParams, name)
	}
	return t, nil
}
