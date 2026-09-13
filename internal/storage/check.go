package storage

import (
	"context"
	"database/sql"
	"fmt"
)

type DeliveryStats struct {
	Pending    int `json:"pending"`
	InFlight   int `json:"inFlight"`
	Delivered  int `json:"delivered"`
	DeadLetter int `json:"deadLetter"`
}

type IntegrityReport struct {
	IntegrityOK        bool          `json:"integrityOk"`
	IntegrityMessage   string        `json:"integrityMessage"`
	MigrationsApplied  int           `json:"migrationsApplied"`
	ConnectionsCount   int           `json:"connectionsCount"`
	ConnectionState    string        `json:"connectionState,omitempty"`
	Generation         int64         `json:"generation"`
	TransactionsCount  int           `json:"transactionsCount"`
	EventsCount        int           `json:"eventsCount"`
	Deliveries         DeliveryStats `json:"deliveries"`
	EndpointsCount     int           `json:"endpointsCount"`
	ActiveEndpoints    int           `json:"activeEndpoints"`
	QuarantinedCount   int           `json:"quarantinedCount"`
	OrphanTransactions int           `json:"orphanTransactions"`
}

func (s *Store) CheckIntegrity(ctx context.Context) (IntegrityReport, error) {
	var rep IntegrityReport

	// 1. PRAGMA integrity_check
	var checkResult string
	err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&checkResult)
	if err != nil {
		return rep, fmt.Errorf("pragma integrity_check: %w", err)
	}
	rep.IntegrityMessage = checkResult
	rep.IntegrityOK = (checkResult == "ok")

	// 2. Migrations count
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rep.MigrationsApplied)

	// 3. Connections info
	var state sql.NullString
	var gen sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT count(*), max(state), max(generation) FROM connections`).Scan(&rep.ConnectionsCount, &state, &gen)
	if err == nil {
		if state.Valid {
			rep.ConnectionState = state.String
		}
		if gen.Valid {
			rep.Generation = gen.Int64
		}
	}

	// 4. Transactions, events, endpoints, quarantine count
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM transactions`).Scan(&rep.TransactionsCount)
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&rep.EventsCount)
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_endpoints`).Scan(&rep.EndpointsCount)
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_endpoints WHERE status = 'ACTIVE'`).Scan(&rep.ActiveEndpoints)
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM transaction_quarantine`).Scan(&rep.QuarantinedCount)

	// 5. Deliveries breakdown
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM deliveries GROUP BY status`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var st string
			var count int
			if err := rows.Scan(&st, &count); err == nil {
				switch st {
				case "PENDING":
					rep.Deliveries.Pending = count
				case "IN_FLIGHT":
					rep.Deliveries.InFlight = count
				case "DELIVERED":
					rep.Deliveries.Delivered = count
				case "DEAD_LETTER":
					rep.Deliveries.DeadLetter = count
				}
			}
		}
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM transactions t
		WHERE t.credit > 0 AND t.baseline_state = 'NONE'
		  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.transaction_id = t.id)
	`).Scan(&rep.OrphanTransactions); err != nil {
		return rep, fmt.Errorf("check orphan transactions: %w", err)
	}

	return rep, nil
}

type SchemaVersionReport struct {
	Version       int    `json:"version"`
	SchemaVersion int    `json:"schemaVersion"`
	Checksum      string `json:"checksum,omitempty"`
	AppliedAt     string `json:"appliedAt,omitempty"`
	AppliedCount  int    `json:"appliedCount"`
}

func (s *Store) SchemaVersion(ctx context.Context) (SchemaVersionReport, error) {
	var rep SchemaVersionReport

	var tableExists int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tableExists)
	if err != nil {
		return rep, fmt.Errorf("check schema_migrations table: %w", err)
	}
	if tableExists == 0 {
		return rep, nil
	}

	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rep.AppliedCount)
	if err != nil {
		return rep, fmt.Errorf("count schema_migrations: %w", err)
	}
	if rep.AppliedCount == 0 {
		return rep, nil
	}

	var version int
	var checksum, appliedAt string
	err = s.db.QueryRowContext(ctx, `SELECT version, checksum, applied_at FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &checksum, &appliedAt)
	if err != nil {
		return rep, fmt.Errorf("query schema_migrations version: %w", err)
	}
	rep.Version = version
	rep.SchemaVersion = version
	rep.Checksum = checksum
	rep.AppliedAt = appliedAt
	return rep, nil
}
