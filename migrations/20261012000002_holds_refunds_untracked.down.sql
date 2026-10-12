-- Entries on untracked accounts have no running balance, so this schema
-- cannot hold them. Refuse rather than fail halfway.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger.accounts WHERE untracked) THEN
        RAISE EXCEPTION 'ledger: cannot downgrade while untracked accounts exist';
    END IF;
END;
$$;

ALTER TABLE ledger.entries
    DROP CONSTRAINT entries_running_balance,
    ALTER COLUMN balance_after SET NOT NULL,
    ALTER COLUMN account_version SET NOT NULL;


CREATE OR REPLACE FUNCTION ledger.guard_transaction_update() RETURNS TRIGGER
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


ALTER TABLE ledger.transactions
    DROP COLUMN refund_of,
    DROP COLUMN hold_id;


DROP TABLE ledger.holds;


DROP FUNCTION ledger.guard_hold_update();


DROP TRIGGER accounts_guard_update ON ledger.accounts;


DROP FUNCTION ledger.guard_account_update();


ALTER TABLE ledger.accounts
    DROP CONSTRAINT accounts_untracked,
    DROP CONSTRAINT accounts_no_overdraft,
    DROP COLUMN pending_debit,
    DROP COLUMN pending_credit,
    DROP COLUMN untracked;


ALTER TABLE ledger.accounts ADD CONSTRAINT accounts_no_overdraft CHECK (allow_negative OR balance >= 0);
