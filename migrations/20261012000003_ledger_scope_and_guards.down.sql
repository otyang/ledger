CREATE CONSTRAINT TRIGGER entries_balanced AFTER INSERT ON ledger.entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_balanced('transaction_id');

DROP TRIGGER entries_check ON ledger.entries;
DROP FUNCTION ledger.check_entry();
DROP TRIGGER holds_forbid_truncate ON ledger.holds;

CREATE OR REPLACE FUNCTION ledger.guard_hold_update() RETURNS TRIGGER
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

ALTER TABLE ledger.transactions DROP COLUMN refund_count;
DROP INDEX ledger.entries_account_txid_idx;
ALTER TABLE ledger.entries DROP COLUMN txid;
ALTER TABLE ledger.transactions DROP COLUMN txid;

ALTER TABLE ledger.holds
    DROP CONSTRAINT holds_reference_key,
    ADD CONSTRAINT holds_reference_key UNIQUE (reference);

-- Commit transactions created by this version have no reference; give them
-- the reference the previous version derived for them.
ALTER TABLE ledger.transactions DISABLE TRIGGER transactions_guard_update;
UPDATE ledger.transactions SET reference = hold_id || ':commit' WHERE reference IS NULL;
ALTER TABLE ledger.transactions ENABLE TRIGGER transactions_guard_update;

ALTER TABLE ledger.transactions
    DROP CONSTRAINT transactions_reference_or_hold,
    DROP CONSTRAINT transactions_reference_key,
    ADD CONSTRAINT transactions_reference_key UNIQUE (reference),
    ALTER COLUMN reference SET NOT NULL;

ALTER TABLE ledger.holds DROP COLUMN ledger_id;
ALTER TABLE ledger.transactions DROP COLUMN ledger_id;
