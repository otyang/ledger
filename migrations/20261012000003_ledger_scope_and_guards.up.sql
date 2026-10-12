-- Transactions and holds belong to exactly one ledger, and references are
-- unique per ledger rather than globally.
ALTER TABLE ledger.transactions ADD COLUMN ledger_id TEXT REFERENCES ledger.ledgers (id);
ALTER TABLE ledger.holds ADD COLUMN ledger_id TEXT REFERENCES ledger.ledgers (id);

UPDATE ledger.transactions t
   SET ledger_id = (SELECT a.ledger_id
                      FROM ledger.entries e JOIN ledger.accounts a ON a.id = e.account_id
                     WHERE e.transaction_id = t.id
                     LIMIT 1);
UPDATE ledger.holds h
   SET ledger_id = (SELECT a.ledger_id FROM ledger.accounts a WHERE a.id = h.legs -> 0 ->> 'account_id');

ALTER TABLE ledger.transactions ALTER COLUMN ledger_id SET NOT NULL;
ALTER TABLE ledger.holds ALTER COLUMN ledger_id SET NOT NULL;

-- A hold's commit transaction is identified by hold_id and has no reference,
-- so no user-chosen reference can collide with it.
ALTER TABLE ledger.transactions
    ALTER COLUMN reference DROP NOT NULL,
    DROP CONSTRAINT transactions_reference_key,
    ADD CONSTRAINT transactions_reference_key UNIQUE (ledger_id, reference),
    ADD CONSTRAINT transactions_reference_or_hold CHECK (reference IS NOT NULL OR hold_id IS NOT NULL);

ALTER TABLE ledger.holds
    DROP CONSTRAINT holds_reference_key,
    ADD CONSTRAINT holds_reference_key UNIQUE (ledger_id, reference);

-- txid is the top-level transaction ID of the database transaction that wrote
-- the row (the same inside savepoints). Entries may only be added by the
-- database transaction that created their ledger transaction, and untracked
-- accounts are paged by txid so that no entry can appear behind a cursor.
ALTER TABLE ledger.transactions ADD COLUMN txid BIGINT NOT NULL DEFAULT (pg_current_xact_id()::text::bigint);
ALTER TABLE ledger.entries ADD COLUMN txid BIGINT NOT NULL DEFAULT (pg_current_xact_id()::text::bigint);
CREATE INDEX entries_account_txid_idx ON ledger.entries (account_id, txid, id);

-- Every refund increments the original's refund_count. Writing that row makes
-- concurrent refunds conflict at every isolation level, so under REPEATABLE
-- READ the second refund fails with a serialization error instead of
-- refunding against a stale view.
ALTER TABLE ledger.transactions ADD COLUMN refund_count BIGINT NOT NULL DEFAULT 0 CHECK (refund_count >= 0);

CREATE OR REPLACE FUNCTION ledger.guard_transaction_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.ledger_id, NEW.reference, NEW.status, NEW.currency, NEW.description, NEW.request_hash,
        NEW.created_at, NEW.refund_of, NEW.hold_id, NEW.txid)
        IS DISTINCT FROM
       (OLD.id, OLD.ledger_id, OLD.reference, OLD.status, OLD.currency, OLD.description, OLD.request_hash,
        OLD.created_at, OLD.refund_of, OLD.hold_id, OLD.txid)
    THEN
        RAISE EXCEPTION 'ledger: only metadata of a transaction can be updated';
    END IF;
    IF NEW.refund_count < OLD.refund_count THEN
        RAISE EXCEPTION 'ledger: refund_count can only increase';
    END IF;
    RETURN NEW;
END;
$$;

-- Holds: identity is immutable, a resolved hold never changes again, and a
-- committed hold must point at the transaction that committed it.
CREATE OR REPLACE FUNCTION ledger.guard_hold_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.ledger_id, NEW.reference, NEW.currency, NEW.description, NEW.legs, NEW.request_hash, NEW.created_at)
        IS DISTINCT FROM
       (OLD.id, OLD.ledger_id, OLD.reference, OLD.currency, OLD.description, OLD.legs, OLD.request_hash, OLD.created_at)
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

CREATE TRIGGER holds_forbid_truncate BEFORE TRUNCATE ON ledger.holds
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();

-- Each entry must belong to a transaction created by the current database
-- transaction, and its account must match the transaction's ledger and
-- currency. Because entries can't be added later, the balance check only needs
-- to run once per transaction (the deferred transactions_balanced trigger).
CREATE FUNCTION ledger.check_entry() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    t ledger.transactions;
    a ledger.accounts;
BEGIN
    SELECT * INTO t FROM ledger.transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key reports it
    END IF;
    IF t.txid <> pg_current_xact_id()::text::bigint THEN
        RAISE EXCEPTION 'ledger: entries can only be added in the database transaction that created transaction %', t.id;
    END IF;
    SELECT * INTO a FROM ledger.accounts WHERE id = NEW.account_id;
    IF FOUND AND (a.ledger_id <> t.ledger_id OR a.currency <> t.currency) THEN
        RAISE EXCEPTION 'ledger: account % (% %) does not match transaction % (% %)',
            a.id, a.ledger_id, a.currency, t.id, t.ledger_id, t.currency;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER entries_check BEFORE INSERT ON ledger.entries
    FOR EACH ROW EXECUTE FUNCTION ledger.check_entry();

-- Replaced by entries_check: running the balance check per entry made commits
-- O(entries^2).
DROP TRIGGER entries_balanced ON ledger.entries;
