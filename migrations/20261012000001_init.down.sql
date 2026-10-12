DROP TABLE IF EXISTS ledger.entries;


DROP TABLE IF EXISTS ledger.transactions;


DROP TABLE IF EXISTS ledger.accounts;


DROP TABLE IF EXISTS ledger.ledgers;


DROP FUNCTION IF EXISTS ledger.check_transaction_balanced();


DROP FUNCTION IF EXISTS ledger.guard_transaction_update();


DROP FUNCTION IF EXISTS ledger.forbid_change();


DROP FUNCTION IF EXISTS ledger.is_id(TEXT, TEXT);
