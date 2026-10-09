package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// VerifiedPaymentCallback contains signature-verified data only. The caller must
// verify the complete provider data before constructing this record.
type VerifiedPaymentCallback struct {
	Data          map[string]any
	OrderCode     int64
	PaymentLinkID string
	Reference     string
	Reason        string
}

// PaymentReview deliberately excludes raw payload and sender account details.
type PaymentReview struct {
	PayloadHash   string `json:"payloadHash"`
	OrderCode     int64  `json:"orderCode,string"`
	PaymentLinkID string `json:"paymentLinkId,omitempty"`
	Reference     string `json:"reference,omitempty"`
	Reason        string `json:"reason"`
	ReceivedAt    string `json:"receivedAt"`
	ProcessedAt   string `json:"processedAt,omitempty"`
}

// PaymentInboxItem is internal reconciliation evidence, never a public DTO.
type PaymentInboxItem struct {
	PaymentReview
	PayloadJSON string `json:"-"`
}

func canonicalPaymentCallback(in VerifiedPaymentCallback) (string, []byte, error) {
	if in.Data == nil || in.Reason == "" {
		return "", nil, errors.New("verified callback data and reason required")
	}
	payload, err := json.Marshal(in.Data)
	if err != nil {
		return "", nil, err
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), payload, nil
}

func (s *Store) insertPaymentInboxTx(ctx context.Context, tx *sql.Tx, in VerifiedPaymentCallback) (string, error) {
	hash, payload, err := canonicalPaymentCallback(in)
	if err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO payos_webhook_inbox(payload_hash,order_code,payment_link_id,reference,payload_json,reason,received_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(payload_hash) DO NOTHING`, hash, in.OrderCode, in.PaymentLinkID, in.Reference, string(payload), in.Reason, now())
	if err != nil {
		return "", err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if inserted == 0 && in.Reason != "AWAITING_ORDER_BIND" && in.Reason != "PAYMENT_DETAILS_PENDING" && in.Reason != "UNKNOWN_ORDER" {
		// A bind candidate proven conflicting becomes review-only, without
		// allocating another row or rewriting an already processed receipt.
		if _, err := tx.ExecContext(ctx, `UPDATE payos_webhook_inbox SET reason=? WHERE payload_hash=? AND processed_at IS NULL AND reason IN ('AWAITING_ORDER_BIND','PAYMENT_DETAILS_PENDING','UNKNOWN_ORDER')`, in.Reason, hash); err != nil {
			return "", err
		}
	}
	if inserted != 0 && (in.Reason == "AWAITING_ORDER_BIND" || in.Reason == "PAYMENT_DETAILS_PENDING") {
		if err := s.wakePaymentOrderInboxTx(ctx, tx, in.OrderCode); err != nil {
			return "", err
		}
		if in.Reason == "PAYMENT_DETAILS_PENDING" {
			if _, err := tx.ExecContext(ctx, `UPDATE payment_orders SET last_error_code='PAYMENT_DETAILS_PENDING' WHERE order_code=? AND status<>'PAID'`, in.OrderCode); err != nil {
				return "", err
			}
		}
	}
	return hash, nil
}

func (s *Store) SaveVerifiedPaymentCallback(ctx context.Context, in VerifiedPaymentCallback) (string, error) {
	var hash string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE payos_webhook_inbox SET reason=reason WHERE payload_hash=''`); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		var err error
		hash, err = s.insertPaymentInboxTx(ctx, tx, in)
		return err
	})
	return hash, err
}

func (s *Store) ListPaymentReviews(ctx context.Context, cursor string, limit int) (Page[PaymentReview], error) {
	limit = normalizePageSize(limit)
	sortValue, key, err := decodeCursor(cursor)
	if err != nil {
		return Page[PaymentReview]{}, err
	}
	args := []any{}
	where := `processed_at IS NULL`
	if key != "" {
		where += ` AND (received_at<? OR (received_at=? AND payload_hash<?))`
		args = append(args, sortValue, sortValue, key)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT payload_hash,COALESCE(order_code,0),COALESCE(payment_link_id,''),COALESCE(reference,''),reason,received_at,COALESCE(processed_at,'') FROM payos_webhook_inbox WHERE `+where+` ORDER BY received_at DESC,payload_hash DESC LIMIT ?`, args...)
	if err != nil {
		return Page[PaymentReview]{}, err
	}
	defer rows.Close()
	page := Page[PaymentReview]{Items: []PaymentReview{}}
	for rows.Next() {
		var item PaymentReview
		if err := rows.Scan(&item.PayloadHash, &item.OrderCode, &item.PaymentLinkID, &item.Reference, &item.Reason, &item.ReceivedAt, &item.ProcessedAt); err != nil {
			return Page[PaymentReview]{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[PaymentReview]{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.ReceivedAt, last.PayloadHash)
	}
	return page, nil
}

// PendingPaymentCallbacks returns order-binding and missing-evidence work.
// Financial conflicts remain review-only until independently resolved.
func (s *Store) PendingPaymentCallbacks(ctx context.Context, orderCode int64) ([]PaymentInboxItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload_hash,COALESCE(order_code,0),COALESCE(payment_link_id,''),COALESCE(reference,''),reason,received_at,COALESCE(processed_at,''),payload_json FROM payos_webhook_inbox WHERE order_code=? AND processed_at IS NULL AND reason IN ('AWAITING_ORDER_BIND','PAYMENT_DETAILS_PENDING') ORDER BY received_at,payload_hash`, orderCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PaymentInboxItem{}
	for rows.Next() {
		var item PaymentInboxItem
		if err := rows.Scan(&item.PayloadHash, &item.OrderCode, &item.PaymentLinkID, &item.Reference, &item.Reason, &item.ReceivedAt, &item.ProcessedAt, &item.PayloadJSON); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// wakePaymentOrderInboxTx schedules newly committed evidence once, never an
// exact replay. It does not overlap a live Create/Cancel/Get network lease.
func (s *Store) wakePaymentOrderInboxTx(ctx context.Context, tx *sql.Tx, code int64) error {
	order, err := scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE order_code=?`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if order.Status == "PAID" {
		return nil
	}
	due := time.Now().UTC()
	if order.OperationLeaseUntil != "" {
		lease, err := time.Parse(time.RFC3339Nano, order.OperationLeaseUntil)
		if err != nil {
			return err
		}
		if lease.After(due) {
			due = lease
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE payment_orders SET next_reconcile_at=?,updated_at=? WHERE id=? AND status<>'PAID'`, due.Format(time.RFC3339Nano), now(), order.ID)
	return err
}

// WakePendingPaymentCallbacks rechecks unknown callbacks against local orders
// only. Promotion is durable, so later scans do not erase provider backoff.
func (s *Store) WakePendingPaymentCallbacks(ctx context.Context, channelID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE payos_webhook_inbox SET reason=reason WHERE payload_hash=''`); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT i.order_code FROM payos_webhook_inbox i JOIN payment_orders o ON o.order_code=i.order_code WHERE i.reason='UNKNOWN_ORDER' AND i.processed_at IS NULL AND o.channel_id=? ORDER BY i.order_code LIMIT 20`, channelID)
		if err != nil {
			return err
		}
		var codes []int64
		for rows.Next() {
			var code int64
			if err := rows.Scan(&code); err != nil {
				_ = rows.Close()
				return err
			}
			codes = append(codes, code)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, code := range codes {
			if _, err := tx.ExecContext(ctx, `UPDATE payos_webhook_inbox SET reason='AWAITING_ORDER_BIND' WHERE order_code=? AND reason='UNKNOWN_ORDER' AND processed_at IS NULL`, code); err != nil {
				return err
			}
			if err := s.wakePaymentOrderInboxTx(ctx, tx, code); err != nil {
				return err
			}
		}
		return nil
	})
}
