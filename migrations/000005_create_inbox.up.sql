-- Inbox: deduplication of incoming messages.
--
-- SQS delivers at least once, so the same message can arrive twice because of
-- network instability or a visibility timeout that expired while the handler
-- was still working. The consumer writes the message identity here inside the
-- same SQL transaction as the domain changes, the ledger and the outbox rows.
--
-- Defence in depth: the primary key (consumer_name, message_id) is what makes
-- the second delivery fail to insert. The handling is then recognised as a
-- repeat and the message is dropped without touching the balance.

CREATE TABLE inbox (
    consumer_name VARCHAR(64)  NOT NULL,
    message_id    VARCHAR(128) NOT NULL,

    -- Hash of the message body, verified on redelivery: the same message id
    -- carrying a different content is a conflict, not a duplicate.
    payload_hash VARCHAR(64) NOT NULL,

    received_at  TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,

    -- Kept for auditing a message that reached the dead letter queue.
    attempts   INTEGER NOT NULL DEFAULT 1,
    last_error TEXT,

    CONSTRAINT inbox_pkey PRIMARY KEY (consumer_name, message_id),

    CONSTRAINT inbox_payload_hash_format CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT inbox_attempts_positive CHECK (attempts >= 1),
    CONSTRAINT inbox_completed_after_received CHECK (
        completed_at IS NULL OR completed_at >= received_at
    )
);

COMMENT ON TABLE inbox IS 'Durable deduplication of at-least-once delivery. Uniqueness of (consumerName, messageId) is the primary key.';
COMMENT ON COLUMN inbox.payload_hash IS 'Lowercase hex SHA-256 of the message body, using the same canonical JSON as wager_transactions.payload_hash.';

-- Resumption of messages accepted but not concluded, after an interruption.
CREATE INDEX inbox_unfinished ON inbox (received_at)
    WHERE completed_at IS NULL;

-- inbox_guard_update keeps the identity of a message stable. Only the progress
-- of its handling may change, so a redelivery can never rewrite the hash that
-- the conflict check is based on.
CREATE FUNCTION inbox_guard_update() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.consumer_name <> OLD.consumer_name
        OR NEW.message_id <> OLD.message_id
        OR NEW.payload_hash <> OLD.payload_hash
        OR NEW.received_at <> OLD.received_at
    THEN
        RAISE EXCEPTION 'inbox identity of message % is immutable', OLD.message_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at THEN
        RAISE EXCEPTION 'message % was already completed at %', OLD.message_id, OLD.completed_at
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER inbox_guard_update
    BEFORE UPDATE ON inbox
    FOR EACH ROW
    EXECUTE FUNCTION inbox_guard_update();
