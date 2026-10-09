package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var payOSSettlementLocation = time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)

// SettlementEvidence is supplied only after the service verifies a signed Get
// response, PAID status, and its transaction. It is not browser input.
type SettlementEvidence struct {
	OrderCode            int64
	PaymentLinkID        string
	Reference            string
	AmountVnd            int64
	TransactionAt        time.Time
	AccountNumber        string
	VirtualAccountNumber string
}

// SettlementInput contains verified provider evidence. VerifiedCallback permits
// rejected callbacks to be retained atomically; InboxHash consumes earlier bind
// work in the same transaction as the financial commit.
type SettlementInput struct {
	ChannelID            string
	ChannelNamespace     string
	OrderCode            int64
	PaymentLinkID        string
	Reference            string
	AmountVnd            int64
	TransactionAt        time.Time
	Description          string
	AccountNumber        string
	VirtualAccountNumber string
	Source               string
	InboxHash            string
	VerifiedCallback     *VerifiedPaymentCallback
	VerifiedGet          *SettlementEvidence
}

type SettlementResult struct {
	Order        PaymentOrder
	Duplicate    bool
	Event        *EventNotification
	ReviewReason string
	InboxHash    string
}

// SettlementReviewError requires callers without callback evidence to preserve
// the reason on the order and retry or expose review, never synthesize money.
type SettlementReviewError struct{ Reason string }

func (e *SettlementReviewError) Error() string {
	return "payment settlement requires review: " + e.Reason
}

func effectivePaymentAccount(account, virtual string) string {
	if virtual != "" {
		return virtual
	}
	return account
}

func settlementCanonicalHash(in SettlementInput) string {
	// JSON tuple avoids ambiguous concatenation and excludes detection/source.
	b, _ := json.Marshal([]any{in.ChannelID, in.PaymentLinkID, in.Reference, in.AmountVnd, "VND", in.TransactionAt.UTC().Format(time.RFC3339Nano)})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func settlementGetMatches(in SettlementInput) bool {
	e := in.VerifiedGet
	return e != nil && e.OrderCode == in.OrderCode && e.PaymentLinkID == in.PaymentLinkID && e.Reference == in.Reference && e.AmountVnd == in.AmountVnd && e.TransactionAt.Equal(in.TransactionAt) && effectivePaymentAccount(e.AccountNumber, e.VirtualAccountNumber) != "" && effectivePaymentAccount(e.AccountNumber, e.VirtualAccountNumber) == effectivePaymentAccount(in.AccountNumber, in.VirtualAccountNumber)
}

// SettlePayment is the sole payOS financial write path. Network leases never
// fence verified money, and all financial rows and their durable event commit
// together. A rejected signed callback commits its review record instead.
func (s *Store) SettlePayment(ctx context.Context, in SettlementInput) (SettlementResult, error) {
	res := SettlementResult{}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Reserve SQLite's writer before reading; distinct gateway/worker Store
		// instances must not race a deferred read transaction into BUSY_SNAPSHOT.
		if _, err := tx.ExecContext(ctx, `UPDATE payment_orders SET id=id WHERE order_code=?`, in.OrderCode); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		review := func(reason string) error {
			res.ReviewReason = reason
			if in.VerifiedCallback != nil {
				callback := *in.VerifiedCallback
				callback.Reason = reason
				var err error
				res.InboxHash, err = s.insertPaymentInboxTx(ctx, tx, callback)
				return err
			}
			if in.InboxHash != "" {
				res.InboxHash = in.InboxHash
				result, err := tx.ExecContext(ctx, `UPDATE payos_webhook_inbox SET reason=? WHERE payload_hash=? AND processed_at IS NULL`, reason, in.InboxHash)
				if err != nil {
					return err
				}
				changed, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if changed != 1 {
					return sql.ErrNoRows
				}
				return nil
			}
			return &SettlementReviewError{Reason: reason}
		}
		if in.TransactionAt.IsZero() {
			return review("INVALID_TRANSACTION_DATE")
		}
		if in.ChannelID == "" || in.OrderCode <= 0 || in.PaymentLinkID == "" || strings.TrimSpace(in.Reference) == "" || in.AmountVnd <= 0 || (in.Source != "REALTIME" && in.Source != "CATCH_UP") {
			return review("PAYMENT_MISMATCH")
		}
		order, err := scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE order_code=?`, in.OrderCode))
		if errors.Is(err, sql.ErrNoRows) {
			return review("UNKNOWN_ORDER")
		}
		if err != nil {
			return err
		}
		res.Order = order
		if order.ChannelID != in.ChannelID {
			return review("PAYMENT_MISMATCH")
		}
		hash := settlementCanonicalHash(in)
		var existingHash, existingOrder, existingTransaction string
		err = tx.QueryRowContext(ctx, `SELECT canonical_hash,order_id,transaction_id FROM payment_receipts WHERE channel_id=? AND reference=?`, in.ChannelID, in.Reference).Scan(&existingHash, &existingOrder, &existingTransaction)
		if err == nil && (existingHash != hash || existingOrder != order.ID) {
			return review("REFERENCE_CONFLICT")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		duplicate := err == nil
		if order.AmountVnd != in.AmountVnd || (order.PaymentLinkID != "" && order.PaymentLinkID != in.PaymentLinkID) {
			return review("PAYMENT_MISMATCH")
		}
		account := effectivePaymentAccount(in.AccountNumber, in.VirtualAccountNumber)
		if account == "" {
			return review("PAYMENT_DETAILS_PENDING")
		}
		if order.AccountNumber != "" && order.AccountNumber != account {
			return review("PAYMENT_MISMATCH")
		}
		if in.VerifiedGet != nil && !settlementGetMatches(in) {
			return review("PAYMENT_MISMATCH")
		}
		if (order.PaymentLinkID == "" || order.AccountNumber == "") && !settlementGetMatches(in) {
			return review("AWAITING_ORDER_BIND")
		}
		if (order.Status == "CANCELLED" || order.Status == "EXPIRED" || order.Status == "FAILED") && !duplicate && !settlementGetMatches(in) {
			return review("PAYMENT_DETAILS_PENDING")
		}
		if in.InboxHash != "" {
			if !settlementGetMatches(in) {
				return review("PAYMENT_DETAILS_PENDING")
			}
			matches, err := settlementInboxMatches(ctx, tx, in)
			if err != nil {
				return err
			}
			if !matches {
				return review("PAYMENT_MISMATCH")
			}
		}
		if duplicate {
			if order.TransactionID != existingTransaction || order.Status != "PAID" {
				return errors.New("payment receipt/order invariant violated")
			}
			res.Duplicate = true
			return markSettlementInboxProcessed(ctx, tx, in.InboxHash)
		}
		if order.Status == "PAID" || order.TransactionID != "" {
			return review("EXTRA_PAYMENT_REVIEW")
		}
		createdAt := now()
		iso := in.TransactionAt.UTC().Format(time.RFC3339Nano)
		day := in.TransactionAt.In(payOSSettlementLocation).Format("2006-01-02")
		namespace := in.ChannelNamespace
		if namespace == "" {
			h := sha256.Sum256([]byte(in.ChannelID))
			namespace = hex.EncodeToString(h[:16])
		}
		if len(namespace) != 32 {
			return errors.New("invalid payment channel namespace")
		}
		if _, err := hex.DecodeString(namespace); err != nil {
			return errors.New("invalid payment channel namespace")
		}
		semanticKey := "PAYOS:" + namespace + ":" + in.Reference
		txnID := id("txn")
		// Use the persisted, application-generated order description, not untrusted
		// free text that could disclose account details or capability identifiers.
		description := order.Description
		if _, err := tx.ExecContext(ctx, `INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,description_envelope,parser_version,baseline_state,first_seen_at,transaction_at_iso,transaction_day,date_precision,ingest_source) VALUES(?,'payos-klb',?,?,?,?,0,?,NULL,?,'payos-v1','NONE',?,?,?,'datetime',?)`, txnID, semanticKey, hash, iso, iso, in.AmountVnd, []byte(description), createdAt, iso, day, in.Source); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO payment_receipts(channel_id,reference,order_id,payment_link_id,amount_vnd,transaction_at,canonical_hash,transaction_id,received_at) VALUES(?,?,?,?,?,?,?,?,?)`, in.ChannelID, in.Reference, order.ID, in.PaymentLinkID, in.AmountVnd, iso, hash, txnID, createdAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE payment_orders SET status='PAID',payment_link_id=?,account_number=?,transaction_id=?,paid_at=?,updated_at=?,last_error_code=NULL,next_reconcile_at=NULL WHERE id=?`, in.PaymentLinkID, account, txnID, iso, createdAt, order.ID); err != nil {
			return err
		}
		data := map[string]any{"bank": "KienlongBank", "provider": "PAYOS", "orderCode": strconv.FormatInt(order.OrderCode, 10), "paymentOrigin": order.Origin, "paymentLinkId": in.PaymentLinkID, "transactionId": txnID, "transactionNumber": in.Reference, "credit": strconv.FormatInt(in.AmountVnd, 10), "debit": "0", "currency": "VND", "transactionDate": iso, "transactionDay": day, "datePrecision": "datetime", "source": in.Source, "description": description, "detectedAt": createdAt}
		res.Event, err = s.emitTransactionEventTx(ctx, tx, txnID, "bank.transaction.credit", "payos:"+namespace, semanticKey, data, createdAt)
		if err != nil {
			return err
		}
		if res.Event == nil {
			return errors.New("new payment transaction did not produce an event")
		}
		if err := markSettlementInboxProcessed(ctx, tx, in.InboxHash); err != nil {
			return err
		}
		res.Order, err = scanPaymentOrder(tx.QueryRowContext(ctx, `SELECT `+paymentOrderColumns+` FROM payment_orders WHERE id=?`, order.ID))
		return err
	})
	if err != nil {
		return SettlementResult{}, err
	}
	if res.Event != nil {
		res.Event.CommittedAt = now()
	}
	return res, nil
}

func markSettlementInboxProcessed(ctx context.Context, tx *sql.Tx, hash string) error {
	if hash == "" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE payos_webhook_inbox SET processed_at=COALESCE(processed_at,?) WHERE payload_hash=?`, now(), hash)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func settlementInboxMatches(ctx context.Context, tx *sql.Tx, in SettlementInput) (bool, error) {
	var code int64
	var link, reference, payload, reason string
	err := tx.QueryRowContext(ctx, `SELECT order_code,payment_link_id,reference,payload_json,reason FROM payos_webhook_inbox WHERE payload_hash=?`, in.InboxHash).Scan(&code, &link, &reference, &payload, &reason)
	if err != nil {
		return false, err
	}
	if code != in.OrderCode || link != in.PaymentLinkID || reference != in.Reference || (reason != "AWAITING_ORDER_BIND" && reason != "PAYMENT_DETAILS_PENDING" && reason != "UNKNOWN_ORDER") {
		return false, nil
	}
	var data map[string]any
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return false, fmt.Errorf("decode verified payment inbox: %w", err)
	}
	orderCode, ok := data["orderCode"].(json.Number)
	if !ok {
		return false, nil
	}
	parsedCode, err := orderCode.Int64()
	if err != nil || parsedCode != in.OrderCode || data["paymentLinkId"] != in.PaymentLinkID || data["reference"] != in.Reference {
		return false, nil
	}
	amount, ok := data["amount"].(json.Number)
	if !ok {
		return false, nil
	}
	v, err := amount.Int64()
	if err != nil || v != in.AmountVnd || data["currency"] != "VND" || data["code"] != "00" {
		return false, nil
	}
	account, _ := data["accountNumber"].(string)
	virtual, _ := data["virtualAccountNumber"].(string)
	if effectivePaymentAccount(account, virtual) != effectivePaymentAccount(in.AccountNumber, in.VirtualAccountNumber) {
		return false, nil
	}
	date, _ := data["transactionDateTime"].(string)
	at, err := time.ParseInLocation("2006-01-02 15:04:05", date, payOSSettlementLocation)
	if err != nil {
		at, err = time.Parse(time.RFC3339Nano, date)
	}
	return err == nil && at.Equal(in.TransactionAt), nil
}
