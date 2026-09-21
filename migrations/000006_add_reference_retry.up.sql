-- Retry control for reversals waiting on a reference.
--
-- A REFUND or ROLLBACK can arrive before the transaction it reverses, because
-- nothing orders two independent producers. Such an operation is stored in
-- PENDING_REFERENCE instead of being refused, and a worker retries it until the
-- reference shows up or the budget runs out.
--
-- The columns mirror the outbox claim on purpose: attempts and next_attempt_at
-- give the backoff, locked_by and locked_until give a lease so that several
-- instances compete for the same pending work without doing it twice, and work
-- abandoned by an instance that died becomes available again on its own.
--
-- They are nullable and default to zero, so every row that already exists stays
-- valid and is treated as due immediately.

ALTER TABLE wager_transactions
    ADD COLUMN reference_attempts        INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN reference_next_attempt_at TIMESTAMPTZ,
    ADD COLUMN reference_locked_by       VARCHAR(128),
    ADD COLUMN reference_locked_until    TIMESTAMPTZ;

ALTER TABLE wager_transactions
    ADD CONSTRAINT wager_transactions_reference_attempts_non_negative
        CHECK (reference_attempts >= 0),
    ADD CONSTRAINT wager_transactions_reference_claim_consistency
        CHECK ((reference_locked_by IS NULL) = (reference_locked_until IS NULL));

COMMENT ON COLUMN wager_transactions.reference_attempts IS
    'Resolution attempts already made by the reference worker. The retry budget is compared against it.';
COMMENT ON COLUMN wager_transactions.reference_next_attempt_at IS
    'When the next resolution attempt becomes due. NULL means due immediately.';

-- The worker claims through this index. Only unresolved reversals are indexed,
-- so it stays small however large the table grows.
CREATE INDEX wager_transactions_reference_due
    ON wager_transactions (reference_next_attempt_at, id)
    WHERE state = 'PENDING_REFERENCE';
