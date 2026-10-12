-- Business features: account controls and types, hold expiry, reversals,
-- batches and exchanges, period close, an event outbox, and balance
-- snapshots for untracked accounts. Every addition defaults to the previous
-- behaviour: existing accounts stay active with no limit, holds never expire.

-- Accounts: status (active, frozen: no money leaves, closed: nothing moves),
-- an optional overdraft limit, and an optional accounting type.
ALTER TABLE ledger.accounts
    ADD COLUMN status          TEXT          NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen', 'closed')),
    ADD COLUMN status_reason   TEXT,
    ADD COLUMN overdraft_limit NUMERIC(78,0) CHECK (overdraft_limit > 0),
    ADD COLUMN type            TEXT          CHECK (type IN ('asset', 'liability', 'equity', 'revenue', 'expense')),
    ADD CONSTRAINT accounts_overdraft_limit CHECK (overdraft_limit IS NULL OR NOT allow_negative),
    ADD CONSTRAINT accounts_closed_empty CHECK (status <> 'closed' OR (balance = 0 AND pending_debit = 0 AND pending_credit = 0));

ALTER TABLE ledger.accounts DROP CONSTRAINT accounts_no_overdraft;
ALTER TABLE ledger.accounts ADD CONSTRAINT accounts_no_overdraft
    CHECK (allow_negative OR balance - pending_debit >= -COALESCE(overdraft_limit, 0));

-- Identity stays immutable; a type can be set once; closing is final.
CREATE OR REPLACE FUNCTION ledger.guard_account_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.ledger_id, NEW.indicator, NEW.currency, NEW.untracked, NEW.created_at)
        IS DISTINCT FROM
       (OLD.id, OLD.ledger_id, OLD.indicator, OLD.currency, OLD.untracked, OLD.created_at)
    THEN
        RAISE EXCEPTION 'ledger: account id, ledger, indicator, currency and untracked are immutable';
    END IF;
    IF OLD.type IS NOT NULL AND NEW.type IS DISTINCT FROM OLD.type THEN
        RAISE EXCEPTION 'ledger: account % already has type %', OLD.id, OLD.type;
    END IF;
    IF OLD.status = 'closed' AND NEW.status <> 'closed' THEN
        RAISE EXCEPTION 'ledger: account % is closed', OLD.id;
    END IF;
    RETURN NEW;
END;
$$;

-- Holds can carry an expiry; expired holds are released like voided ones.
ALTER TABLE ledger.holds
    ADD COLUMN expires_at TIMESTAMPTZ,
    DROP CONSTRAINT holds_status_check,
    ADD CONSTRAINT holds_status_check CHECK (status IN ('pending', 'committed', 'voided', 'expired'));

CREATE INDEX holds_expiry_idx ON ledger.holds (expires_at) WHERE status = 'pending' AND expires_at IS NOT NULL;

CREATE OR REPLACE FUNCTION ledger.guard_hold_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.ledger_id, NEW.reference, NEW.currency, NEW.description, NEW.legs, NEW.request_hash,
        NEW.created_at, NEW.expires_at)
        IS DISTINCT FROM
       (OLD.id, OLD.ledger_id, OLD.reference, OLD.currency, OLD.description, OLD.legs, OLD.request_hash,
        OLD.created_at, OLD.expires_at)
    THEN
        RAISE EXCEPTION 'ledger: only the status and metadata of a hold can be updated';
    END IF;
    IF OLD.status <> 'pending'
       AND (NEW.status, NEW.transaction_id, NEW.resolved_at) IS DISTINCT FROM (OLD.status, OLD.transaction_id, OLD.resolved_at)
    THEN
        RAISE EXCEPTION 'ledger: hold % is already %', OLD.id, OLD.status;
    END IF;
    IF NEW.status = 'committed' AND NOT EXISTS (
        SELECT 1 FROM ledger.transactions WHERE id = NEW.transaction_id AND hold_id = NEW.id
    ) THEN
        RAISE EXCEPTION 'ledger: hold % must point at its own commit transaction', NEW.id;
    END IF;
    RETURN NEW;
END;
$$;

-- Batches post several transactions at once; an exchange is a batch of two
-- transactions in different currencies with the rate recorded.
CREATE TABLE ledger.batches (
    id           TEXT        PRIMARY KEY CHECK (ledger.is_id('bat', id)),
    ledger_id    TEXT        NOT NULL REFERENCES ledger.ledgers (id),
    reference    TEXT        NOT NULL CHECK (reference <> ''),
    kind         TEXT        NOT NULL CHECK (kind IN ('batch', 'exchange')),
    rate         TEXT        CHECK (rate IS NULL OR kind = 'exchange'),
    description  TEXT,
    metadata     JSONB,
    request_hash TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    CONSTRAINT batches_reference_key UNIQUE (ledger_id, reference)
);

CREATE TRIGGER batches_forbid_change BEFORE UPDATE OR DELETE ON ledger.batches
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();

CREATE TRIGGER batches_forbid_truncate BEFORE TRUNCATE ON ledger.batches
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();

-- Batch members need no reference of their own. A reversal is a refund of
-- everything that is left, with a reason.
ALTER TABLE ledger.transactions
    ADD COLUMN batch_id        TEXT REFERENCES ledger.batches (id),
    ADD COLUMN reversal_reason TEXT,
    ADD CONSTRAINT transactions_reversal CHECK (reversal_reason IS NULL OR (refund_of IS NOT NULL AND reversal_reason <> '')),
    DROP CONSTRAINT transactions_reference_or_hold,
    ADD CONSTRAINT transactions_reference_or_hold CHECK (reference IS NOT NULL OR hold_id IS NOT NULL OR batch_id IS NOT NULL);

CREATE INDEX transactions_batch_id_idx ON ledger.transactions (batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX transactions_ledger_id_idx ON ledger.transactions (ledger_id, id);
CREATE INDEX transactions_metadata_idx ON ledger.transactions USING gin (metadata jsonb_path_ops);

CREATE OR REPLACE FUNCTION ledger.guard_transaction_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.ledger_id, NEW.reference, NEW.status, NEW.currency, NEW.description, NEW.request_hash,
        NEW.created_at, NEW.refund_of, NEW.hold_id, NEW.txid, NEW.batch_id, NEW.reversal_reason)
        IS DISTINCT FROM
       (OLD.id, OLD.ledger_id, OLD.reference, OLD.status, OLD.currency, OLD.description, OLD.request_hash,
        OLD.created_at, OLD.refund_of, OLD.hold_id, OLD.txid, OLD.batch_id, OLD.reversal_reason)
    THEN
        RAISE EXCEPTION 'ledger: only metadata of a transaction can be updated';
    END IF;
    IF NEW.refund_count < OLD.refund_count THEN
        RAISE EXCEPTION 'ledger: refund_count can only increase';
    END IF;
    RETURN NEW;
END;
$$;

-- Period close: nothing can be posted at or before a ledger's closed_through,
-- which only moves forward. Each close keeps the trial balance it was signed
-- off with.
ALTER TABLE ledger.ledgers ADD COLUMN closed_through TIMESTAMPTZ;

CREATE FUNCTION ledger.guard_ledger_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.created_at) IS DISTINCT FROM (OLD.id, OLD.created_at) THEN
        RAISE EXCEPTION 'ledger: ledger id and created_at are immutable';
    END IF;
    IF OLD.closed_through IS NOT NULL AND (NEW.closed_through IS NULL OR NEW.closed_through < OLD.closed_through) THEN
        RAISE EXCEPTION 'ledger: ledger % is closed through %; periods cannot be reopened', OLD.id, OLD.closed_through;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER ledgers_guard_update BEFORE UPDATE ON ledger.ledgers
    FOR EACH ROW EXECUTE FUNCTION ledger.guard_ledger_update();

CREATE TABLE ledger.period_closes (
    ledger_id      TEXT        NOT NULL REFERENCES ledger.ledgers (id),
    closed_through TIMESTAMPTZ NOT NULL,
    closed_at      TIMESTAMPTZ NOT NULL,
    trial_balance  JSONB       NOT NULL,
    PRIMARY KEY (ledger_id, closed_through)
);

CREATE TRIGGER period_closes_forbid_change BEFORE UPDATE OR DELETE ON ledger.period_closes
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();

CREATE TRIGGER period_closes_forbid_truncate BEFORE TRUNCATE ON ledger.period_closes
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();

-- Entries: besides the existing checks, an account's status and the ledger's
-- closed period are enforced here, independent of the Go code.
CREATE OR REPLACE FUNCTION ledger.check_entry() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    t ledger.transactions;
    a ledger.accounts;
    closed TIMESTAMPTZ;
BEGIN
    SELECT * INTO t FROM ledger.transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key reports it
    END IF;
    IF t.txid <> pg_current_xact_id()::text::bigint THEN
        RAISE EXCEPTION 'ledger: entries can only be added in the database transaction that created transaction %', t.id;
    END IF;
    -- KEY SHARE (what the foreign key check takes anyway) waits for a status
    -- change in progress, then reads the new status.
    SELECT * INTO a FROM ledger.accounts WHERE id = NEW.account_id FOR KEY SHARE;
    IF FOUND AND (a.ledger_id <> t.ledger_id OR a.currency <> t.currency) THEN
        RAISE EXCEPTION 'ledger: account % (% %) does not match transaction % (% %)',
            a.id, a.ledger_id, a.currency, t.id, t.ledger_id, t.currency;
    END IF;
    IF FOUND AND (a.status = 'closed' OR (a.status = 'frozen' AND NEW.direction = 'debit')) THEN
        RAISE EXCEPTION 'ledger: account % is %', a.id, a.status;
    END IF;
    SELECT closed_through INTO closed FROM ledger.ledgers WHERE id = t.ledger_id;
    IF closed IS NOT NULL AND NEW.created_at <= closed THEN
        RAISE EXCEPTION 'ledger: ledger % is closed through %', t.ledger_id, closed;
    END IF;
    RETURN NEW;
END;
$$;

-- Event outbox: one row per change, written in the same database transaction
-- as the change. Consumers page by (txid, seq) like untracked entries, so no
-- event can appear behind a cursor.
CREATE TABLE ledger.events (
    seq        BIGSERIAL   PRIMARY KEY,
    id         TEXT        NOT NULL UNIQUE CHECK (ledger.is_id('evt', id)),
    type       TEXT        NOT NULL,
    ledger_id  TEXT        REFERENCES ledger.ledgers (id),
    subject_id TEXT        NOT NULL,
    data       JSONB       NOT NULL,
    txid       BIGINT      NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX events_txid_idx ON ledger.events (txid, seq);

CREATE TRIGGER events_forbid_update BEFORE UPDATE ON ledger.events
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();

-- Delivery progress of the webhook dispatcher.
CREATE TABLE ledger.webhook_cursors (
    name       TEXT        PRIMARY KEY,
    event_id   TEXT        NOT NULL REFERENCES ledger.events (id),
    updated_at TIMESTAMPTZ NOT NULL
);

-- Running totals of untracked accounts over all entries written by database
-- transactions older than txid_bound, so reads only sum newer entries.
CREATE TABLE ledger.account_snapshots (
    account_id   TEXT        PRIMARY KEY REFERENCES ledger.accounts (id),
    txid_bound   BIGINT      NOT NULL,
    credit_total NUMERIC     NOT NULL CHECK (credit_total >= 0),
    debit_total  NUMERIC     NOT NULL CHECK (debit_total >= 0),
    entry_count  BIGINT      NOT NULL CHECK (entry_count >= 0),
    refreshed_at TIMESTAMPTZ NOT NULL
);
