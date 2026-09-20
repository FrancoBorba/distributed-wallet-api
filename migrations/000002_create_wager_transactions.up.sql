-- Wager transactions: the persisted mirror of the WagerTransaction entity.
--
-- One table holds both origins, kept apart by CHECK constraints: the internal
-- OPENING carries no provider metadata, every external kind carries all of it.
--
-- Defence in depth: the uniqueness of (provider_id, external_transaction_id)
-- and of (provider_id, idempotency_key) is what stops a duplicated movement
-- when two identical requests race past the in-memory checks. The second INSERT
-- fails, and the caller replays the stored result instead of applying it again.

CREATE TABLE wager_transactions (
    id           UUID        NOT NULL,
    wallet_id    UUID        NOT NULL,
    player_id    UUID        NOT NULL,
    currency     CHAR(3)     NOT NULL,
    amount_cents BIGINT      NOT NULL,
    kind         VARCHAR(16) NOT NULL,
    state        VARCHAR(20) NOT NULL,

    -- Provider metadata. NULL on an internally originated OPENING, which also
    -- keeps those rows out of the uniqueness rules below, since NULL never
    -- collides in a UNIQUE constraint.
    provider_id             VARCHAR(64),
    external_transaction_id VARCHAR(128),
    idempotency_key         VARCHAR(255),
    payload_hash            VARCHAR(64),
    round_id                VARCHAR(128),
    game_id                 VARCHAR(128),

    -- Reversal reference: what the provider sent, and the internal transaction
    -- it was resolved to.
    reference_external_transaction_id VARCHAR(128),
    reference_transaction_id          UUID,

    -- Outcome, only present once the transaction reaches a terminal state.
    failure_code         VARCHAR(64),
    result_balance_cents BIGINT,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT wager_transactions_pkey PRIMARY KEY (id),

    -- The composite foreign key makes the database reject an operation whose
    -- currency differs from the wallet currency.
    CONSTRAINT wager_transactions_wallet_fkey
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),

    CONSTRAINT wager_transactions_reference_fkey
        FOREIGN KEY (reference_transaction_id) REFERENCES wager_transactions (id),

    -- A financial operation identified by (providerId, externalTransactionId)
    -- can never be applied twice, whatever key it arrives with.
    CONSTRAINT wager_transactions_provider_external_key
        UNIQUE (provider_id, external_transaction_id),

    -- And an idempotency key is never reused for a second operation.
    CONSTRAINT wager_transactions_provider_key_key
        UNIQUE (provider_id, idempotency_key),

    CONSTRAINT wager_transactions_kind_known CHECK (
        kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')
    ),

    CONSTRAINT wager_transactions_state_known CHECK (
        state IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')
    ),

    CONSTRAINT wager_transactions_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$'),

    -- Zero policy of each kind: LOSS is recorded with exactly 0 and moves
    -- nothing, every other kind requires an amount greater than zero.
    CONSTRAINT wager_transactions_amount_policy CHECK (
        (kind = 'LOSS' AND amount_cents = 0)
        OR (kind <> 'LOSS' AND amount_cents > 0)
    ),

    -- The schema tells internal and external origins apart: OPENING carries no
    -- provider metadata, and no external kind may be missing any of it.
    CONSTRAINT wager_transactions_origin_metadata CHECK (
        (kind = 'OPENING'
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND payload_hash IS NULL
            AND round_id IS NULL
            AND game_id IS NULL)
        OR (kind <> 'OPENING'
            AND provider_id IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL
            AND game_id IS NOT NULL)
    ),

    -- Lowercase hex SHA-256 of the canonical JSON of the business fields.
    CONSTRAINT wager_transactions_payload_hash_format CHECK (
        payload_hash IS NULL OR payload_hash ~ '^[0-9a-f]{64}$'
    ),

    -- REFUND and ROLLBACK must name what they reverse, WIN may name a bet of
    -- the same round, and the remaining kinds reverse nothing.
    CONSTRAINT wager_transactions_reference_policy CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR (kind = 'WIN')
        OR (kind IN ('OPENING', 'BET', 'LOSS') AND reference_external_transaction_id IS NULL)
    ),

    -- A resolved reference only exists for a reference that was asked for, and
    -- a transaction never reverses itself.
    CONSTRAINT wager_transactions_resolved_reference CHECK (
        reference_transaction_id IS NULL
        OR (reference_external_transaction_id IS NOT NULL AND reference_transaction_id <> id)
    ),

    -- A reversal is never concluded before knowing which transaction it undoes.
    CONSTRAINT wager_transactions_processed_reversal_resolved CHECK (
        state <> 'PROCESSED'
        OR kind NOT IN ('REFUND', 'ROLLBACK')
        OR reference_transaction_id IS NOT NULL
    ),

    -- Only a kind that can wait for a reference may sit in PENDING_REFERENCE.
    CONSTRAINT wager_transactions_pending_reference_policy CHECK (
        state <> 'PENDING_REFERENCE' OR kind IN ('REFUND', 'ROLLBACK', 'WIN')
    ),

    -- A refusal always carries its failure code, and nothing else ever does.
    CONSTRAINT wager_transactions_failure_code_policy CHECK (
        (state IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    ),

    -- A processed transaction always stores the balance returned to the
    -- provider, which is what an idempotent replay answers with.
    CONSTRAINT wager_transactions_result_balance_policy CHECK (
        (state = 'PROCESSED') = (result_balance_cents IS NOT NULL)
    ),

    CONSTRAINT wager_transactions_result_balance_non_negative CHECK (
        result_balance_cents IS NULL OR result_balance_cents >= 0
    ),

    CONSTRAINT wager_transactions_timestamps_ordered CHECK (updated_at >= created_at)
);

COMMENT ON COLUMN wager_transactions.payload_hash IS 'Lowercase hex SHA-256 of the canonical JSON of the business fields, excluding the idempotency key and transport metadata.';
COMMENT ON COLUMN wager_transactions.result_balance_cents IS 'Wallet balance observed when the operation was processed. A replay answers with it, not with the current balance.';

-- A wallet is opened once, so its initial credit can never be duplicated.
CREATE UNIQUE INDEX wager_transactions_single_opening
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

-- A reference never receives two successful reversals. Because the index does
-- not include the kind, a bet that was already refunded cannot also be rolled
-- back, which is what stops the same debit from being returned twice.
CREATE UNIQUE INDEX wager_transactions_single_successful_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE state = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Durable resumption: any instance can pick up work left behind by another.
CREATE INDEX wager_transactions_unfinished
    ON wager_transactions (updated_at)
    WHERE state IN ('PENDING', 'PENDING_REFERENCE');

-- Resolution of a reversal by (providerId, referenceExternalTransactionId).
CREATE INDEX wager_transactions_reference_lookup
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE reference_external_transaction_id IS NOT NULL;

CREATE INDEX wager_transactions_wallet_history
    ON wager_transactions (wallet_id, created_at DESC, id DESC);

-- wager_transactions_guard_update keeps the state machine and the append-only
-- nature of the record honest at the database level: a terminal transaction
-- accepts no further transition, and the business fields of any transaction are
-- immutable once written. Only the outcome columns may still change.
CREATE FUNCTION wager_transactions_guard_update() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.state IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'transaction % is terminal (%) and cannot be changed', OLD.id, OLD.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.id <> OLD.id
        OR NEW.wallet_id <> OLD.wallet_id
        OR NEW.player_id <> OLD.player_id
        OR NEW.currency <> OLD.currency
        OR NEW.amount_cents <> OLD.amount_cents
        OR NEW.kind <> OLD.kind
        OR NEW.created_at <> OLD.created_at
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
        OR NEW.round_id IS DISTINCT FROM OLD.round_id
        OR NEW.game_id IS DISTINCT FROM OLD.game_id
        OR NEW.reference_external_transaction_id IS DISTINCT FROM OLD.reference_external_transaction_id
    THEN
        RAISE EXCEPTION 'business fields of transaction % are immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- A reference resolves once. Retrying the resolution worker is harmless,
    -- pointing the reversal at another transaction is not.
    IF OLD.reference_transaction_id IS NOT NULL
        AND NEW.reference_transaction_id IS DISTINCT FROM OLD.reference_transaction_id
    THEN
        RAISE EXCEPTION 'reference of transaction % was already resolved to %', OLD.id, OLD.reference_transaction_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wager_transactions_guard_update
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW
    EXECUTE FUNCTION wager_transactions_guard_update();
