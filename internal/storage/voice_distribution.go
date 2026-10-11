package storage

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

type VoiceAmountCount struct {
	AmountVND string `json:"amountVnd"`
	Count     int64  `json:"count"`
}

type VoiceAmountDistribution struct {
	SchemaVersion    int                `json:"schemaVersion"`
	From             string             `json:"from"`
	To               string             `json:"to"`
	TotalEligible    int64              `json:"totalEligible"`
	ReturnedCount    int                `json:"returnedCount"`
	Truncated        bool               `json:"truncated"`
	InvalidTimeCount int64              `json:"invalidTimeCount"`
	Amounts          []VoiceAmountCount `json:"amounts"`
}

func (s *Store) ReadVoiceAmountDistribution(ctx context.Context, from, to time.Time) (VoiceAmountDistribution, error) {
	fromStr := from.UTC().Format(time.RFC3339)
	toStr := to.UTC().Format(time.RFC3339)

	dist := VoiceAmountDistribution{
		SchemaVersion: 1,
		From:          fromStr,
		To:            toStr,
		Amounts:       []VoiceAmountCount{},
	}

	// 1. Check for unparsable timestamps in eligible transactions
	var invalidTimeCount int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM transactions
		WHERE credit > 0
		  AND credit <= 9007199254740991
		  AND debit = 0
		  AND ingest_source = 'REALTIME'
		  AND parser_version IN ('payos-v1', 'sepay-telegram-v1', 'sepay-native-v1')
		  AND julianday(first_seen_at) IS NULL
	`).Scan(&invalidTimeCount)
	if err != nil {
		return dist, fmt.Errorf("query invalid first_seen_at count: %w", err)
	}
	dist.InvalidTimeCount = invalidTimeCount

	// 2. Query total distinct count to determine truncation
	var distinctCount int64
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT credit)
		FROM transactions
		WHERE credit > 0
		  AND credit <= 9007199254740991
		  AND debit = 0
		  AND ingest_source = 'REALTIME'
		  AND parser_version IN ('payos-v1', 'sepay-telegram-v1', 'sepay-native-v1')
		  AND julianday(first_seen_at) >= julianday(?)
		  AND julianday(first_seen_at) < julianday(?)
	`, fromStr, toStr).Scan(&distinctCount)
	if err != nil {
		return dist, fmt.Errorf("query distinct eligible amounts count: %w", err)
	}
	dist.Truncated = distinctCount > 10000

	// 3. Query grouped distribution with window total before LIMIT 10000
	rows, err := s.db.QueryContext(ctx, `
		WITH filtered AS (
			SELECT credit
			FROM transactions
			WHERE credit > 0
			  AND credit <= 9007199254740991
			  AND debit = 0
			  AND ingest_source = 'REALTIME'
			  AND parser_version IN ('payos-v1', 'sepay-telegram-v1', 'sepay-native-v1')
			  AND julianday(first_seen_at) >= julianday(?)
			  AND julianday(first_seen_at) < julianday(?)
		),
		grouped AS (
			SELECT credit, COUNT(*) AS cnt
			FROM filtered
			GROUP BY credit
		)
		SELECT credit, cnt, SUM(cnt) OVER () AS total_eligible
		FROM grouped
		ORDER BY cnt DESC, credit ASC
		LIMIT 10000
	`, fromStr, toStr)
	if err != nil {
		return dist, fmt.Errorf("query voice amount distribution: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			credit        int64
			count         int64
			totalEligible int64
		)
		if err := rows.Scan(&credit, &count, &totalEligible); err != nil {
			return dist, fmt.Errorf("scan voice amount distribution row: %w", err)
		}
		dist.TotalEligible = totalEligible
		dist.Amounts = append(dist.Amounts, VoiceAmountCount{
			AmountVND: strconv.FormatInt(credit, 10),
			Count:     count,
		})
	}
	if err := rows.Err(); err != nil {
		return dist, fmt.Errorf("iterate voice amount distribution: %w", err)
	}

	dist.ReturnedCount = len(dist.Amounts)
	return dist, nil
}
