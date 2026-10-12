-- IDs are "<prefix>_<uuidv7>" (see ids.go).
CREATE FUNCTION ledger.is_id(prefix TEXT, value TEXT) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT value ~ ('^' || prefix || '_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')
$$;


CREATE TABLE ledger.ledgers (
    id         TEXT        PRIMARY KEY CHECK (ledger.is_id('ldg', id)),
    name       TEXT        NOT NULL CHECK (name <> ''),
    metadata   JSONB,
    created_at TIMESTAMPTZ NOT NULL
);


CREATE TABLE ledger.accounts (
    id             TEXT        PRIMARY KEY CHECK (ledger.is_id('acc', id)),
    ledger_id      TEXT        NOT NULL REFERENCES ledger.ledgers (id),
    indicator      TEXT        CHECK (indicator ~ '^@[A-Za-z0-9_.:-]+$'),
    currency       TEXT        NOT NULL CHECK (currency ~ '^[A-Z][A-Z0-9]{2,9}$'),
    allow_negative BOOLEAN     NOT NULL DEFAULT false,
    credit_total   BIGINT      NOT NULL DEFAULT 0 CHECK (credit_total >= 0),
    debit_total    BIGINT      NOT NULL DEFAULT 0 CHECK (debit_total >= 0),
    balance        BIGINT      NOT NULL DEFAULT 0,
    version        BIGINT      NOT NULL DEFAULT 0 CHECK (version >= 0),
    metadata       JSONB,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    CONSTRAINT accounts_balance_consistent CHECK (balance = credit_total - debit_total),
    CONSTRAINT accounts_no_overdraft CHECK (allow_negative OR balance >= 0),
    CONSTRAINT accounts_indicator_key UNIQUE (ledger_id, indicator, currency)
);


CREATE TABLE ledger.transactions (
    id           TEXT        PRIMARY KEY CHECK (ledger.is_id('txn', id)),
    reference    TEXT        NOT NULL CHECK (reference <> ''),
    status       TEXT        NOT NULL CHECK (status IN ('posted')),
    currency     TEXT        NOT NULL,
    description  TEXT,
    metadata     JSONB,
    request_hash TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    CONSTRAINT transactions_reference_key UNIQUE (reference)
);


CREATE TABLE ledger.entries (
    id              TEXT        PRIMARY KEY CHECK (ledger.is_id('ent', id)),
    transaction_id  TEXT        NOT NULL REFERENCES ledger.transactions (id),
    account_id      TEXT        NOT NULL REFERENCES ledger.accounts (id),
    direction       TEXT        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    balance_after   BIGINT      NOT NULL,
    account_version BIGINT      NOT NULL CHECK (account_version > 0),
    created_at      TIMESTAMPTZ NOT NULL,
    CONSTRAINT entries_account_version_key UNIQUE (account_id, account_version)
);


CREATE INDEX entries_transaction_id_idx ON ledger.entries (transaction_id);


CREATE INDEX entries_account_created_at_idx ON ledger.entries (account_id, created_at);


-- Immutability: entries can never change; transactions may only have their
-- metadata updated.
CREATE FUNCTION ledger.forbid_change() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger: % on %.% is not allowed', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME;
END;
$$;


CREATE FUNCTION ledger.guard_transaction_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.reference, NEW.status, NEW.currency, NEW.description, NEW.request_hash, NEW.created_at)
        IS DISTINCT FROM
       (OLD.id, OLD.reference, OLD.status, OLD.currency, OLD.description, OLD.request_hash, OLD.created_at)
    THEN
        RAISE EXCEPTION 'ledger: only metadata of a transaction can be updated';
    END IF;
    RETURN NEW;
END;
$$;


CREATE TRIGGER transactions_guard_update BEFORE UPDATE ON ledger.transactions
    FOR EACH ROW EXECUTE FUNCTION ledger.guard_transaction_update();


CREATE TRIGGER transactions_forbid_delete BEFORE DELETE ON ledger.transactions
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();


CREATE TRIGGER transactions_forbid_truncate BEFORE TRUNCATE ON ledger.transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();


CREATE TRIGGER entries_forbid_change BEFORE UPDATE OR DELETE ON ledger.entries
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();


CREATE TRIGGER entries_forbid_truncate BEFORE TRUNCATE ON ledger.entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();


-- Double-entry invariant, checked at COMMIT so the Go code can insert a
-- transaction's entries in any order: every transaction has at least two
-- entries and its debits equal its credits.
CREATE FUNCTION ledger.check_transaction_balanced() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    -- TG_ARGV[0] names the column holding the transaction id on the firing table.
    txn_id  TEXT := to_jsonb(NEW) ->> TG_ARGV[0];
    n       BIGINT;
    net     NUMERIC;
BEGIN
    SELECT count(*), COALESCE(sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END), 0)
      INTO n, net
      FROM ledger.entries
     WHERE transaction_id = txn_id;

    IF n < 2 THEN
        RAISE EXCEPTION 'ledger: transaction % has % entries, need at least 2', txn_id, n;
    END IF;
    IF net <> 0 THEN
        RAISE EXCEPTION 'ledger: transaction % is unbalanced (debits - credits = %)', txn_id, net;
    END IF;
    RETURN NULL;
END;
$$;


CREATE CONSTRAINT TRIGGER transactions_balanced AFTER INSERT ON ledger.transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_balanced('id');


CREATE CONSTRAINT TRIGGER entries_balanced AFTER INSERT ON ledger.entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_balanced('transaction_id');
