-- Migration 017: encrypted Telegram evidence and independent SePay Store receipts.
CREATE TABLE sepay_telegram_inbox (
    bot_id INTEGER NOT NULL,
    update_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    payload_hash TEXT NOT NULL,
    payload_envelope BLOB NOT NULL,
    store_key TEXT NOT NULL,
    reason TEXT NOT NULL,
    received_at TEXT NOT NULL,
    transaction_id TEXT REFERENCES transactions(id),
    PRIMARY KEY (bot_id, update_id)
);
CREATE INDEX idx_sepay_inbox_received ON sepay_telegram_inbox(received_at, update_id);
CREATE INDEX idx_sepay_inbox_message ON sepay_telegram_inbox(bot_id, chat_id, message_id);
CREATE INDEX idx_sepay_inbox_store_reason ON sepay_telegram_inbox(store_key, reason, received_at);

CREATE TABLE sepay_receipts (
    store_key TEXT NOT NULL,
    bank_code TEXT NOT NULL,
    account_number TEXT NOT NULL,
    reference TEXT NOT NULL,
    canonical_hash TEXT NOT NULL,
    transaction_id TEXT NOT NULL UNIQUE REFERENCES transactions(id),
    message_id INTEGER NOT NULL,
    received_at TEXT NOT NULL,
    PRIMARY KEY (store_key, reference)
);
