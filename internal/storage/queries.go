package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
)

type TransactionView struct {
	ID             string `json:"id"`
	SemanticKey    string `json:"semanticKey"`
	TransactionAt  string `json:"transactionDate"`
	TransactionDay string `json:"transactionDay,omitempty"`
	DatePrecision  string `json:"datePrecision,omitempty"`
	EffectiveAt    string `json:"effectiveDate"`
	Debit          int64  `json:"debit"`
	Credit         int64  `json:"credit"`
	Balance        *int64 `json:"balance,omitempty"`
	Description    string `json:"description"`
	FirstSeenAt    string `json:"firstSeenAt"`
	Source         string `json:"source,omitempty"`
}

type TransactionSummary struct {
	TotalCount int64 `json:"count"`
	Incoming   int64 `json:"incoming"`
	Outgoing   int64 `json:"outgoing"`
}

type TransactionsPage struct {
	Items      []TransactionView   `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
	Summary    *TransactionSummary `json:"summary,omitempty"`
}

type TransactionFilter struct {
	From      string
	To        string
	Direction string
	Query     string
	Limit     int
	Cursor    string
}

func escapeLike(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%', '_', '\\':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

type DeliveryView struct {
	ID           string `json:"id"`
	EventID      string `json:"eventId"`
	EndpointID   string `json:"endpointId"`
	EndpointName string `json:"endpointName,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Status       string `json:"status"`
	Attempts     int    `json:"attempts"`
	NextAttempt  string `json:"nextAttemptAt"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

type AuditLogView struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	Role      string `json:"role"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	CreatedAt string `json:"createdAt"`
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

func normalizePageSize(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func encodeCursor(sortValue, id string) string {
	if sortValue == "" || id == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(sortValue + "\x00" + id))
}

func decodeCursor(cursor string) (string, string, error) {
	if cursor == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", errors.New("invalid pagination cursor")
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("invalid pagination cursor")
	}
	return parts[0], parts[1], nil
}

func (s *Store) ListTransactionsFiltered(ctx context.Context, filter TransactionFilter) (TransactionsPage, error) {
	var conditions []string
	var filterArgs []any

	if filter.From != "" {
		conditions = append(conditions, "(transaction_day >= ? OR (transaction_day = '' AND substr(first_seen_at, 1, 10) >= ?))")
		filterArgs = append(filterArgs, filter.From, filter.From)
	}
	if filter.To != "" {
		conditions = append(conditions, "(transaction_day <= ? OR (transaction_day = '' AND substr(first_seen_at, 1, 10) <= ?))")
		filterArgs = append(filterArgs, filter.To, filter.To)
	}
	if filter.Direction == "credit" {
		conditions = append(conditions, "credit > 0")
	} else if filter.Direction == "debit" {
		conditions = append(conditions, "debit > 0")
	}
	if filter.Query != "" {
		escaped := "%" + escapeLike(filter.Query) + "%"
		conditions = append(conditions, "(CAST(description_envelope AS TEXT) LIKE ? ESCAPE '\\' OR semantic_key LIKE ? ESCAPE '\\')")
		filterArgs = append(filterArgs, escaped, escaped)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + strings.Join(conditions, " AND ")
	}

	// 1. Calculate summary aggregate across the entire filtered range
	summaryQuery := `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN credit > 0 THEN credit ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN debit > 0 THEN debit ELSE 0 END), 0)
		FROM transactions` + whereClause

	var summary TransactionSummary
	if err := s.db.QueryRowContext(ctx, summaryQuery, filterArgs...).Scan(&summary.TotalCount, &summary.Incoming, &summary.Outgoing); err != nil {
		return TransactionsPage{}, err
	}

	// 2. Query page items with cursor
	limit := normalizePageSize(filter.Limit)
	sortValue, cursorID, err := decodeCursor(filter.Cursor)
	if err != nil {
		return TransactionsPage{}, err
	}

	itemConditions := make([]string, len(conditions))
	copy(itemConditions, conditions)
	itemArgs := make([]any, len(filterArgs))
	copy(itemArgs, filterArgs)

	if sortValue != "" {
		itemConditions = append(itemConditions, "(first_seen_at < ? OR (first_seen_at = ? AND id < ?))")
		itemArgs = append(itemArgs, sortValue, sortValue, cursorID)
	}

	itemWhere := ""
	if len(itemConditions) > 0 {
		itemWhere = " WHERE " + strings.Join(itemConditions, " AND ")
	}

	query := `
		SELECT
			id,
			semantic_key,
			COALESCE(NULLIF(transaction_at_iso, ''), transaction_date),
			COALESCE(transaction_day, ''),
			COALESCE(date_precision, 'unknown'),
			effective_date,
			debit,
			credit,
			balance,
			COALESCE(CAST(description_envelope AS TEXT), ''),
			first_seen_at,
			COALESCE(ingest_source, 'REALTIME')
		FROM transactions` + itemWhere + ` ORDER BY first_seen_at DESC, id DESC LIMIT ?`
	itemArgs = append(itemArgs, limit+1)

	rows, err := s.db.QueryContext(ctx, query, itemArgs...)
	if err != nil {
		return TransactionsPage{}, err
	}
	defer rows.Close()

	items := make([]TransactionView, 0, limit)
	for rows.Next() {
		var item TransactionView
		var description string
		if err := rows.Scan(
			&item.ID,
			&item.SemanticKey,
			&item.TransactionAt,
			&item.TransactionDay,
			&item.DatePrecision,
			&item.EffectiveAt,
			&item.Debit,
			&item.Credit,
			&item.Balance,
			&description,
			&item.FirstSeenAt,
			&item.Source,
		); err != nil {
			return TransactionsPage{}, err
		}
		item.Description = description
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return TransactionsPage{}, err
	}

	page := TransactionsPage{
		Items:   items,
		Summary: &summary,
	}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(last.FirstSeenAt, last.ID)
	}
	return page, nil
}

func (s *Store) ListTransactionsPage(ctx context.Context, limit int, cursor string) (Page[TransactionView], error) {
	p, err := s.ListTransactionsFiltered(ctx, TransactionFilter{Limit: limit, Cursor: cursor})
	if err != nil {
		return Page[TransactionView]{}, err
	}
	return Page[TransactionView]{
		Items:      p.Items,
		NextCursor: p.NextCursor,
	}, nil
}

func (s *Store) GetTransactionByID(ctx context.Context, id string) (*TransactionView, error) {
	query := `
		SELECT
			id,
			semantic_key,
			COALESCE(NULLIF(transaction_at_iso, ''), transaction_date),
			COALESCE(transaction_day, ''),
			COALESCE(date_precision, 'unknown'),
			effective_date,
			debit,
			credit,
			balance,
			COALESCE(CAST(description_envelope AS TEXT), ''),
			first_seen_at,
			COALESCE(ingest_source, 'REALTIME')
		FROM transactions
		WHERE id = ?`

	var item TransactionView
	var description string
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&item.ID,
		&item.SemanticKey,
		&item.TransactionAt,
		&item.TransactionDay,
		&item.DatePrecision,
		&item.EffectiveAt,
		&item.Debit,
		&item.Credit,
		&item.Balance,
		&description,
		&item.FirstSeenAt,
		&item.Source,
	)
	if err != nil {
		return nil, err
	}
	item.Description = description
	return &item, nil
}

func (s *Store) ListDeliveriesPage(ctx context.Context, limit int, cursor string) (Page[DeliveryView], error) {
	limit = normalizePageSize(limit)
	sortValue, cursorID, err := decodeCursor(cursor)
	if err != nil {
		return Page[DeliveryView]{}, err
	}
	query := `SELECT d.id, d.event_id, d.endpoint_id, COALESCE(e.name, ''), COALESCE(e.provider, 'WEBHOOK'), d.status, d.attempts, d.next_attempt_at, d.created_at, d.updated_at
	          FROM deliveries d
	          LEFT JOIN webhook_endpoints e ON e.id = d.endpoint_id`
	args := []any{}
	if sortValue != "" {
		query += ` WHERE d.created_at < ? OR (d.created_at = ? AND d.id < ?)`
		args = append(args, sortValue, sortValue, cursorID)
	}
	query += ` ORDER BY d.created_at DESC, d.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[DeliveryView]{}, err
	}
	defer rows.Close()
	items := make([]DeliveryView, 0, limit)
	for rows.Next() {
		var item DeliveryView
		if err := rows.Scan(&item.ID, &item.EventID, &item.EndpointID, &item.EndpointName, &item.Provider, &item.Status, &item.Attempts, &item.NextAttempt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return Page[DeliveryView]{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[DeliveryView]{}, err
	}
	page := Page[DeliveryView]{Items: items}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

func (s *Store) ListPollRunsPage(ctx context.Context, limit int, cursor string) (Page[PollRun], error) {
	limit = normalizePageSize(limit)
	sortValue, cursorID, err := decodeCursor(cursor)
	if err != nil {
		return Page[PollRun]{}, err
	}
	query := `SELECT id,connection_id,generation,status,COALESCE(classifier,''),COALESCE(http_status,0),pages,rows_seen,COALESCE(sanitized_error,''),started_at,COALESCE(finished_at,'') FROM poll_runs`
	args := []any{}
	if sortValue != "" {
		query += ` WHERE started_at < ? OR (started_at = ? AND id < ?)`
		args = append(args, sortValue, sortValue, cursorID)
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[PollRun]{}, err
	}
	defer rows.Close()
	items := make([]PollRun, 0, limit)
	for rows.Next() {
		var item PollRun
		if err := rows.Scan(&item.ID, &item.ConnectionID, &item.Generation, &item.Status, &item.Classifier, &item.HTTPStatus, &item.Pages, &item.RowsSeen, &item.Error, &item.StartedAt, &item.FinishedAt); err != nil {
			return Page[PollRun]{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[PollRun]{}, err
	}
	page := Page[PollRun]{Items: items}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(last.StartedAt, last.ID)
	}
	return page, nil
}

func (s *Store) ListAuditLogsPage(ctx context.Context, limit int, cursor string) (Page[AuditLogView], error) {
	limit = normalizePageSize(limit)
	sortValue, cursorID, err := decodeCursor(cursor)
	if err != nil {
		return Page[AuditLogView]{}, err
	}
	query := `SELECT id,COALESCE(actor_subject,''),COALESCE(actor_role,''),action,target,created_at FROM audit_logs`
	args := []any{}
	if sortValue != "" {
		query += ` WHERE created_at < ? OR (created_at = ? AND id < ?)`
		args = append(args, sortValue, sortValue, cursorID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[AuditLogView]{}, err
	}
	defer rows.Close()
	items := make([]AuditLogView, 0, limit)
	for rows.Next() {
		var item AuditLogView
		if err := rows.Scan(&item.ID, &item.Subject, &item.Role, &item.Action, &item.Target, &item.CreatedAt); err != nil {
			return Page[AuditLogView]{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[AuditLogView]{}, err
	}
	page := Page[AuditLogView]{Items: items}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

func (s *Store) ListTransactions(ctx context.Context, limit int) ([]TransactionView, error) {
	page, err := s.ListTransactionsPage(ctx, limit, "")
	return page.Items, err
}

func (s *Store) ListDeliveries(ctx context.Context, limit int) ([]DeliveryView, error) {
	page, err := s.ListDeliveriesPage(ctx, limit, "")
	return page.Items, err
}

func (s *Store) ListPollRuns(ctx context.Context, limit int) ([]PollRun, error) {
	page, err := s.ListPollRunsPage(ctx, limit, "")
	return page.Items, err
}

func (s *Store) ListAuditLogs(ctx context.Context, limit int) ([]AuditLogView, error) {
	page, err := s.ListAuditLogsPage(ctx, limit, "")
	return page.Items, err
}

// CompleteAuthSession saves the verified session and transitions the connection to MONITORING.
func (s *Store) CompleteAuthSession(ctx context.Context, attemptID string, sessionEnvelope []byte) (Connection, error) {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var connectionID string
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_id,generation FROM auth_attempts WHERE id=? AND status IN ('STARTING','IN_PROGRESS')`, attemptID).Scan(&connectionID, &generation); err != nil {
			return err
		}
		nowString := now()
		if _, err := tx.ExecContext(ctx, `UPDATE auth_attempts SET status='VERIFIED',finished_at=? WHERE id=?`, nowString, attemptID); err != nil {
			return err
		}
		var hasBaseline bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM transactions WHERE connection_id = ?
				UNION ALL
				SELECT 1 FROM poll_runs WHERE connection_id = ? AND status = 'SUCCEEDED'
				UNION ALL
				SELECT 1 FROM recovery_runs WHERE connection_id = ? AND status = 'COMPLETED'
				UNION ALL
				SELECT 1 FROM history_coverage WHERE connection_id = ? AND status = 'COMPLETE'
			)
		`, connectionID, connectionID, connectionID, connectionID).Scan(&hasBaseline); err != nil {
			return err
		}
		recoveryReason := RecoveryReasonInitialAuth
		if hasBaseline {
			recoveryReason = RecoveryReasonReauth
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO recovery_runs(id, connection_id, generation, event_key, reason, status, progress_json, created_at, updated_at)
			VALUES(?,?,?,?, ?, 'PENDING', '{}', ?, ?)
			ON CONFLICT(connection_id, generation, event_key) DO NOTHING
		`, id("recovery"), connectionID, generation, attemptID, recoveryReason, nowString, nowString); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,?,?,'k1',?,?) ON CONFLICT(connection_id) DO UPDATE SET generation=excluded.generation,envelope=excluded.envelope,verified_at=excluded.verified_at,updated_at=excluded.updated_at`, connectionID, generation, sessionEnvelope, nowString, nowString); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE connections SET state='MONITORING',updated_at=? WHERE id=? AND generation=? AND state='AUTH_STARTING'`, nowString, connectionID, generation)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrGenerationFenceMismatch
		}
		return nil
	})
	if err != nil {
		return Connection{}, err
	}
	return s.Connection(ctx)
}
