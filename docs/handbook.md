# ledger handbook

Everything about running ledger and building on it, in one place. New to double-entry bookkeeping? Start with the [beginner guide](guide.md). Just want the endpoints? Open the [interactive API reference](https://otyang.github.io/ledger/docs/api.html).

- **Correct by construction.** Every transaction balances, entries are immutable, and balances can't drift: the database enforces these rules itself, independent of the Go code.
- **Exact amounts.** Whole numbers of a currency's smallest unit, up to 78 digits, so fiat cents and 18-decimal crypto tokens (wei) both fit. Nothing is ever rounded.
- **Decimals when you want them.** Send `"5403.23"`; the ledger converts it using the currency's registered decimals.
- **Safe to retry.** Every write is idempotent on a reference you choose.
- **Holds, partial captures and refunds**, with refunds that can never exceed the original, even under concurrency.
- **Fast under contention.** Busy system accounts such as `@world` can be "untracked" so they never become a lock bottleneck.
- **Account controls.** Freeze an account (money can still arrive, none can leave), close it for good, or give it an overdraft limit.
- **Holds that expire**, like real card authorizations, so forgotten holds don't lock money forever.
- **Batches and currency exchange.** Post a payroll run atomically, or convert between currencies with the rate recorded.
- **Reporting.** Search transactions, trial balance, balance sheet, income statement, account statements as CSV, and period close.
- **Integrations.** An event feed and signed webhooks for every change; health checks and Prometheus metrics for operations.

## Documentation

| Page | Read it to |
|---|---|
| [Beginner guide](guide.md) | Understand double-entry bookkeeping and this ledger through a worked example: top-up, purchase with a fee, refund, card hold, withdrawal |
| [Interactive API reference](https://otyang.github.io/ledger/docs/api.html) | Browse every endpoint, its schemas and code samples, generated from the [OpenAPI 3.0 spec](openapi.yaml) |
| [API reference (Markdown)](api.md) | Read every endpoint's fields, status codes, errors and real example responses |
| [Architecture and internals](architecture.md) | Learn how posting, locking, idempotency, holds, refunds and the database rules work, before changing the code |

This handbook is also the home page of the website, [otyang.github.io/ledger/docs](https://otyang.github.io/ledger/docs/).

## Contents

- [Documentation](#documentation)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [HTTP API](#http-api)
- [Go library](#go-library)
- [Concepts](#concepts)
- [Guarantees](#guarantees)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [License](#license)

## Quick start

You need Go 1.25+ and PostgreSQL (any recent version; tests run on 18). Run these from the repository root.

### Run locally

```sh
createdb ledger
cp .env.example .env        # then edit LEDGER_DATABASE_URL and LEDGER_API_TOKEN
make run
```

Or without the Makefile:

```sh
LEDGER_DATABASE_URL="postgres://postgres@localhost:5432/ledger?sslmode=disable" \
LEDGER_API_TOKEN=change-me \
go run ./cmd/ledgerd
```

The service applies its database migrations on startup, then logs `ledgerd listening` on port **7710**.

### Run with Docker

```sh
make docker-build VERSION=v0.1.0
docker run --rm -p 7710:7710 \
  -e LEDGER_DATABASE_URL="postgres://postgres@host.docker.internal:5432/ledger?sslmode=disable" \
  -e LEDGER_API_TOKEN=change-me \
  ledgerd:v0.1.0
```

`host.docker.internal` reaches a Postgres running on your machine (Docker Desktop). The image contains only the static binary, runs as a non-root user, and has no shell.

### First requests

```sh
T="Authorization: Bearer change-me"
API=http://localhost:7710

curl -H "$T" -X POST $API/currencies -d '{"code": "USD", "decimals": 2}'
curl -H "$T" -X POST $API/ledgers    -d '{"name": "main"}'
curl -H "$T" -X POST $API/accounts   -d '{
  "ledger_id": "ldg_…",
  "currency": "USD",
  "indicator": "@world",
  "allow_negative": true,
  "untracked": true
}'
curl -H "$T" -X POST $API/accounts   -d '{"ledger_id": "ldg_…", "currency": "USD"}'

# Deposit 5403.23 USD from outside the ledger into the new account
curl -H "$T" -X POST $API/transactions -d '{
  "reference": "deposit-1",
  "currency": "USD",
  "legs": [
    {
      "account_id": "acc_<world>",
      "direction": "debit",
      "value": "5403.23"
    },
    {
      "account_id": "acc_<alice>",
      "direction": "credit",
      "value": "5403.23"
    }
  ]
}'

curl -H "$T" "$API/accounts/acc_<alice>"
```

## Configuration

`ledgerd` is configured with environment variables. `make run` also reads them from a `.env` file (see [.env.example](../.env.example)).

| Variable | Default | Description |
|---|---|---|
| `LEDGER_DATABASE_URL` | *(required)* | Postgres connection URL. All tables go in a `ledger` schema inside this database, so it can be shared with other applications. |
| `LEDGER_API_TOKEN` | *(required)* | Clients must send `Authorization: Bearer <token>`. One token grants access to all ledgers. |
| `LEDGER_ADDR` | `:7710` | Listen address. |
| `LEDGER_REQUEST_TIMEOUT` | `10s` | Upper bound for each request, including time spent waiting for account locks. |
| `LEDGER_INSECURE_NO_AUTH` | `false` | Set to `true` to run without a token. Only do this when something in front of the service already authenticates requests. |
| `LEDGER_RATE_LIMIT` | `0` (off) | Requests per second allowed per client IP. Excess requests get `429 rate_limited`. Behind a proxy, all clients share its IP, so limit at the proxy instead. |
| `LEDGER_RATE_BURST` | the rate | How many requests a client may send at once before the rate applies. |
| `LEDGER_HOLD_EXPIRY_INTERVAL` | `1m` | How often expired holds are released. `0` disables it. Holds without `expires_at` are never touched. |
| `LEDGER_SNAPSHOT_INTERVAL` | `5m` | How often untracked accounts' balance snapshots are refreshed, which keeps reading their balance fast. `0` disables it. |
| `LEDGER_WEBHOOK_URL` | *(off)* | Send every [event](api.md#events) to this URL. |
| `LEDGER_WEBHOOK_SECRET` | | Required with `LEDGER_WEBHOOK_URL`: signs each request so the receiver can verify it. |
| `LEDGER_SHUTDOWN_DRAIN` | `0` | On shutdown, keep serving this long with `/readyz` failing, so a load balancer stops sending traffic before the listener closes. `10s` suits most load balancers. |

`ledgerd -version` prints the version the binary was built with.

## HTTP API

### Conventions

- **Authentication.** Every request needs `Authorization: Bearer <LEDGER_API_TOKEN>`. Without it: `401 unauthorized`.
- **Request bodies** are JSON. Unknown fields are rejected, so typos fail loudly instead of being ignored.
- **IDs** are `<prefix>_<uuidv7>`: `ldg_` ledgers, `acc_` accounts, `txn_` transactions, `ent_` entries, `hld_` holds.
- **Amounts** are whole numbers of the currency's smallest unit (cents for USD, wei for ETH), written as JSON strings so that large values keep every digit in JavaScript: `"amount": "540323"`. Requests may also send JSON integers.
- **Values** are the same amounts as decimals: `"value": "5403.23"`. Anywhere a request takes an `amount`, you can send a `value` instead. Values are converted with the currency's registered decimals and **never rounded**: `"5403.234"` USD is rejected. If you send both, they must agree.
- **Responses** include both forms for every amount: `amount`/`value`, `balance`/`balance_value`, `balance_after`/`balance_after_value`, and so on.
- **Idempotency.** Transactions, holds and refunds take a `reference`, unique per ledger. Sending the same request again returns the original result (same status, same body). Reusing a reference for a *different* request returns `409 reference_conflict`. A request written with `value` and the same request written with `amount` count as the same request.

### Endpoints

| Method | Path | Success | Description |
|---|---|---|---|
| `POST` | `/currencies` | 201 | Register a currency and its decimals |
| `GET` | `/currencies/{code}` | 200 | Get a currency |
| `POST` | `/ledgers` | 201 | Create a ledger |
| `GET` | `/ledgers/{id}` | 200 | Get a ledger |
| `POST` | `/accounts` | 201 | Create an account |
| `GET` | `/accounts/{id}` | 200 | Get an account with its balances |
| `GET` | `/accounts/{id}/entries?after=&limit=` | 200 | List an account's entries, oldest first |
| `GET` | `/accounts/{id}/balance?at=` | 200 | Balance at a point in time (RFC 3339; default now) |
| `POST` | `/transactions` | 201 | Post a transaction |
| `GET` | `/transactions/{id}` | 200 | Get a transaction with its entries |
| `POST` | `/transactions/{id}/refunds` | 201 | Refund all or part of a transaction |
| `GET` | `/transactions/{id}/refundable` | 200 | What is left to refund |
| `POST` | `/holds` | 201 | Reserve funds |
| `GET` | `/holds/{id}` | 200 | Get a hold |
| `POST` | `/holds/{id}/commit` | 201 | Post all or part of a hold as a transaction |
| `POST` | `/holds/{id}/void` | 200 | Release a hold |
| `PATCH` | `/accounts/{id}` | 200 | Freeze, unfreeze or close; set the overdraft limit, type or metadata |
| `GET` | `/accounts/{id}/statement?from=&to=` | 200 | Account statement: opening balance, lines, closing balance |
| `GET` | `/accounts/{id}/statement.csv?from=&to=` | 200 | The same statement as CSV |
| `PATCH` | `/transactions/{id}` | 200 | Update a transaction's metadata |
| `POST` | `/transactions/{id}/reversals` | 201 | Undo everything left of a transaction, with a reason |
| `POST` | `/batches` | 201 | Post several transactions at once, all or nothing |
| `GET` | `/batches/{id}` | 200 | Get a batch with its transactions |
| `POST` | `/exchanges` | 201 | Convert between currencies |
| `GET` | `/ledgers/{id}/transactions` | 200 | Search transactions by account, date and metadata |
| `GET` | `/ledgers/{id}/trial-balance?at=` | 200 | Every account's balance, totals per currency |
| `GET` | `/ledgers/{id}/balance-sheet?currency=&at=` | 200 | Assets, liabilities, equity |
| `GET` | `/ledgers/{id}/income-statement?currency=&from=&to=` | 200 | Revenue, expenses, net income |
| `POST` | `/ledgers/{id}/periods` | 201 | Close the books through a date |
| `GET` | `/ledgers/{id}/periods` | 200 | List closed periods |
| `GET` | `/events?after=&ledger_id=` | 200 | Every change, in order, for integrations |
| `GET` | `/healthz` | 200 | The process is alive (no token needed) |
| `GET` | `/readyz` | 200 | Ready to serve: database reachable, schema up to date (no token needed) |
| `GET` | `/metrics` | 200 | Prometheus metrics |

Every field and response is in the [interactive API reference](https://otyang.github.io/ledger/docs/api.html), where you can also try requests against a local `ledgerd`, and in the [Markdown API reference](api.md).

#### Currencies

```http
POST /currencies

{
  "code": "USD",
  "decimals": 2
}
```

`code` is 3–10 uppercase letters or digits (`USD`, `ETH`, `USDC`). `decimals` (0–36) is required: it is how many digits follow the decimal point, and **it can never change** once registered, because every stored amount would change meaning. Registering the same code with the same decimals again is a no-op; different decimals return `409 currency_conflict`.

```json
{
  "code": "USD",
  "decimals": 2,
  "created_at": "2026-10-12T12:51:07.593863Z"
}
```

#### Ledgers and accounts

```http
POST /ledgers

{
  "name": "main",
  "metadata": {
    "team": "payments"
  }
}
```

```http
POST /accounts

{
  "ledger_id": "ldg_…",
  "currency": "USD",
  "indicator": "@world",
  "allow_negative": true,
  "untracked": true
}
```

| Account field | Description |
|---|---|
| `currency` | Must be registered first, otherwise `422 unknown_currency`. Can't change later. |
| `indicator` | Optional name such as `@world`, unique per ledger and currency. |
| `allow_negative` | Whether the balance may go below zero. Default `false`. |
| `untracked` | High-traffic accounts: no locking, balance computed on read. Requires `allow_negative`. See [Untracked accounts](#concepts). |
| `metadata` | Any JSON object. |

```json
{
  "id": "acc_…",
  "ledger_id": "ldg_…",
  "currency": "USD",
  "allow_negative": false,
  "untracked": false,
  "credit_total": "540323",
  "credit_total_value": "5403.23",
  "debit_total": "0",
  "debit_total_value": "0.00",
  "balance": "540323",
  "balance_value": "5403.23",
  "pending_debit": "0",
  "pending_debit_value": "0.00",
  "pending_credit": "0",
  "pending_credit_value": "0.00",
  "version": 1,
  "created_at": "…",
  "updated_at": "…"
}
```

`GET /accounts/{id}/entries` pages with `after=<last entry id>` and `limit` (default 100, max 1000), and returns `{"entries": [...]}`. `GET /accounts/{id}/balance?at=2026-10-12T12:00:00Z` returns `{"account_id", "balance", "balance_value", "at"}`.

#### Transactions

```http
POST /transactions

{
  "reference": "order-8841",
  "currency": "USD",
  "description": "Concert ticket",
  "metadata": {
    "customer_id": "c_77"
  },
  "legs": [
    {
      "account_id": "acc_<alice>",
      "direction": "debit",
      "value": "50.00"
    },
    {
      "account_id": "acc_<venue>",
      "direction": "credit",
      "value": "47.00"
    },
    {
      "account_id": "acc_<platform>",
      "direction": "credit",
      "value": "3.00"
    }
  ]
}
```

A transaction needs at least two legs across at least two accounts, all in the transaction's currency and ledger, and its debits must equal its credits. A **debit** lowers an account's balance and a **credit** raises it. The response contains one entry per leg:

```json
{
  "id": "txn_…",
  "ledger_id": "ldg_…",
  "reference": "order-8841",
  "status": "posted",
  "currency": "USD",
  "description": "Concert ticket",
  "metadata": {
    "customer_id": "c_77"
  },
  "created_at": "…",
  "entries": [
    {
      "id": "ent_…",
      "transaction_id": "txn_…",
      "account_id": "acc_<alice>",
      "direction": "debit",
      "amount": "5000",
      "value": "50.00",
      "balance_after": "490323",
      "balance_after_value": "4903.23",
      "account_version": 2,
      "created_at": "…"
    },
    {
      "id": "ent_…",
      "transaction_id": "txn_…",
      "account_id": "acc_<venue>",
      "direction": "credit",
      "amount": "4700",
      "value": "47.00",
      "balance_after": "4700",
      "balance_after_value": "47.00",
      "account_version": 1,
      "created_at": "…"
    },
    {
      "id": "ent_…",
      "transaction_id": "txn_…",
      "account_id": "acc_<platform>",
      "direction": "credit",
      "amount": "300",
      "value": "3.00",
      "balance_after": "300",
      "balance_after_value": "3.00",
      "account_version": 1,
      "created_at": "…"
    }
  ]
}
```

#### Refunds

```http
POST /transactions/{id}/refunds

{
  "reference": "refund-1",
  "value": "10.00"
}
```

| Body | Refunds |
|---|---|
| `{"reference": "…"}` | Everything not yet refunded |
| `{"reference": "…", "value": "10.00"}` or `"amount"` | Part of a two-leg transaction |
| `{"reference": "…", "legs": [...]}` | Specific legs of a multi-leg transaction, e.g. take money back from the merchant but not the fee account |

An explicit zero (`"0"`, `"0.00"`) is rejected; it never means "everything". Refunds can't add up to more than the original (`422 refund_exceeded`). `GET /transactions/{id}/refundable` returns `{"legs": [...]}`: what a full refund would post now.

#### Holds

A hold reserves funds without moving them, like a card authorization. While it's pending, its debits reduce the source account's available balance (`balance - pending_debit`).

```http
POST /holds

{
  "reference": "auth-9",
  "currency": "USD",
  "legs": [
    {
      "account_id": "acc_<alice>",
      "direction": "debit",
      "value": "100.00"
    },
    {
      "account_id": "acc_<bob>",
      "direction": "credit",
      "value": "100.00"
    }
  ]
}
```

```http
POST /holds/{id}/commit

{
  "value": "80.00"
}
```

```http
POST /holds/{id}/void
```

Commit with an empty body to post the whole hold, with `amount`/`value` for part of a two-leg hold, or with `legs` for part of a multi-leg one. Whatever isn't committed is released. A hold resolves exactly once: retrying the same commit returns the same transaction; anything else returns `409 hold_not_pending`. A hold only expires if it was given an `expires_at`: after that, commits return `409 hold_expired` and the funds are released.

### Errors

Errors are returned as:

```json
{
  "error": {
    "code": "insufficient_funds",
    "message": "ledger: insufficient funds: account acc_…"
  }
}
```

Match on `code`; `message` is for humans and may change.

| HTTP | `code` | Meaning |
|---|---|---|
| 400 | `invalid_params` | Malformed request: bad JSON, unknown field, invalid amount or value, too many decimals, zero amount |
| 400 | `invalid_id` | An ID doesn't have the expected `<prefix>_<uuidv7>` form |
| 400 | `amount_overflow` | An amount or an account total would exceed 78 digits |
| 401 | `unauthorized` | Missing or wrong bearer token |
| 404 | `not_found` | The resource in the URL doesn't exist |
| 409 | `already_exists` | An account with that indicator and currency already exists in the ledger |
| 409 | `reference_conflict` | The reference was already used for a different request |
| 409 | `hold_not_pending` | The hold is already committed or voided |
| 409 | `currency_conflict` | The currency is registered with different decimals |
| 409 | `hold_expired` | The hold passed its `expires_at` |
| 409 | `period_closed` | The ledger's books are closed through a later date |
| 422 | `unknown_reference` | An account or ledger named in the body doesn't exist |
| 422 | `unknown_currency` | The currency isn't registered, or its decimals aren't yet |
| 422 | `unbalanced` | Debits don't equal credits |
| 422 | `currency_mismatch` | An account's currency differs from the transaction's |
| 422 | `ledger_mismatch` | The accounts belong to different ledgers |
| 422 | `insufficient_funds` | The available balance would go negative |
| 422 | `refund_exceeded` | The refund is larger than what's left to refund |
| 422 | `account_frozen` | Money can't leave a frozen account |
| 422 | `account_closed` | The account is closed |
| 429 | `rate_limited` | Too many requests from this client; `Retry-After` is set |
| 503 | `retryable` | A concurrent update got in the way. Retry the same request; `Retry-After` is set |
| 503 | `timeout` | The request exceeded `LEDGER_REQUEST_TIMEOUT`. Retry the same request; `Retry-After` is set |
| 500 | `internal` | Unexpected failure; details are logged by the server |

Retrying after a 503, or after a network error, is always safe: the reference makes every write idempotent.

## Go library

```sh
go get github.com/otyang/ledger
```

```go
import "github.com/otyang/ledger"

db := ledger.OpenDB("postgres://localhost:5432/app?sslmode=disable") // or your own *bun.DB
svc := ledger.New(db)
if err := svc.Migrate(ctx); err != nil { ... }

// Once per currency: how many decimals it has.
svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "USD", Decimals: 2})

l, _ := svc.CreateLedger(ctx, ledger.CreateLedgerParams{Name: "main"})
world, _ := svc.EnsureIndicatorAccount(ctx, ledger.CreateAccountParams{
	LedgerID: l.ID, Currency: "USD", Indicator: "@world", AllowNegative: true, Untracked: true,
})
alice, _ := svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: l.ID, Currency: "USD"})
bob, _ := svc.CreateAccount(ctx, ledger.CreateAccountParams{LedgerID: l.ID, Currency: "USD"})

// Amounts in minor units: 600000 cents is 6000.00 USD.
svc.Post(ctx, ledger.Transfer("deposit-123", "USD", world.ID, alice.ID, ledger.NewAmount(600000)))

// The same kind of posting, written as a decimal: Alice pays Bob 5403.23 USD.
svc.Post(ctx, ledger.PostParams{Reference: "pay-1", Currency: "USD", Legs: []ledger.Leg{
	{AccountID: alice.ID, Direction: ledger.Debit, Value: "5403.23"},
	{AccountID: bob.ID, Direction: ledger.Credit, Value: "5403.23"},
}})

// Reserve funds, then capture part of them; the rest is released.
hold, _ := svc.CreateHold(ctx, ledger.HoldParams{Reference: "auth-9", Currency: "USD",
	Legs: ledger.Transfer("", "", alice.ID, bob.ID, ledger.NewAmount(1000)).Legs})
txn, _ := svc.CommitHold(ctx, hold.ID, ledger.CommitHoldParams{Amount: ledger.NewAmount(800)})

// Refund part of it, then the rest.
svc.Refund(ctx, txn.ID, ledger.RefundParams{Reference: "refund-1", Amount: ledger.NewAmount(300)})
svc.Refund(ctx, txn.ID, ledger.RefundParams{Reference: "refund-2"})
```

Match errors with `errors.Is` against the sentinels in [errors.go](../errors.go) (`ErrInsufficientFunds`, `ErrReferenceConflict`, …). `ErrRetryable` covers serialization failures, deadlocks, lock timeouts, and idempotency races that your snapshot can't see yet.

### Using your own database transaction

`PostTx`, `CreateHoldTx`, `CommitHoldTx`, `VoidHoldTx` and `RefundTx` run inside **your** `bun.Tx`, so ledger movements commit or roll back together with your own writes. `GetAccountTx`, `GetTransactionTx`, `GetHoldTx` and `RefundableTx` read inside it, including your uncommitted postings.

- Each call runs in a savepoint, so a rejected operation (insufficient funds, a conflict) doesn't abort your transaction.
- Your `ctx` deadline applies, including time spent waiting for account locks. If it expires mid-statement, the Postgres driver drops the connection: the call returns an error matching both `context.DeadlineExceeded` and `ErrTxAborted`, and your transaction can no longer commit. A call that returns an error never leaves a posting behind.
- Repeatable read and serializable transactions work. When a concurrent change makes your snapshot stale, the call returns `ErrRetryable`; retry your whole transaction.

### Testing your application

`ledgertest.Run` in `TestMain`, then `ledgertest.NewDB(t)`, gives each test a fresh, migrated database: an embedded Postgres by default, or your own server via `LEDGER_TEST_DATABASE_URL`. Register the currencies your tests use with `RegisterCurrency`.

## Concepts

- **Ledger** (`ldg_`) groups accounts. A transaction or hold can only involve accounts from one ledger.
- **Account** (`acc_`) has one currency and a cached `balance = credit_total - debit_total`. It can't go negative unless `allow_negative` is set.
- **Amounts** are `ledger.Amount`: exact whole numbers of the currency's minor unit (cents for USD, wei for ETH), up to 78 digits, stored as `NUMERIC(78,0)`. That fits any 256-bit token amount. Build them with `NewAmount(2500)`, `ParseAmount("1000000000000000000")` or `AmountFromBig`, or from a decimal with a currency (below). Compare amounts with `Cmp` or `Sign`, not `==`. Exceeding 78 digits, in a leg or an account's totals, returns `ErrAmountOverflow`.
- **Currency** must be registered before any account uses it: `RegisterCurrency(ctx, CurrencyParams{Code: "USD", Decimals: 2})`. Registering again with the same decimals is a no-op; different decimals return `ErrCurrencyConflict`, and the database never lets decimals change. Unregistered currencies return `ErrUnknownCurrency`.
- **Decimal values.** Anywhere you pass an amount, you can pass a decimal `Value` instead: `Leg{Value: "5403.23"}`, `CommitHoldParams{Value: "12.50"}`, `RefundParams{Value: "12.50"}`. It is converted with the currency's decimals (`5403.23` USD → `540323`) and never rounded: `5403.234` USD returns `ErrInvalidParams`, while `5403.230` is fine. If you set both `Amount` and `Value`, they must agree. Only amounts are stored, so a retry may use either form. To convert yourself, use `cur.Parse("5403.23")` and `cur.Format(amount)` on a `Currency` from `GetCurrency`.
- **Transaction** (`txn_`) has two or more **entries** (`ent_`), one per leg. Its debits must equal its credits. `reference` is the idempotency key, unique per ledger: re-posting the same reference returns the original transaction, and reusing it with different legs returns `ErrReferenceConflict`. Leg order doesn't matter for this comparison.
- **Entry** records `balance_after` and `account_version`, a gap-free per-account sequence. Entry timestamps never go backwards within an account, which makes account history and `BalanceAt` exact and cheap.
- **External money** enters and leaves through a system account, conventionally `@world`, with `allow_negative` set. Its negative balance is the total that has entered the ledger, and all balances together always sum to zero.
- **Hold** (`hld_`) reserves funds without moving them. Debits count against `available = balance - pending_debit` until the hold is **committed** (fully or partially; any remainder is released), **voided**, or it expires. Holds without `expires_at` never expire. A hold resolves exactly once: commit retries return the same transaction, and anything else (a different commit, committing after a void, or the reverse) returns `ErrHoldNotPending`. The commit transaction has no reference; it is identified by `hold_id`, so no user reference can get in its way.
- **Refunds** are new transactions with `refund_of` set. Leave the amount (and value) out to refund everything that's left; an explicit zero is an error, never "everything". Pass `Amount` for part of a two-leg transaction, or pass explicit `Legs` for a multi-leg one, which decides who gives money back. Refunds can never add up to more than the original, even when requested concurrently or from repeatable-read transactions.
- **Account controls.** `UpdateAccount` freezes an account (no debits, no new holds that debit it; it can still receive), unfreezes it, or closes it (only when its balance and pending amounts are zero; closed accounts accept nothing, and closing is final), with an optional `StatusReason`. `OverdraftLimit` lets an account without `AllowNegative` go down to minus the limit. `Type` (`asset`, `liability`, `equity`, `revenue`, `expense`) classifies an account for financial statements; it can be set once.
- **Hold expiry.** `HoldParams.ExpiresAt` makes a hold lapse: after it, commits return `ErrHoldExpired`, and `ExpireHolds` (run by `ledgerd` every minute) releases its funds and marks it `expired`.
- **Reversals** (`Reverse`) undo everything not yet refunded of a transaction, with a required reason. They're refunds underneath, so refunds and reversals together never exceed the original.
- **Batches** (`PostBatch`) post up to 1,000 transactions atomically, checking funds once on their combined effect. **Exchanges** (`Exchange`) are batches of two transfers in different currencies, with the rate recorded.
- **Metadata** of accounts and transactions can be updated (`UpdateAccount`, `UpdateTransactionMetadata`); keys set to `null` are removed. Nothing else about a transaction ever changes.
- **Reports:** `ListTransactions` (filter by account, date range and metadata), `TrialBalance`, `BalanceSheet`, `IncomeStatement` and `Statement`.
- **Period close.** `ClosePeriod(ledger, through)` stores the trial balance at that moment, and from then on the database refuses entries dated at or before it. Periods can't be reopened.
- **Events.** Every change writes an event in the same database transaction (`ListEvents`), so integrations never miss one or see one that didn't happen. The `webhook` package delivers them to an HTTP endpoint.
- **Untracked accounts** (`Untracked: true`, which requires `AllowNegative`) take no lock and keep no cached totals, so a busy system account like `@world` never serializes deposits. Their balance is computed from entries on read, and their entries have no `balance_after` or `account_version`. Paging their entries is safe to tail: entries from a database transaction that is still running are withheld until it ends, so nothing can appear behind a cursor. The trade-off is that any long-running transaction on the database delays new entries on untracked accounts. Reading their balance adds up their entries since the last snapshot (`RefreshSnapshots`, run by `ledgerd` every 5 minutes).

## Guarantees

- Each posting locks the tracked accounts it touches (`SELECT … FOR NO KEY UPDATE`, in sorted ID order so it can't deadlock), then writes the transaction, entries and balances in one database transaction. Hold commits/voids and refunds first lock the hold or original transaction, then the accounts.
- The database enforces the invariants on its own, independent of the Go code:
  - ID format checks.
  - `balance = credit_total - debit_total`.
  - No overdraft unless allowed, counting pending holds.
  - Immutable entries; for transactions, only `metadata` can change.
  - Holds move only from `pending` to `committed`, `voided` or `expired`, never back.
  - An account's currency and tracking mode can't change; its type can be set once; closing is final, and only an empty account can be closed.
  - Frozen accounts can't be debited and closed accounts can't take any entry.
  - Overdraft limits: no account goes below minus its limit.
  - Nothing can be posted into a closed period, and periods can't be reopened.
  - Batches and period closes can't be changed or deleted; events can't be changed.
  - A currency's decimals can be set once and never changed; currencies can't be deleted.
  - A deferred trigger rejects any transaction that is unbalanced or has fewer than 2 entries at COMMIT.
  - Entries can only be added by the database transaction that created their transaction, and must match its ledger and currency.

## Operations

### Database and migrations

Everything lives in the `ledger` Postgres schema, so it never collides with the host application's tables. `ledgerd` runs `Migrate` on every start; library users call `svc.Migrate(ctx)` themselves.

- Applied versions are recorded in `ledger.schema_migrations`. Each migration commits together with its record.
- A Postgres advisory lock makes concurrent `Migrate` calls (several replicas starting at once) wait their turn. If a process dies, the lock is released automatically.
- `MigrateDown` reverts the latest migration, for operators and tests.

Upgrade notes:

- **`0004` (amounts to `NUMERIC(78,0)`)** rewrites the `accounts` and `entries` tables under an exclusive lock. On a large ledger, run it in a maintenance window. Reverting it is refused while any amount is too large for `BIGINT`.
- **`0005` (currencies)** registers currencies already used by accounts, without decimals. Amounts keep working for them, but decimal values return `unknown_currency` until you register their decimals (allowed once). New accounts need a registered currency, so add a `RegisterCurrency` call to your setup. Reverting `0005` is refused once any currency has decimals registered.
- **`0006` (business features)** adds account status, overdraft limits and types, hold expiry, batches, reversals, period close, events and snapshots. Everything defaults to the previous behaviour. Reverting it is refused while frozen, closed or overdraft accounts, expiring holds, batches, reversals or closed periods exist; events and snapshots are dropped.
- **Rolling deploys across `0005`:** servers still on the previous version can't register currencies. If one of them creates an account in an unregistered currency, it fails with a misleading "ledger not found" (`422 unknown_reference`). Register every currency you use right after the upgrade, before old servers are drained.

### Deployment

- Build a versioned binary with `make build VERSION=v1.2.3`, or an image with `make docker-build VERSION=v1.2.3`.
- Logs are JSON on stdout.
- On `SIGTERM` or `SIGINT`, `/readyz` starts failing, the server waits `LEDGER_SHUTDOWN_DRAIN` (if set), then stops accepting connections and lets in-flight requests finish (up to 15 seconds).
- The token in `LEDGER_API_TOKEN` grants full access. Keep it in a secret store, and put TLS in front of the service (a load balancer or reverse proxy); `ledgerd` itself serves plain HTTP.
- **Health checks:** point liveness probes at `/healthz` and readiness probes at `/readyz`. Readiness fails while the database is unreachable, while the schema is behind the binary, and as soon as shutdown starts, so traffic drains first.
- **Metrics:** scrape `/metrics` with the bearer token. It exposes request counts and latency per route, requests in flight, and database connection pool usage.
- **Background jobs** run in every `ledgerd` replica: expiring holds and refreshing snapshots are safe to run concurrently, and webhook delivery uses a database lock so only one replica delivers at a time.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `address :7710 is already in use by another program` | Another process holds the port. Find it with `lsof -nP -iTCP:7710 -sTCP:LISTEN`, or pick a free port with `LEDGER_ADDR=:7711`. |
| `connect to database: …` | Check `LEDGER_DATABASE_URL`: host, port, user, database name, and `sslmode`. The database must exist (`createdb ledger`). From Docker, use `host.docker.internal` instead of `localhost`. |
| `LEDGER_API_TOKEN is required` | Set a token, or `LEDGER_INSECURE_NO_AUTH=true` if a proxy authenticates requests. |
| `422 unknown_currency` when creating an account | Register the currency first: `POST /currencies`. |
| `422 unknown_currency` when sending `value` | The currency predates migration `0005`; register its decimals once with `POST /currencies`. |
| Requests return `503` under load | Many requests are contending for the same account. Retry with the same reference; consider making system accounts `untracked`. |

## Development

```sh
make help        # list targets
make check       # gofmt, go vet, and all tests with the race detector
make test        # tests only (about 40s)
make build       # bin/ledgerd
```

Tests start an embedded Postgres (downloaded once, cached in `~/.embedded-postgres-go`). To use a local server instead, where each test gets its own throwaway database:

```sh
LEDGER_TEST_DATABASE_URL="postgres://postgres@localhost:5432/postgres?sslmode=disable" go test -race ./...
```

### Project layout

| Path | Contents |
|---|---|
| `*.go` (root) | The `ledger` library: accounts, posting, holds, refunds, currencies, amounts, queries |
| `migrations/` | SQL migrations, embedded in the binary |
| `httpapi/` | The HTTP API (optional; wraps `ledger.Service`) |
| `cmd/ledgerd/` | The standalone service |
| `ledgertest/` | Test helpers for this package and for applications embedding it |
| `webhook/` | Delivers events to an HTTP endpoint, signed and in order |
| `docs/` | Guides, the OpenAPI specification (`openapi.yaml`), and the GitHub Pages site (`index.html`, `api.html`) |
| `tools/site/` | Builds `docs/index.html` (the website's home page) from this handbook: `make site` |
| `LICENSE`, `NOTICE` | The Apache License 2.0 and the copyright notice |
| `AUTHORS` | The project's contributors |


## License

Copyright 2026 The ledger Authors.

Licensed under the [Apache License, Version 2.0](../LICENSE). You may use, modify and distribute this software, including in commercial products, provided you keep the license and the [NOTICE](../NOTICE) file. It is provided "as is", without warranties of any kind.
