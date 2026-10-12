-- Refuse while any data relies on these features: reverting would change what
-- it means (a frozen account becoming spendable, a closed period reopening).
-- Events, snapshots, webhook progress and account types are simply dropped.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger.accounts WHERE status <> 'active' OR overdraft_limit IS NOT NULL)
       OR EXISTS (SELECT 1 FROM ledger.holds WHERE status = 'expired' OR expires_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM ledger.batches)
       OR EXISTS (SELECT 1 FROM ledger.transactions WHERE reversal_reason IS NOT NULL)
       OR EXISTS (SELECT 1 FROM ledger.ledgers WHERE closed_through IS NOT NULL)
    THEN
        RAISE EXCEPTION 'ledger: cannot downgrade while frozen, closed or overdraft accounts, expiring holds, batches, reversals or closed periods exist';
    END IF;
END;
$$;

DROP TABLE ledger.account_snapshots;
DROP TABLE ledger.webhook_cursors;
DROP TABLE ledger.events;
DROP TABLE ledger.period_closes;

DROP TRIGGER ledgers_guard_update ON ledger.ledgers;
DROP FUNCTION ledger.guard_ledger_update();
ALTER TABLE ledger.ledgers DROP COLUMN closed_through;

DROP INDEX ledger.transactions_metadata_idx;
DROP INDEX ledger.transactions_ledger_id_idx;
ALTER TABLE ledger.transactions
    DROP CONSTRAINT transactions_reference_or_hold,
    ADD CONSTRAINT transactions_reference_or_hold CHECK (reference IS NOT NULL OR hold_id IS NOT NULL),
    DROP CONSTRAINT transactions_reversal,
    DROP COLUMN reversal_reason,
    DROP COLUMN batch_id;
DROP TABLE ledger.batches;

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

DROP INDEX ledger.holds_expiry_idx;
ALTER TABLE ledger.holds
    DROP CONSTRAINT holds_status_check,
    ADD CONSTRAINT holds_status_check CHECK (status IN ('pending', 'committed', 'voided')),
    DROP COLUMN expires_at;

CREATE OR REPLACE FUNCTION ledger.check_entry() RETURNS TRIGGER
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

CREATE OR REPLACE FUNCTION ledger.guard_account_update() RETURNS TRIGGER
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

ALTER TABLE ledger.accounts DROP CONSTRAINT accounts_no_overdraft;
ALTER TABLE ledger.accounts
    DROP CONSTRAINT accounts_closed_empty,
    DROP CONSTRAINT accounts_overdraft_limit,
    DROP COLUMN type,
    DROP COLUMN overdraft_limit,
    DROP COLUMN status_reason,
    DROP COLUMN status,
    ADD CONSTRAINT accounts_no_overdraft CHECK (allow_negative OR balance - pending_debit >= 0);
