CREATE TABLE sepay_native_inbox (
 store_key TEXT NOT NULL,
 provider_id INTEGER NOT NULL CHECK(provider_id>0),
 reference TEXT NOT NULL DEFAULT '',
 canonical_hash TEXT NOT NULL,
 payload_envelope BLOB NOT NULL,
 reason TEXT NOT NULL,
 transaction_id TEXT REFERENCES transactions(id),
 transaction_at TEXT NOT NULL,
 received_at TEXT NOT NULL,
 PRIMARY KEY(store_key,provider_id)
);
CREATE INDEX sepay_native_received ON sepay_native_inbox(store_key,received_at,provider_id);
CREATE INDEX sepay_native_transaction ON sepay_native_inbox(transaction_id);
CREATE INDEX sepay_native_reference ON sepay_native_inbox(store_key,reference);
