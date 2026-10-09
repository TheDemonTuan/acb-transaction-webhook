package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// Activity is a read-only projection of durable provider evidence.
type PaymentActivity struct {
	LastWebhookAt    *string
	LastReconciledAt *string
	PendingOrders    int64
	ReviewCount      int64
}

func paymentActivityKey(channel, kind string) string {
	hash := sha256.Sum256([]byte(channel))
	return "PAYOS_ACTIVITY:" + hex.EncodeToString(hash[:16]) + ":" + kind
}

// RecordPaymentActivity stores bounded metadata in the existing system-events table.
// Call only after signature verification/acceptance; failures must never undo money.
func (s *Store) RecordPaymentActivity(ctx context.Context, channel, kind string) error {
	if channel == "" || (kind != "WEBHOOK" && kind != "RECONCILED") {
		return errors.New("invalid payment activity")
	}
	key := paymentActivityKey(channel, kind)
	_, err := s.db.ExecContext(ctx, `INSERT INTO system_events(id,incident_key,event_type,state,details_json,created_at) VALUES(?,?,?,'OK','{}',?) ON CONFLICT(id) DO UPDATE SET created_at=excluded.created_at`, key, key, "PAYOS_"+kind, now())
	return err
}

func (s *Store) PaymentActivity(ctx context.Context, channel string) (PaymentActivity, error) {
	var result PaymentActivity
	var webhook, reconciled string
	err := s.db.QueryRowContext(ctx, `SELECT
	COALESCE((SELECT created_at FROM system_events WHERE id=?),''),
	COALESCE((SELECT created_at FROM system_events WHERE id=?),''),
	(SELECT count(*) FROM payment_orders WHERE channel_id=? AND status IN ('CREATING','PENDING','PROCESSING','UNDERPAID')),
	(SELECT count(*) FROM payos_webhook_inbox WHERE processed_at IS NULL) +
	(SELECT count(*) FROM payment_orders WHERE channel_id=? AND last_error_code IS NOT NULL AND last_error_code<>'')`, paymentActivityKey(channel, "WEBHOOK"), paymentActivityKey(channel, "RECONCILED"), channel, channel).Scan(&webhook, &reconciled, &result.PendingOrders, &result.ReviewCount)
	if webhook != "" {
		result.LastWebhookAt = &webhook
	}
	if reconciled != "" {
		result.LastReconciledAt = &reconciled
	}
	return result, err
}
