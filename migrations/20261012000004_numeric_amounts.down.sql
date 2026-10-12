-- BIGINT can't hold larger amounts. Refuse rather than fail halfway.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM ledger.accounts
         WHERE greatest(credit_total, debit_total, abs(balance), pending_debit, pending_credit) > 9223372036854775807
    ) OR EXISTS (
        SELECT 1 FROM ledger.entries WHERE greatest(amount, abs(balance_after)) > 9223372036854775807
    ) OR EXISTS (
        SELECT 1 FROM ledger.holds, jsonb_array_elements(legs) AS leg
         WHERE (leg ->> 'amount')::numeric > 9223372036854775807
    ) THEN
        RAISE EXCEPTION 'ledger: cannot downgrade while amounts exceed BIGINT';
    END IF;
END;
$$;

ALTER TABLE ledger.accounts
    ALTER COLUMN credit_total   TYPE BIGINT,
    ALTER COLUMN debit_total    TYPE BIGINT,
    ALTER COLUMN balance        TYPE BIGINT,
    ALTER COLUMN pending_debit  TYPE BIGINT,
    ALTER COLUMN pending_credit TYPE BIGINT;

ALTER TABLE ledger.entries
    ALTER COLUMN amount        TYPE BIGINT,
    ALTER COLUMN balance_after TYPE BIGINT;

-- This version writes hold leg amounts as JSON strings; the previous one
-- reads only JSON numbers.
ALTER TABLE ledger.holds DISABLE TRIGGER holds_guard_update;
UPDATE ledger.holds
   SET legs = (SELECT jsonb_agg(jsonb_set(leg, '{amount}', to_jsonb((leg ->> 'amount')::bigint)) ORDER BY i)
                 FROM jsonb_array_elements(legs) WITH ORDINALITY AS l(leg, i));
ALTER TABLE ledger.holds ENABLE TRIGGER holds_guard_update;
