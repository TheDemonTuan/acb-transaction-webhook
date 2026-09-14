package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
)

type TransactionInput struct {
	ConnectionID  string
	SemanticKey   string
	CanonicalHash string
	TransactionAt string
	EffectiveAt   string
	Debit         int64
	Credit        int64
	Balance       *int64
	Description   []byte
	ParserVersion string
}

type IngestResult struct {
	TransactionID string
	Inserted      bool
	Conflict      bool
}

type BatchTransactionItem struct {
	Number        string
	Credit        int64
	Debit         int64
	Balance       *int64
	TransactionAt string
	EffectiveAt   string
	Description   string
}

type EventNotification struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	TransactionID string `json:"transactionId"`
	Payload       []byte `json:"payload"`
	CreatedAt     string `json:"createdAt"`
	CommittedAt   string `json:"committedAt,omitempty"`
	JournalSeq    int64  `json:"journalSeq"`
	Epoch         string `json:"epoch"`
}

type BatchIngestResult struct {
	InsertedCount int
	SkippedCount  int
	ConflictCount int
	NewEvents     []EventNotification
}

var ErrGenerationFenceMismatch = errors.New("generation fence mismatch")

// IngestTransaction preserves the first observation. A distinct payload for an
// existing semantic key is quarantined instead of silently replacing money data.
func (s *Store) IngestTransaction(ctx context.Context, in TransactionInput) (IngestResult, error) {
	if in.ConnectionID == "" || in.SemanticKey == "" || in.CanonicalHash == "" || in.TransactionAt == "" || in.EffectiveAt == "" || in.ParserVersion == "" {
		return IngestResult{}, errors.New("incomplete transaction")
	}
	result := IngestResult{}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var existingID, existingHash string
		err := tx.QueryRowContext(ctx, `SELECT id,canonical_hash FROM transactions WHERE connection_id=? AND semantic_key=?`, in.ConnectionID, in.SemanticKey).Scan(&existingID, &existingHash)
		if err == nil {
			result.TransactionID = existingID
			if existingHash != in.CanonicalHash {
				result.Conflict = true
				candidate := in.Description
				if candidate == nil {
					candidate = []byte{}
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO transaction_quarantine(id,connection_id,semantic_key,existing_hash,candidate_envelope,reason,created_at) VALUES(?,?,?,?,?,?,?)`, id("quarantine"), in.ConnectionID, in.SemanticKey, existingHash, candidate, "CANONICAL_HASH_MISMATCH", now())
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		norm, normErr := acb.NormalizeDate(in.TransactionAt, nil)
		iso := in.TransactionAt
		day := ""
		precision := "unknown"
		if normErr == nil {
			iso = norm.TransactionAtISO
			day = norm.TransactionDay
			precision = norm.DatePrecision
		}
		result.TransactionID = id("txn")
		_, err = tx.ExecContext(ctx, `INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,description_envelope,parser_version,first_seen_at,transaction_at_iso,transaction_day,date_precision,ingest_source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			result.TransactionID, in.ConnectionID, in.SemanticKey, in.CanonicalHash, in.TransactionAt, in.EffectiveAt, in.Debit, in.Credit, in.Balance, in.Description, in.ParserVersion, now(), iso, day, precision, "REALTIME")
		if err != nil {
			return err
		}
		result.Inserted = true
		return nil
	})
	return result, err
}

// IngestTransactionsBatch atomically inserts transactions, events, deliveries, and journal entries
// in a single SQLite transaction guarded by a generation fence with default REALTIME source.
func (s *Store) IngestTransactionsBatch(ctx context.Context, connectionID string, expectedGeneration int64, accountMasked string, items []BatchTransactionItem, isBaseline bool) (BatchIngestResult, error) {
	return s.IngestTransactionsBatchWithSource(ctx, connectionID, expectedGeneration, accountMasked, items, isBaseline, "REALTIME")
}

// IngestTransactionsBatchWithSource atomically inserts transactions with explicit source policy
// (REALTIME, CATCH_UP, FILTER_SYNC, BOOTSTRAP).
func (s *Store) IngestTransactionsBatchWithSource(ctx context.Context, connectionID string, expectedGeneration int64, accountMasked string, items []BatchTransactionItem, isBaseline bool, source string) (BatchIngestResult, error) {
	if connectionID == "" {
		return BatchIngestResult{}, errors.New("connection ID is required")
	}
	if source == "" {
		source = "REALTIME"
	}

	res := BatchIngestResult{}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// 1. Generation fence
		var gen int64
		var state string
		err := tx.QueryRowContext(ctx, `SELECT generation, state FROM connections WHERE id = ?`, connectionID).Scan(&gen, &state)
		if err != nil {
			return fmt.Errorf("check generation fence: %w", err)
		}
		if gen != expectedGeneration || state != "MONITORING" {
			return fmt.Errorf("%w: expected gen %d in state MONITORING, got gen %d in state %s", ErrGenerationFenceMismatch, expectedGeneration, gen, state)
		}

		// 2. Query active endpoints for delivery creation
		type endpointRef struct {
			id       string
			revision int
			provider string
			keyID    string
		}
		var activeEndpoints []endpointRef
		rows, err := tx.QueryContext(ctx, `
			SELECT e.id, e.current_revision, COALESCE(e.provider, 'WEBHOOK'), COALESCE(s.key_id, 'k1')
			FROM webhook_endpoints e
			LEFT JOIN endpoint_secrets s ON s.endpoint_id = e.id AND s.status = 'ACTIVE'
			WHERE e.status = 'ACTIVE'
		`)
		if err != nil {
			return fmt.Errorf("query active endpoints: %w", err)
		}
		for rows.Next() {
			var ep endpointRef
			if err := rows.Scan(&ep.id, &ep.revision, &ep.provider, &ep.keyID); err == nil {
				activeEndpoints = append(activeEndpoints, ep)
			}
		}
		_ = rows.Close()

		// 3. Process items atomically
		for _, item := range items {
			if item.Number == "" || item.TransactionAt == "" {
				continue
			}
			semanticKey := fmt.Sprintf("ACB:%s", item.Number)
			canonicalHash := fmt.Sprintf("%s:%d:%d:%s", item.Number, item.Credit, item.Debit, item.TransactionAt)

			var existingID, existingHash string
			err := tx.QueryRowContext(ctx, `SELECT id, canonical_hash FROM transactions WHERE connection_id = ? AND semantic_key = ?`, connectionID, semanticKey).Scan(&existingID, &existingHash)
			if err == nil {
				if existingHash != canonicalHash {
					res.ConflictCount++
					_, _ = tx.ExecContext(ctx, `INSERT INTO transaction_quarantine(id, connection_id, semantic_key, existing_hash, candidate_envelope, reason, created_at) VALUES(?, ?, ?, ?, ?, 'CANONICAL_HASH_MISMATCH', ?)`, id("quarantine"), connectionID, semanticKey, existingHash, []byte(item.Description), now())
				}
				res.SkippedCount++
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			norm, normErr := acb.NormalizeDate(item.TransactionAt, nil)
			iso := item.TransactionAt
			day := ""
			precision := "unknown"
			if normErr == nil {
				iso = norm.TransactionAtISO
				day = norm.TransactionDay
				precision = norm.DatePrecision
			}

			txnID := id("txn")
			baselineState := "NONE"
			if isBaseline {
				baselineState = "BASELINE"
			}
			nowTime := now()
			_, err = tx.ExecContext(ctx, `INSERT INTO transactions(id, connection_id, semantic_key, canonical_hash, transaction_date, effective_date, debit, credit, balance, description_envelope, parser_version, baseline_state, first_seen_at, transaction_at_iso, transaction_day, date_precision, ingest_source) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'v1', ?, ?, ?, ?, ?, ?)`,
				txnID, connectionID, semanticKey, canonicalHash, item.TransactionAt, item.EffectiveAt, item.Debit, item.Credit, item.Balance, []byte(item.Description), baselineState, nowTime, iso, day, precision, source)
			if err != nil {
				return fmt.Errorf("insert transaction: %w", err)
			}
			res.InsertedCount++

			// Notification & delivery policy:
			// FILTER_SYNC and BOOTSTRAP: suppressed webhooks and voice events.
			// CATCH_UP: enqueues webhooks for newly found credit transactions, but source is CATCH_UP.
			// REALTIME: enqueues webhooks and emits voice-eligible credit events.
			emitCreditEvent := !isBaseline && item.Credit > 0 && source != "FILTER_SYNC" && source != "BOOTSTRAP"
			deliverWebhooks := emitCreditEvent && (source == "REALTIME" || source == "CATCH_UP")

			if emitCreditEvent {
				sum := sha256.Sum256([]byte("acb" + "\x00" + semanticKey + "\x00" + "bank.transaction.credit"))
				eventID := "bevt_" + hex.EncodeToString(sum[:16])
				payloadHash := hex.EncodeToString(sum[:])

				eventData := map[string]any{
					"bank":              "ACB",
					"accountMasked":     accountMasked,
					"transactionId":     txnID,
					"transactionNumber": item.Number,
					"credit":            fmt.Sprintf("%d", item.Credit),
					"debit":             fmt.Sprintf("%d", item.Debit),
					"currency":          "VND",
					"transactionDate":   iso,
					"transactionDay":    day,
					"datePrecision":     precision,
					"source":            source,
					"description":       item.Description,
					"detectedAt":        nowTime,
				}
				if item.Balance != nil {
					eventData["balance"] = fmt.Sprintf("%d", *item.Balance)
				}
				dataBytes, err := json.Marshal(eventData)
				if err != nil {
					return fmt.Errorf("marshal event payload: %w", err)
				}

				_, err = tx.ExecContext(ctx, `
					INSERT INTO events(id, transaction_id, event_type, payload, payload_hash, created_at)
					VALUES(?, ?, 'bank.transaction.credit', ?, ?, ?)
					ON CONFLICT(transaction_id, event_type) DO NOTHING
				`, eventID, txnID, dataBytes, payloadHash, nowTime)
				if err != nil {
					return fmt.Errorf("insert event: %w", err)
				}

				if deliverWebhooks {
					for _, ep := range activeEndpoints {
						deliveryID := id("deliv")
						_, err = tx.ExecContext(ctx, `
							INSERT INTO deliveries(id, event_id, endpoint_id, endpoint_revision, key_id, status, attempts, next_attempt_at, created_at, updated_at)
							VALUES(?, ?, ?, ?, ?, 'PENDING', 0, ?, ?, ?)
							ON CONFLICT(event_id, endpoint_id) DO NOTHING
						`, deliveryID, eventID, ep.id, ep.revision, ep.keyID, nowTime, nowTime, nowTime)
						if err != nil {
							return fmt.Errorf("insert delivery: %w", err)
						}
					}
				}

				// Insert into event_journal
				resJournal, err := tx.ExecContext(ctx, `
					INSERT INTO event_journal(epoch, event_type, aggregate_id, payload_json, created_at)
					VALUES('ep1', 'bank.transaction.credit', ?, ?, ?)
				`, txnID, string(dataBytes), nowTime)
				if err != nil {
					return fmt.Errorf("insert event journal: %w", err)
				}
				seqID, err := resJournal.LastInsertId()
				if err != nil {
					return fmt.Errorf("read event journal sequence: %w", err)
				}

				res.NewEvents = append(res.NewEvents, EventNotification{
					EventID:       eventID,
					EventType:     "bank.transaction.credit",
					TransactionID: txnID,
					Payload:       dataBytes,
					CreatedAt:     nowTime,
					JournalSeq:    seqID,
					Epoch:         "ep1",
				})
			}
		}
		return nil
	})
	if err == nil && len(res.NewEvents) > 0 {
		committedAt := time.Now().UTC().Format(time.RFC3339Nano)
		for i := range res.NewEvents {
			res.NewEvents[i].CommittedAt = committedAt
		}
	}

	return res, err
}

// BackfillCanonicalDates updates any transactions missing canonical day/ISO representation.
func (s *Store) BackfillCanonicalDates(ctx context.Context) (int, error) {
	totalUpdated := 0
	for {
		type rowToUpdate struct {
			id  string
			raw string
		}
		var batch []rowToUpdate

		rows, err := s.db.QueryContext(ctx, `
			SELECT id, transaction_date
			FROM transactions
			WHERE (transaction_day IS NULL OR transaction_day = '')
			  AND (date_precision IS NULL OR date_precision != 'unknown')
			LIMIT 200
		`)
		if err != nil {
			return totalUpdated, err
		}
		for rows.Next() {
			var r rowToUpdate
			if err := rows.Scan(&r.id, &r.raw); err == nil {
				batch = append(batch, r)
			}
		}
		_ = rows.Close()

		if len(batch) == 0 {
			break
		}

		err = s.withTx(ctx, func(tx *sql.Tx) error {
			stmt, err := tx.PrepareContext(ctx, `
				UPDATE transactions
				SET transaction_at_iso = ?, transaction_day = ?, date_precision = ?
				WHERE id = ?
			`)
			if err != nil {
				return err
			}
			defer stmt.Close()

			for _, r := range batch {
				norm, err := acb.NormalizeDate(r.raw, nil)
				if err == nil {
					_, _ = stmt.ExecContext(ctx, norm.TransactionAtISO, norm.TransactionDay, norm.DatePrecision, r.id)
					totalUpdated++
				} else {
					_, _ = stmt.ExecContext(ctx, r.raw, "", "unknown", r.id)
				}
			}
			return nil
		})
		if err != nil {
			return totalUpdated, err
		}
	}
	return totalUpdated, nil
}
