package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type JournalEntry struct {
	Seq         int64  `json:"seq"`
	Epoch       string `json:"epoch"`
	EventType   string `json:"eventType"`
	AggregateID string `json:"aggregateId"`
	Payload     []byte `json:"payload"`
	CreatedAt   string `json:"createdAt"`
}

// PollCompletedPayload builds the JSON payload for a poll.completed journal event.
func PollCompletedPayload(p PollRun, insertedCount int) ([]byte, error) {
	return json.Marshal(map[string]any{
		"pollId":        p.ID,
		"status":        p.Status,
		"classifier":    p.Classifier,
		"httpStatus":    p.HTTPStatus,
		"pages":         p.Pages,
		"rowsSeen":      p.RowsSeen,
		"insertedCount": insertedCount,
		"error":         p.Error,
		"startedAt":     p.StartedAt,
		"finishedAt":    p.FinishedAt,
	})
}

// AppendJournalEvent records a new event into the ordered event journal.
func (s *Store) AppendJournalEvent(ctx context.Context, epoch, eventType, aggregateID string, payload []byte) (int64, error) {
	if epoch == "" {
		epoch = "ep1"
	}
	createdAt := now()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO event_journal(epoch, event_type, aggregate_id, payload_json, created_at)
		VALUES(?, ?, ?, ?, ?)
	`, epoch, eventType, aggregateID, string(payload), createdAt)
	if err != nil {
		return 0, fmt.Errorf("append journal event: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// ReadJournalEvents fetches events ordered by seq strictly after afterSeq.
func (s *Store) ReadJournalEvents(ctx context.Context, epoch string, afterSeq int64, limit int) ([]JournalEntry, error) {
	if epoch == "" {
		epoch = "ep1"
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, epoch, event_type, aggregate_id, payload_json, created_at
		FROM event_journal
		WHERE epoch = ? AND seq > ?
		ORDER BY seq ASC
		LIMIT ?
	`, epoch, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("query journal events: %w", err)
	}
	defer rows.Close()

	var entries []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var payloadStr string
		if err := rows.Scan(&e.Seq, &e.Epoch, &e.EventType, &e.AggregateID, &payloadStr, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Payload = []byte(payloadStr)
		entries = append(entries, e)
	}
	return entries, nil
}

// GetMaxJournalSeq returns the highest sequence number in the given epoch.
func (s *Store) GetMaxJournalSeq(ctx context.Context, epoch string) (int64, error) {
	if epoch == "" {
		epoch = "ep1"
	}
	var maxSeq sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max(seq) FROM event_journal WHERE epoch = ?`, epoch).Scan(&maxSeq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if !maxSeq.Valid {
		return 0, nil
	}
	return maxSeq.Int64, nil
}

// GetMinJournalSeq returns the lowest sequence number in the given epoch.
func (s *Store) DeleteJournalBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM event_journal WHERE created_at < ?`, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("delete expired journal events: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) GetMinJournalSeq(ctx context.Context, epoch string) (int64, error) {
	if epoch == "" {
		epoch = "ep1"
	}
	var minSeq sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT min(seq) FROM event_journal WHERE epoch = ?`, epoch).Scan(&minSeq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if !minSeq.Valid {
		return 0, nil
	}
	return minSeq.Int64, nil
}
