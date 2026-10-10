package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
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
func (s *Store) ensureSePayConnectionTx(ctx context.Context, tx *sql.Tx, in SePayNotificationInput, receivedAt string) error {
	connectionID := "sepay-store:" + in.StoreKey
	var bank string
	var accountEnvelope []byte
	err := tx.QueryRowContext(ctx, `SELECT bank_code,account_envelope FROM connections WHERE id=?`, connectionID).Scan(&bank, &accountEnvelope)
	if err == nil {
		if bank != in.BankCode || len(accountEnvelope) == 0 {
			return ErrSePayReceiverMismatch
		}
		account, err := s.sepayDecrypt(accountEnvelope, []byte(connectionID))
		if err != nil {
			return fmt.Errorf("decrypt SePay receiver: %w", err)
		}
		if string(account) != in.AccountNumber {
			return ErrSePayReceiverMismatch
		}
		_, err = tx.ExecContext(ctx, `UPDATE connections SET state='WEBHOOK',updated_at=? WHERE id=?`, receivedAt, connectionID)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	accountEnvelope, err = s.sepayEncrypt([]byte(in.AccountNumber), []byte(connectionID))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO connections(id,bank_code,account_envelope,state,created_at,updated_at) VALUES(?,?,?,'WEBHOOK',?,?)`, connectionID, in.BankCode, accountEnvelope, receivedAt, receivedAt)
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
		if err := s.ensureSePayConnectionTx(ctx, tx, in, receivedAt); err != nil {
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
		iso := credit.TransactionAt.UTC().Format(time.RFC3339Nano)
		day := credit.TransactionAt.In(bankdate.DefaultLocation).Format("2006-01-02")
		semanticKey := "SEPAY:" + in.StoreKey + ":" + credit.Reference
		transactionID := id("txn")
		const description = "Thanh toán QR cửa hàng"
		if _, err := tx.ExecContext(ctx, `INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,description_envelope,parser_version,baseline_state,first_seen_at,transaction_at_iso,transaction_day,date_precision,ingest_source) VALUES(?,?,?,?,?,?,0,?,NULL,?,'sepay-telegram-v1','NONE',?,?,?,'datetime',?)`, transactionID, "sepay-store:"+in.StoreKey, semanticKey, hash, iso, iso, credit.AmountVND, []byte(description), receivedAt, iso, day, source); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sepay_receipts(store_key,bank_code,account_number,reference,canonical_hash,transaction_id,message_id,received_at) VALUES(?,?,?,?,?,?,?,?)`, in.StoreKey, in.BankCode, in.AccountNumber, credit.Reference, hash, transactionID, in.MessageID, receivedAt); err != nil {
			return err
		}
		if err := finish("ACCEPTED", transactionID); err != nil {
			return err
		}
		data := map[string]any{"bank": in.BankCode, "provider": "SEPAY", "transactionId": transactionID, "transactionNumber": credit.Reference, "credit": strconv.FormatInt(credit.AmountVND, 10), "debit": "0", "currency": "VND", "transactionDate": iso, "transactionDay": day, "datePrecision": "datetime", "source": source, "description": description, "detectedAt": receivedAt}
		res.Event, err = s.emitTransactionEventTx(ctx, tx, transactionID, "bank.transaction.credit", "sepay-store:"+in.StoreKey, semanticKey, data, receivedAt)
		if err != nil {
			return err
		}
		if res.Event == nil {
			return errors.New("new SePay transaction did not produce an event")
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

// LastSePayMessageAt counts only valid parsed notifications, not arbitrary
// authenticated bot chatter or errors. Silence is not an upstream health signal.
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

// SePayReview excludes encrypted evidence, sender identity and financial data.
type SePayReview struct {
	StoreKey   string `json:"storeKey"`
	MessageID  string `json:"messageId"`
	Reason     string `json:"reason"`
	ReceivedAt string `json:"receivedAt"`
}

var ErrInvalidSePayReviewCursor = errors.New("invalid SePay review cursor")

// OUTGOING, observation and pre-activation notifications are not review errors.
const sepayReviewPredicate = `reason IN ('INVALID_TEMPLATE','ACCOUNT_MISMATCH','INVALID_AMOUNT','INVALID_TRANSACTION_DATE','MISSING_REFERENCE','REFERENCE_CONFLICT','EDITED_MESSAGE')`

func (s *Store) SePayReviewCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sepay_telegram_inbox WHERE `+sepayReviewPredicate).Scan(&count)
	return count, err
}

// ListSePayReviews reads inbox decisions only. Quarantined candidates that reuse
// an existing update do not replace its first observation or expose raw evidence.
func (s *Store) ListSePayReviews(ctx context.Context, cursor string, limit int) (Page[SePayReview], error) {
	limit = normalizePageSize(limit)
	receivedAt, key, err := decodeCursor(cursor)
	if err != nil {
		return Page[SePayReview]{}, ErrInvalidSePayReviewCursor
	}
	where := sepayReviewPredicate
	args := []any{}
	if cursor != "" {
		parts := strings.Split(key, ":")
		if len(parts) != 2 {
			return Page[SePayReview]{}, ErrInvalidSePayReviewCursor
		}
		botID, botErr := strconv.ParseInt(parts[0], 10, 64)
		updateID, updateErr := strconv.ParseInt(parts[1], 10, 64)
		_, timeErr := time.Parse(time.RFC3339Nano, receivedAt)
		if botErr != nil || updateErr != nil || timeErr != nil || botID <= 0 || updateID < 0 {
			return Page[SePayReview]{}, ErrInvalidSePayReviewCursor
		}
		where += ` AND (received_at < ? OR (received_at = ? AND (bot_id < ? OR (bot_id = ? AND update_id < ?))))`
		args = append(args, receivedAt, receivedAt, botID, botID, updateID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT store_key,message_id,reason,received_at,bot_id,update_id FROM sepay_telegram_inbox WHERE `+where+` ORDER BY received_at DESC,bot_id DESC,update_id DESC LIMIT ?`, args...)
	if err != nil {
		return Page[SePayReview]{}, err
	}
	defer rows.Close()
	page := Page[SePayReview]{Items: []SePayReview{}}
	var lastBotID, lastUpdateID int64
	for rows.Next() {
		var item SePayReview
		var messageID, botID, updateID int64
		if err := rows.Scan(&item.StoreKey, &messageID, &item.Reason, &item.ReceivedAt, &botID, &updateID); err != nil {
			return Page[SePayReview]{}, err
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeCursor(last.ReceivedAt, fmt.Sprintf("%d:%d", lastBotID, lastUpdateID))
			break
		}
		item.MessageID = strconv.FormatInt(messageID, 10)
		page.Items = append(page.Items, item)
		lastBotID, lastUpdateID = botID, updateID
	}
	if err := rows.Err(); err != nil {
		return Page[SePayReview]{}, err
	}
	return page, nil
}
