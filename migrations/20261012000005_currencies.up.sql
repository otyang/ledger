-- A currency's decimals say how to read its amounts: 540323 is 5403.23 USD
-- (2 decimals) but 540323 JPY (0 decimals). Every account's currency must be
-- registered here.
CREATE TABLE ledger.currencies (
    code       TEXT        PRIMARY KEY,
    -- NULL only for currencies already in use before this table existed,
    -- until their decimals are registered.
    decimals   SMALLINT    CHECK (decimals BETWEEN 0 AND 36),
    created_at TIMESTAMPTZ NOT NULL
);

INSERT INTO ledger.currencies (code, created_at)
SELECT DISTINCT currency, now() FROM ledger.accounts;

ALTER TABLE ledger.accounts
    ADD CONSTRAINT accounts_currency_fkey FOREIGN KEY (currency) REFERENCES ledger.currencies (code);

-- Stored amounts are only meaningful with their decimals, so decimals can be
-- set once and never changed, and currencies can't be removed.
CREATE FUNCTION ledger.guard_currency_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.code, NEW.created_at) IS DISTINCT FROM (OLD.code, OLD.created_at)
       OR (OLD.decimals IS NOT NULL AND NEW.decimals IS DISTINCT FROM OLD.decimals)
    THEN
        RAISE EXCEPTION 'ledger: currency % can only have its decimals set once', OLD.code;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER currencies_guard_update BEFORE UPDATE ON ledger.currencies
    FOR EACH ROW EXECUTE FUNCTION ledger.guard_currency_update();

CREATE TRIGGER currencies_forbid_delete BEFORE DELETE ON ledger.currencies
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();

CREATE TRIGGER currencies_forbid_truncate BEFORE TRUNCATE ON ledger.currencies
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();
