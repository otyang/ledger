# ledger

[![Go Reference](https://pkg.go.dev/badge/github.com/otyang/ledger.svg)](https://pkg.go.dev/github.com/otyang/ledger)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

**A double-entry ledger for Go and PostgreSQL.** Money goes in, money goes out, and the books always add up to exactly zero. Not "roughly zero". Not "zero after the nightly job". Zero.

Use it as a Go library inside your app, or run it as a standalone HTTP service (`ledgerd`).

## Why you might like it

- **The database has your back.** Every transaction balances, entries never change, and balances can't drift. Postgres enforces it, so even a bug in your code can't cook the books.
- **No rounding. Ever.** Amounts are exact up to 78 digits: cents, yen, or 18-decimal wei. Send `"5403.23"` and it's stored as exactly `540323` cents.
- **Retry without fear.** Every write is idempotent on a reference you choose. Network blip? Send it again. Nobody gets charged twice.
- **Real-world money moves.** Card holds that expire, partial captures, refunds, reversals, payroll batches, currency exchange, frozen accounts and overdraft limits.
- **Accountant-friendly.** Trial balance, balance sheet, income statement, CSV statements and month-end close, plus webhooks for everything that happens.

## Try it in a minute

You need Go 1.25+ and PostgreSQL.

```sh
createdb ledger
LEDGER_DATABASE_URL="postgres://postgres@localhost:5432/ledger?sslmode=disable" \
LEDGER_API_TOKEN=change-me \
go run ./cmd/ledgerd
```

`ledgerd` sets up its tables and listens on port **7710**. Say hello:

```sh
curl -H "Authorization: Bearer change-me" -X POST localhost:7710/currencies -d '{"code": "USD", "decimals": 2}'
```

Prefer Docker? `make docker-build`, then see the [handbook](docs/handbook.md#run-with-docker).

## Or embed it in Go

```sh
go get github.com/otyang/ledger
```

```go
svc := ledger.New(ledger.OpenDB(dsn))
svc.Migrate(ctx)
svc.RegisterCurrency(ctx, ledger.CurrencyParams{Code: "USD", Decimals: 2})

// Alice pays Bob 5403.23 USD. Debits equal credits, or it doesn't happen.
svc.Post(ctx, ledger.PostParams{Reference: "pay-1", Currency: "USD", Legs: []ledger.Leg{
	{AccountID: alice, Direction: ledger.Debit, Value: "5403.23"},
	{AccountID: bob, Direction: ledger.Credit, Value: "5403.23"},
}})
```

## Where to next

| I want to… | Go here |
|---|---|
| Browse the HTTP API and try requests | **[Interactive API reference](https://otyang.github.io/ledger/docs/api.html)** on the website |
| Set it up properly, configure it, run it in production | **[The handbook](docs/handbook.md)**: configuration, Go library, concepts, guarantees, operations, troubleshooting |
| Understand double-entry bookkeeping from scratch | [Beginner guide](docs/guide.md), with a worked example |
| Look up every request and response field | [API reference (Markdown)](docs/api.md) or the [OpenAPI spec](docs/openapi.yaml) |
| Know how it works inside before changing it | [Architecture and internals](docs/architecture.md) |

Everything is also on the website: **[otyang.github.io/ledger/docs](https://otyang.github.io/ledger/docs/)**.

## Documentation

All the docs in one list:

| Doc | What's in it |
|---|---|
| [Website](https://otyang.github.io/ledger/docs/) | The handbook as a website, with a contents sidebar and copyable code |
| [Interactive API reference](https://otyang.github.io/ledger/docs/api.html) | Every HTTP endpoint with schemas and code samples, rendered from the OpenAPI spec |
| [OpenAPI 3.0 spec](docs/openapi.yaml) | The machine-readable API definition, for code generators, Postman or your own tools |
| [Handbook](docs/handbook.md) | Setup, configuration, the Go library, concepts, guarantees, operations and troubleshooting |
| [Beginner guide](docs/guide.md) | Double-entry bookkeeping explained with a worked example, from top-up to month-end |
| [API reference (Markdown)](docs/api.md) | Every endpoint's fields, status codes, errors and real example responses |
| [Architecture and internals](docs/architecture.md) | How posting, locking, idempotency, holds, events and the database rules work |
| [Go package docs](https://pkg.go.dev/github.com/otyang/ledger) | Generated reference for every exported Go type and function |

## Contributing

Pull requests are welcome. Run `make check` first (formatting, vet, and every test with the race detector), add tests for new behaviour, and add your name or GitHub handle to [AUTHORS](AUTHORS). No email needed.

Maintained by [otyang](https://github.com/otyang), with thanks to [everyone who has contributed](https://github.com/otyang/ledger/graphs/contributors).

## License

[Apache License 2.0](LICENSE). Copyright 2026 The ledger Authors. Use it, change it, ship it in commercial products; just keep the [LICENSE](LICENSE) and [NOTICE](NOTICE) files.
