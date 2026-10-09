CREATE TABLE payment_orders (
    id TEXT PRIMARY KEY,
    order_code INTEGER NOT NULL UNIQUE CHECK(order_code > 0 AND order_code <= 9007199254740991),
    channel_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    amount_vnd INTEGER NOT NULL CHECK(amount_vnd > 0),
    description TEXT NOT NULL,
    origin TEXT NOT NULL CHECK(origin IN ('STATIC_URL','OPERATOR_DYNAMIC')),
    status TEXT NOT NULL CHECK(status IN ('CREATING','PENDING','PROCESSING','UNDERPAID','PAID','CANCELLED','EXPIRED','FAILED')),
    payment_link_id TEXT UNIQUE,
    qr_code TEXT,
    checkout_url TEXT,
    bank_bin TEXT,
    account_number TEXT,
    account_name TEXT,
    transaction_id TEXT UNIQUE REFERENCES transactions(id),
    last_error_code TEXT,
    next_reconcile_at TEXT,
    reconcile_attempts INTEGER NOT NULL DEFAULT 0,
    operation_token TEXT,
    operation_lease_until TEXT,
    qr_recovery_attempted INTEGER NOT NULL DEFAULT 0 CHECK(qr_recovery_attempted IN (0,1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    paid_at TEXT,
    UNIQUE(channel_id, idempotency_key)
);
CREATE INDEX idx_payment_orders_reconcile ON payment_orders(status, next_reconcile_at);
CREATE INDEX idx_payment_orders_page ON payment_orders(created_at DESC, id DESC);

CREATE TABLE payment_receipts (
    channel_id TEXT NOT NULL,
    reference TEXT NOT NULL,
    order_id TEXT NOT NULL REFERENCES payment_orders(id),
    payment_link_id TEXT NOT NULL,
    amount_vnd INTEGER NOT NULL,
    transaction_at TEXT NOT NULL,
    canonical_hash TEXT NOT NULL,
    transaction_id TEXT NOT NULL UNIQUE REFERENCES transactions(id),
    received_at TEXT NOT NULL,
    PRIMARY KEY(channel_id, reference)
);

CREATE TABLE payos_webhook_inbox (
    payload_hash TEXT PRIMARY KEY,
    order_code INTEGER,
    payment_link_id TEXT,
    reference TEXT,
    payload_json TEXT NOT NULL,
    reason TEXT NOT NULL,
    received_at TEXT NOT NULL,
    processed_at TEXT
);
CREATE INDEX idx_payos_webhook_inbox_pending ON payos_webhook_inbox(order_code, reason) WHERE processed_at IS NULL;
CREATE INDEX idx_payos_webhook_inbox_page ON payos_webhook_inbox(received_at DESC, payload_hash DESC);

INSERT INTO connections(id, bank_code, state, generation, created_at, updated_at)
VALUES('payos-klb', 'KienlongBank', 'WEBHOOK', 0,
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
