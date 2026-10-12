// Package httpapi exposes a ledger.Service over JSON/HTTP. It is optional:
// applications embedding the ledger can call the Service directly.
//
// Routes:
//
//	POST /currencies           {"code":"USD","decimals":2}
//	GET  /currencies/{code}
//	POST /ledgers
//	GET  /ledgers/{id}
//	POST /accounts
//	GET  /accounts/{id}
//	GET  /accounts/{id}/entries?after=<entry id>&limit=
//	GET  /accounts/{id}/balance?at=<RFC3339>
//	POST /transactions
//	GET  /transactions/{id}
//	POST /transactions/{id}/refunds
//	GET  /transactions/{id}/refundable
//	POST /holds
//	GET  /holds/{id}
//	POST /holds/{id}/commit   (empty body commits the full hold)
//	POST /holds/{id}/void
//
// Business operations and reports:
//
//	PATCH /accounts/{id}                 status, overdraft limit, type, metadata
//	GET   /accounts/{id}/statement?from=&to=
//	GET   /accounts/{id}/statement.csv?from=&to=
//	PATCH /transactions/{id}             {"metadata":{...}}
//	POST  /transactions/{id}/reversals   {"reference":"...","reason":"..."}
//	POST  /batches
//	GET   /batches/{id}
//	POST  /exchanges
//	GET   /ledgers/{id}/transactions?account_id=&from=&to=&metadata.<key>=<value>&after=&limit=
//	GET   /ledgers/{id}/trial-balance?at=
//	GET   /ledgers/{id}/balance-sheet?currency=&at=
//	GET   /ledgers/{id}/income-statement?currency=&from=&to=
//	POST  /ledgers/{id}/periods          {"through":"<RFC3339>"}
//	GET   /ledgers/{id}/periods
//	GET   /events?after=<event id>&ledger_id=&limit=
//
// Operations (no token needed for the health checks):
//
//	GET /healthz   the process is alive
//	GET /readyz    the database is reachable and its schema up to date
//	GET /metrics   Prometheus metrics (needs the token)
//
// Amounts are whole numbers of the currency's minor unit, written as JSON
// strings ("540323") so that large token amounts keep every digit. Requests may
// also send them as JSON integers, or as decimals in a "value" field instead
// ("value": "5403.23" for USD), converted with the currency's registered
// decimals and never rounded. Responses carry both: every amount field has a
// decimal "*_value" twin, e.g. "balance" and "balance_value".
//
// Errors are returned as {"error":{"code":"...","message":"..."}}. Retryable
// failures (concurrent updates, request timeouts) answer 503 with a
// Retry-After header; retrying with the same reference is always safe.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/otyang/ledger"
)

const maxBodyBytes = 1 << 20

type Config struct {
	// Logger receives internal errors. Defaults to slog.Default().
	Logger *slog.Logger
	// Token, if set, is required as "Authorization: Bearer <token>" on every
	// request. Leave it empty only when another layer authenticates requests.
	Token string
	// RequestTimeout bounds each request, including time spent waiting for
	// account locks. Zero means no timeout.
	RequestTimeout time.Duration
	// Ready, if set, is consulted by GET /readyz: returning false (e.g. while
	// shutting down) takes the server out of a load balancer's rotation.
	Ready func() bool
	// RateLimit, if positive, limits each client IP to this many requests
	// per second on average, with bursts of up to RateBurst (default: the
	// rate, rounded up). Excess requests get 429. Behind a proxy every
	// client shares the proxy's IP, so set the limit there instead.
	RateLimit float64
	RateBurst int
}

type handler struct {
	svc     *ledger.Service
	cfg     Config
	metrics *metrics
	limiter *limiter
}

func New(svc *ledger.Service, cfg Config) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	h := &handler{svc: svc, cfg: cfg, metrics: newMetrics(), limiter: newLimiter(cfg.RateLimit, cfg.RateBurst)}
	mux := http.NewServeMux()
	route := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, h.metrics.instrument(pattern, fn))
	}
	route("POST /currencies", h.registerCurrency)
	route("GET /currencies/{code}", h.getCurrency)
	route("POST /ledgers", h.createLedger)
	route("GET /ledgers/{id}", h.getLedger)
	route("POST /accounts", h.createAccount)
	route("GET /accounts/{id}", h.getAccount)
	route("GET /accounts/{id}/entries", h.listEntries)
	route("GET /accounts/{id}/balance", h.balanceAt)
	route("POST /transactions", h.postTransaction)
	route("GET /transactions/{id}", h.getTransaction)
	route("POST /transactions/{id}/refunds", h.refund)
	route("GET /transactions/{id}/refundable", h.refundable)
	route("POST /holds", h.createHold)
	route("GET /holds/{id}", h.getHold)
	route("POST /holds/{id}/commit", h.commitHold)
	route("POST /holds/{id}/void", h.voidHold)

	route("PATCH /accounts/{id}", h.updateAccount)
	route("GET /accounts/{id}/statement", h.statement)
	route("GET /accounts/{id}/statement.csv", h.statementCSV)
	route("PATCH /transactions/{id}", h.updateTransaction)
	route("POST /transactions/{id}/reversals", h.reverse)
	route("POST /batches", h.postBatch)
	route("GET /batches/{id}", h.getBatch)
	route("POST /exchanges", h.exchange)
	route("GET /ledgers/{id}/transactions", h.listTransactions)
	route("GET /ledgers/{id}/trial-balance", h.trialBalance)
	route("GET /ledgers/{id}/balance-sheet", h.balanceSheet)
	route("GET /ledgers/{id}/income-statement", h.incomeStatement)
	route("POST /ledgers/{id}/periods", h.closePeriod)
	route("GET /ledgers/{id}/periods", h.listPeriods)
	route("GET /events", h.listEvents)

	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /metrics", h.metricsHandler)
	return h.middleware(mux)
}

// public routes skip authentication and rate limiting.
var public = map[string]bool{"/healthz": true, "/readyz": true}

func (h *handler) middleware(next http.Handler) http.Handler {
	want := []byte("Bearer " + h.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if h.limiter != nil {
			if ok, wait := h.limiter.allow(clientIP(r), time.Now()); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()+0.999))))
				writeJSON(w, http.StatusTooManyRequests, errorBody("rate_limited", "too many requests; slow down"))
				return
			}
		}
		if h.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorBody("unauthorized", "missing or invalid bearer token"))
			return
		}
		if h.cfg.RequestTimeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), h.cfg.RequestTimeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func (h *handler) registerCurrency(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Code string `json:"code"`
		// A pointer, so a missing "decimals" is an error rather than 0:
		// decimals can never be changed once registered.
		Decimals *int `json:"decimals"`
	}
	if !h.decode(w, r, &p) {
		return
	}
	if p.Decimals == nil {
		h.respond(w, r, 0, nil, fmt.Errorf("%w: decimals is required", ledger.ErrInvalidParams))
		return
	}
	c, err := h.svc.RegisterCurrency(r.Context(), ledger.CurrencyParams{Code: p.Code, Decimals: *p.Decimals})
	h.respond(w, r, http.StatusCreated, c, err)
}

func (h *handler) getCurrency(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.GetCurrency(r.Context(), r.PathValue("code"))
	if errors.Is(err, ledger.ErrUnknownCurrency) {
		err = fmt.Errorf("%w: %w", ledger.ErrNotFound, err)
	}
	h.respond(w, r, http.StatusOK, c, err)
}

func (h *handler) createLedger(w http.ResponseWriter, r *http.Request) {
	var p ledger.CreateLedgerParams
	if !h.decode(w, r, &p) {
		return
	}
	l, err := h.svc.CreateLedger(r.Context(), p)
	h.respond(w, r, http.StatusCreated, l, err)
}

func (h *handler) getLedger(w http.ResponseWriter, r *http.Request) {
	l, err := h.svc.GetLedger(r.Context(), ledger.LedgerID(r.PathValue("id")))
	h.respond(w, r, http.StatusOK, l, err)
}

func (h *handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var p ledger.CreateAccountParams
	if !h.decode(w, r, &p) {
		return
	}
	a, err := h.svc.CreateAccount(r.Context(), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).account(a), referenced(err))
}

func (h *handler) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.GetAccount(r.Context(), ledger.AccountID(r.PathValue("id")))
	h.respond(w, r, http.StatusOK, h.views(r.Context()).account(a), err)
}

func (h *handler) listEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := ledger.ListEntriesParams{After: ledger.EntryID(q.Get("after"))}
	if v := q.Get("limit"); v != "" {
		var err error
		if p.Limit, err = strconv.Atoi(v); err != nil {
			h.respond(w, r, 0, nil, fmt.Errorf("%w: limit must be an integer", ledger.ErrInvalidParams))
			return
		}
	}
	id := ledger.AccountID(r.PathValue("id"))
	entries, err := h.svc.ListEntries(r.Context(), id, p)
	var body any
	if err == nil {
		body = map[string]any{"entries": h.views(r.Context()).entries(h.currencyOf(r.Context(), "accounts", string(id)), entries)}
	}
	h.respond(w, r, http.StatusOK, body, err)
}

func (h *handler) balanceAt(w http.ResponseWriter, r *http.Request) {
	at := time.Now()
	if v := r.URL.Query().Get("at"); v != "" {
		var err error
		if at, err = time.Parse(time.RFC3339Nano, v); err != nil {
			h.respond(w, r, 0, nil, fmt.Errorf("%w: at must be RFC 3339", ledger.ErrInvalidParams))
			return
		}
	}
	id := ledger.AccountID(r.PathValue("id"))
	balance, err := h.svc.BalanceAt(r.Context(), id, at)
	var body any
	if err == nil {
		body = map[string]any{
			"account_id":    id,
			"balance":       balance,
			"balance_value": h.views(r.Context()).format(h.currencyOf(r.Context(), "accounts", string(id)), balance),
			"at":            at.UTC(),
		}
	}
	h.respond(w, r, http.StatusOK, body, err)
}

// postTransaction answers 201 for both a new posting and an idempotent replay
// of the same reference, returning the same body either way.
func (h *handler) postTransaction(w http.ResponseWriter, r *http.Request) {
	var p ledger.PostParams
	if !h.decode(w, r, &p) {
		return
	}
	txn, err := h.svc.Post(r.Context(), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).transaction(txn), referenced(err))
}

func (h *handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	txn, err := h.svc.GetTransaction(r.Context(), ledger.TransactionID(r.PathValue("id")))
	h.respond(w, r, http.StatusOK, h.views(r.Context()).transaction(txn), err)
}

func (h *handler) refund(w http.ResponseWriter, r *http.Request) {
	var p ledger.RefundParams
	if !h.decode(w, r, &p) {
		return
	}
	txn, err := h.svc.Refund(r.Context(), ledger.TransactionID(r.PathValue("id")), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).transaction(txn), err)
}

func (h *handler) refundable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	legs, err := h.svc.Refundable(r.Context(), ledger.TransactionID(id))
	var body any
	if err == nil {
		body = map[string]any{"legs": h.views(r.Context()).legs(h.currencyOf(r.Context(), "transactions", id), legs)}
	}
	h.respond(w, r, http.StatusOK, body, err)
}

func (h *handler) createHold(w http.ResponseWriter, r *http.Request) {
	var p ledger.HoldParams
	if !h.decode(w, r, &p) {
		return
	}
	hold, err := h.svc.CreateHold(r.Context(), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).hold(hold), referenced(err))
}

func (h *handler) getHold(w http.ResponseWriter, r *http.Request) {
	hold, err := h.svc.GetHold(r.Context(), ledger.HoldID(r.PathValue("id")))
	h.respond(w, r, http.StatusOK, h.views(r.Context()).hold(hold), err)
}

func (h *handler) commitHold(w http.ResponseWriter, r *http.Request) {
	var p ledger.CommitHoldParams
	if !h.decodeOptional(w, r, &p) {
		return
	}
	txn, err := h.svc.CommitHold(r.Context(), ledger.HoldID(r.PathValue("id")), p)
	h.respond(w, r, http.StatusCreated, h.views(r.Context()).transaction(txn), err)
}

func (h *handler) voidHold(w http.ResponseWriter, r *http.Request) {
	hold, err := h.svc.VoidHold(r.Context(), ledger.HoldID(r.PathValue("id")))
	h.respond(w, r, http.StatusOK, h.views(r.Context()).hold(hold), err)
}

// currencyOf reads the currency of an account or transaction. It is a single
// column read: GetAccount would also sum every entry of an untracked account.
func (h *handler) currencyOf(ctx context.Context, table, id string) string {
	var currency string
	if err := h.svc.DB().NewSelect().Table("ledger."+table).Column("currency").Where("id = ?", id).Scan(ctx, &currency); err != nil {
		h.cfg.Logger.WarnContext(ctx, "omitting *_value fields: currency lookup failed", "table", table, "id", id, "err", err)
	}
	return currency
}

func (h *handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return h.decodeBody(w, r, v, false)
}

// decodeOptional accepts an empty body, leaving v at its zero value.
func (h *handler) decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	return h.decodeBody(w, r, v, true)
}

func (h *handler) decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if optional && errors.Is(err, io.EOF) {
		return true
	}
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("unexpected data after the JSON object")
	}
	if err != nil {
		h.respond(w, r, 0, nil, fmt.Errorf("%w: invalid JSON body: %v", ledger.ErrInvalidParams, err))
		return false
	}
	return true
}

// errReferenced marks a not-found error for a resource named in the request
// body (e.g. an account in a transaction's legs), which is a 422 rather than
// a 404 for the URL itself.
var errReferenced = errors.New("referenced resource not found")

func referenced(err error) error {
	if errors.Is(err, ledger.ErrNotFound) {
		return fmt.Errorf("%w: %w", errReferenced, err)
	}
	return err
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorMap is checked in order; the first match wins.
var errorMap = []struct {
	err    error
	status int
	code   string
}{
	{ledger.ErrRetryable, http.StatusServiceUnavailable, "retryable"},
	{context.DeadlineExceeded, http.StatusServiceUnavailable, "timeout"},
	{errReferenced, http.StatusUnprocessableEntity, "unknown_reference"},
	{ledger.ErrNotFound, http.StatusNotFound, "not_found"},
	{ledger.ErrUnknownCurrency, http.StatusUnprocessableEntity, "unknown_currency"},
	{ledger.ErrCurrencyConflict, http.StatusConflict, "currency_conflict"},
	{ledger.ErrHoldExpired, http.StatusConflict, "hold_expired"},
	{ledger.ErrPeriodClosed, http.StatusConflict, "period_closed"},
	{ledger.ErrAlreadyExists, http.StatusConflict, "already_exists"},
	{ledger.ErrReferenceConflict, http.StatusConflict, "reference_conflict"},
	{ledger.ErrHoldNotPending, http.StatusConflict, "hold_not_pending"},
	{ledger.ErrInvalidID, http.StatusBadRequest, "invalid_id"},
	{ledger.ErrInvalidParams, http.StatusBadRequest, "invalid_params"},
	{ledger.ErrAmountOverflow, http.StatusBadRequest, "amount_overflow"},
	{ledger.ErrUnbalanced, http.StatusUnprocessableEntity, "unbalanced"},
	{ledger.ErrCurrencyMismatch, http.StatusUnprocessableEntity, "currency_mismatch"},
	{ledger.ErrInsufficientFunds, http.StatusUnprocessableEntity, "insufficient_funds"},
	{ledger.ErrRefundExceeded, http.StatusUnprocessableEntity, "refund_exceeded"},
	{ledger.ErrLedgerMismatch, http.StatusUnprocessableEntity, "ledger_mismatch"},
	{ledger.ErrAccountFrozen, http.StatusUnprocessableEntity, "account_frozen"},
	{ledger.ErrAccountClosed, http.StatusUnprocessableEntity, "account_closed"},
}

func (h *handler) respond(w http.ResponseWriter, r *http.Request, status int, body any, err error) {
	if err != nil {
		status, body = http.StatusInternalServerError, errorBody("internal", "internal error")
		for _, m := range errorMap {
			if errors.Is(err, m.err) {
				status, body = m.status, errorBody(m.code, err.Error())
				break
			}
		}
		switch status {
		case http.StatusInternalServerError:
			h.cfg.Logger.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		case http.StatusServiceUnavailable:
			w.Header().Set("Retry-After", "1")
		}
	}
	writeJSON(w, status, body)
}

func errorBody(code, message string) map[string]apiError {
	return map[string]apiError{"error": {code, message}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
