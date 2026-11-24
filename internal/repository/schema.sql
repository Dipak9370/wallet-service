-- Money is BIGINT in minor units (paise/cents). Never floats.

CREATE TABLE IF NOT EXISTS wallets (
    id              TEXT PRIMARY KEY,
    opening_balance BIGINT NOT NULL DEFAULT 0 CHECK (opening_balance >= 0),
    balance         BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),  -- DB-level overdraft guard
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS transfers (
    id             TEXT PRIMARY KEY,
    from_wallet_id TEXT   NOT NULL,
    to_wallet_id   TEXT   NOT NULL,
    amount         BIGINT NOT NULL CHECK (amount > 0),
    status         TEXT   NOT NULL CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    failure_reason TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_wallet_id <> to_wallet_id)
);
CREATE INDEX IF NOT EXISTS idx_transfers_from ON transfers (from_wallet_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_transfers_to   ON transfers (to_wallet_id, created_at DESC);

-- PRIMARY KEY on idempotency_key is THE deduplication mechanism.
-- The FK is DEFERRABLE so we can claim the key first and insert the transfer after,
-- inside the same transaction (FK is checked at COMMIT).
CREATE TABLE IF NOT EXISTS idempotency_records (
    idempotency_key TEXT PRIMARY KEY,
    request_hash    TEXT NOT NULL,
    transfer_id     TEXT NOT NULL REFERENCES transfers(id) DEFERRABLE INITIALLY DEFERRED,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id          BIGSERIAL PRIMARY KEY,
    transfer_id TEXT   NOT NULL REFERENCES transfers(id),
    wallet_id   TEXT   NOT NULL REFERENCES wallets(id),
    entry_type  TEXT   NOT NULL CHECK (entry_type IN ('DEBIT','CREDIT')),
    amount      BIGINT NOT NULL CHECK (amount > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, entry_type)   -- at most ONE debit and ONE credit per transfer
);
CREATE INDEX IF NOT EXISTS idx_ledger_wallet ON ledger_entries (wallet_id, id);
