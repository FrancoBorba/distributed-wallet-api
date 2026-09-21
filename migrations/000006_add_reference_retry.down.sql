-- Reverts the retry control of pending references.

DROP INDEX IF EXISTS wager_transactions_reference_due;

ALTER TABLE wager_transactions
    DROP CONSTRAINT IF EXISTS wager_transactions_reference_claim_consistency,
    DROP CONSTRAINT IF EXISTS wager_transactions_reference_attempts_non_negative;

ALTER TABLE wager_transactions
    DROP COLUMN IF EXISTS reference_locked_until,
    DROP COLUMN IF EXISTS reference_locked_by,
    DROP COLUMN IF EXISTS reference_next_attempt_at,
    DROP COLUMN IF EXISTS reference_attempts;
