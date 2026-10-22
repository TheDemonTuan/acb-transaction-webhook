CREATE TABLE sepay_notification_details (
    transaction_id TEXT PRIMARY KEY NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
    details_envelope BLOB NOT NULL
);
