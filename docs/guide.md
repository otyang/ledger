# Beginner guide: how the ledger works

This guide explains double-entry bookkeeping as this ledger does it, using one running story: Alice tops up her wallet, buys a concert ticket, has a card payment authorized and captured, gets a refund, and withdraws money. Every number below is what the ledger actually returns for these requests.

No accounting background is needed. For exact request and response formats, see the [API reference](api.md).

## Contents

- [The core idea](#the-core-idea)
- [The building blocks](#the-building-blocks)
- [Debit and credit in this ledger](#debit-and-credit-in-this-ledger)
- [Amounts and values](#amounts-and-values)
- [The story](#the-story)
  - [1. Setting up](#1-setting-up)
  - [2. Alice tops up 100.00 USD](#2-alice-tops-up-10000-usd)
  - [3. Alice buys a 50.00 USD ticket](#3-alice-buys-a-5000-usd-ticket)
  - [4. The venue refunds its part](#4-the-venue-refunds-its-part)
  - [5. A card authorization (hold)](#5-a-card-authorization-hold)
  - [6. A cancelled authorization](#6-a-cancelled-authorization)
  - [7. Alice withdraws money](#7-alice-withdraws-money)
- [Reading an account's history](#reading-an-accounts-history)
- [Retrying safely](#retrying-safely)
- [Crypto amounts](#crypto-amounts)
- [Running the business](#running-the-business)
- [Common mistakes](#common-mistakes)
- [Glossary](#glossary)

## The core idea

Money is never created or destroyed inside the ledger. It only **moves** between accounts. Every movement is recorded twice: once where the money leaves, once where it arrives. That is "double entry".

Because of this, a simple rule always holds:

> **The balances of all accounts in a ledger add up to zero.**

If that rule ever broke, something would be wrong. This ledger makes it impossible to break: the database rejects any transaction where the money leaving doesn't equal the money arriving.

## The building blocks

| Thing | Real-life equivalent | Example |
|---|---|---|
| **Currency** | A currency and how many decimals it has | USD with 2 decimals |
| **Ledger** | One set of books | Your company's books |
| **Account** | A place money is held | Alice's wallet, the venue's payout balance, your fee income |
| **Transaction** | One business event | "Alice bought a ticket" |
| **Entry** | One line of that event, for one account | "Alice's wallet: −50.00" |
| **Hold** | A card authorization | "50.00 reserved at a hotel" |

The difference between a **transaction** and an **entry** matters most:

- A **transaction** is the whole event, like a store receipt. It covers several accounts, and must balance.
- An **entry** is one account's side of it, like one line on that account's bank statement. A transaction has one entry per leg, at least two.

## Debit and credit in this ledger

Each leg of a transaction has a **direction**:

| Direction | Effect on the account | Think of it as |
|---|---|---|
| `debit` | Balance goes **down** | Money leaves this account |
| `credit` | Balance goes **up** | Money arrives in this account |

So a transfer from Alice to Bob is: **debit** Alice, **credit** Bob. Every transaction's debits must equal its credits.

(Accountants sometimes use the opposite convention for some account types. This ledger uses one simple rule for every account: `balance = credits − debits`.)

## Amounts and values

The ledger stores amounts as **whole numbers of the smallest unit**: cents for USD, wei for ETH. 50.00 USD is stored as `5000`. This keeps every calculation exact.

You don't have to convert by hand. Every request accepts either form:

| Field | Example for 50.00 USD | Notes |
|---|---|---|
| `amount` | `"5000"` | Smallest unit. Always exact. |
| `value` | `"50.00"` | Decimal. Converted using the currency's registered decimals. |

Values are **never rounded**. `"50.001"` USD is rejected, because USD has only 2 decimals. Responses always contain both forms, for example `"amount": "5000"` and `"value": "50.00"`.

## The story

The examples use `curl` against a local server. Set these once in your terminal:

```sh
T="Authorization: Bearer dev-secret"     # your LEDGER_API_TOKEN
API=http://localhost:7710
```

IDs are shortened in this guide (`acc_alice`, `txn_ticket`, …). Real IDs look like `acc_01a1304b-d11c-7318-9858-3f67ffa53e7a`.

### 1. Setting up

**Register the currency.** This tells the ledger USD has 2 decimals. It can never change afterwards, because every stored amount depends on it.

```sh
curl -H "$T" -X POST $API/currencies -d '{"code": "USD", "decimals": 2}'
```

**Create a ledger**, a book to hold the accounts:

```sh
curl -H "$T" -X POST $API/ledgers -d '{"name": "main"}'
```

**Create the accounts:**

| Account | Who it represents | Settings |
|---|---|---|
| `@world` | Everything outside the ledger: banks, card networks | `allow_negative: true`, `untracked: true` |
| Alice | A customer's wallet | defaults |
| Venue | A merchant you pay out to | defaults |
| Fees | Your platform's income | defaults |

```sh
curl -H "$T" -X POST $API/accounts -d '{
  "ledger_id": "ldg_main",
  "currency": "USD",
  "indicator": "@world",
  "allow_negative": true,
  "untracked": true
}'
curl -H "$T" -X POST $API/accounts -d '{
  "ledger_id": "ldg_main",
  "currency": "USD",
  "metadata": {
    "owner": "alice"
  }
}'
curl -H "$T" -X POST $API/accounts -d '{"ledger_id": "ldg_main", "currency": "USD"}'   # venue
curl -H "$T" -X POST $API/accounts -d '{"ledger_id": "ldg_main", "currency": "USD"}'   # fees
```

Why does `@world` need `allow_negative`? Money enters the ledger *from* `@world`, so its balance goes below zero. Its negative balance is exactly "how much money has come in from outside". Regular accounts can't go negative, so customers can't overspend.

Why `untracked`? Almost every deposit and withdrawal touches `@world`. Untracked accounts aren't locked during postings, so `@world` never slows the system down. Their balance is calculated when you read it.

### 2. Alice tops up 100.00 USD

Alice pays 100.00 with her card. The money comes from outside the ledger (`@world`) into her wallet.

```sh
curl -H "$T" -X POST $API/transactions -d '{
  "reference": "deposit-1",
  "currency": "USD",
  "description": "Card top-up",
  "legs": [
    {
      "account_id": "acc_world",
      "direction": "debit",
      "value": "100.00"
    },
    {
      "account_id": "acc_alice",
      "direction": "credit",
      "value": "100.00"
    }
  ]
}'
```

The `reference` is your own unique name for this request. If the network fails and you send it again, the ledger returns the original transaction instead of charging twice. See [Retrying safely](#retrying-safely).

**Balances after:**

| Account | Balance |
|---|---|
| `@world` | −100.00 |
| Alice | 100.00 |
| Venue | 0.00 |
| Fees | 0.00 |
| **Total** | **0.00** |

### 3. Alice buys a 50.00 USD ticket

The ticket costs 50.00: the venue gets 47.00 and your platform keeps a 3.00 fee. That's **one transaction with three entries**, because three accounts are involved.

```sh
curl -H "$T" -X POST $API/transactions -d '{
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
}'
```

The last leg uses `amount` (300 cents) instead of `value` to show that both forms work, even mixed in one request. Debits (50.00) equal credits (47.00 + 3.00), so the transaction is accepted.

The response contains the three entries. Each records the account's balance right after it:

| Entry | Account | Direction | Value | Balance after |
|---|---|---|---|---|
| 1 | Alice | debit | 50.00 | 50.00 |
| 2 | Venue | credit | 47.00 | 47.00 |
| 3 | Fees | credit | 3.00 | 3.00 |

The transaction doesn't store "from" and "to". The entries do: debits are where money came from, credits are where it went. That's why one transaction can have several sources and destinations.

**Balances after:**

| Account | Balance |
|---|---|
| `@world` | −100.00 |
| Alice | 50.00 |
| Venue | 47.00 |
| Fees | 3.00 |
| **Total** | **0.00** |

### 4. The venue refunds its part

The concert is moved and Alice asks for her money back. The venue returns its 47.00, but your platform keeps its fee. Because the original had three legs, you say exactly which money goes back:

```sh
curl -H "$T" -X POST $API/transactions/txn_ticket/refunds -d '{
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
}'
```

A refund is a **new transaction** with `refund_of` pointing to the original. The original is never changed or deleted: the history shows both the sale and the refund.

Other ways to refund:

| Request body | Refunds |
|---|---|
| `{"reference": "r1"}` | Everything not yet refunded |
| `{"reference": "r1", "value": "10.00"}` | 10.00, for two-leg transactions only |
| `{"reference": "r1", "legs": [...]}` | Exactly the legs you list (as above) |

Refunds can never add up to more than the original. Asking now what is left:

```sh
curl -H "$T" $API/transactions/txn_ticket/refundable
```

returns only the fee: 3.00 from Fees back to Alice.

**Balances after:**

| Account | Balance |
|---|---|
| `@world` | −100.00 |
| Alice | 97.00 |
| Venue | 0.00 |
| Fees | 3.00 |
| **Total** | **0.00** |

### 5. A card authorization (hold)

Alice books a 20.00 ride. Like a card authorization, you **reserve** the money first and charge the final price later.

```sh
curl -H "$T" -X POST $API/holds -d '{
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
}'
```

No money moves yet. Alice's account now shows:

| Field | Value | Meaning |
|---|---|---|
| `balance_value` | 97.00 | Unchanged |
| `pending_debit_value` | 20.00 | Reserved by the hold |
| available | 77.00 | `balance − pending_debit`: what she can still spend |

If Alice now tried to spend 90.00, it would fail with `insufficient_funds`, because only 77.00 is available.

The ride ends up costing 15.00. **Commit** (capture) only that much:

```sh
curl -H "$T" -X POST $API/holds/hld_ride/commit -d '{"value": "15.00"}'
```

This posts a normal transaction for 15.00 and releases the remaining 5.00. A hold is committed at most once. An empty body (`{}`) commits the full amount.

**Balances after:**

| Account | Balance | Pending debit |
|---|---|---|
| `@world` | −100.00 | |
| Alice | 82.00 | 0.00 |
| Venue | 15.00 | |
| Fees | 3.00 | |
| **Total** | **0.00** | |

### 6. A cancelled authorization

Alice reserves another 5.00, then cancels. **Void** the hold to release it:

```sh
curl -H "$T" -X POST $API/holds -d '{
  "reference": "auth-10",
  "currency": "USD",
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "debit",
      "value": "5.00"
    },
    {
      "account_id": "acc_venue",
      "direction": "credit",
      "value": "5.00"
    }
  ]
}'

curl -H "$T" -X POST $API/holds/hld_cancelled/void
```

Nothing is posted and no balances change. The hold's status becomes `voided`, and it can no longer be committed.

### 7. Alice withdraws money

Alice moves 30.00 back to her bank. Money leaves the ledger, so it goes **to** `@world`:

```sh
curl -H "$T" -X POST $API/transactions -d '{
  "reference": "withdrawal-1",
  "currency": "USD",
  "legs": [
    {
      "account_id": "acc_alice",
      "direction": "debit",
      "value": "30.00"
    },
    {
      "account_id": "acc_world",
      "direction": "credit",
      "value": "30.00"
    }
  ]
}'
```

**Final balances:**

| Account | Balance | Meaning |
|---|---|---|
| `@world` | −70.00 | 70.00 net has come in from outside (100 in, 30 out) |
| Alice | 52.00 | Her wallet |
| Venue | 15.00 | Owed to the venue |
| Fees | 3.00 | Your income |
| **Total** | **0.00** | Always |

## Reading an account's history

An account's entries are its statement, oldest first:

```sh
curl -H "$T" "$API/accounts/acc_alice/entries"
```

| # | Event | Direction | Value | Balance after |
|---|---|---|---|---|
| 1 | Top-up | credit | 100.00 | 100.00 |
| 2 | Ticket | debit | 50.00 | 50.00 |
| 3 | Venue refund | credit | 47.00 | 97.00 |
| 4 | Ride captured | debit | 15.00 | 82.00 |
| 5 | Withdrawal | debit | 30.00 | 52.00 |

Each entry's `account_version` (1, 2, 3, …) numbers them without gaps, so you can tell nothing is missing. For long histories, page with `?limit=100` and then `?after=<last entry id>`.

To get the balance at a past moment:

```sh
curl -H "$T" "$API/accounts/acc_alice/balance?at=2026-10-12T12:00:00Z"
```

Entries for untracked accounts like `@world` don't have `balance_after` or `account_version`. Their balance is computed by adding up their entries.

## Retrying safely

Networks fail. If a request times out, you can't know whether it was applied. The `reference` solves this:

| You send | The ledger does |
|---|---|
| The same request with the same reference again | Returns the original result. Nothing happens twice. |
| A **different** request with a used reference | Rejects it: `409 reference_conflict` |
| A request that got `503` (`retryable` or `timeout`) | Nothing was applied. Send exactly the same request again. |

So: pick a reference that identifies the business event (an order ID, a payment ID), and always retry with the same one. A request sent with `"value": "50.00"` and the same one with `"amount": "5000"` count as the same request.

## Crypto amounts

Crypto works the same way, with more decimals. ETH has 18:

```sh
curl -H "$T" -X POST $API/currencies -d '{"code": "ETH", "decimals": 18}'
```

`"value": "1.5"` is stored as `"amount": "1500000000000000000"` (wei). Amounts can have up to 78 digits, enough for any token. Amounts are sent as JSON strings so that JavaScript clients don't lose digits.

Each account has one currency, and a transaction can only move one currency. A USD account and an ETH account can't be in the same transaction; to convert between them, use an [exchange](#converting-currencies).

## Running the business

The story above covers moving money. These features cover running a business on top of it. Each has full details in the [API reference](api.md).

### Freezing an account

Alice's account shows suspicious activity. Freeze it: money can still arrive, but none can leave.

```sh
curl -H "$T" -X PATCH $API/accounts/acc_alice -d '{"status": "frozen", "status_reason": "fraud review"}'
```

Any payment from Alice now fails with `422 account_frozen`. When the review is done, set `"status": "active"` again. When a customer leaves for good, empty their account and set `"status": "closed"`. Closing is final.

### Holds that expire

Real card authorizations lapse after about a week. Give a hold an `expires_at`, and if nobody commits it in time, the reserved money is released automatically:

```sh
curl -H "$T" -X POST $API/holds -d '{
  "reference": "auth-11",
  "currency": "USD",
  "expires_at": "2026-10-19T12:00:00Z",
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
}'
```

After that moment, committing fails with `409 hold_expired`, and the hold's status becomes `expired`.

### Fixing a mistake

You sent 10.00 to the wrong account. Don't delete anything (entries can never be deleted); **reverse** the transaction, and say why:

```sh
curl -H "$T" -X POST $API/transactions/txn_mistake/reversals -d '{"reference": "fix-1", "reason": "sent to the wrong account"}'
```

The reversal is a new transaction that puts the money back, so the history shows both the mistake and its correction. To add a note to a transaction without moving money, update its metadata:

```sh
curl -H "$T" -X PATCH $API/transactions/txn_ticket -d '{"metadata": {"invoice": "inv-42"}}'
```

### Paying many people at once

Payroll or marketplace payouts should happen completely or not at all. A **batch** posts many transactions together:

```sh
curl -H "$T" -X POST $API/batches -d '{
  "reference": "payroll-october",
  "transactions": [
    {
      "currency": "USD",
      "legs": [
        {
          "account_id": "acc_company",
          "direction": "debit",
          "value": "3000.00"
        },
        {
          "account_id": "acc_employee_1",
          "direction": "credit",
          "value": "3000.00"
        }
      ]
    },
    {
      "currency": "USD",
      "legs": [
        {
          "account_id": "acc_company",
          "direction": "debit",
          "value": "2800.00"
        },
        {
          "account_id": "acc_employee_2",
          "direction": "credit",
          "value": "2800.00"
        }
      ]
    }
  ]
}'
```

If the company can't pay everyone, nobody is paid and nothing changes.

### Converting currencies

A transaction can only move one currency, so a conversion is two transfers posted together as an **exchange**. Alice sells 50.00 USD to your USD account and receives 46.00 EUR from your EUR account:

```sh
curl -H "$T" -X POST $API/exchanges -d '{
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
}'
```

### Month-end

| Task | Request |
|---|---|
| Check the books balance | `GET /ledgers/ldg_main/trial-balance?at=2026-09-30T23:59:59Z`: every account, with totals that must be zero |
| Find a customer's transactions | `GET /ledgers/ldg_main/transactions?metadata.customer_id=c_77` |
| Send a customer a statement | `GET /accounts/acc_alice/statement.csv?from=2026-09-01T00:00:00Z&to=2026-10-01T00:00:00Z` |
| Financial statements | `GET /ledgers/ldg_main/balance-sheet?currency=USD` and `/income-statement?currency=USD&from=…`. Give accounts a `type` (asset, liability, equity, revenue, expense) first |
| Sign off the month | `POST /ledgers/ldg_main/periods` with `{"through": "2026-09-30T23:59:59.999999Z"}`. Nothing can be posted into September afterwards |

### Getting notified

Every change, such as a posted payment, an expired hold or a frozen account, is recorded as an **event**. Other systems can read `GET /events`, or `ledgerd` can push each event to a URL you choose (`LEDGER_WEBHOOK_URL`), signed so the receiver can check it really came from the ledger.

## Common mistakes

| Mistake | What happens | Fix |
|---|---|---|
| Creating an account before registering its currency | `422 unknown_currency` | `POST /currencies` first |
| Debits ≠ credits | `422 unbalanced` | Make both sides add up |
| `"value": "10.005"` for USD | `400 invalid_params` | Use at most the currency's decimals |
| Sending `"amount": "50.00"` | `400 invalid_params` | `amount` is in cents: `"5000"`, or use `value` |
| Spending more than is available | `422 insufficient_funds` | Remember holds reduce what's available |
| A new reference for a retry | The payment happens twice | Always retry with the original reference |
| `"value": "0"` to refund everything | `400 invalid_params` | Leave `amount` and `value` out entirely |
| Paying from a frozen account | `422 account_frozen` | Unfreeze it first, if the review allows |
| Committing a hold after its `expires_at` | `409 hold_expired` | Create a new hold |

## Glossary

| Term | Meaning |
|---|---|
| **Available** | `balance − pending_debit`: what an account can still spend |
| **Batch** | Several transactions posted together, all or nothing |
| **Commit** | Turn a hold into a real transaction (fully or partly) |
| **Credit** | A leg that increases an account's balance |
| **Debit** | A leg that decreases an account's balance |
| **Entry** | One account's line in a transaction; never changes once written |
| **Event** | A record of one change, for other systems to follow |
| **Exchange** | A currency conversion: two linked transfers in different currencies |
| **Frozen** | An account money can arrive in but not leave |
| **Hold** | Money reserved but not moved yet |
| **Indicator** | A readable account name such as `@world`, unique per ledger and currency |
| **Leg** | One part of a request: an account, a direction and an amount. Each leg becomes an entry |
| **Minor unit** | The smallest unit of a currency: cents, wei |
| **Period close** | Signing off the books through a date; nothing can be posted before it afterwards |
| **Reference** | Your unique name for a request, which makes retries safe |
| **Reversal** | A transaction that undoes a mistaken one, with a reason |
| **Untracked account** | A high-traffic account that isn't locked; its balance is computed on read |
| **Void** | Cancel a hold, releasing the reserved money |
| **`@world`** | The account representing everything outside the ledger |
