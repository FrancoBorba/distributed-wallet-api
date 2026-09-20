-- Wallet ledger: the append-only history of every balance movement.
--
-- A financial correction is a new entry, never an edit, so reconciliation can
-- always rebuild the balance from credits minus debits.
--
-- Defence in depth: the uniqueness of (wallet_id, transaction_id) is what makes
-- a duplicated movement impossible even if the same transaction is applied
-- twice, and the arithmetic CHECK makes a balance chain that does not add up
-- impossible to write in the first place.

CREATE TABLE wallet_ledger (
    id             UUID        NOT NULL,
    -- Monotonic sequence used as the stable ordering behind the opaque cursor
    -- of GET /wallets/:walletId/ledger.
    seq            BIGSERIAL   NOT NULL,
    wallet_id      UUID        NOT NULL,
    transaction_id UUID        NOT NULL,
    direction      VARCHAR(6)  NOT NULL,
    currency       CHAR(3)     NOT NULL,

    amount_cents         BIGINT NOT NULL,
    balance_before_cents BIGINT NOT NULL,
    balance_after_cents  BIGINT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT wallet_ledger_pkey PRIMARY KEY (id),
    CONSTRAINT wallet_ledger_seq_key UNIQUE (seq),

    -- One transaction produces at most one entry per wallet, so the same bet
    -- can never be debited twice.
    CONSTRAINT wallet_ledger_wallet_transaction_key UNIQUE (wallet_id, transaction_id),

    CONSTRAINT wallet_ledger_wallet_fkey
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),

    CONSTRAINT wallet_ledger_transaction_fkey
        FOREIGN KEY (transaction_id) REFERENCES wager_transactions (id),

    CONSTRAINT wallet_ledger_direction_known CHECK (direction IN ('DEBIT', 'CREDIT')),

    -- An entry always moves money, which is what keeps LOSS and every rejected
    -- operation out of the ledger.
    CONSTRAINT wallet_ledger_amount_positive CHECK (amount_cents > 0),

    CONSTRAINT wallet_ledger_balances_non_negative CHECK (
        balance_before_cents >= 0 AND balance_after_cents >= 0
    ),

    -- balanceAfter = balanceBefore ± money, according to the direction.
    CONSTRAINT wallet_ledger_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after_cents = balance_before_cents + amount_cents)
        OR (direction = 'DEBIT' AND balance_after_cents = balance_before_cents - amount_cents)
    )
);

COMMENT ON COLUMN wallet_ledger.seq IS 'Stable ordering for cursor pagination. The cursor is opaque to the client.';

CREATE INDEX wallet_ledger_wallet_cursor ON wallet_ledger (wallet_id, seq);

-- wallet_ledger_deny_mutation makes the ledger append-only at the database
-- level. Nothing but an INSERT is accepted, so an entry can never be edited or
-- deleted, by the application or by hand.
CREATE FUNCTION wallet_ledger_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_ledger_deny_row_mutation
    BEFORE UPDATE OR DELETE ON wallet_ledger
    FOR EACH ROW
    EXECUTE FUNCTION wallet_ledger_deny_mutation();

CREATE TRIGGER wallet_ledger_deny_truncate
    BEFORE TRUNCATE ON wallet_ledger
    FOR EACH STATEMENT
    EXECUTE FUNCTION wallet_ledger_deny_mutation();
