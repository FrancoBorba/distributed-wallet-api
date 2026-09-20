-- Outbox: integration events waiting to be published.
--
-- The event row is written in the same SQL transaction as the balance, the
-- ledger entry and the transaction state. If the process dies between the
-- commit and the publication, the event is still here and another publisher
-- picks it up, so an event whose record was confirmed in the database is never
-- lost and is never published before that commit.
--
-- Defence in depth: the payload is an immutable snapshot enforced by a trigger,
-- and the event_id is the primary key, so a republication after a crash keeps
-- the same identity and the consumer can deduplicate it.

CREATE TABLE outbox (
    event_id       UUID        NOT NULL,
    aggregate_type VARCHAR(32) NOT NULL,
    aggregate_id   UUID        NOT NULL,
    event_type     VARCHAR(64) NOT NULL,
    event_version  INTEGER     NOT NULL DEFAULT 1,
    payload        JSONB       NOT NULL,

    correlation_id UUID,
    causation_id   UUID,
    occurred_at    TIMESTAMPTZ NOT NULL,

    -- Retry with exponential backoff, survived across restarts.
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    last_error      TEXT,

    -- Claim of a publisher, so several of them can compete for rows and
    -- abandoned work can be taken over once the claim expires.
    locked_by    VARCHAR(128),
    locked_until TIMESTAMPTZ,

    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,

    CONSTRAINT outbox_pkey PRIMARY KEY (event_id),

    CONSTRAINT outbox_event_type_known CHECK (
        event_type IN (
            'WagerTransactionProcessed',
            'WagerTransactionRejected',
            'WagerTransactionPendingReference',
            'WalletBalanceChanged'
        )
    ),

    CONSTRAINT outbox_aggregate_type_known CHECK (
        aggregate_type IN ('Wallet', 'WagerTransaction')
    ),

    CONSTRAINT outbox_payload_is_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_event_version_positive CHECK (event_version >= 1),
    CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT outbox_claim_consistency CHECK (
        (locked_by IS NULL) = (locked_until IS NULL)
    ),
    CONSTRAINT outbox_published_after_occurred CHECK (
        published_at IS NULL OR published_at >= occurred_at
    )
);

COMMENT ON COLUMN outbox.event_id IS 'Stable event identity. A republication after a crash reuses it, so consumers can deduplicate.';
COMMENT ON COLUMN outbox.payload IS 'Immutable snapshot of the event at the moment it was produced. Money is serialized as a decimal string, timestamps as RFC 3339 UTC.';

-- The publisher claims rows through this index, ordered by due time. Only
-- unpublished rows are indexed, so the index stays small as the table grows.
CREATE INDEX outbox_pending ON outbox (next_attempt_at, event_id)
    WHERE published_at IS NULL;

-- Outbox lag metric and auditing by aggregate.
CREATE INDEX outbox_aggregate ON outbox (aggregate_type, aggregate_id, occurred_at);

-- outbox_guard_update keeps the snapshot immutable: a publisher may only record
-- progress (attempts, backoff, claim, publication), never rewrite what the
-- event says or what it happened to.
CREATE FUNCTION outbox_guard_update() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.event_id <> OLD.event_id
        OR NEW.aggregate_type <> OLD.aggregate_type
        OR NEW.aggregate_id <> OLD.aggregate_id
        OR NEW.event_type <> OLD.event_type
        OR NEW.event_version <> OLD.event_version
        OR NEW.payload <> OLD.payload
        OR NEW.occurred_at <> OLD.occurred_at
        OR NEW.created_at <> OLD.created_at
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.causation_id IS DISTINCT FROM OLD.causation_id
    THEN
        RAISE EXCEPTION 'outbox event % is an immutable snapshot', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- A published event is done. Publishing it again must not reopen the row,
    -- otherwise a slow publisher could undo the confirmation of a faster one.
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'outbox event % was already published at %', OLD.event_id, OLD.published_at
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_guard_update
    BEFORE UPDATE ON outbox
    FOR EACH ROW
    EXECUTE FUNCTION outbox_guard_update();
