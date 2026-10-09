package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// Historical records are seeded explicitly, not through a retired bank runtime.
// This fixture is test-only; new payments always use atomic payOS settlement.
type historicalTransactionFixture struct {
	Number        string
	Credit        int64
	Debit         int64
	Balance       *int64
	TransactionAt string
	EffectiveAt   string
	Description   string
}

type historicalFixtureResult struct {
	NewEvents []storage.EventNotification
}

func seedHistoricalTransactions(ctx context.Context, store *storage.Store, connectionID, accountMasked string, items []historicalTransactionFixture, source string) (historicalFixtureResult, error) {
	var result historicalFixtureResult
	for _, item := range items {
		canonical, err := json.Marshal(item)
		if err != nil {
			return result, err
		}
		hash := sha256.Sum256(canonical)
		semanticKey := "ACB:" + item.Number
		ingested, err := store.IngestTransaction(ctx, storage.TransactionInput{
			ConnectionID: connectionID, SemanticKey: semanticKey, CanonicalHash: hex.EncodeToString(hash[:]),
			TransactionAt: item.TransactionAt, EffectiveAt: item.EffectiveAt,
			Debit: item.Debit, Credit: item.Credit, Balance: item.Balance,
			Description: []byte(item.Description), ParserVersion: "historical-fixture",
		})
		if err != nil {
			return result, err
		}
		if _, err := store.DB().ExecContext(ctx, `UPDATE transactions SET ingest_source=? WHERE id=?`, source, ingested.TransactionID); err != nil {
			return result, err
		}
		if !ingested.Inserted || item.Credit <= 0 {
			continue
		}
		createdAt := time.Now().UTC().Format(time.RFC3339Nano)
		payload := map[string]any{
			"bank": "ACB", "accountMasked": accountMasked, "transactionId": ingested.TransactionID,
			"transactionNumber": item.Number, "credit": strconv.FormatInt(item.Credit, 10), "debit": strconv.FormatInt(item.Debit, 10),
			"currency": "VND", "transactionDate": item.TransactionAt, "description": item.Description,
			"source": source, "detectedAt": createdAt,
		}
		eventID, err := store.EmitTransactionEvent(ctx, ingested.TransactionID, "bank.transaction.credit", "ACB", semanticKey, payload)
		if err != nil {
			return result, err
		}
		encoded, err := store.EventPayload(ctx, eventID)
		if err != nil {
			return result, err
		}
		var seq int64
		if err := store.DB().QueryRowContext(ctx, `SELECT seq FROM event_journal WHERE aggregate_id=? AND event_type='bank.transaction.credit'`, ingested.TransactionID).Scan(&seq); err != nil {
			return result, err
		}
		result.NewEvents = append(result.NewEvents, storage.EventNotification{EventID: eventID, EventType: "bank.transaction.credit", TransactionID: ingested.TransactionID, Payload: encoded, CreatedAt: createdAt, JournalSeq: seq, Epoch: "ep1"})
	}
	return result, nil
}
