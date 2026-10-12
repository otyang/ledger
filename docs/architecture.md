# Architecture and internals

How the ledger works under the hood: the code layout, what happens during a posting, how idempotency, locking, holds, refunds and untracked accounts are implemented, and which rules the database enforces on its own. Read this before changing the code.

## Contents

- [Design principles](#design-principles)
- [Code layout](#code-layout)
- [Database schema](#database-schema)
- [The life of a posting](#the-life-of-a-posting)
- [Transactions, savepoints and errors](#transactions-savepoints-and-errors)
- [Idempotency](#idempotency)
- [Locking and concurrency](#locking-and-concurrency)
- [Holds](#holds)
- [Refunds](#refunds)
- [Untracked accounts](#untracked-accounts)
- [History and timestamps](#history-and-timestamps)
- [Amounts](#amounts)
- [Currencies and decimal values](#currencies-and-decimal-values)
- [Account controls](#account-controls)
- [Hold expiry](#hold-expiry)
- [Reversals and metadata updates](#reversals-and-metadata-updates)
- [Batches and exchanges](#batches-and-exchanges)
- [Events and webhooks](#events-and-webhooks)
- [Snapshots](#snapshots)
- [Reports and period close](#reports-and-period-close)
- [Operations endpoints](#operations-endpoints)
- [Database-enforced invariants](#database-enforced-invariants)
- [Migrations](#migrations)
- [HTTP layer](#http-layer)
- [Testing](#testing)
- [Known limitations](#known-limitations)

## Design principles

1. **The database is the last line of defence.** Every rule that keeps the books correct (balanced transactions, immutable entries, no overdrafts, consistent balances) is enforced by Postgres constraints and triggers, not only by Go code. A bug in Go, or someone writing SQL by hand, can't corrupt the ledger.
2. **Exact integers only.** Amounts are whole numbers of a currency's smallest unit. Nothing is ever rounded; decimals exist only at the edges, converted exactly.
3. **Every write is idempotent.** Each write is keyed by a caller-chosen reference (or, for hold commits, the hold), so retries are always safe.
4. **Embeddable first.** The core is a Go library that can join the caller's database transaction. The HTTP service is a thin optional layer on top.
5. **Append-only history.** Entries are never updated or deleted. Corrections are new transactions (refunds), so history is complete.

## Code layout

Package `ledger` (repository root):

| File | Responsibility |
|---|---|
| `ledger.go` | `Service`, `New`, `OpenDB`, the UTC clock (`now`) |
| `models.go` | `Ledger`, `Account`, `Transaction`, `Entry`, `Hold` structs and their database mapping |
| `ids.go` | Typed IDs (`AccountID`, …), generation and validation of `<prefix>_<uuidv7>` |
| `amount.go` | `Amount`: arbitrary-precision integer, JSON, SQL scanning |
| `currency.go` | `Currency`, registration, the cache, decimal `Parse`/`Format`, converting `Value` to `Amount` |
| `account.go` | Creating and reading ledgers and accounts; computing untracked balances |
| `post.go` | `Post`, locking (`lockAccounts`), applying legs (`posting`), validation, request hashing |
| `holds.go` | Create, commit, void and expire holds |
| `refunds.go` | Refunds, reversals and what is refundable |
| `batch.go` | Batches and exchanges |
| `reports.go` | Transaction search, trial balance, balance sheet, income statement, statements, period close |
| `events.go` | The event outbox (`emit`), `ListEvents`, consumer cursors |
| `snapshot.go` | Running totals for untracked accounts |
| `partial.go` | Shared logic for partial commits and refunds (`aggregate`, `pickLegs`) |
| `query.go` | Reading transactions, listing entries, `BalanceAt`, updating transaction metadata |
| `tx.go` | Transactions and savepoints (`inTx`), idempotent execution (`once`), error classification |
| `errors.go` | Sentinel errors and Postgres error matching |
| `migrate.go` | Embedded migration runner, `CheckSchema` |
| `migrations/` | SQL schema, one up/down pair per version |

Other packages:

| Path | Responsibility |
|---|---|
| `httpapi/httpapi.go` | Routes, auth and timeout middleware, request decoding, error mapping |
| `httpapi/business.go` | Handlers for account controls, corrections, batches, reports, period close, events |
| `httpapi/views.go` | Response shapes that add the decimal `*_value` fields |
| `httpapi/ops.go` | Health checks, Prometheus metrics, rate limiting |
| `webhook/` | Delivers events to an HTTP endpoint |
| `cmd/ledgerd/` | The standalone server: configuration, port binding, background jobs, graceful shutdown |
| `ledgertest/` | Test helpers: an embedded Postgres and a fresh migrated database per test |

## Database schema

Everything lives in the `ledger` schema.

| Table | Purpose | Notable columns |
|---|---|---|
| `currencies` | Registered currencies | `code` (PK), `decimals` (NULL only for pre-`0005` currencies until registered) |
| `ledgers` | Books | `id`, `name`, `metadata`, `closed_through` |
| `accounts` | Balances | `credit_total`, `debit_total`, `balance`, `pending_debit`, `pending_credit` (all `NUMERIC(78,0)`), `version`, `untracked`, `allow_negative`, `overdraft_limit`, `status`, `status_reason`, `type`; FK to `currencies` |
| `transactions` | Business events | `reference` (unique per ledger), `request_hash`, `refund_of`, `reversal_reason`, `hold_id` (unique), `batch_id`, `refund_count`, `txid` |
| `entries` | One per leg | `amount`, `direction`, `balance_after`, `account_version` (unique per account), `txid` |
| `holds` | Reservations | `legs` (JSONB), `status`, `request_hash`, `transaction_id`, `expires_at`, `resolved_at` |
| `batches` | Groups of transactions posted together | `reference` (unique per ledger), `kind`, `rate`, `request_hash` |
| `period_closes` | Closed periods | `ledger_id`, `closed_through`, `trial_balance` (JSONB) |
| `events` | Outbox of every change | `seq`, `id`, `type`, `subject_id`, `data` (JSONB), `txid` |
| `webhook_cursors` | Each event consumer's progress | `name`, `event_id` |
| `account_snapshots` | Running totals of untracked accounts | `txid_bound`, `credit_total`, `debit_total`, `entry_count` |
| `schema_migrations` | Applied migration versions | `version`, `applied_at` |

Columns that aren't exposed in the API:

- `request_hash`: fingerprint of the request, for [idempotency](#idempotency).
- `txid`: the Postgres transaction ID that wrote the row. Used to make sure entries are only added together with their transaction, and to page untracked accounts safely.
- `refund_count`: incremented by every refund, to serialize concurrent refunds.

Main indexes: `entries (account_id, created_at)` for `BalanceAt`, `entries (account_id, txid, id)` for paging untracked accounts, the unique `entries (account_id, account_version)` for paging tracked ones, `entries (transaction_id)`, partial indexes on pending holds, expiring holds, refunds and batch members, `transactions (ledger_id, id)` for search, a GIN index on transaction metadata, and `events (txid, seq)` for the event feed.

## The life of a posting

`Service.PostTx` ([post.go](../post.go)), step by step:

1. **Validate the request without touching the database:** reference length, currency code format.
2. **Convert values.** If any leg has a `Value`, look up the currency's decimals (cached) and convert each to an `Amount`, then clear `Value`. From here on only amounts exist.
3. **Validate the legs:** at least 2, valid account IDs, positive amounts within 78 digits, valid directions, debits = credits, at least 2 distinct accounts.
4. **Hash the request:** `requestHash("post", currency, legsKey(legs))`.
5. **Run `once`** inside a database transaction (or a savepoint in the caller's):
   1. **Find:** look for a transaction with this reference in the ledger of the first account. Same hash → return it (a replay). Different hash → `ErrReferenceConflict`. Not found → continue.
   2. **Lock:** `lockAccounts` loads every account, locking the tracked ones (`FOR NO KEY UPDATE`, sorted by ID). It checks they all exist and share one ledger, and picks the timestamp for this posting.
   3. **Apply in memory** (`posting.post`): check each account's currency, then for each leg add the amount to the account's credit or debit total, recompute the balance, bump the version, and build an entry carrying `balance_after` and `account_version`. Untracked accounts get entries but no totals.
   4. **Check funds** on the *net* result of all legs: every locked account without `allow_negative` must have `balance − pending_debit ≥ 0`.
   5. **Save:** insert the transaction and its entries, update the locked accounts.
6. **At COMMIT**, the deferred `transactions_balanced` trigger re-checks that the transaction has at least 2 entries and that they balance.

## Transactions, savepoints and errors

[tx.go](../tx.go) holds the plumbing every write uses.

**`inTx`** runs a function in a new database transaction, or, if it was given the caller's `bun.Tx`, in a **savepoint** inside it. A failed ledger operation then rolls back only its savepoint, and the caller's transaction stays usable. The savepoint:

- runs `SET CONSTRAINTS ledger.transactions_balanced DEFERRED`, so the balance check still waits for COMMIT even if the caller made constraints immediate;
- is released or rolled back with a context that can't be cancelled, so the result returned always matches what the caller's transaction will commit;
- returns `ErrTxAborted` if even the rollback fails, telling the caller to abandon their transaction.

**`classify`** turns Postgres concurrency failures (serialization failure, deadlock, lock not available) into `ErrRetryable`, and wraps context cancellation so `errors.Is(err, context.DeadlineExceeded)` works.

## Idempotency

Every write stores a `request_hash`: SHA-256 over the request's parts, separated by NUL bytes.

| Operation | Hashed parts | Looked up by |
|---|---|---|
| Post | `"post"`, currency, legs | `(ledger_id, reference)` |
| Create hold | `"hold"`, currency, legs | `(ledger_id, reference)` on holds |
| Commit hold | `"commit"`, hold ID, amount, legs | `hold_id` (unique) |
| Refund | `"refund"`, original ID, amount, legs | `(ledger_id, reference)` |

**`legsKey`** makes the comparison independent of how the request was written: legs are merged per (account, direction), sorted, and serialized as `account|direction|amount;`. So reordered legs, a leg split in two, or a `value` instead of an `amount` all give the same key. Description and metadata aren't part of the hash.

**Races.** Two identical requests can run at the same time. Both miss in "find"; one inserts first, and the other hits a unique violation on `transactions_reference_key`, `transactions_hold_id_key` or `holds_reference_key`. `once` catches that and runs "find" again, returning the winner's result. If the caller's REPEATABLE READ snapshot can't see the winner yet, it returns `ErrRetryable` instead.

A duplicate can also lose earlier: it waits for the winner's account locks, and once the winner commits, its own checks fail (the funds are spent, nothing is left to refund, a batch member's reference is taken) before it ever reaches the unique insert. So whenever `create` fails, `once` runs "find" again in a fresh transaction, and returns the winner's result if there is one with the same request hash. A genuinely different request keeps its error.

**Hold commits** have no reference. The commit transaction carries `hold_id`, which is unique, so a hold commits at most once and no user-chosen reference can block it.

## Locking and concurrency

- **Account locks:** `SELECT … FOR NO KEY UPDATE`, which is the lock an `UPDATE` of non-key columns takes. Unlike `FOR UPDATE`, it doesn't block other transactions' foreign-key checks, so inserting entries that reference an account isn't blocked by a lock on that account.
- **Deadlock freedom:** every operation locks accounts in sorted ID order.
- **Parent before accounts:** hold commits and voids lock the hold first; refunds lock the original transaction first. Then the accounts. The order is the same everywhere.
- **Untracked accounts are never locked.**
- **Timeouts:** the caller's context deadline (or `LEDGER_REQUEST_TIMEOUT` over HTTP) also bounds time spent waiting for locks.

Two postings that touch different accounts run fully in parallel. Two that share an account run one after the other on that account. Hot accounts that everyone touches (like `@world`) should be untracked.

## Holds

[holds.go](../holds.go):

- **Create:** validate and hash, lock the accounts, check currencies, then `reserve(+1)`: add each debit leg to the account's `pending_debit` and each credit leg to `pending_credit` (tracked accounts only). Check funds, since pending debits reduce available balance. Insert the hold with its legs as JSONB.
- **Commit:** lock the hold. If it's already committed, return its transaction (if the hash matches); if voided, fail. Choose the legs to post with `pickLegs` (full, an amount, or explicit legs within what was held). Lock the accounts, `reserve(−1)` the **whole** hold, post the chosen legs as a transaction with `hold_id`, and mark the hold `committed`. Whatever wasn't committed is thereby released.
- **Void:** lock the hold. Return it if already voided; fail if committed. Otherwise `reserve(−1)` and mark it `voided`.

The `holds_guard_update` trigger only lets a hold move from `pending` to `committed` or `voided`, never back, and requires a committed hold to point at its own commit transaction.

## Refunds

[refunds.go](../refunds.go):

1. **Lock the original** (`lockForRefund`): `SELECT … FOR NO KEY UPDATE`, then `refund_count = refund_count + 1`. Concurrent refunds of the same transaction therefore run one at a time. Because each one *writes* the row, a refund in a REPEATABLE READ transaction with a stale snapshot fails with a serialization error (`ErrRetryable`) instead of computing what's left from old data. A refund of a refund is rejected.
2. **Compute what's refundable:** the original entries with directions reversed, merged per (account, direction), minus the sum of all earlier refund entries.
3. **Choose the legs** with `pickLegs`, the same logic as partial hold commits:
   - nothing given → everything left;
   - an amount → only if what's left is one debit and one credit leg, both reduced by that amount;
   - explicit legs → each (account, direction) at most what's left.

   An explicit zero amount is an error. `Amount.isSet` tells "not given" apart from 0.
4. **Post** a normal transaction with `refund_of` set.

## Untracked accounts

Untracked accounts trade read speed for write throughput.

| | Tracked | Untracked |
|---|---|---|
| Locked during postings | Yes | No |
| Stored totals | Yes, updated on every posting | Always 0 (the `accounts_untracked` constraint enforces it) |
| Reading the balance | One row | Sums all of the account's entries (`fillUntracked`) |
| Entries have `balance_after` and `account_version` | Yes | No (NULL) |
| Pending amounts from holds | Yes | Not tracked |
| Overdraft check | Yes | None, so `allow_negative` is required |

**Paging untracked entries safely.** Without a lock, two postings can commit in a different order than they started. Ordering by ID could then insert an older entry *behind* a cursor a client already passed. So untracked entries are ordered by `(txid, id)`, where `txid` is the writing Postgres transaction's ID, and only entries with `txid < pg_snapshot_xmin(pg_current_snapshot())` are returned: those from transactions older than every transaction still running. An entry appears only once nothing older can still commit. The cost: a long-running transaction anywhere in the database delays new entries on untracked accounts.

Tracked accounts don't need this: `account_version` is assigned under the lock, gap-free, so ordering by it is final.

## History and timestamps

`BalanceAt(t)`:

- **Tracked:** the `balance_after` of the latest entry at or before `t`.
- **Untracked:** the sum of entries at or before `t`.

For this to be correct, an account's entry timestamps must never go backwards, even across servers with skewed clocks. `lockAccounts` therefore sets the posting's timestamp to the later of the current time and every locked account's `updated_at`. Timestamps are UTC, truncated to microseconds (what Postgres stores), so values returned by writes equal what later reads return.

## Amounts

`Amount` ([amount.go](../amount.go)) wraps an immutable `*big.Int`.

- **Database:** `NUMERIC(78,0)`. 78 digits holds any 256-bit value (enough for any token in its smallest unit). The driver returns `NUMERIC` as text, which `Scan` parses; writes send the decimal string.
- **JSON:** written as a string (`"5000"`) so JavaScript doesn't round large values. Reads accept strings or integer numbers; fractions, exponents, `+` and spaces are rejected.
- **Limits:** `ParseAmount` rejects more than 78 digits *before* parsing (parsing huge numbers is slow). Account totals and pending amounts are checked after every change (`inRange`) and return `ErrAmountOverflow` instead of failing in the database.
- **Comparison:** `Amount` can't be compared with `==` (a zero-size `func` field makes that a compile error), because `==` would compare pointers. Use `Cmp` or `Sign`.
- **Zero vs. unset:** `Amount{}` means "not given". `isSet` distinguishes it from an explicit 0, which matters for "commit or refund everything".

## Currencies and decimal values

[currency.go](../currency.go):

- **Registration** is a single `INSERT … ON CONFLICT (code) DO UPDATE SET decimals = … WHERE decimals IS NULL RETURNING …`. That one statement covers new currencies, setting decimals on a pre-`0005` currency, and detecting an existing one (no row returned, then it compares decimals). Concurrent registrations have exactly one winner.
- **Conversion** (`parseValue`) splits on `.`, rejects anything but digits, accepts extra fractional digits only if they're all zeros, pads to the currency's decimals, and parses the result as an integer. Nothing is rounded. `formatValue` does the reverse, always with exactly `decimals` digits after the point.
- **Where conversion happens:** at the start of each write, before validation and hashing, so the rest of the system only ever sees amounts.
- **Cache:** a `sync.Map` on the `Service`, keyed by code. It only holds currencies whose decimals are set (those never change), only from committed reads through `*bun.DB` (never through a caller's transaction, which could roll back), and hands out copies so callers can't modify it.

## Account controls

Status, overdraft limits and types live on the account row ([account.go](../account.go) `UpdateAccount`).

- **Enforcement in Go:** `posting.checkStatus` runs on every posting and on reservations (`reserve(+1)`). A closed account refuses every leg; a frozen account refuses debits. Releasing a hold (`reserve(−1)`) never checks status, so voids and expiries always work.
- **Enforcement in the database:** `entries_check` raises an exception for an entry on a closed account, or a debit on a frozen one. It reads the account `FOR KEY SHARE` (the lock its foreign-key check takes anyway), so an entry inserted while a status change is in progress waits for it and then sees the new status. `mapTriggerError` turns those exceptions back into `ErrAccountFrozen` / `ErrAccountClosed`.
- **Status changes lock the account `FOR UPDATE`.** That conflicts with a posting's lock on a tracked account, and with the key-share lock that entries on an untracked account take. So a freeze waits for postings already writing to the account, and postings after it see it frozen: there is no window in either direction. Closing requires a zero balance and nothing pending, checked in Go and by the `accounts_closed_empty` constraint. `accounts_guard_update` makes closing final and a type settable once.
- **Overdraft limit:** `checkFunds` compares `available` with the account's `floor()`: 0, or minus the limit. The `accounts_no_overdraft` constraint uses the same formula. Lowering a limit below what the account owes is refused.

## Hold expiry

- `expires_at` is stored on the hold and isn't part of its request hash, so a client that recomputes "now + 7 days" on a retry still gets the original hold.
- **Commit** checks `expired(now)` after locking the hold and refuses with an error wrapping both `ErrHoldNotPending` and `ErrHoldExpired`, without changing anything.
- **`ExpireHolds`** finds pending holds past their expiry (partial index `holds_expiry_idx`), then releases each in its own transaction. It locks the hold with `FOR NO KEY UPDATE SKIP LOCKED`, so a hold being committed or voided at that moment is skipped. Its accounts are locked with `lock_timeout = 2s`; if they stay busy longer, the hold is skipped too. Skipped holds are picked up by the next run, so the job never stalls behind a long user transaction. Releasing uses the same `release` function as voiding, with status `expired`.
- `ledgerd` runs it every `LEDGER_HOLD_EXPIRY_INTERVAL`. Several replicas running it at once is safe.

## Reversals and metadata updates

- **`Reverse`** is `refund` with no amount and no legs (everything left) plus a reason, stored in `reversal_reason` (the `transactions_reversal` constraint requires `refund_of` to be set). Its hash is `("reverse", original ID)`, so the reason can be worded differently on a retry.
- **`UpdateTransactionMetadata`** and the metadata part of `UpdateAccount` lock the row, merge the patch in Go (`mergeMetadata`: `null` deletes a key), and write it back. `transactions_guard_update` lets nothing else change.

## Batches and exchanges

[batch.go](../batch.go) builds on the posting engine, split for this purpose into steps:

1. Validate every member and convert values, as `Post` does. Hash `("batch", kind, rate, each member's reference, currency and legs)`.
2. In `once`: lock the union of all members' accounts in one sorted pass (still deadlock-free), and check that member references aren't taken.
3. `posting.apply` each member in order, **without** checking funds; then `checkFunds` once on the combined result. So an earlier member can fund a later one, and the batch fails as a whole if the end result overdraws an account.
4. Insert the batch row, then each transaction with `batch_id` (`insertTransaction`), then write the accounts back once (`saveAccounts`).

Members' `reference` is optional (the `transactions_reference_or_hold` constraint accepts `batch_id` instead). A batch is limited to 1,000 transactions to bound lock and transaction size.

**Exchanges** are batches of kind `exchange` with exactly two two-leg members in different currencies. The rate is stored as given; the ledger doesn't compute or verify one amount from the other.

## Events and webhooks

**The outbox.** `emit` inserts a row into `ledger.events` inside the same database transaction (or savepoint) as the change it describes. So an event exists if and only if its change committed: no lost events, no events for rolled-back changes. Every write path emits: currencies, ledgers, accounts, transactions (including refunds, reversals, hold commits and batch members), holds, batches and period closes.

**Ordering without gaps.** Events get a `seq` (BIGSERIAL) and the `txid` of the writing transaction. Like untracked entries, `ListEvents` orders by `(txid, seq)` and only returns events with `txid < pg_snapshot_xmin(pg_current_snapshot())`: those whose transactions, and every older one, have finished. A consumer's cursor therefore never passes an event that could still appear.

**Webhooks** ([webhook/](../webhook/webhook.go)):

- The dispatcher takes a session-level advisory lock (keyed by its name) on a dedicated connection, so only one process delivers. It reads and writes its cursor on that same connection, and checks `pg_locks` before every delivery attempt. If the session was lost (network blip, database restart), it stops and waits for the lock again instead of delivering alongside the new holder. Cursors only move forward (`SetEventCursor` ignores older events). Session locks don't work through a transaction-mode pooler such as PgBouncer.
- Redirects aren't followed (a redirected POST would become a GET); a `3xx` is a failed delivery.
- It reads its cursor (`webhook_cursors`), lists the next 100 events, and POSTs them one at a time. It retries each until a 2xx (backoff from 1 s, doubling up to 1 minute), then advances the cursor. Delivery is at least once and strictly ordered.
- Requests are signed: `X-Ledger-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>`.

## Snapshots

Reading an untracked account's balance used to sum all of its entries. [snapshot.go](../snapshot.go) keeps a running total per untracked account in `account_snapshots`:

- **Refresh** is one `INSERT … SELECT … ON CONFLICT DO UPDATE` statement per account. It reads the bound `b = pg_snapshot_xmin(pg_current_snapshot())`, the previous snapshot, and the sum of entries with `previous bound ≤ txid < b`, all from the same database snapshot. Every transaction with `txid < b` has finished, so those entries are final. The update only applies if the new bound isn't older, so concurrent refreshes are harmless.
- **Reading** (`fillUntracked`) adds the snapshot to the sum of entries with `txid ≥ bound`, again in one statement. Any transaction still running, including the caller's own, has a txid at or above the bound, so its entries are always counted.
- The result is identical with or without snapshots; `TestSnapshots` checks this under concurrent postings and refreshes. `ledgerd` refreshes every `LEDGER_SNAPSHOT_INTERVAL`.

## Reports and period close

[reports.go](../reports.go):

- **`TrialBalance(at)`** is one query over all accounts of the ledger. For tracked accounts it takes the latest `balance_after` at or before `at` (index `entries (account_id, created_at)`); for untracked ones it sums entries up to `at`. Totals per currency are computed in Go, and are zero by construction.
- **`BalanceSheet`** and **`IncomeStatement`** are built from trial balances, grouping by account `type`, and present each amount in the account's normal direction (negated for assets and expenses). The income statement is the difference between two trial balances.
- **`Statement`** reads the opening balance (strictly before `from`) and the entries in `[from, to)`, computing running balances in Go. Periods over 100,000 entries are refused.
- **`ListTransactions`** filters with `EXISTS` on entries for an account, a GIN index for metadata containment (`@>`), and orders by ID (UUIDv7, so creation order).
- **`ClosePeriod`** locks the ledger row, computes the trial balance at `through`, stores it in `period_closes`, and sets `ledgers.closed_through`. From then on, `entries_check` rejects entries with `created_at ≤ closed_through`, and `ledgers_guard_update` stops it from moving back. `through` must be at least a minute in the past, so postings that already took their timestamp have time to commit.

## Operations endpoints

[httpapi/ops.go](../httpapi/ops.go):

- **`/healthz`** always answers 200. **`/readyz`** pings the database (2 s timeout), calls `CheckSchema` (every embedded migration applied), and consults `Config.Ready`, which `ledgerd` turns off as soon as shutdown starts. Both skip authentication and rate limiting.
- **Metrics** are collected by wrapping each route's handler at registration, so the route label is the pattern (`GET /accounts/{id}`), never a raw path with IDs in it. `/metrics` writes the Prometheus text format by hand; there's no dependency.
- **Rate limiting** is a token bucket per client IP (`RemoteAddr`), checked before authentication. Idle buckets are dropped every 1,024 requests so memory stays bounded.

## Database-enforced invariants

| Rule | Enforced by |
|---|---|
| IDs are `<prefix>_<uuidv7>` | `CHECK (ledger.is_id(…))` on every ID column |
| `balance = credit_total − debit_total` | `accounts_balance_consistent` |
| No overdraft unless allowed, counting holds and overdraft limits | `accounts_no_overdraft`: `allow_negative OR balance − pending_debit ≥ −COALESCE(overdraft_limit, 0)`; `accounts_overdraft_limit` (a limit only without `allow_negative`) |
| Only empty accounts are closed; closing is final; a type is set once | `accounts_closed_empty`, `accounts_guard_update` |
| No entries on closed accounts, no debits on frozen ones, none in a closed period | `entries_check` |
| Periods can't be reopened | `ledgers_guard_update` |
| A reversal is a refund with a reason | `transactions_reversal` |
| Batches and period closes never change or disappear; events never change | `batches_forbid_change`, `batches_forbid_truncate`, `period_closes_forbid_change`, `period_closes_forbid_truncate`, `events_forbid_update` |
| Untracked accounts keep no totals | `accounts_untracked` |
| An account's ledger, indicator, currency and tracking mode never change | `accounts_guard_update` |
| Every account's currency is registered | FK `accounts_currency_fkey` |
| Transactions balance and have ≥ 2 entries | `transactions_balanced` (deferred to COMMIT) |
| Entries are added only by the database transaction that created their transaction, and match its ledger and currency | `entries_check` |
| Entries never change or disappear | `entries_forbid_change`, `entries_forbid_truncate` |
| Only a transaction's metadata (and refund count, upward) can change; transactions can't be deleted | `transactions_guard_update`, `transactions_forbid_delete`, `transactions_forbid_truncate` |
| `balance_after` and `account_version` are both set or both NULL; versions are unique per account | `entries_running_balance`, `entries_account_version_key` |
| Holds only move from pending to committed, voided or expired; can't be deleted | `holds_guard_update`, `holds_resolution`, `holds_forbid_delete`, `holds_forbid_truncate` |
| A hold commits at most once | `transactions_hold_id_key` |
| References are unique per ledger | `transactions_reference_key`, `holds_reference_key` |
| A currency's decimals are set once; currencies can't be deleted | `currencies_guard_update`, `currencies_forbid_delete`, `currencies_forbid_truncate` |

## Migrations

[migrate.go](../migrate.go) runs the SQL files embedded from `migrations/` in version order.

- A Postgres **advisory lock** serializes `Migrate` across processes, so replicas starting together take turns. It's tied to the session, so it's released if a process dies.
- Each migration and its `schema_migrations` row commit in **one transaction**: a crash never leaves a migration half-applied or unrecorded.
- Files run through `database/sql` directly, not Bun, so `?` in SQL isn't treated as a placeholder.
- `MigrateDown` reverts the latest migration. Down migrations refuse to run when they would lose information: `0002` while untracked accounts exist, `0004` while amounts exceed `BIGINT`, `0005` while any currency has decimals, `0006` while frozen, closed or overdraft accounts, expiring holds, batches, reversals or closed periods exist.

| Version | Change |
|---|---|
| `0001` | Ledgers, accounts, transactions, entries; immutability and balance triggers |
| `0002` | Holds, refunds, untracked accounts, pending amounts |
| `0003` | Ledger scoping of transactions and holds, per-ledger references, `txid`, `refund_count`, `entries_check` |
| `0004` | Amount columns `BIGINT` → `NUMERIC(78,0)`; hold leg amounts as strings |
| `0005` | `currencies` table with immutable decimals; FK from accounts |
| `0006` | Account status, overdraft limits and types; hold expiry; batches; reversals; period close; events; webhook cursors; snapshots |

To add a migration: create `migrations/<next version>_<name>.up.sql` and `.down.sql`, make the down migration refuse if it would lose data, and add a test that migrates down and up again.

## HTTP layer

[httpapi](../httpapi/) translates HTTP to `Service` calls and nothing more.

- **Routes** are registered through a helper that wraps each handler with metrics, labelled by route pattern.
- **Middleware:** health checks pass straight through; everything else is rate limited (if configured), then authenticated with a constant-time bearer token comparison, then given a per-request timeout context.
- **Decoding:** a 1 MiB body limit, `DisallowUnknownFields`, and a check that nothing follows the JSON object.
- **Responses** go through `views`, which embeds the library structs and adds the `*_value` fields. Each currency is looked up at most once per request. Lookup failures other than "unknown currency" are logged and the value fields left out.
- **Errors** are mapped by walking `errorMap` in order and taking the first `errors.Is` match. Order matters: `ErrRetryable` and timeouts come first, so a retryable failure that also wraps another error is still a 503. A not-found for something named in the request *body* is wrapped by `referenced()` and becomes `422 unknown_reference` instead of `404`. Anything unmatched is a logged `500`.

## Testing

- `ledgertest` starts an embedded Postgres once per test binary, or uses `LEDGER_TEST_DATABASE_URL`, and gives each test its own freshly migrated database, so tests never see each other's data.
- `TestInvariants` runs random transfers, holds and refunds concurrently, with both a tracked and an untracked `@world`, then checks that all balances sum to zero and every cached balance equals the sum of its entries.
- Raw-SQL tests bypass Go to prove the database rejects invalid writes on its own.
- Migration tests go down and up again, and check that down migrations refuse to lose data.

Run everything with `make check` (formatting, vet, tests with the race detector).

## Known limitations

- **One token, full access.** There are no per-ledger permissions, roles or audit of who made a change.
- **A long-running database transaction delays** new entries on untracked accounts in `ListEntries`, and new events in `ListEvents`.
- **Untracked balances between snapshots** are still summed from entries, as are untracked balances in `BalanceAt`, trial balances and statements, which filter by time rather than by snapshot.
- **Events are kept forever.** There is no pruning job yet; delete old rows from `ledger.events` yourself if you need to (deletes are allowed, updates aren't), but never past a consumer's cursor.
- **Metadata search compares strings:** `metadata.amount=5` matches the string `"5"`, not the number 5.
- **A period close relies on a one-minute margin:** a library caller's database transaction that stays open for more than a minute across the close moment could still commit entries into the closed period.
- **Exchange rates are informational:** the ledger doesn't check that the two amounts match the rate.
- **Rate limiting is per IP, in memory, per process:** behind a proxy all clients share one bucket, and each replica counts separately.
- **The currency lookup for values runs before the savepoint** inside a caller's transaction. A timeout there can break the caller's transaction, as timeouts while waiting for locks already do.
- **Each transaction has exactly one currency.** Conversions are two linked transactions (an exchange).
