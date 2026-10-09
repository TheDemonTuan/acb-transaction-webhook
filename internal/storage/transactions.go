package storage

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bankdate"
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
		norm, normErr := bankdate.NormalizeDate(in.TransactionAt, nil)
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
				norm, err := bankdate.NormalizeDate(r.raw, nil)
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
