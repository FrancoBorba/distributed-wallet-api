-- Wallets: the financial aggregate root.
--
-- Money is stored as BIGINT minor units, mirroring the int64 cents of the Money
-- value object. Nothing here is ever a floating point type.
--
-- Defence in depth: even if the Go code has a bug, the balance cannot go
-- negative, a player cannot end up with two wallets in the same currency and a
-- balance cannot change without moving the optimistic concurrency version.

CREATE TABLE wallets (
    id            UUID        NOT NULL,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL,
    balance_cents BIGINT      NOT NULL,
    version       BIGINT      NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,

    CONSTRAINT wallets_pkey PRIMARY KEY (id),

    -- The pair (playerId, currency) identifies a single wallet.
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),

    -- Referenced by the composite foreign keys of wager_transactions and
    -- wallet_ledger, so that the database itself refuses a movement whose
    -- currency differs from the wallet currency.
    CONSTRAINT wallets_id_currency_key UNIQUE (id, currency),

    CONSTRAINT wallets_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$'),

    -- Last line of defence against a negative balance.
    CONSTRAINT wallets_balance_non_negative CHECK (balance_cents >= 0),

    -- The initial version is 1 and it only ever moves forward.
    CONSTRAINT wallets_version_positive CHECK (version >= 1),

    CONSTRAINT wallets_timestamps_ordered CHECK (updated_at >= created_at)
);

COMMENT ON COLUMN wallets.balance_cents IS 'Balance in minor units (cents). Never a floating point value.';
COMMENT ON COLUMN wallets.version IS 'Optimistic concurrency version. Starts at 1 and is incremented only when the balance changes.';

-- wallets_guard_update enforces the aggregate invariants that a CHECK cannot
-- express, because they compare the new row against the previous one.
--
-- It is what makes a lost update visible instead of silent: a writer that
-- commits a balance without incrementing the version exactly by one is
-- rejected, so two concurrent writers can never overwrite each other with the
-- same version number.
CREATE FUNCTION wallets_guard_update() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.id <> OLD.id
        OR NEW.player_id <> OLD.player_id
        OR NEW.currency <> OLD.currency
        OR NEW.created_at <> OLD.created_at
    THEN
        RAISE EXCEPTION 'wallet identity is immutable (wallet %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.balance_cents <> OLD.balance_cents AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'balance change must increment the version exactly by one (wallet %, version % -> %)',
            OLD.id, OLD.version, NEW.version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.balance_cents = OLD.balance_cents AND NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'version cannot change without a balance change (wallet %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallets_guard_update
    BEFORE UPDATE ON wallets
    FOR EACH ROW
    EXECUTE FUNCTION wallets_guard_update();
