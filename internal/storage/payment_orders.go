package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"
)

const PaymentOperationLease = 30 * time.Second

var (
	ErrPaymentIdempotencyConflict = errors.New("payment idempotency request conflicts")
	ErrPaymentOperationLost       = errors.New("payment operation token no longer owns order")
	ErrInvalidPaymentOrder        = errors.New("invalid payment order intent")
)

// PaymentOrder is the durable snapshot. Internal coordination and provider binding
// fields are deliberately excluded from its default JSON representation.
type PaymentOrder struct {
	ID                  string `json:"id"`
	OrderCode           int64  `json:"orderCode,string"`
	ChannelID           string `json:"-"`
	IdempotencyKey      string `json:"-"`
	RequestHash         string `json:"-"`
	AmountVnd           int64  `json:"amountVnd"`
	Description         string `json:"-"`
	Origin              string `json:"origin"`
	Status              string `json:"status"`
	PaymentLinkID       string `json:"-"`
	QRCode              string `json:"qrCode,omitempty"`
	CheckoutURL         string `json:"checkoutUrl,omitempty"`
	BankBin             string `json:"bankBin,omitempty"`
	AccountNumber       string `json:"accountNumber,omitempty"`
	AccountName         string `json:"accountName,omitempty"`
	TransactionID       string `json:"transactionId,omitempty"`
	LastErrorCode       string `json:"errorCode,omitempty"`
	NextReconcileAt     string `json:"-"`
	ReconcileAttempts   int    `json:"-"`
	OperationToken      string `json:"-"`
	OperationLeaseUntil string `json:"-"`
	QRRecoveryAttempted bool   `json:"-"`
	CreatedAt           string `json:"createdAt"`
	UpdatedAt           string `json:"-"`
	ExpiresAt           string `json:"expiresAt"`
	PaidAt              string `json:"paidAt,omitempty"`
}

type PaymentOrderIntent struct {
	ChannelID      string
	IdempotencyKey string
	RequestHash    string
	AmountVnd      int64
	Origin         string
}

// PaymentOrderUpdate completes a claimed provider operation, not settlement.
// Empty metadata leaves existing bindings intact; LastErrorCode can be cleared.
// ReconcileAttempts only resets explicitly; QRRecoveryAttempted is monotonic.
type PaymentOrderUpdate struct {
	Status                 string
	PaymentLinkID          string
	QRCode                 string
	CheckoutURL            string
	BankBin                string
	AccountNumber          string
	AccountName            string
	LastErrorCode          string
	NextReconcileAt        time.Time
	ReconcileAttempts      int
	ResetReconcileAttempts bool
	QRRecoveryAttempted    bool
}

type PaymentOrderFilter struct {
	ChannelID string
	Status    string
	Cursor    string
	Limit     int
}

const paymentOrderColumns = `id, order_code, channel_id, idempotency_key, request_hash,
amount_vnd, description, origin, status, COALESCE(payment_link_id,''), COALESCE(qr_code,''),
COALESCE(checkout_url,''), COALESCE(bank_bin,''), COALESCE(account_number,''), COALESCE(account_name,''),
COALESCE(transaction_id,''), COALESCE(last_error_code,''), COALESCE(next_reconcile_at,''),
reconcile_attempts, COALESCE(operation_token,''), COALESCE(operation_lease_until,''),
qr_recovery_attempted, created_at, updated_at, expires_at, COALESCE(paid_at,'')`

func scanPaymentOrder(row interface{ Scan(...any) error }) (PaymentOrder, error) {
	var o PaymentOrder
	err := row.Scan(&o.ID, &o.OrderCode, &o.ChannelID, &o.IdempotencyKey, &o.RequestHash,
		&o.AmountVnd, &o.Description, &o.Origin, &o.Status, &o.PaymentLinkID, &o.QRCode,
		&o.CheckoutURL, &o.BankBin, &o.AccountNumber, &o.AccountName, &o.TransactionID,
		&o.LastErrorCode, &o.NextReconcileAt, &o.ReconcileAttempts, &o.OperationToken,
		&o.OperationLeaseUntil, &o.QRRecoveryAttempted, &o.CreatedAt, &o.UpdatedAt, &o.ExpiresAt, &o.PaidAt)
	return o, err
}

func (s *Store) PaymentOrder(ctx context.Context, id string) (PaymentOrder, error) {
	return scanPaymentOrder(s.db.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=?`, id))
}

func (s *Store) PaymentOrderByCode(ctx context.Context, orderCode int64) (PaymentOrder, error) {
	return scanPaymentOrder(s.db.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE order_code=?`, orderCode))
}

func (s *Store) PaymentOrderByKey(ctx context.Context, channelID, key string) (PaymentOrder, error) {
	return scanPaymentOrder(s.db.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE channel_id=? AND idempotency_key=?`, channelID, key))
}

func randomPaymentToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate payment capability: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func randomPaymentOrderCode() (int64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000000000))
	if err != nil {
		return 0, fmt.Errorf("generate payment order code: %w", err)
	}
	return n.Int64() + 100000000000, nil
}

func validPaymentStatus(status string) bool {
	switch status {
	case "CREATING", "PENDING", "PROCESSING", "UNDERPAID", "PAID", "CANCELLED", "EXPIRED", "FAILED":
		return true
	default:
		return false
	}
}

// ReservePaymentOrder atomically deduplicates the intent and leases only a new
// order. Replay is a read even when the deployment gate is closed.
func (s *Store) ReservePaymentOrder(ctx context.Context, in PaymentOrderIntent) (PaymentOrder, bool, error) {
	if in.ChannelID == "" || in.IdempotencyKey == "" || in.RequestHash == "" || in.AmountVnd <= 0 || (in.Origin != "STATIC_URL" && in.Origin != "OPERATOR_DYNAMIC") {
		return PaymentOrder{}, false, ErrInvalidPaymentOrder
	}
	var order PaymentOrder
	created := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Obtain SQLite's writer reservation before reading, including across
		// gateway Store instances. A deferred read-then-write transaction could
		// otherwise fail its lock upgrade rather than wait for the first writer.
		if _, err := tx.ExecContext(ctx, `UPDATE payment_orders SET id=id WHERE channel_id=? AND idempotency_key=?`, in.ChannelID, in.IdempotencyKey); err != nil {
			return err
		}
		var err error
		order, err = scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE channel_id=? AND idempotency_key=?`, in.ChannelID, in.IdempotencyKey))
		if err == nil {
			if order.RequestHash != in.RequestHash || order.AmountVnd != in.AmountVnd || order.Origin != in.Origin {
				return ErrPaymentIdempotencyConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		id, err := randomPaymentToken()
		if err != nil {
			return err
		}
		token, err := randomPaymentToken()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		stamp := now.Format(time.RFC3339Nano)
		lease := now.Add(PaymentOperationLease).Format(time.RFC3339Nano)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			code, err := randomPaymentOrderCode()
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `INSERT INTO payment_orders
			(id,order_code,channel_id,idempotency_key,request_hash,amount_vnd,description,origin,status,
			 next_reconcile_at,operation_token,operation_lease_until,created_at,updated_at,expires_at)
			VALUES(?,?,?,?,?,?,?,?,'CREATING',?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
				id, code, in.ChannelID, in.IdempotencyKey, in.RequestHash, in.AmountVnd, "DH"+strconv.FormatInt(code, 10), in.Origin,
				lease, token, lease, stamp, stamp, now.Add(30*time.Minute).Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				// Only random primary-key or order-code collisions remain: the
				// channel/key was checked under the SQLite writer reservation.
				id, err = randomPaymentToken()
				if err != nil {
					return err
				}
				continue
			}
			created = true
			order, err = scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=?`, id))
			return err
		}
	})
	if err != nil {
		return PaymentOrder{}, false, err
	}
	return order, created, nil
}

// ClaimPaymentOrder provides a shared 30-second lease for Create, Cancel and
// reconcile. A live lease or a settled order returns its snapshot without claim.
func (s *Store) ClaimPaymentOrder(ctx context.Context, id string, now time.Time) (PaymentOrder, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	token, err := randomPaymentToken()
	if err != nil {
		return PaymentOrder{}, false, err
	}
	var order PaymentOrder
	claimed := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE payment_orders SET id=id WHERE id=?`, id); err != nil {
			return err
		}
		var err error
		order, err = scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=?`, id))
		if err != nil {
			return err
		}
		if order.Status == "PAID" {
			return nil
		}
		if order.OperationToken != "" {
			until, err := time.Parse(time.RFC3339Nano, order.OperationLeaseUntil)
			if err != nil {
				return err
			}
			if now.Before(until) {
				return nil
			}
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		leaseTime := now.Add(PaymentOperationLease)
		lease := leaseTime.Format(time.RFC3339Nano)
		next := lease
		if order.NextReconcileAt != "" {
			due, err := time.Parse(time.RFC3339Nano, order.NextReconcileAt)
			if err != nil {
				return err
			}
			if due.After(leaseTime) {
				next = order.NextReconcileAt
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE payment_orders SET operation_token=?, operation_lease_until=?, updated_at=?, next_reconcile_at=?
		 WHERE id=? AND status<>'PAID' AND COALESCE(operation_token,'')=?`,
			token, lease, now.Format(time.RFC3339Nano), next, id, order.OperationToken)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		claimed = n == 1
		order, err = scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=?`, id))
		return err
	})
	if err != nil {
		return PaymentOrder{}, false, err
	}
	return order, claimed, nil
}

// RenewPaymentOrderOperation extends the same network-operation token before
// another provider request. Recovery intent is committed before its Create call,
// so a crash cannot repeat that one-time request after restart.
func (s *Store) RenewPaymentOrderOperation(ctx context.Context, id, token string, qrRecoveryAttempted bool) (PaymentOrder, error) {
	if token == "" {
		return PaymentOrder{}, ErrPaymentOperationLost
	}
	var order PaymentOrder
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE payment_orders SET id=id WHERE id=? AND operation_token=?`, id, token); err != nil {
			return err
		}
		current, err := scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=? AND operation_token=?`, id, token))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && current.Status == "PAID") {
			return ErrPaymentOperationLost
		}
		if err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		now := time.Now().UTC()
		lease := now.Add(PaymentOperationLease)
		next := lease.Format(time.RFC3339Nano)
		if current.NextReconcileAt != "" {
			due, err := time.Parse(time.RFC3339Nano, current.NextReconcileAt)
			if err != nil {
				return err
			}
			if due.After(lease) {
				next = current.NextReconcileAt
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE payment_orders SET operation_lease_until=?,
		 next_reconcile_at=?,qr_recovery_attempted=MAX(qr_recovery_attempted,?),updated_at=?
		 WHERE id=? AND operation_token=? AND status<>'PAID'`, lease.Format(time.RFC3339Nano), next,
			qrRecoveryAttempted, now.Format(time.RFC3339Nano), id, token)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrPaymentOperationLost
		}
		order, err = scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=?`, id))
		return err
	})
	return order, err
}

// CompletePaymentOrderOperation uses token CAS, not wall-clock lease expiry: a
// late response is usable only until another worker claims a replacement token.
// Settlement needs no network lease and PAID always wins the status race.
func (s *Store) CompletePaymentOrderOperation(ctx context.Context, id, token string, in PaymentOrderUpdate) (PaymentOrder, error) {
	if token == "" {
		return PaymentOrder{}, ErrPaymentOperationLost
	}
	if (in.Status != "" && (!validPaymentStatus(in.Status) || in.Status == "PAID")) || in.ReconcileAttempts < 0 {
		return PaymentOrder{}, ErrInvalidPaymentOrder
	}
	var next any
	if !in.NextReconcileAt.IsZero() {
		next = in.NextReconcileAt.UTC().Format(time.RFC3339Nano)
	}
	var order PaymentOrder
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE payment_orders SET id=id WHERE id=? AND operation_token=?`, id, token); err != nil {
			return err
		}
		current, err := scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=? AND operation_token=?`, id, token))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPaymentOperationLost
		}
		if err != nil {
			return err
		}
		status := in.Status
		if status == "" {
			status = current.Status
		}
		if status == "CREATING" || status == "PROCESSING" {
			lease, err := time.Parse(time.RFC3339Nano, current.OperationLeaseUntil)
			if err != nil {
				return err
			}
			if in.NextReconcileAt.IsZero() || in.NextReconcileAt.Before(lease) {
				next = current.OperationLeaseUntil
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE payment_orders SET
		 status=CASE WHEN status='PAID' OR ?='' THEN status ELSE ? END,
		 payment_link_id=COALESCE(payment_link_id,NULLIF(?,'')), qr_code=COALESCE(qr_code,NULLIF(?,'')),
		 checkout_url=COALESCE(checkout_url,NULLIF(?,'')), bank_bin=COALESCE(bank_bin,NULLIF(?,'')),
		 account_number=COALESCE(account_number,NULLIF(?,'')), account_name=COALESCE(account_name,NULLIF(?,'')),
		 last_error_code=CASE WHEN status='PAID' THEN last_error_code ELSE NULLIF(?,'') END,
		 next_reconcile_at=CASE WHEN status='PAID' THEN NULL ELSE ? END,
		 reconcile_attempts=CASE WHEN ? THEN 0 ELSE MAX(reconcile_attempts,?) END, qr_recovery_attempted=MAX(qr_recovery_attempted,?),
		 operation_token=NULL, operation_lease_until=NULL, updated_at=?
		 WHERE id=? AND operation_token=?`,
			in.Status, in.Status, in.PaymentLinkID, in.QRCode, in.CheckoutURL, in.BankBin,
			in.AccountNumber, in.AccountName, in.LastErrorCode, next,
			in.ResetReconcileAttempts, in.ReconcileAttempts, in.QRRecoveryAttempted, time.Now().UTC().Format(time.RFC3339Nano), id, token)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrPaymentOperationLost
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		order, err = scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=?`, id))
		return err
	})
	return order, err
}

func (s *Store) ListPaymentOrders(ctx context.Context, filter PaymentOrderFilter) (Page[PaymentOrder], error) {
	limit := normalizePageSize(filter.Limit)
	stamp, cursorID, err := decodeCursor(filter.Cursor)
	if err != nil {
		return Page[PaymentOrder]{}, err
	}
	if filter.Status != "" && !validPaymentStatus(filter.Status) {
		return Page[PaymentOrder]{}, ErrInvalidPaymentOrder
	}
	query := `SELECT ` + paymentOrderColumns + ` FROM payment_orders WHERE 1=1`
	args := []any{}
	if filter.ChannelID != "" {
		query += ` AND channel_id=?`
		args = append(args, filter.ChannelID)
	}
	if filter.Status != "" {
		query += ` AND status=?`
		args = append(args, filter.Status)
	}
	if stamp != "" {
		query += ` AND (created_at<? OR (created_at=? AND id<?))`
		args = append(args, stamp, stamp, cursorID)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[PaymentOrder]{}, err
	}
	defer rows.Close()
	items := make([]PaymentOrder, 0, limit)
	for rows.Next() {
		order, err := scanPaymentOrder(rows)
		if err != nil {
			return Page[PaymentOrder]{}, err
		}
		items = append(items, order)
	}
	if err := rows.Err(); err != nil {
		return Page[PaymentOrder]{}, err
	}
	page := Page[PaymentOrder]{Items: items}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// DuePaymentOrders reads local scheduling state without making provider calls.
// Active network leases remain excluded even when an inbox consumer wakes early.
func (s *Store) DuePaymentOrders(ctx context.Context, channelID string, now time.Time, limit int) ([]PaymentOrder, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders
	 WHERE channel_id=? AND status<>'PAID' AND next_reconcile_at IS NOT NULL
	 AND julianday(next_reconcile_at)<=julianday(?)
	 AND (operation_token IS NULL OR julianday(operation_lease_until)<=julianday(?))
	 ORDER BY next_reconcile_at,id LIMIT ?`, channelID, stamp, stamp, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PaymentOrder, 0, limit)
	for rows.Next() {
		order, err := scanPaymentOrder(rows)
		if err != nil {
			return nil, err
		}
		due, err := time.Parse(time.RFC3339Nano, order.NextReconcileAt)
		if err != nil {
			return nil, err
		}
		if due.After(now) {
			continue
		}
		if order.OperationToken != "" {
			lease, err := time.Parse(time.RFC3339Nano, order.OperationLeaseUntil)
			if err != nil {
				return nil, err
			}
			if now.Before(lease) {
				continue
			}
		}
		items = append(items, order)
	}
	return items, rows.Err()
}
