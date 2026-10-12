-- Decimals give every stored amount its meaning and can never change.
-- Dropping them would let a currency be registered again with different
-- decimals, and running processes would keep their cached ones. Refuse.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger.currencies WHERE decimals IS NOT NULL) THEN
        RAISE EXCEPTION 'ledger: cannot downgrade while currencies have registered decimals';
    END IF;
END;
$$;

ALTER TABLE ledger.accounts DROP CONSTRAINT accounts_currency_fkey;
DROP TABLE ledger.currencies;
DROP FUNCTION ledger.guard_currency_update();
