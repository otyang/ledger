-- Holds reserve funds in pending_debit / pending_credit until committed or
-- voided. Available balance is balance - pending_debit.
--
-- Untracked accounts (e.g. @world) keep no cached totals: postings to them
-- take no row lock, and their balance is computed from entries on read.
ALTER TABLE ledger.accounts
    ADD COLUMN pending_debit  BIGINT  NOT NULL DEFAULT 0 CHECK (pending_debit >= 0),
    ADD COLUMN pending_credit BIGINT  NOT NULL DEFAULT 0 CHECK (pending_credit >= 0),
    ADD COLUMN untracked      BOOLEAN NOT NULL DEFAULT false;


ALTER TABLE ledger.accounts DROP CONSTRAINT accounts_no_overdraft;


ALTER TABLE ledger.accounts
    ADD CONSTRAINT accounts_no_overdraft CHECK (allow_negative OR balance - pending_debit >= 0),
    ADD CONSTRAINT accounts_untracked CHECK (
        NOT untracked OR (
            allow_negative AND credit_total = 0 AND debit_total = 0 AND balance = 0
            AND pending_debit = 0 AND pending_credit = 0 AND version = 0
        )
    );


-- An account's identity and tracking mode never change after creation.
CREATE FUNCTION ledger.guard_account_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.ledger_id, NEW.indicator, NEW.currency, NEW.untracked, NEW.created_at)
        IS DISTINCT FROM
       (OLD.id, OLD.ledger_id, OLD.indicator, OLD.currency, OLD.untracked, OLD.created_at)
    THEN
        RAISE EXCEPTION 'ledger: account id, ledger, indicator, currency and untracked are immutable';
    END IF;
    RETURN NEW;
END;
$$;


CREATE TRIGGER accounts_guard_update BEFORE UPDATE ON ledger.accounts
    FOR EACH ROW EXECUTE FUNCTION ledger.guard_account_update();


CREATE TABLE ledger.holds (
    id             TEXT        PRIMARY KEY CHECK (ledger.is_id('hld', id)),
    reference      TEXT        NOT NULL CHECK (reference <> ''),
    status         TEXT        NOT NULL CHECK (status IN ('pending', 'committed', 'voided')),
    currency       TEXT        NOT NULL,
    description    TEXT,
    metadata       JSONB,
    legs           JSONB       NOT NULL,
    request_hash   TEXT        NOT NULL,
    transaction_id TEXT        REFERENCES ledger.transactions (id),
    created_at     TIMESTAMPTZ NOT NULL,
    resolved_at    TIMESTAMPTZ,
    CONSTRAINT holds_reference_key UNIQUE (reference),
    CONSTRAINT holds_resolution CHECK (
        (status = 'pending') = (resolved_at IS NULL)
        AND (status = 'committed') = (transaction_id IS NOT NULL)
    )
);


CREATE INDEX holds_pending_idx ON ledger.holds (created_at) WHERE status = 'pending';


-- A hold resolves exactly once (pending -> committed | voided); apart from that
-- only metadata may change.
CREATE FUNCTION ledger.guard_hold_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'pending' AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'ledger: hold % is already %', OLD.id, OLD.status;
    END IF;
    IF (NEW.id, NEW.reference, NEW.currency, NEW.description, NEW.legs, NEW.request_hash, NEW.created_at)
        IS DISTINCT FROM
       (OLD.id, OLD.reference, OLD.currency, OLD.description, OLD.legs, OLD.request_hash, OLD.created_at)
    THEN
        RAISE EXCEPTION 'ledger: only the status and metadata of a hold can be updated';
    END IF;
    RETURN NEW;
END;
$$;


CREATE TRIGGER holds_guard_update BEFORE UPDATE ON ledger.holds
    FOR EACH ROW EXECUTE FUNCTION ledger.guard_hold_update();


CREATE TRIGGER holds_forbid_delete BEFORE DELETE ON ledger.holds
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();


-- refund_of links a refund to the transaction it refunds; hold_id links the
-- transaction that committed a hold (at most one per hold).
ALTER TABLE ledger.transactions
    ADD COLUMN refund_of TEXT REFERENCES ledger.transactions (id),
    ADD COLUMN hold_id   TEXT REFERENCES ledger.holds (id),
    ADD CONSTRAINT transactions_hold_id_key UNIQUE (hold_id),
    ADD CONSTRAINT transactions_refund_not_hold CHECK (refund_of IS NULL OR hold_id IS NULL);


CREATE INDEX transactions_refund_of_idx ON ledger.transactions (refund_of) WHERE refund_of IS NOT NULL;


CREATE OR REPLACE FUNCTION ledger.guard_transaction_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.reference, NEW.status, NEW.currency, NEW.description, NEW.request_hash, NEW.created_at, NEW.refund_of, NEW.hold_id)
        IS DISTINCT FROM
       (OLD.id, OLD.reference, OLD.status, OLD.currency, OLD.description, OLD.request_hash, OLD.created_at, OLD.refund_of, OLD.hold_id)
    THEN
        RAISE EXCEPTION 'ledger: only metadata of a transaction can be updated';
    END IF;
    RETURN NEW;
END;
$$;


-- Entries on untracked accounts carry no running balance or version.
ALTER TABLE ledger.entries
    ALTER COLUMN balance_after DROP NOT NULL,
    ALTER COLUMN account_version DROP NOT NULL,
    ADD CONSTRAINT entries_running_balance CHECK ((balance_after IS NULL) = (account_version IS NULL));
