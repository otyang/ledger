-- Amounts stay whole numbers of a currency's minor unit, but NUMERIC(78,0)
-- holds any 256-bit value, so token amounts in their smallest unit fit (ETH
-- has 18 decimals: BIGINT tops out near 9.2 ETH in wei). Each ALTER TABLE
-- rewrites its table once, under an exclusive lock.
ALTER TABLE ledger.accounts
    ALTER COLUMN credit_total   TYPE NUMERIC(78,0),
    ALTER COLUMN debit_total    TYPE NUMERIC(78,0),
    ALTER COLUMN balance        TYPE NUMERIC(78,0),
    ALTER COLUMN pending_debit  TYPE NUMERIC(78,0),
    ALTER COLUMN pending_credit TYPE NUMERIC(78,0);


ALTER TABLE ledger.entries
    ALTER COLUMN amount        TYPE NUMERIC(78,0),
    ALTER COLUMN balance_after TYPE NUMERIC(78,0);
