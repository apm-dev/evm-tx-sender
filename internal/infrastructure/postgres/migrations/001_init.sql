CREATE TABLE IF NOT EXISTS nonce_cursors (
    sender      CHAR(42) NOT NULL,
    chain_id    BIGINT NOT NULL,
    next_nonce  BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sender, chain_id)
);

CREATE TABLE IF NOT EXISTS transactions (
    id               TEXT PRIMARY KEY,
    idempotency_key  TEXT NOT NULL UNIQUE,
    chain_id         BIGINT NOT NULL,
    sender           CHAR(42) NOT NULL,
    to_address       CHAR(42) NOT NULL,
    value            NUMERIC(78, 0) NOT NULL DEFAULT 0,
    data             BYTEA,
    gas_limit        BIGINT,
    nonce            BIGINT,
    priority         TEXT NOT NULL DEFAULT 'normal',

    status           TEXT NOT NULL DEFAULT 'QUEUED',
    error_reason     TEXT,
    error_code       TEXT,

    final_tx_hash    CHAR(66),
    block_number     BIGINT,
    block_hash       CHAR(66),
    gas_used         BIGINT,
    effective_gas_price NUMERIC(78, 0),
    receipt_status   SMALLINT,
    receipt_data     JSONB,
    confirmations    INTEGER DEFAULT 0,

    transfer_token     TEXT NOT NULL,
    transfer_amount    TEXT NOT NULL,
    transfer_recipient CHAR(42) NOT NULL,
    token_contract     CHAR(42),
    token_decimals     SMALLINT NOT NULL,

    metadata         JSONB,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    submitted_at     TIMESTAMPTZ,
    confirmed_at     TIMESTAMPTZ,

    claimed_by       TEXT,
    claimed_at       TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_transactions_status_chain_sender
    ON transactions (status, chain_id, sender)
    WHERE status IN ('QUEUED', 'PENDING', 'SUBMITTED');

CREATE INDEX IF NOT EXISTS idx_transactions_chain_sender_nonce
    ON transactions (chain_id, sender, nonce)
    WHERE nonce IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_transactions_submitted_at
    ON transactions (submitted_at)
    WHERE status = 'SUBMITTED';

CREATE INDEX IF NOT EXISTS idx_transactions_transfer_token
    ON transactions (chain_id, transfer_token);

CREATE TABLE IF NOT EXISTS tx_attempts (
    id               TEXT PRIMARY KEY,
    transaction_id   TEXT NOT NULL REFERENCES transactions(id),
    attempt_number   INTEGER NOT NULL,
    tx_hash          CHAR(66) NOT NULL,

    gas_limit        BIGINT NOT NULL,
    max_fee_per_gas         NUMERIC(78, 0),
    max_priority_fee_per_gas NUMERIC(78, 0),
    gas_price               NUMERIC(78, 0),
    tx_type          SMALLINT NOT NULL,

    raw_tx           BYTEA NOT NULL,

    status           TEXT NOT NULL DEFAULT 'BROADCAST',
    error_reason     TEXT,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at     TIMESTAMPTZ,

    UNIQUE (transaction_id, attempt_number)
);

CREATE INDEX IF NOT EXISTS idx_tx_attempts_transaction_id ON tx_attempts (transaction_id);
CREATE INDEX IF NOT EXISTS idx_tx_attempts_tx_hash ON tx_attempts (tx_hash);

CREATE TABLE IF NOT EXISTS tx_state_log (
    id               BIGSERIAL PRIMARY KEY,
    transaction_id   TEXT NOT NULL,
    from_status      TEXT,
    to_status        TEXT NOT NULL,
    actor            TEXT NOT NULL,
    reason           TEXT,
    metadata         JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tx_state_log_transaction_id ON tx_state_log (transaction_id);
