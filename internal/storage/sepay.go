package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bankdate"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

var ErrSePayReceiverMismatch = errors.New("SePay store receiver is immutable; use a new store key")

// SePayNotificationInput is supplied only after authenticating the Telegram
// envelope. Review inputs retain evidence but never create financial rows.
type SePayNotificationInput struct {
	StoreKey, BankCode, AccountNumber, Mode, ReviewReason string
	BotID, UpdateID, ChatID, MessageID                    int64
	ConfigRevision                                        int64
	ActivationAt, MessageAt                               time.Time
	RawPayload                                            []byte
	Credit                                                *SePayCredit
}

type SePayCredit struct {
	AmountVND     int64
	Reference     string
	TransactionAt time.Time
}

type SePayIngestResult struct {
	Duplicate    bool
	ReviewReason string
	Event        *EventNotification
}

func sepayHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func sepayCanonicalHash(in SePayNotificationInput) string {
	data, _ := json.Marshal([]any{in.StoreKey, in.BankCode, in.AccountNumber, in.Credit.Reference, in.Credit.AmountVND, in.Credit.TransactionAt.UTC().Format(time.RFC3339Nano)})
	return sepayHash(data)
}

// Canonicalize the full object without converting Telegram int64 identifiers to
// float64. The message fingerprint excludes update_id: redelivery of the same
// message under another update must not be mistaken for an edit.
func sepayPayloadHashes(raw []byte) (string, string, error) {
	if len(raw) == 0 || len(raw) > 64*1024 {
		return "", "", errors.New("invalid SePay payload size")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil || value == nil {
		return "", "", errors.New("invalid SePay payload object")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return "", "", errors.New("invalid SePay trailing payload")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	payloadHash := sepayHash(canonical)
	delete(value, "update_id")
	message, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	return payloadHash, sepayHash(message), nil
}

func sepayInboxAAD(botID, updateID int64) []byte {
	return []byte(fmt.Sprintf("sepay-telegram:%d:%d", botID, updateID))
}

func (s *Store) sepayEncrypt(raw, aad []byte) ([]byte, error) {
	envelope, err := s.keyring.Encrypt(raw, aad)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (s *Store) sepayDecrypt(blob, aad []byte) ([]byte, error) {
	var envelope security.Envelope
	if err := json.Unmarshal(blob, &envelope); err != nil {
		return nil, err
	}
	return s.keyring.Decrypt(envelope, aad)
}

func validSePayReviewReason(reason string) bool {
	switch reason {
	case "INVALID_TEMPLATE", "OUTGOING", "ACCOUNT_MISMATCH", "INVALID_AMOUNT", "INVALID_TRANSACTION_DATE", "MISSING_REFERENCE", "REFERENCE_CONFLICT", "EDITED_MESSAGE":
		return true
	default:
		return false
	}
}

// The connection pins the receiver before the first observation, not merely
// after the first credit. This also protects stores with no accepted receipts.
func (s *Store) ensureSePayConnectionTx(ctx context.Context, tx *sql.Tx, storeKey, bankCode, accountNumber, receivedAt string) error {
	connectionID := "sepay-store:" + storeKey
	var bank string
	var accountEnvelope []byte
	err := tx.QueryRowContext(ctx, `SELECT bank_code,account_envelope FROM connections WHERE id=?`, connectionID).Scan(&bank, &accountEnvelope)
	if err == nil {
		if bank != bankCode || len(accountEnvelope) == 0 {
			return ErrSePayReceiverMismatch
		}
		account, err := s.sepayDecrypt(accountEnvelope, []byte(connectionID))
		if err != nil {
			return fmt.Errorf("decrypt SePay receiver: %w", err)
		}
		if string(account) != accountNumber {
			return ErrSePayReceiverMismatch
		}
		_, err = tx.ExecContext(ctx, `UPDATE connections SET state='WEBHOOK',updated_at=? WHERE id=?`, receivedAt, connectionID)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	accountEnvelope, err = s.sepayEncrypt([]byte(accountNumber), []byte(connectionID))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO connections(id,bank_code,account_envelope,state,created_at,updated_at) VALUES(?,?,?,'WEBHOOK',?,?)`, connectionID, bankCode, accountEnvelope, receivedAt, receivedAt)
	return err
}

// IngestSePayNotification commits evidence, receipt, credit and the event journal
// together. It does not settle, associate, or mutate any payOS payment order.
func (s *Store) IngestSePayNotification(ctx context.Context, in SePayNotificationInput) (SePayIngestResult, error) {
	if s.keyring == nil {
		return SePayIngestResult{}, errors.New("master keyring required for SePay ingestion")
	}
	if in.StoreKey == "" || in.BankCode == "" || in.AccountNumber == "" || in.BotID <= 0 || in.UpdateID < 0 || in.ChatID >= 0 || in.MessageID <= 0 || (in.Mode != "active" && in.Mode != "observe") || in.ActivationAt.IsZero() {
		return SePayIngestResult{}, errors.New("incomplete SePay notification input")
	}
	if in.ReviewReason != "" && !validSePayReviewReason(in.ReviewReason) {
		return SePayIngestResult{}, errors.New("invalid SePay review reason")
	}
	payloadHash, messageHash, err := sepayPayloadHashes(in.RawPayload)
	if err != nil {
		return SePayIngestResult{}, err
	}
	encrypted, err := s.sepayEncrypt(in.RawPayload, sepayInboxAAD(in.BotID, in.UpdateID))
	if err != nil {
		return SePayIngestResult{}, err
	}
	res := SePayIngestResult{}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		received := time.Now().UTC()
		receivedAt := received.Format(time.RFC3339Nano)
		// INSERT first reserves SQLite's writer across independent Store handles;
		// every later read observes the preceding writer's committed decision.
		insert, err := tx.ExecContext(ctx, `INSERT INTO sepay_telegram_inbox(bot_id,update_id,chat_id,message_id,payload_hash,payload_envelope,store_key,reason,received_at) VALUES(?,?,?,?,?,?,?,'INVALID_TEMPLATE',?) ON CONFLICT(bot_id,update_id) DO NOTHING`, in.BotID, in.UpdateID, in.ChatID, in.MessageID, payloadHash, encrypted, in.StoreKey, receivedAt)
		if err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		if err := checkSePayRevisionTx(ctx, tx, in.ConfigRevision); err != nil {
			return err
		}
		if err := s.ensureSePayConnectionTx(ctx, tx, in.StoreKey, in.BankCode, in.AccountNumber, receivedAt); err != nil {
			return err
		}
		inserted, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		finish := func(reason string, transactionID any) error {
			if reason == "DUPLICATE" {
				res.Duplicate = true
			} else if reason != "ACCEPTED" {
				res.ReviewReason = reason
			}
			_, err := tx.ExecContext(ctx, `UPDATE sepay_telegram_inbox SET reason=?,transaction_id=? WHERE bot_id=? AND update_id=?`, reason, transactionID, in.BotID, in.UpdateID)
			return err
		}
		quarantine := func(reason, existingHash string) error {
			res.ReviewReason = reason
			semanticKey := fmt.Sprintf("SEPAY:%s:telegram:%d:%d:%d", in.StoreKey, in.BotID, in.ChatID, in.MessageID)
			if in.Credit != nil && in.Credit.Reference != "" {
				semanticKey = "SEPAY:" + in.StoreKey + ":" + in.Credit.Reference
			}
			// Stable candidate identity makes conflict retries idempotent, while
			// retaining distinct encrypted candidates for operator comparison.
			candidateID := "sepay_quarantine_" + sepayHash([]byte(fmt.Sprintf("%s:%d:%d:%s:%s", in.StoreKey, in.BotID, in.UpdateID, payloadHash, reason)))
			_, err := tx.ExecContext(ctx, `INSERT INTO transaction_quarantine(id,connection_id,semantic_key,existing_hash,candidate_envelope,reason,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, candidateID, "sepay-store:"+in.StoreKey, semanticKey, existingHash, encrypted, reason, receivedAt)
			if err != nil || inserted == 0 {
				return err
			}
			return finish(reason, nil)
		}
		if inserted == 0 {
			var existingHash, existingStore, reason string
			if err := tx.QueryRowContext(ctx, `SELECT payload_hash,store_key,reason FROM sepay_telegram_inbox WHERE bot_id=? AND update_id=?`, in.BotID, in.UpdateID).Scan(&existingHash, &existingStore, &reason); err != nil {
				return err
			}
			if existingHash != payloadHash || existingStore != in.StoreKey {
				conflictReason := "REFERENCE_CONFLICT"
				if in.ReviewReason == "EDITED_MESSAGE" {
					conflictReason = "EDITED_MESSAGE"
				}
				return quarantine(conflictReason, existingHash)
			}
			res.Duplicate = true
			if reason != "ACCEPTED" && reason != "DUPLICATE" {
				res.ReviewReason = reason
			}
			return nil // First observation (including received_at) stays unchanged.
		}
		var previousUpdate int64
		var previousBlob []byte
		var previousHash, previousStore, previousReason string
		var previousTransaction sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT update_id,payload_envelope,payload_hash,store_key,reason,transaction_id FROM sepay_telegram_inbox WHERE bot_id=? AND chat_id=? AND message_id=? AND update_id<>? ORDER BY rowid LIMIT 1`, in.BotID, in.ChatID, in.MessageID, in.UpdateID).Scan(&previousUpdate, &previousBlob, &previousHash, &previousStore, &previousReason, &previousTransaction)
		if err == nil {
			previousRaw, err := s.sepayDecrypt(previousBlob, sepayInboxAAD(in.BotID, previousUpdate))
			if err != nil {
				return err
			}
			_, previousMessageHash, err := sepayPayloadHashes(previousRaw)
			if err != nil {
				return err
			}
			if in.ReviewReason == "EDITED_MESSAGE" {
				return quarantine("EDITED_MESSAGE", previousHash)
			}
			if previousMessageHash != messageHash || previousStore != in.StoreKey {
				return quarantine("REFERENCE_CONFLICT", previousHash)
			}
			res.Duplicate = true
			if previousReason == "ACCEPTED" || previousReason == "DUPLICATE" || previousReason == "OBSERVATION" {
				return finish("DUPLICATE", previousTransaction)
			}
			return finish(previousReason, nil)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if in.ReviewReason != "" {
			return finish(in.ReviewReason, nil)
		}
		if in.Credit == nil {
			return finish("INVALID_TEMPLATE", nil)
		}
		credit := in.Credit
		if credit.AmountVND <= 0 || credit.AmountVND > 9007199254740991 {
			return finish("INVALID_AMOUNT", nil)
		}
		if credit.TransactionAt.IsZero() {
			return finish("INVALID_TRANSACTION_DATE", nil)
		}
		if strings.TrimSpace(credit.Reference) == "" || utf8.RuneCountInString(credit.Reference) > 200 {
			return finish("MISSING_REFERENCE", nil)
		}
		hash := sepayCanonicalHash(in)
		var receiptHash, receiptTransaction string
		err = tx.QueryRowContext(ctx, `SELECT canonical_hash,transaction_id FROM sepay_receipts WHERE store_key=? AND reference=?`, in.StoreKey, credit.Reference).Scan(&receiptHash, &receiptTransaction)
		if err == nil {
			if receiptHash != hash {
				return quarantine("REFERENCE_CONFLICT", receiptHash)
			}
			return finish("DUPLICATE", receiptTransaction)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if credit.TransactionAt.Before(in.ActivationAt) {
			return finish("PRE_ACTIVATION", nil)
		}
		if in.Mode == "observe" {
			return finish("OBSERVATION", nil)
		}
		source := "CATCH_UP"
		age := received.Sub(in.MessageAt)
		if !in.MessageAt.IsZero() && age >= -30*time.Second && age <= 120*time.Second {
			source = "REALTIME"
		}
		transactionID, event, err := s.insertSePayCreditTx(ctx, tx, sepayAcceptedCredit{StoreKey: in.StoreKey, BankCode: in.BankCode, AccountNumber: in.AccountNumber, SemanticKey: "SEPAY:"+in.StoreKey+":"+credit.Reference, CanonicalHash: hash, Reference: credit.Reference, Source: source, ParserVersion: "sepay-telegram-v1", AmountVND: credit.AmountVND, TransactionAt: credit.TransactionAt, ReceivedAt: receivedAt})
		if err != nil { return err }
		res.Event = event
		if _, err := tx.ExecContext(ctx, `INSERT INTO sepay_receipts(store_key,bank_code,account_number,reference,canonical_hash,transaction_id,message_id,received_at) VALUES(?,?,?,?,?,?,?,?)`, in.StoreKey, in.BankCode, in.AccountNumber, credit.Reference, hash, transactionID, in.MessageID, receivedAt); err != nil {
			return err
		}
		if err := finish("ACCEPTED", transactionID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return SePayIngestResult{}, err
	}
	if res.Event != nil {
		res.Event.CommittedAt = now()
	}
	return res, nil
}

type sepayAcceptedCredit struct {
	StoreKey, BankCode, AccountNumber, SemanticKey, CanonicalHash, Reference, Source, ParserVersion string
	AmountVND int64
	TransactionAt time.Time
	ReceivedAt string
}

func (s *Store) insertSePayCreditTx(ctx context.Context, tx *sql.Tx, in sepayAcceptedCredit) (string, *EventNotification, error) {
	iso := in.TransactionAt.UTC().Format(time.RFC3339Nano)
	day := in.TransactionAt.In(bankdate.DefaultLocation).Format("2006-01-02")
	transactionID := id("txn")
	const description = "Thanh toán QR cửa hàng"
	_, err := tx.ExecContext(ctx, `INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,description_envelope,parser_version,baseline_state,first_seen_at,transaction_at_iso,transaction_day,date_precision,ingest_source) VALUES(?,?,?,?,?,?,0,?,NULL,?,?,'NONE',?,?,?,'datetime',?)`, transactionID, "sepay-store:"+in.StoreKey, in.SemanticKey, in.CanonicalHash, iso, iso, in.AmountVND, []byte(description), in.ParserVersion, in.ReceivedAt, iso, day, in.Source)
	if err != nil { return "", nil, err }
	data := map[string]any{"bank": in.BankCode, "provider": "SEPAY", "transactionId": transactionID, "transactionNumber": in.Reference, "credit": strconv.FormatInt(in.AmountVND,10), "debit":"0", "currency":"VND", "transactionDate":iso, "transactionDay":day, "datePrecision":"datetime", "source":in.Source, "description":description, "detectedAt":in.ReceivedAt}
	event, err := s.emitTransactionEventTx(ctx,tx,transactionID,"bank.transaction.credit","sepay-store:"+in.StoreKey,in.SemanticKey,data,in.ReceivedAt)
	if err == nil && event == nil { err = errors.New("new SePay transaction did not produce an event") }
	return transactionID,event,err
}

// LastSePayMessageAt counts valid Telegram notifications only. Native webhook
// observations have their own timestamp and do not imply Telegram connectivity.
func (s *Store) LastSePayMessageAt(ctx context.Context, storeKey string) (*time.Time, error) {
	var value sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM sepay_telegram_inbox WHERE store_key=? AND reason IN ('ACCEPTED','OBSERVATION','DUPLICATE')`, storeKey).Scan(&value); err != nil {
		return nil, err
	}
	if !value.Valid {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &at, nil
}

func (s *Store) LastSePayWebhookAt(ctx context.Context, storeKey string) (*time.Time, error) {
	var value sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM sepay_native_inbox WHERE store_key=? AND reason IN ('ACCEPTED','OBSERVATION','DUPLICATE')`, storeKey).Scan(&value); err != nil {
		return nil, err
	}
	if !value.Valid {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// SePayReview excludes encrypted evidence, sender identity and financial data.
type SePayReview struct {
	StoreKey   string `json:"storeKey"`
	Transport  string `json:"transport"`
	SourceID   string `json:"sourceId"`
	Reason     string `json:"reason"`
	ReceivedAt string `json:"receivedAt"`
}

var ErrInvalidSePayReviewCursor = errors.New("invalid SePay review cursor")

const (
	sepayTelegramReviewPredicate = `reason IN ('INVALID_TEMPLATE','ACCOUNT_MISMATCH','INVALID_AMOUNT','INVALID_TRANSACTION_DATE','MISSING_REFERENCE','REFERENCE_CONFLICT','EDITED_MESSAGE')`
	sepayNativeReviewPredicate   = `reason IN ('ACCOUNT_MISMATCH','INVALID_AMOUNT','INVALID_TRANSACTION_DATE','MISSING_REFERENCE','REFERENCE_CONFLICT')`
)

func (s *Store) SePayReviewCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM sepay_telegram_inbox WHERE `+sepayTelegramReviewPredicate+`) + (SELECT COUNT(*) FROM sepay_native_inbox WHERE `+sepayNativeReviewPredicate+`)`).Scan(&count)
	return count, err
}


func encodeSePayReviewCursor(receivedAt, transport, key string) string {
	raw, _ := json.Marshal([]any{2, receivedAt, transport, key})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeSePayReviewCursor(cursor string) (receivedAt, transport, key string, err error) {
	if cursor == "" {
		return "", "", "", nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil {
		return "", "", "", ErrInvalidSePayReviewCursor
	}
	var tuple []any
	if json.Unmarshal(raw, &tuple) != nil || len(tuple) != 4 {
		return "", "", "", ErrInvalidSePayReviewCursor
	}
	v, ok1 := tuple[0].(float64)
	rAt, ok2 := tuple[1].(string)
	tr, ok3 := tuple[2].(string)
	k, ok4 := tuple[3].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 || v != 2 || (tr != "telegram" && tr != "native") {
		return "", "", "", ErrInvalidSePayReviewCursor
	}
	if _, err := time.Parse(time.RFC3339Nano, rAt); err != nil {
		return "", "", "", ErrInvalidSePayReviewCursor
	}
	parts := strings.Split(k, ":")
	if len(parts) != 2 { return "", "", "", ErrInvalidSePayReviewCursor }
	providerID, parseErr := strconv.ParseInt(parts[1],10,64)
	if parseErr != nil || strconv.FormatInt(providerID,10) != parts[1] || providerID < 0 || tr == "native" && providerID == 0 { return "", "", "", ErrInvalidSePayReviewCursor }
	if tr == "telegram" {
		botID, parseErr := strconv.ParseInt(parts[0],10,64)
		if parseErr != nil || botID <= 0 || strconv.FormatInt(botID,10) != parts[0] { return "", "", "", ErrInvalidSePayReviewCursor }
	} else {
		if len(parts[0]) == 0 || len(parts[0]) > 48 { return "", "", "", ErrInvalidSePayReviewCursor }
		for _, c := range parts[0] { if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') { return "", "", "", ErrInvalidSePayReviewCursor } }
	}
	if encodeSePayReviewCursor(rAt,tr,k) != cursor { return "", "", "", ErrInvalidSePayReviewCursor }
	return rAt, tr, k, nil
}

// ListSePayReviews returns combined reviews across Telegram and Native transports
// using canonical pagination order: received_at DESC, transport DESC, key DESC.
func (s *Store) ListSePayReviews(ctx context.Context, cursor string, limit int) (Page[SePayReview], error) {
	limit = normalizePageSize(limit)
	cursorReceivedAt, cursorTransport, cursorKey, err := decodeSePayReviewCursor(cursor)
	if err != nil {
		return Page[SePayReview]{}, ErrInvalidSePayReviewCursor
	}
	unionQuery := `
SELECT store_key, 'telegram' AS transport, (CAST(bot_id AS TEXT) || ':' || CAST(update_id AS TEXT)) AS source_id, reason, received_at
FROM sepay_telegram_inbox
WHERE ` + sepayTelegramReviewPredicate + `
UNION ALL
SELECT store_key, 'native' AS transport, (store_key || ':' || CAST(provider_id AS TEXT)) AS source_id, reason, received_at
FROM sepay_native_inbox
WHERE ` + sepayNativeReviewPredicate

	where := ""
	args := []any{}
	if cursor != "" {
		where = ` WHERE (received_at < ? OR (received_at = ? AND (transport < ? OR (transport = ? AND source_id < ?))))`
		args = append(args, cursorReceivedAt, cursorReceivedAt, cursorTransport, cursorTransport, cursorKey)
	}
	query := `SELECT store_key, transport, source_id, reason, received_at FROM (` + unionQuery + `)` + where + ` ORDER BY received_at DESC, transport DESC, source_id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[SePayReview]{}, err
	}
	defer rows.Close()
	page := Page[SePayReview]{Items: []SePayReview{}}
	for rows.Next() {
		var item SePayReview
		if err := rows.Scan(&item.StoreKey, &item.Transport, &item.SourceID, &item.Reason, &item.ReceivedAt); err != nil {
			return Page[SePayReview]{}, err
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeSePayReviewCursor(last.ReceivedAt, last.Transport, last.SourceID)
			break
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[SePayReview]{}, err
	}
	return page, nil
}

type SePayWebhookInput struct {
	StoreKey                   string
	BankCode                   string
	AccountNumber              string
	Mode                       string
	ReviewReason               string
	Gateway                    string
	NotificationAccountNumber string
	SubAccount                 string
	Reference                  string
	TransferType               string
	ProviderID                 int64
	AmountVND                  int64
	ConfigRevision             int64
	ActivationAt               time.Time
	TransactionAt              time.Time
	RawPayload                 []byte
	NativeReferenceVerified    bool
}

func (s *Store) IngestSePayWebhook(ctx context.Context, in SePayWebhookInput) (SePayIngestResult, error) {
	if s.keyring == nil {
		return SePayIngestResult{}, errors.New("master keyring required for SePay ingestion")
	}
	if in.StoreKey == "" || in.BankCode == "" || in.AccountNumber == "" || in.ProviderID <= 0 || (in.Mode != "active" && in.Mode != "observe") || in.ActivationAt.IsZero() {
		return SePayIngestResult{}, errors.New("incomplete SePay webhook input")
	}
	if in.ReviewReason != "" && !validSePayReviewReason(in.ReviewReason) {
		return SePayIngestResult{}, errors.New("invalid SePay review reason")
	}
	aad := []byte(fmt.Sprintf("sepay-native:%s:%d", in.StoreKey, in.ProviderID))
	encrypted, err := s.sepayEncrypt(in.RawPayload, aad)
	if err != nil {
		return SePayIngestResult{}, err
	}
	data, _ := json.Marshal([]any{in.StoreKey, in.ProviderID, in.Gateway, in.NotificationAccountNumber, in.SubAccount, in.AmountVND, in.TransferType, in.Reference, in.TransactionAt.UTC().Format(time.RFC3339Nano)})
	canonicalHash := sepayHash(data)

	res := SePayIngestResult{}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		received := time.Now().UTC()
		receivedAt := received.Format(time.RFC3339Nano)
		insert, err := tx.ExecContext(ctx, `INSERT INTO sepay_native_inbox(store_key,provider_id,reference,canonical_hash,payload_envelope,reason,transaction_at,received_at) VALUES(?,?,?,?,?,'INVALID_TEMPLATE',?,?) ON CONFLICT(store_key,provider_id) DO NOTHING`, in.StoreKey, in.ProviderID, in.Reference, canonicalHash, encrypted, in.TransactionAt.UTC().Format(time.RFC3339Nano), receivedAt)
		if err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		if err := checkSePayRevisionTx(ctx, tx, in.ConfigRevision); err != nil {
			return err
		}
		if err := s.ensureSePayConnectionTx(ctx, tx, in.StoreKey, in.BankCode, in.AccountNumber, receivedAt); err != nil {
			return err
		}
		inserted, err := insert.RowsAffected()
		if err != nil {
			return err
		}
		finish := func(reason string, transactionID any) error {
			if reason == "DUPLICATE" {
				res.Duplicate = true
			} else if reason != "ACCEPTED" {
				res.ReviewReason = reason
			}
			_, err := tx.ExecContext(ctx, `UPDATE sepay_native_inbox SET reason=?,transaction_id=? WHERE store_key=? AND provider_id=?`, reason, transactionID, in.StoreKey, in.ProviderID)
			return err
		}
		quarantine := func(reason, existingHash string) error {
			res.ReviewReason = reason
			semanticKey := fmt.Sprintf("SEPAY_NATIVE:%s:%d", in.StoreKey, in.ProviderID)
			candidateID := "sepay_quarantine_" + sepayHash([]byte(fmt.Sprintf("%s:%d:%s:%s", in.StoreKey, in.ProviderID, canonicalHash, reason)))
			_, err := tx.ExecContext(ctx, `INSERT INTO transaction_quarantine(id,connection_id,semantic_key,existing_hash,candidate_envelope,reason,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, candidateID, "sepay-store:"+in.StoreKey, semanticKey, existingHash, encrypted, reason, receivedAt)
			if err != nil || inserted == 0 {
				return err
			}
			return finish(reason, nil)
		}
		if inserted == 0 {
			var existingHash, reason string
			var existingTx sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT canonical_hash,reason,transaction_id FROM sepay_native_inbox WHERE store_key=? AND provider_id=?`, in.StoreKey, in.ProviderID).Scan(&existingHash, &reason, &existingTx); err != nil {
				return err
			}
			if existingHash != canonicalHash {
				return quarantine("REFERENCE_CONFLICT", existingHash)
			}
			res.Duplicate = true
			if reason != "ACCEPTED" && reason != "DUPLICATE" {
				res.ReviewReason = reason
			}
			return nil
		}
		if in.ReviewReason != "" {
			return finish(in.ReviewReason, nil)
		}
		if in.AmountVND <= 0 || in.AmountVND > 9007199254740991 {
			return finish("INVALID_AMOUNT", nil)
		}
		if in.TransactionAt.IsZero() {
			return finish("INVALID_TRANSACTION_DATE", nil)
		}
		var telegramReceiptCount int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sepay_receipts WHERE store_key=?`, in.StoreKey).Scan(&telegramReceiptCount); err != nil {
			return err
		}
		if telegramReceiptCount > 0 && in.Reference == "" {
			return finish("MISSING_REFERENCE", nil)
		}
		if in.Mode == "active" && telegramReceiptCount > 0 && !in.NativeReferenceVerified { return finish("REFERENCE_CONFLICT", nil) }
		if in.Reference != "" {
			var existingNativeID int64
			var existingNativeHash string
			err := tx.QueryRowContext(ctx, `SELECT provider_id,canonical_hash FROM sepay_native_inbox WHERE store_key=? AND reference=? AND provider_id<>? LIMIT 1`, in.StoreKey, in.Reference, in.ProviderID).Scan(&existingNativeID, &existingNativeHash)
			if err == nil {
				return quarantine("REFERENCE_CONFLICT", existingNativeHash)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var telegramHash, telegramTx string
			err = tx.QueryRowContext(ctx, `SELECT canonical_hash,transaction_id FROM sepay_receipts WHERE store_key=? AND reference=?`, in.StoreKey, in.Reference).Scan(&telegramHash, &telegramTx)
			if err == nil {
				if !in.NativeReferenceVerified {
					return quarantine("REFERENCE_CONFLICT", telegramHash)
				}
				var tAmount int64
				var tDate string
				err = tx.QueryRowContext(ctx, `SELECT credit,transaction_at_iso FROM transactions WHERE id=?`, telegramTx).Scan(&tAmount, &tDate)
				if err != nil { return err }
				if tAmount == in.AmountVND && tDate == in.TransactionAt.UTC().Format(time.RFC3339Nano) {
					return finish("DUPLICATE", telegramTx)
				}
				return quarantine("REFERENCE_CONFLICT", telegramHash)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if in.TransactionAt.Before(in.ActivationAt) {
			return finish("PRE_ACTIVATION", nil)
		}
		if in.Mode == "observe" {
			return finish("OBSERVATION", nil)
		}
		source := "CATCH_UP"
		age := received.Sub(in.TransactionAt)
		if age >= -30*time.Second && age <= 120*time.Second {
			source = "REALTIME"
		}
		semanticKey := fmt.Sprintf("SEPAY_NATIVE:%s:%d", in.StoreKey, in.ProviderID)
		transactionID, event, err := s.insertSePayCreditTx(ctx, tx, sepayAcceptedCredit{
			StoreKey: in.StoreKey, BankCode: in.BankCode, AccountNumber: in.AccountNumber,
			SemanticKey: semanticKey, CanonicalHash: canonicalHash, Reference: in.Reference,
			Source: source, ParserVersion: "sepay-native-v1", AmountVND: in.AmountVND,
			TransactionAt: in.TransactionAt, ReceivedAt: receivedAt,
		})
		if err != nil {
			return err
		}
		res.Event = event
		if err := finish("ACCEPTED", transactionID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return SePayIngestResult{}, err
	}
	if res.Event != nil {
		res.Event.CommittedAt = now()
	}
	return res, nil
}
