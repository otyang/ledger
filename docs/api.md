# HTTP API reference

Every endpoint of `ledgerd`, with its fields, status codes and real example responses. For a gentler introduction, read the [beginner guide](guide.md) first.

The examples were captured from a running server. IDs are shortened for readability (`acc_alice` instead of `acc_01a1304b-d11c-7318-9858-3f67ffa53e7a`) and timestamps are trimmed; everything else is exactly what the server returns.

## Contents

- [Basics](#basics): base URL, authentication, request format, IDs, amounts, idempotency
- [Errors](#errors)
- [Currencies](#currencies): [register](#post-currencies) · [get](#get-currenciescode)
- [Ledgers](#ledgers): [create](#post-ledgers) · [get](#get-ledgersid)
- [Accounts](#accounts): [create](#post-accounts) · [get](#get-accountsid) · [entries](#get-accountsidentries) · [balance](#get-accountsidbalance)
- [Transactions](#transactions): [post](#post-transactions) · [get](#get-transactionsid)
- [Refunds](#refunds): [refund](#post-transactionsidrefunds) · [refundable](#get-transactionsidrefundable)
- [Holds](#holds): [create](#post-holds) · [get](#get-holdsid) · [commit](#post-holdsidcommit) · [void](#post-holdsidvoid)
- [Account controls](#account-controls): [freeze, close, overdraft, type, metadata](#patch-accountsid)
- [Statements](#statements): [JSON](#get-accountsidstatement) · [CSV](#get-accountsidstatementcsv)
- [Corrections](#corrections): [metadata](#patch-transactionsid) · [reversals](#post-transactionsidreversals)
- [Batches and exchanges](#batches-and-exchanges): [batch](#post-batches) · [get](#get-batchesid) · [exchange](#post-exchanges)
- [Search and reports](#search-and-reports): [search](#get-ledgersidtransactions) · [trial balance](#get-ledgersidtrial-balance) · [financial statements](#financial-statements)
- [Period close](#period-close): [close](#post-ledgersidperiods) · [list](#get-ledgersidperiods)
- [Events](#events): [list](#get-events) · [webhooks](#webhooks)
- [Operations](#operations): [health](#get-healthz) · [readiness](#get-readyz) · [metrics](#get-metrics) · [rate limiting](#rate-limiting)
- [Object reference](#object-reference)

## Basics

### Base URL

`http://localhost:7710` by default. Change the port with `LEDGER_ADDR`. The service speaks plain HTTP; put TLS in front of it in production.

### Authentication

Every request needs the token from `LEDGER_API_TOKEN`:



A missing or wrong token returns `401`:

```json
{
  "error": {
    "code": "unauthorized",
    "message": "missing or invalid bearer token"
  }
}
```

### Request format

- Bodies are JSON objects, at most 1 MiB.
- **Unknown fields are rejected** with `400 invalid_params`, so a typo like `"refrence"` fails instead of being silently ignored.
- Anything after the JSON object (such as a second object) is rejected.
- `metadata`, where accepted, is any JSON object. It is stored and returned as is.

### IDs

All IDs are `<prefix>_<uuidv7>`, for example `txn_01a1304b-d17b-753f-a3ad-9d31fdb0e2f4`. A malformed ID in a URL or body returns `400 invalid_id`.

| Prefix | Object |
|---|---|
| `ldg_` | Ledger |
| `acc_` | Account |
| `txn_` | Transaction |
| `ent_` | Entry |
| `hld_` | Hold |

UUIDv7 IDs sort by creation time.

### Amounts and values

| Form | Type | Example (USD) | Rules |
|---|---|---|---|
| `amount` | string or integer | `"5000"` | Whole number of the smallest unit (cents). Up to 78 digits. Responses always use strings. |
| `value` | string | `"50.00"` | Decimal in the currency's registered decimals. Never rounded: `"50.001"` is rejected; `"50.000"` is accepted. Digits and one `.` only: no `+`, `,`, `$`, exponents or spaces. |

- Wherever a request takes an `amount`, it also takes a `value`. If you send both, they must agree.
- Every amount field in a response has a `*_value` twin: `amount`/`value`, `balance`/`balance_value`, `balance_after`/`balance_after_value`, and so on.
- `*_value` fields are left out if the currency's decimals aren't registered (currencies in use before migration `0005`).

### Idempotency

Posting a transaction, creating a hold and refunding all require a `reference`: a string of 1–255 characters, unique per ledger, that names the business event.

| Situation | Result |
|---|---|
| Same reference, same request | The original object, with the same status code as the first time. Nothing is applied twice. |
| Same reference, different request | `409 reference_conflict` |
| Leg order differs | Same request (legs are compared after sorting and merging) |
| `value` vs equivalent `amount` | Same request |

Hold commits have no reference: a hold is committed at most once, and retrying the same commit returns the same transaction.

### Pagination

`GET /accounts/{id}/entries` is the only list endpoint. It uses a cursor: pass the `id` of the last entry you received as `after`. Results never change behind a cursor, so it is safe to poll for new entries.

## Errors

```json
{
  "error": {
    "code": "insufficient_funds",
    "message": "ledger: insufficient funds: account acc_alice"
  }
}
```

Branch on `code`. `message` is for people and may change between versions.

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_params` | Bad JSON, unknown field, missing or invalid field, too many decimals, zero or negative amount, `amount` and `value` disagree |
| 400 | `invalid_id` | An ID isn't `<prefix>_<uuidv7>`, or has the wrong prefix |
| 400 | `amount_overflow` | An amount, or an account's resulting total, would exceed 78 digits |
| 401 | `unauthorized` | Missing or wrong token |
| 404 | `not_found` | The object in the URL doesn't exist |
| 409 | `already_exists` | An account with this indicator and currency already exists in the ledger |
| 409 | `reference_conflict` | The reference was used for a different request |
| 409 | `hold_not_pending` | The hold is already committed or voided, or was committed with different parameters |
| 409 | `currency_conflict` | The currency is registered with different decimals |
| 409 | `hold_expired` | The hold passed its `expires_at` and can't be committed |
| 409 | `period_closed` | The ledger is closed through a later moment |
| 422 | `unknown_reference` | A ledger or account named in the body doesn't exist |
| 422 | `unknown_currency` | The currency isn't registered, or (for `value`) its decimals aren't |
| 422 | `unbalanced` | Debits don't equal credits |
| 422 | `currency_mismatch` | An account's currency isn't the transaction's |
| 422 | `ledger_mismatch` | The accounts belong to different ledgers |
| 422 | `insufficient_funds` | An account's available balance would go below zero |
| 422 | `refund_exceeded` | More than what's left to refund, or nothing is left |
| 422 | `account_frozen` | Money can't leave a frozen account |
| 422 | `account_closed` | The account is closed |
| 429 | `rate_limited` | Too many requests from this client. Has `Retry-After` |
| 503 | `retryable` | A concurrent request got in the way. Retry the same request. Has `Retry-After: 1` |
| 503 | `timeout` | The request took longer than `LEDGER_REQUEST_TIMEOUT`. Retry the same request. Has `Retry-After: 1` |
| 500 | `internal` | Unexpected failure. Details are in the server log |

A request that returns an error applied nothing. After a `503` or a network failure, retrying with the same reference is always safe.

## Currencies

### `POST /currencies`

Registers a currency and how many decimals it has. Required before any account can use the currency.

| Field | Type | Required | Description |
|---|---|---|---|
| `code` | string | yes | 3–10 characters: an uppercase letter, then uppercase letters or digits. `USD`, `ETH`, `USDC` |
| `decimals` | integer | yes | 0–36. USD 2, JPY 0, ETH 18. **Can never be changed.** |

```http
POST /currencies

{
  "code": "USD",
  "decimals": 2
}
```

Response: `201 Created`

```json
{
  "code": "USD",
  "decimals": 2,
  "created_at": "2026-10-12T13:27:17.433Z"
}
```

| Case | Result |
|---|---|
| Already registered with the same decimals | `201` with the existing currency |
| Already registered with different decimals | `409 currency_conflict` |
| In use from before migration `0005` without decimals | Sets the decimals (once), `201` |
| `decimals` missing | `400 invalid_params` (it never defaults to 0) |

### `GET /currencies/{code}`

Response: `200 OK`

```json
{
  "code": "USD",
  "decimals": 2,
  "created_at": "2026-10-12T13:27:17.433Z"
}
```

`decimals` is `null` for a currency from before migration `0005` whose decimals haven't been registered yet. Unknown code: `404 not_found`.

## Ledgers

### `POST /ledgers`

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Any non-empty name |
| `metadata` | object | no | Your own data |

```http
POST /ledgers

{
  "name": "main",
  "metadata": {
    "team": "payments"
  }
}
```

Response: `201 Created`

```json
{
  "id": "ldg_main",
  "name": "main",
  "metadata": {
    "team": "payments"
  },
  "created_at": "2026-10-12T13:27:17.446Z"
}
```

### `GET /ledgers/{id}`

Returns the same object. Unknown ID: `404 not_found`.

## Accounts

### `POST /accounts`

| Field | Type | Required | Description |
|---|---|---|---|
| `ledger_id` | string | yes | The ledger this account belongs to |
| `currency` | string | yes | A registered currency. Can't change later |
| `indicator` | string | no | A readable name like `@world` or `@fees:card`: `@` followed by letters, digits or `_ . : -`. Unique per ledger and currency |
| `allow_negative` | boolean | no | Whether the balance may go below zero. Default `false` |
| `untracked` | boolean | no | No locking; balance computed on read. Requires `allow_negative: true`. Default `false`. Can't change later |
| `overdraft_limit` or `overdraft_limit_value` | string | no | Lets the account (without `allow_negative`) go down to minus this amount |
| `type` | string | no | `asset`, `liability`, `equity`, `revenue` or `expense`, for [financial statements](#financial-statements). Can be set once |
| `metadata` | object | no | Your own data |

```http
POST /accounts

{
  "ledger_id": "ldg_main",
  "currency": "USD",
  "indicator": "@world",
  "allow_negative": true,
  "untracked": true
}
```

Response: `201 Created`

```json
{
  "id": "acc_world",
  "ledger_id": "ldg_main",
  "indicator": "@world",
  "currency": "USD",
  "allow_negative": true,
  "untracked": true,
  "credit_total": "0",
  "credit_total_value": "0.00",
  "debit_total": "0",
  "debit_total_value": "0.00",
  "balance": "0",
  "balance_value": "0.00",
  "pending_debit": "0",
  "pending_debit_value": "0.00",
  "pending_credit": "0",
  "pending_credit_value": "0.00",
  "version": 0,
  "created_at": "2026-10-12T13:27:17.503Z",
  "updated_at": "2026-10-12T13:27:17.503Z"
}
```

| Error | When |
|---|---|
| `422 unknown_currency` | The currency isn't registered |
| `422 unknown_reference` | The ledger doesn't exist |
| `409 already_exists` | The indicator is taken in this ledger and currency |
| `400 invalid_params` | `untracked` without `allow_negative`, or a malformed indicator or currency |

To get or create a named account in one step from Go, use `EnsureIndicatorAccount`.

### `GET /accounts/{id}`

The account with its current balances. Alice, after a 100.00 top-up, a 50.00 purchase, a 47.00 refund and a pending 20.00 hold:

Response: `200 OK`

```json
{
  "id": "acc_alice",
  "ledger_id": "ldg_main",
  "currency": "USD",
  "allow_negative": false,
  "untracked": false,
  "credit_total": "14700",
  "credit_total_value": "147.00",
  "debit_total": "5000",
  "debit_total_value": "50.00",
  "balance": "9700",
  "balance_value": "97.00",
  "pending_debit": "2000",
  "pending_debit_value": "20.00",
  "pending_credit": "0",
  "pending_credit_value": "0.00",
  "version": 3,
  "metadata": {
    "owner": "alice"
  },
  "created_at": "2026-10-12T13:27:17.532Z",
  "updated_at": "2026-10-12T13:27:17.736Z"
}
```

What she can still spend is `balance − pending_debit` = 77.00. For untracked accounts, the totals are computed from all of the account's entries on every read, so this is slower for accounts with very many entries.

### `GET /accounts/{id}/entries`

The account's entries, oldest first.

| Query | Default | Description |
|---|---|---|
| `limit` | 100 | Page size, capped at 1000 |
| `after` | | ID of the last entry from the previous page |

```http
GET /accounts/acc_alice/entries?limit=1
```

Response: `200 OK`

```json
{
  "entries": [
    {
      "id": "ent_1",
      "transaction_id": "txn_deposit",
      "account_id": "acc_alice",
      "direction": "credit",
      "amount": "10000",
      "value": "100.00",
      "balance_after": "10000",
      "balance_after_value": "100.00",
      "account_version": 1,
      "created_at": "2026-10-12T13:27:17.613Z"
    }
  ]
}
```

Next page: `GET /accounts/acc_alice/entries?after=ent_1`. An empty `entries` array means you've reached the end (for now). An `after` that isn't an entry of this account returns `400 invalid_params`.

For untracked accounts, entries have no `balance_after` or `account_version`, and entries from database transactions still in progress are held back until they finish, so the list never changes behind a cursor.

### `GET /accounts/{id}/balance`

The balance at a moment in time.

| Query | Default | Description |
|---|---|---|
| `at` | now | RFC 3339 timestamp, e.g. `2026-10-12T12:00:00Z` |

Response: `200 OK`

```json
{
  "account_id": "acc_alice",
  "at": "2020-01-01T00:00:00Z",
  "balance": "0",
  "balance_value": "0.00"
}
```

This is the posted balance; it ignores holds.

## Transactions

### `POST /transactions`

| Field | Type | Required | Description |
|---|---|---|---|
| `reference` | string | yes | Idempotency key, 1–255 characters, unique per ledger |
| `currency` | string | yes | Every account must be in this currency |
| `legs` | array | yes | At least 2 legs, across at least 2 accounts, debits = credits |
| `description` | string | no | Free text |
| `metadata` | object | no | Your own data |

Each leg:

| Field | Type | Required | Description |
|---|---|---|---|
| `account_id` | string | yes | All legs must be in the same ledger |
| `direction` | string | yes | `debit` (balance down) or `credit` (balance up) |
| `amount` or `value` | string | one of them | Must be positive |

The same account may appear in several legs. Funds are checked on each account's net result, so an account can be debited and credited in one transaction.

```http
POST /transactions

{
  "reference": "order-8841",
  "currency": "USD",
  "description": "Concert ticket",
  "metadata": {
    "order_id": "8841"
  },
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "debit",
      "value": "50.00"
    },
    {
      "account_id": "acc_venue",
      "direction": "credit",
      "value": "47.00"
    },
    {
      "account_id": "acc_fees",
      "direction": "credit",
      "amount": "300"
    }
  ]
}
```

Response: `201 Created`

```json
{
  "id": "txn_ticket",
  "ledger_id": "ldg_main",
  "reference": "order-8841",
  "status": "posted",
  "currency": "USD",
  "description": "Concert ticket",
  "metadata": {
    "order_id": "8841"
  },
  "created_at": "2026-10-12T13:27:17.627Z",
  "entries": [
    {
      "id": "ent_a",
      "transaction_id": "txn_ticket",
      "account_id": "acc_alice",
      "direction": "debit",
      "amount": "5000",
      "value": "50.00",
      "balance_after": "5000",
      "balance_after_value": "50.00",
      "account_version": 2,
      "created_at": "2026-10-12T13:27:17.627Z"
    },
    {
      "id": "ent_b",
      "transaction_id": "txn_ticket",
      "account_id": "acc_venue",
      "direction": "credit",
      "amount": "4700",
      "value": "47.00",
      "balance_after": "4700",
      "balance_after_value": "47.00",
      "account_version": 1,
      "created_at": "2026-10-12T13:27:17.627Z"
    },
    {
      "id": "ent_c",
      "transaction_id": "txn_ticket",
      "account_id": "acc_fees",
      "direction": "credit",
      "amount": "300",
      "value": "3.00",
      "balance_after": "300",
      "balance_after_value": "3.00",
      "account_version": 1,
      "created_at": "2026-10-12T13:27:17.627Z"
    }
  ]
}
```

Entries are returned in leg order. A retry with the same reference and legs returns this same body with `201`.

| Error | Example message |
|---|---|
| `422 insufficient_funds` | `ledger: insufficient funds: account acc_alice` |
| `409 reference_conflict` | `ledger: reference already used for a different transaction: "order-8841"` |
| `422 unbalanced` | `ledger: debits and credits do not balance: debits 100, credits 200` |
| `400 invalid_params` | `leg 0: ledger: invalid params: value "1.001" has more than 2 decimals` |
| `422 currency_mismatch` | An account isn't in the transaction's currency |
| `422 ledger_mismatch` | Accounts from different ledgers |
| `422 unknown_reference` | An account doesn't exist |
| `422 unknown_currency` | `value` used with a currency without registered decimals |

### `GET /transactions/{id}`

Returns the transaction in the same shape, including entries. Refunds have `refund_of`; hold commits have `hold_id` and no `reference`.

## Refunds

### `POST /transactions/{id}/refunds`

Posts a new transaction that reverses all or part of transaction `id`.

| Field | Type | Required | Description |
|---|---|---|---|
| `reference` | string | yes | Idempotency key. Each partial refund needs its own |
| `amount` or `value` | string | no | Refund this much. Only for two-leg transactions |
| `legs` | array | no | Refund exactly these legs. Each must reverse a leg of the original, within what is left |
| `description` | string | no | |
| `metadata` | object | no | |

Send at most one of `amount`/`value` and `legs`. Send neither to refund everything that is left. An explicit zero is rejected; it never means "everything".

Returning the venue's 47.00 of the three-leg ticket, while the platform keeps its fee:

```http
POST /transactions/txn_ticket/refunds

{
  "reference": "refund-1",
  "legs": [
    {
      "account_id": "acc_venue",
      "direction": "debit",
      "value": "47.00"
    },
    {
      "account_id": "acc_alice",
      "direction": "credit",
      "value": "47.00"
    }
  ]
}
```

Response: `201 Created`

```json
{
  "id": "txn_refund",
  "ledger_id": "ldg_main",
  "reference": "refund-1",
  "status": "posted",
  "currency": "USD",
  "refund_of": "txn_ticket",
  "created_at": "2026-10-12T13:27:17.719Z",
  "entries": [
    {
      "id": "ent_d",
      "transaction_id": "txn_refund",
      "account_id": "acc_venue",
      "direction": "debit",
      "amount": "4700",
      "value": "47.00",
      "balance_after": "0",
      "balance_after_value": "0.00",
      "account_version": 2,
      "created_at": "2026-10-12T13:27:17.719Z"
    },
    {
      "id": "ent_e",
      "transaction_id": "txn_refund",
      "account_id": "acc_alice",
      "direction": "credit",
      "amount": "4700",
      "value": "47.00",
      "balance_after": "9700",
      "balance_after_value": "97.00",
      "account_version": 3,
      "created_at": "2026-10-12T13:27:17.719Z"
    }
  ]
}
```

| Error | When |
|---|---|
| `422 refund_exceeded` | More than what's left, or nothing left |
| `400 invalid_params` | Zero amount, `amount` with a multi-leg original, both `amount` and `legs`, or the original is itself a refund |
| `422 insufficient_funds` | The account giving money back doesn't have it |
| `404 not_found` | Transaction `id` doesn't exist |

Refunds of the same transaction are processed one at a time, so concurrent refunds can never add up to more than the original.

### `GET /transactions/{id}/refundable`

The legs a full refund would post right now. After the refund above, only the fee is left:

Response: `200 OK`

```json
{
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "credit",
      "amount": "300",
      "value": "3.00"
    },
    {
      "account_id": "acc_fees",
      "direction": "debit",
      "amount": "300",
      "value": "3.00"
    }
  ]
}
```

An empty `legs` array means everything has been refunded.

## Holds

A hold reserves money without moving it. While it is pending, its debit legs count against the source accounts' available balance (`pending_debit`), and its credit legs show as `pending_credit` on the destinations. A hold only expires if you give it an `expires_at`.

### `POST /holds`

Same fields as [`POST /transactions`](#post-transactions): `reference`, `currency`, `legs`, `description`, `metadata`, plus:

| Field | Type | Required | Description |
|---|---|---|---|
| `expires_at` | timestamp | no | When the hold lapses, like a card authorization (typically 7 days). After it, commits return `409 hold_expired`, and `ledgerd` releases the funds within a minute, setting `status` to `expired`. Must be in the future. Not part of the idempotency check, so a retry may compute it again |

```http
POST /holds

{
  "reference": "auth-9",
  "currency": "USD",
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "debit",
      "value": "20.00"
    },
    {
      "account_id": "acc_venue",
      "direction": "credit",
      "value": "20.00"
    }
  ]
}
```

Response: `201 Created`

```json
{
  "id": "hld_ride",
  "ledger_id": "ldg_main",
  "reference": "auth-9",
  "status": "pending",
  "currency": "USD",
  "created_at": "2026-10-12T13:27:17.736Z",
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "debit",
      "amount": "2000",
      "value": "20.00"
    },
    {
      "account_id": "acc_venue",
      "direction": "credit",
      "amount": "2000",
      "value": "20.00"
    }
  ]
}
```

Fails with `422 insufficient_funds` if the reservation would take an account's available balance below zero.

### `GET /holds/{id}`

A committed hold has `transaction_id` and `resolved_at`:

Response: `200 OK`

```json
{
  "id": "hld_ride",
  "ledger_id": "ldg_main",
  "reference": "auth-9",
  "status": "committed",
  "currency": "USD",
  "transaction_id": "txn_capture",
  "created_at": "2026-10-12T13:27:17.736Z",
  "resolved_at": "2026-10-12T13:27:17.772Z",
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "debit",
      "amount": "2000",
      "value": "20.00"
    },
    {
      "account_id": "acc_venue",
      "direction": "credit",
      "amount": "2000",
      "value": "20.00"
    }
  ]
}
```

`status` is `pending`, `committed` or `voided`. `legs` always shows what was reserved, not what was committed.

### `POST /holds/{id}/commit`

Posts all or part of the hold as a transaction and releases the rest. The body is optional.

| Body | Commits |
|---|---|
| empty, or `{}` | The full hold |
| `{"value": "15.00"}` or `{"amount": "1500"}` | That much, on both legs of a two-leg hold |
| `{"legs": [...]}` | Exactly these legs of a multi-leg hold; each (account, direction) at most what was held |

`description` and `metadata` may also be given; they default to the hold's.

```http
POST /holds/hld_ride/commit

{
  "value": "15.00"
}
```

Response: `201 Created`

```json
{
  "id": "txn_capture",
  "ledger_id": "ldg_main",
  "status": "posted",
  "currency": "USD",
  "hold_id": "hld_ride",
  "created_at": "2026-10-12T13:27:17.772Z",
  "entries": [
    {
      "id": "ent_f",
      "transaction_id": "txn_capture",
      "account_id": "acc_alice",
      "direction": "debit",
      "amount": "1500",
      "value": "15.00",
      "balance_after": "8200",
      "balance_after_value": "82.00",
      "account_version": 4,
      "created_at": "2026-10-12T13:27:17.772Z"
    },
    {
      "id": "ent_g",
      "transaction_id": "txn_capture",
      "account_id": "acc_venue",
      "direction": "credit",
      "amount": "1500",
      "value": "15.00",
      "balance_after": "1500",
      "balance_after_value": "15.00",
      "account_version": 3,
      "created_at": "2026-10-12T13:27:17.772Z"
    }
  ]
}
```

| Case | Result |
|---|---|
| Same commit again | `201` with the same transaction |
| Different commit of a committed hold | `409 hold_not_pending` |
| Commit of a voided hold | `409 hold_not_pending` |
| More than was held | `400 invalid_params` |
| Zero amount or value | `400 invalid_params` |

### `POST /holds/{id}/void`

Releases a pending hold without posting anything. No body.

Response: `200 OK`

```json
{
  "id": "hld_cancelled",
  "ledger_id": "ldg_main",
  "reference": "auth-10",
  "status": "voided",
  "currency": "USD",
  "created_at": "2026-10-12T13:27:17.787Z",
  "resolved_at": "2026-10-12T13:27:17.816Z",
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "debit",
      "amount": "500",
      "value": "5.00"
    },
    {
      "account_id": "acc_venue",
      "direction": "credit",
      "amount": "500",
      "value": "5.00"
    }
  ]
}
```

Voiding a voided hold returns it unchanged (`200`). Voiding a committed hold returns `409 hold_not_pending`.

## Account controls

### `PATCH /accounts/{id}`

Changes an account. Every field is optional; fields you leave out don't change. The account is locked while it changes, so a status change can't interleave with a posting.

| Field | Type | Description |
|---|---|---|
| `status` | string | `active`, `frozen` or `closed` (see below) |
| `status_reason` | string | Why, e.g. `"fraud review"`. Replaced on every status change; a change without it clears the old reason |
| `overdraft_limit` or `overdraft_limit_value` | string | Lets an account without `allow_negative` go down to minus this amount. `"0"` removes the limit. It can't be set below what the account already owes |
| `type` | string | `asset`, `liability`, `equity`, `revenue` or `expense`, for [financial statements](#financial-statements). Can be set once |
| `metadata` | object | Merged into the existing metadata. A key set to `null` is removed |

| Status | Money in | Money out | Notes |
|---|---|---|---|
| `active` | yes | yes | The default |
| `frozen` | yes | **no** | Debits and new holds that debit the account return `422 account_frozen`. Voids and expiries still release funds. Unfreeze by setting `active` |
| `closed` | **no** | **no** | Only an account with a zero balance and nothing pending can be closed. Anything that touches it returns `422 account_closed`. **Closing is final** |

```http
PATCH /accounts/acc_alice

{
  "status": "frozen",
  "status_reason": "fraud review"
}
```

Response: `200 OK`

```json
{
  "id": "acc_alice",
  "ledger_id": "ldg_main",
  "currency": "USD",
  "allow_negative": false,
  "overdraft_limit": "2500",
  "untracked": false,
  "status": "frozen",
  "status_reason": "fraud review",
  "type": "liability",
  "credit_total": "10000",
  "debit_total": "0",
  "balance": "10000",
  "pending_debit": "0",
  "pending_credit": "0",
  "version": 1,
  "created_at": "2026-10-12T14:07:35.765Z",
  "updated_at": "2026-10-12T14:07:35.982Z",
  "credit_total_value": "100.00",
  "debit_total_value": "0.00",
  "balance_value": "100.00",
  "pending_debit_value": "0.00",
  "pending_credit_value": "0.00",
  "overdraft_limit_value": "25.00"
}
```

`overdraft_limit` and `type` can also be given when [creating](#post-accounts) the account.

## Statements

### `GET /accounts/{id}/statement`

An account's entries in a period, like a bank statement: the balance before the period, every entry with the running balance after it, and the balance at the end.

| Query | Required | Description |
|---|---|---|
| `from` | yes | Start, inclusive (RFC 3339) |
| `to` | no | End, exclusive. Default: now |

A period with more than 100,000 entries is refused (`400`); ask for a shorter one.

```http
GET /accounts/acc_alice/statement?from=2026-10-01T00:00:00Z
```

Response: `200 OK`

```json
{
  "account": {
    "id": "acc_alice",
    "currency": "USD",
    "balance": "1850",
    "balance_value": "18.50"
  },
  "from": "2026-10-01T00:00:00Z",
  "to": "2026-10-12T14:07:36.102Z",
  "opening_balance": "0",
  "opening_balance_value": "0.00",
  "closing_balance": "1850",
  "closing_balance_value": "18.50",
  "lines": [
    {
      "entry_id": "ent_1",
      "transaction_id": "txn_deposit",
      "reference": "deposit-1",
      "direction": "credit",
      "amount": "10000",
      "balance": "10000",
      "created_at": "2026-10-12T14:07:35.940Z",
      "value": "100.00",
      "balance_value": "100.00"
    },
    {
      "entry_id": "ent_2",
      "transaction_id": "txn_payout",
      "reference": "payout-bob",
      "direction": "debit",
      "amount": "3000",
      "balance": "7000",
      "created_at": "2026-10-12T14:07:36.009Z",
      "value": "30.00",
      "balance_value": "70.00"
    }
  ]
}
```

The `account` object is the full [Account](#account); it is shortened here.

### `GET /accounts/{id}/statement.csv`

The same statement as a CSV download (`text/csv`), with an opening and a closing balance row:

```csv
created_at,entry_id,transaction_id,reference,description,direction,amount,value,balance,balance_value
2026-10-01T00:00:00Z,,,,Opening balance,,,,0,0.00
2026-10-12T14:07:35.940849Z,ent_1,txn_deposit,deposit-1,,credit,10000,100.00,10000,100.00
2026-10-12T14:07:36.009584Z,ent_2,txn_payout,payout-bob,,debit,3000,30.00,7000,70.00
2026-10-12T14:07:36.109012Z,,,,Closing balance,,,,7000,70.00
```

References and descriptions starting with `=`, `+`, `-` or `@` are prefixed with `'`, so spreadsheets don't run them as formulas.

## Corrections

### `PATCH /transactions/{id}`

Updates a transaction's metadata, the only part of a transaction that can ever change. The body's `metadata` is merged in; keys set to `null` are removed.

```http
PATCH /transactions/txn_deposit

{
  "metadata": {
    "invoice": "inv-42"
  }
}
```

Response: `200 OK` with the whole transaction, now with `"metadata": {"customer_id": "c_77", "invoice": "inv-42"}`. Metadata isn't part of the idempotency check, so retrying the original request still returns this transaction.

### `POST /transactions/{id}/reversals`

Undoes a transaction that was a mistake: posts the opposite of everything not yet refunded, and records why.

| Field | Type | Required | Description |
|---|---|---|---|
| `reference` | string | yes | Idempotency key |
| `reason` | string | yes | Stored on the reversal as `reversal_reason` |
| `description`, `metadata` | | no | |

```http
POST /transactions/txn_mistake/reversals

{
  "reference": "fix-mistake",
  "reason": "sent to the wrong account"
}
```

Response: `201 Created`

```json
{
  "id": "txn_reversal",
  "ledger_id": "ldg_main",
  "reference": "fix-mistake",
  "status": "posted",
  "currency": "USD",
  "refund_of": "txn_mistake",
  "reversal_reason": "sent to the wrong account",
  "created_at": "2026-10-12T14:07:36.067Z",
  "entries": [
    {
      "id": "ent_r1",
      "transaction_id": "txn_reversal",
      "account_id": "acc_alice",
      "direction": "credit",
      "amount": "1000",
      "balance_after": "1850",
      "account_version": 6,
      "created_at": "2026-10-12T14:07:36.067Z",
      "value": "10.00",
      "balance_after_value": "18.50"
    },
    {
      "id": "ent_r2",
      "transaction_id": "txn_reversal",
      "account_id": "acc_bob",
      "direction": "debit",
      "amount": "1000",
      "balance_after": "3000",
      "account_version": 3,
      "created_at": "2026-10-12T14:07:36.067Z",
      "value": "10.00",
      "balance_after_value": "30.00"
    }
  ]
}
```

A reversal is a refund underneath (`refund_of` is set), so refunds and reversals together can never exceed the original. Reversing a fully refunded or already reversed transaction returns `422 refund_exceeded`.

## Batches and exchanges

### `POST /batches`

Posts up to 1,000 transactions together: all of them or none. Use it for payroll runs, marketplace payouts, or anything that must not be half done.

| Field | Type | Required | Description |
|---|---|---|---|
| `reference` | string | yes | The batch's idempotency key, unique per ledger |
| `transactions` | array | yes | Each like a [`POST /transactions`](#post-transactions) body, except that `reference` is optional |
| `description`, `metadata` | | no | |

- All accounts must be in one ledger. Members may use different currencies.
- Funds are checked once, on the combined effect: an earlier member can fund a later one.
- Retrying returns the original batch. Reusing the reference for a different batch returns `409 reference_conflict`.

```http
POST /batches

{
  "reference": "payout-oct",
  "description": "October payouts",
  "transactions": [
    {
      "reference": "payout-bob",
      "currency": "USD",
      "legs": [
        {
          "account_id": "acc_alice",
          "direction": "debit",
          "value": "30.00"
        },
        {
          "account_id": "acc_bob",
          "direction": "credit",
          "value": "30.00"
        }
      ]
    },
    {
      "currency": "USD",
      "legs": [
        {
          "account_id": "acc_alice",
          "direction": "debit",
          "value": "1.50"
        },
        {
          "account_id": "acc_fees",
          "direction": "credit",
          "value": "1.50"
        }
      ]
    }
  ]
}
```

Response: `201 Created`

```json
{
  "id": "bat_payout",
  "ledger_id": "ldg_main",
  "reference": "payout-oct",
  "kind": "batch",
  "description": "October payouts",
  "created_at": "2026-10-12T14:07:36.009Z",
  "transactions": [
    {
      "id": "txn_b1",
      "ledger_id": "ldg_main",
      "reference": "payout-bob",
      "status": "posted",
      "currency": "USD",
      "batch_id": "bat_payout",
      "created_at": "2026-10-12T14:07:36.009Z",
      "entries": []
    },
    {
      "id": "txn_b2",
      "ledger_id": "ldg_main",
      "status": "posted",
      "currency": "USD",
      "batch_id": "bat_payout",
      "created_at": "2026-10-12T14:07:36.009Z",
      "entries": []
    }
  ]
}
```

The `entries` arrays are emptied here for brevity; the real response contains them, as in [`POST /transactions`](#post-transactions).

### `GET /batches/{id}`

Returns the batch in the same shape.

### `POST /exchanges`

Converts between currencies. An exchange is a batch of two transfers, one per currency, posted together with the rate recorded.

| Field | Type | Required | Description |
|---|---|---|---|
| `reference` | string | yes | Idempotency key |
| `source` | object | yes | What is sold: `currency`, `from`, `to`, and `amount` or `value` |
| `target` | object | yes | What is bought: same fields, in a different currency |
| `rate` | string | no | Recorded as given, e.g. `"0.92"`. Informational: the ledger doesn't derive one amount from the other |
| `description`, `metadata` | | no | |

A typical setup uses one liquidity account per currency (here `@fx`): the customer's USD goes to your USD liquidity account, and EUR comes from your EUR liquidity account to the customer.

```http
POST /exchanges

{
  "reference": "fx-1",
  "rate": "0.92",
  "source": {
    "currency": "USD",
    "from": "acc_alice_usd",
    "to": "acc_fx_usd",
    "value": "50.00"
  },
  "target": {
    "currency": "EUR",
    "from": "acc_fx_eur",
    "to": "acc_alice_eur",
    "value": "46.00"
  }
}
```

Response: `201 Created`, a batch with `"kind": "exchange"`, `"rate": "0.92"`, and two transactions, one in USD and one in EUR.

## Search and reports

### `GET /ledgers/{id}/transactions`

Searches a ledger's transactions, oldest first, with their entries.

| Query | Description |
|---|---|
| `account_id` | Only transactions with an entry on this account |
| `from`, `to` | `from` ≤ `created_at` < `to` (RFC 3339) |
| `metadata.<key>` | Only transactions whose metadata has this key with this string value. Repeat for several keys: `metadata.customer_id=c_77&metadata.channel=web` |
| `after` | ID of the last transaction from the previous page |
| `limit` | Default 100, maximum 1000 |

```http
GET /ledgers/ldg_main/transactions?metadata.customer_id=c_77
```

Response: `200 OK`, `{"transactions": [...]}` with full [transactions](#transaction).

This is for browsing and support tools. To follow new transactions reliably as they happen, use [events](#events), which never skip one.

### `GET /ledgers/{id}/trial-balance`

Every account in the ledger with its posted balance at a moment (holds aren't included), and the total per currency. **The totals are always zero.**

| Query | Description |
|---|---|
| `at` | RFC 3339. Default: now |

Response: `200 OK`

```json
{
  "ledger_id": "ldg_main",
  "at": "2026-10-12T14:07:36.082Z",
  "accounts": [
    {
      "account_id": "acc_world",
      "indicator": "@world",
      "type": "asset",
      "currency": "USD",
      "balance": "-10000",
      "balance_value": "-100.00"
    },
    {
      "account_id": "acc_alice",
      "type": "liability",
      "currency": "USD",
      "balance": "1850",
      "balance_value": "18.50"
    }
  ],
  "totals": [
    {
      "currency": "USD",
      "total": "0",
      "total_value": "0.00"
    }
  ]
}
```

### Financial statements

The balance sheet and income statement group accounts by their `type`, one currency at a time. Amounts are shown the way accountants expect: **positive in the account's normal direction**.

| Type | Normal direction | Shown as | Typical accounts |
|---|---|---|---|
| `asset` | debit | `−balance` | `@world` (money you hold at banks and processors) |
| `expense` | debit | `−balance` | Rent, processing costs |
| `liability` | credit | `balance` | Customer wallets, amounts owed to merchants |
| `equity` | credit | `balance` | Owner's capital |
| `revenue` | credit | `balance` | Fees you earn |

Accounts without a type appear under `unclassified`, at their plain balance.

#### `GET /ledgers/{id}/balance-sheet`

| Query | Required | Description |
|---|---|---|
| `currency` | yes | |
| `at` | no | Default: now |

Response: `200 OK`

```json
{
  "ledger_id": "ldg_main",
  "currency": "USD",
  "at": "2026-10-12T14:07:36.089Z",
  "assets": {
    "accounts": [
      {
        "account_id": "acc_world",
        "indicator": "@world",
        "amount": "10000",
        "amount_value": "100.00"
      }
    ],
    "total": "10000",
    "total_value": "100.00"
  },
  "liabilities": {
    "accounts": [
      {
        "account_id": "acc_alice",
        "amount": "9850",
        "amount_value": "98.50"
      }
    ],
    "total": "9850",
    "total_value": "98.50"
  },
  "equity": {
    "accounts": [],
    "total": "0",
    "total_value": "0.00"
  },
  "net_income": "150",
  "net_income_value": "1.50",
  "unclassified": {
    "accounts": [],
    "total": "0",
    "total_value": "0.00"
  }
}
```

It always satisfies **assets = liabilities + equity + net_income + unclassified**, where `net_income` is revenue minus expenses to date.

#### `GET /ledgers/{id}/income-statement`

Revenue and expenses over a period: the change in each such account after `from`, up to and including `to`.

| Query | Required | Description |
|---|---|---|
| `currency` | yes | |
| `from` | yes | RFC 3339 |
| `to` | no | Default: now |

Response: `200 OK`

```json
{
  "ledger_id": "ldg_main",
  "currency": "USD",
  "from": "2026-10-01T00:00:00Z",
  "to": "2026-10-12T14:07:36.095Z",
  "revenue": {
    "accounts": [
      {
        "account_id": "acc_fees",
        "indicator": "@fees",
        "amount": "150",
        "amount_value": "1.50"
      }
    ],
    "total": "150",
    "total_value": "1.50"
  },
  "expenses": {
    "accounts": [],
    "total": "0",
    "total_value": "0.00"
  },
  "net_income": "150",
  "net_income_value": "1.50"
}
```

## Period close

### `POST /ledgers/{id}/periods`

Closes the books through a moment, for example the end of September. From then on, **the database refuses any entry dated at or before it**, and the trial balance at that moment is stored with the close, so the figures finance signed off on can always be retrieved.

| Field | Type | Required | Description |
|---|---|---|---|
| `through` | timestamp | yes | The end of the period. Must be at least one minute in the past, so postings in flight can finish |

```http
POST /ledgers/ldg_main/periods

{
  "through": "2026-09-30T23:59:59.999999Z"
}
```

Response: `201 Created`

```json
{
  "ledger_id": "ldg_main",
  "closed_through": "2026-09-30T23:59:59.999999Z",
  "closed_at": "2026-10-12T14:07:36.162Z",
  "trial_balance": {
    "ledger_id": "ldg_main",
    "at": "2026-09-30T23:59:59.999999Z",
    "accounts": [],
    "totals": []
  }
}
```

(`accounts` and `totals` are the full [trial balance](#get-ledgersidtrial-balance) at that moment; emptied here for brevity.)

| Case | Result |
|---|---|
| Closing the same moment again | `201` with the existing close |
| A moment before the current close | `409 period_closed`. Periods can't be reopened |
| Less than a minute ago | `400 invalid_params` |

The ledger's `closed_through` field shows the current close.

### `GET /ledgers/{id}/periods`

All closes of the ledger, oldest first: `{"periods": [...]}`.

## Events

Every change writes an event, in the same database transaction as the change itself: an event exists exactly when its change does. Integrations (notifications, analytics, accounting software, data warehouses) can follow them instead of polling.

| Type | When | `data` |
|---|---|---|
| `currency.registered` | A currency is registered | Currency |
| `ledger.created` | | Ledger |
| `account.created` | | Account |
| `account.updated` | Status, limit, type or metadata changed | Account |
| `transaction.posted` | Any transaction: payments, refunds, reversals, hold commits, batch members | Transaction |
| `transaction.updated` | Metadata changed | Transaction |
| `hold.created` | | Hold |
| `hold.committed` | | Hold |
| `hold.voided` | | Hold |
| `hold.expired` | | Hold |
| `batch.posted` | A batch or exchange | Batch |
| `period.closed` | | Period close |

`data` holds the object after the change, in the same JSON as the API returns, without the `*_value` fields.

### `GET /events`

| Query | Description |
|---|---|
| `after` | ID of the last event you processed. Leave out to start from the oldest |
| `ledger_id` | Only this ledger's events |
| `limit` | Default 100, maximum 1000 |

```http
GET /events?ledger_id=ldg_main&limit=2
```

Response: `200 OK`

```json
{
  "events": [
    {
      "id": "evt_1",
      "type": "ledger.created",
      "ledger_id": "ldg_main",
      "subject_id": "ldg_main",
      "data": {
        "id": "ldg_main",
        "name": "main",
        "created_at": "2026-10-12T14:07:35.699Z"
      },
      "created_at": "2026-10-12T14:07:35.699Z"
    },
    {
      "id": "evt_2",
      "type": "account.created",
      "ledger_id": "ldg_main",
      "subject_id": "acc_world",
      "data": {
        "id": "acc_world",
        "indicator": "@world",
        "currency": "USD",
        "status": "active"
      },
      "created_at": "2026-10-12T14:07:35.736Z"
    }
  ]
}
```

Events come in a fixed order, and **an event never appears behind a cursor you've already passed**, so polling with `after` never misses one. An event shows up once every older database transaction has finished; a long-running transaction elsewhere in the database delays new events.

### Webhooks

Set `LEDGER_WEBHOOK_URL` and `LEDGER_WEBHOOK_SECRET`, and `ledgerd` POSTs every event to that URL, one at a time, in order:

```http
POST /your-endpoint
Content-Type: application/json
X-Ledger-Event-Id: evt_1
X-Ledger-Event-Type: transaction.posted
X-Ledger-Signature: t=1760278056,v1=5f2b…

{
  "id": "evt_1",
  "type": "transaction.posted",
  "ledger_id": "ldg_main",
  "subject_id": "txn_deposit",
  "data": {
    "id": "txn_deposit"
  },
  "created_at": "2026-10-12T14:07:35.940Z"
}
```

- **Answer with any 2xx** to acknowledge. Anything else, or no answer within 10 seconds, is retried with backoff (1 s, doubling up to 1 minute) until it succeeds. Later events wait, so order is preserved.
- **Delivery is at least once**: deduplicate on `X-Ledger-Event-Id`.
- **Verify the signature**: compute HMAC-SHA256 of `<t>.<raw body>` with the secret, hex-encode it, compare it to `v1`, and reject old `t` values to block replays. In Go, `webhook.Sign(secret, t, body)` computes it.
- Progress is stored in the database, so deliveries resume after a restart. With several replicas, only one delivers at a time, using a Postgres session lock; connect `ledgerd` to Postgres directly, not through a transaction-mode pooler such as PgBouncer.
- **Redirects aren't followed.** A `3xx` answer counts as a failed delivery and is retried; point `LEDGER_WEBHOOK_URL` at the final URL (including `https://`).

## Operations

### `GET /healthz`

Liveness: `200 {"status": "ok"}` whenever the process is running. **No token needed.** It doesn't check the database, so an outage doesn't make your orchestrator restart every replica.

### `GET /readyz`

Readiness: `200 {"status": "ok"}`, or `503` with a reason when the service shouldn't receive traffic. **No token needed.**

```json
{
  "status": "unavailable",
  "reason": "database unreachable"
}
```

| Reason | When |
|---|---|
| `database unreachable` | Postgres doesn't answer within 2 seconds |
| `database schema is not up to date` | A migration this binary needs isn't applied |
| `shutting down` | The server received `SIGTERM`; it keeps finishing requests while traffic drains |

### `GET /metrics`

Prometheus metrics in the text format. **Needs the token** (Prometheus supports bearer tokens in its scrape config).

```text
ledger_http_requests_total{route="POST /transactions",code="201"} 1520
ledger_http_request_duration_seconds_bucket{route="POST /transactions",le="0.05"} 1498
ledger_http_request_duration_seconds_sum{route="POST /transactions"} 21.7
ledger_http_request_duration_seconds_count{route="POST /transactions"} 1520
ledger_http_requests_in_flight 3
ledger_db_connections{state="in_use"} 2
ledger_db_connections{state="idle"} 8
ledger_db_wait_total 0
ledger_db_wait_seconds_total 0
```

### Rate limiting

With `LEDGER_RATE_LIMIT` set, each client IP may make that many requests per second on average, in bursts of up to `LEDGER_RATE_BURST`. Excess requests get:

```json
{
  "error": {
    "code": "rate_limited",
    "message": "too many requests; slow down"
  }
}
```

with status `429` and a `Retry-After` header. Health checks are never limited. Behind a proxy or load balancer, every client appears with the proxy's IP, so set the limit there instead.

## Object reference

Fields marked "omitted if empty" don't appear in the JSON when they have no value.

### Currency

| Field | Type | Description |
|---|---|---|
| `code` | string | Currency code |
| `decimals` | integer or null | Digits after the decimal point; `null` until registered for currencies from before migration `0005` |
| `created_at` | timestamp | |

### Ledger

| Field | Type | Description |
|---|---|---|
| `id` | string | `ldg_…` |
| `name` | string | |
| `metadata` | object | Omitted if empty |
| `closed_through` | timestamp | End of the last [closed period](#period-close). Omitted if none |
| `created_at` | timestamp | |

### Account

| Field | Type | Description |
|---|---|---|
| `id` | string | `acc_…` |
| `ledger_id` | string | |
| `indicator` | string | Omitted if empty |
| `currency` | string | |
| `allow_negative` | boolean | |
| `overdraft_limit` / `_value` | amount | Omitted if none |
| `untracked` | boolean | |
| `status` | string | `active`, `frozen` or `closed` |
| `status_reason` | string | Omitted if empty |
| `type` | string | `asset`, `liability`, `equity`, `revenue` or `expense`. Omitted if not set |
| `credit_total` / `_value` | amount | Sum of all credits |
| `debit_total` / `_value` | amount | Sum of all debits |
| `balance` / `_value` | amount | `credit_total − debit_total` |
| `pending_debit` / `_value` | amount | Reserved by pending holds, leaving this account |
| `pending_credit` / `_value` | amount | Reserved by pending holds, arriving in this account |
| `version` | integer | Number of entries posted to the account |
| `metadata` | object | Omitted if empty |
| `created_at`, `updated_at` | timestamp | |

Untracked accounts don't track pending amounts; theirs are always 0.

### Transaction

| Field | Type | Description |
|---|---|---|
| `id` | string | `txn_…` |
| `ledger_id` | string | |
| `reference` | string | Omitted on hold commits |
| `status` | string | Always `posted` |
| `currency` | string | |
| `description` | string | Omitted if empty |
| `metadata` | object | Omitted if empty. The only field that can change later ([`PATCH`](#patch-transactionsid)) |
| `refund_of` | string | Set on refunds and reversals: the original transaction |
| `reversal_reason` | string | Set on [reversals](#post-transactionsidreversals) |
| `hold_id` | string | Set on hold commits: the committed hold |
| `batch_id` | string | Set on transactions posted in a [batch or exchange](#batches-and-exchanges) |
| `created_at` | timestamp | |
| `entries` | array of Entry | In leg order |

### Entry

Entries never change once written.

| Field | Type | Description |
|---|---|---|
| `id` | string | `ent_…` |
| `transaction_id` | string | |
| `account_id` | string | |
| `direction` | string | `debit` or `credit` |
| `amount` / `value` | amount | Always positive |
| `balance_after` / `_value` | amount | The account's balance right after this entry. Omitted for untracked accounts |
| `account_version` | integer | 1, 2, 3, … per account, without gaps. Omitted for untracked accounts |
| `created_at` | timestamp | Never earlier than the account's previous entry |

### Hold

| Field | Type | Description |
|---|---|---|
| `id` | string | `hld_…` |
| `ledger_id` | string | |
| `reference` | string | |
| `status` | string | `pending`, `committed`, `voided` or `expired` |
| `currency` | string | |
| `description` | string | Omitted if empty |
| `metadata` | object | Omitted if empty |
| `legs` | array of Leg | What was reserved |
| `transaction_id` | string | Set when committed |
| `expires_at` | timestamp | Omitted if the hold never expires |
| `created_at` | timestamp | |
| `resolved_at` | timestamp | Set when committed, voided or expired |

### Leg

| Field | Type | Description |
|---|---|---|
| `account_id` | string | |
| `direction` | string | `debit` or `credit` |
| `amount` / `value` | amount | |

### Batch

| Field | Type | Description |
|---|---|---|
| `id` | string | `bat_…` |
| `ledger_id` | string | |
| `reference` | string | |
| `kind` | string | `batch` or `exchange` |
| `rate` | string | Exchanges only, as given. Omitted if empty |
| `description` | string | Omitted if empty |
| `metadata` | object | Omitted if empty |
| `created_at` | timestamp | |
| `transactions` | array of Transaction | In the order given |

### Event

| Field | Type | Description |
|---|---|---|
| `id` | string | `evt_…` |
| `type` | string | See [Events](#events) |
| `ledger_id` | string | Omitted for currency events |
| `subject_id` | string | The ID of the changed object (for currencies, the code) |
| `data` | object | The object after the change |
| `created_at` | timestamp | |
