DROP TRIGGER IF EXISTS wallet_ledger_deny_truncate ON wallet_ledger;
DROP TRIGGER IF EXISTS wallet_ledger_deny_row_mutation ON wallet_ledger;
DROP FUNCTION IF EXISTS wallet_ledger_deny_mutation();
DROP TABLE IF EXISTS wallet_ledger;
