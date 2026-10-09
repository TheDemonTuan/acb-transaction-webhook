package storage

import (
	"context"
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
	Bank           string `json:"bank"`
	Provider       string `json:"provider,omitempty"`
	OrderCode      string `json:"orderCode,omitempty"`
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
		conditions = append(conditions, "(t.transaction_day >= ? OR (t.transaction_day = '' AND substr(t.first_seen_at, 1, 10) >= ?))")
		filterArgs = append(filterArgs, filter.From, filter.From)
	}
	if filter.To != "" {
		conditions = append(conditions, "(t.transaction_day <= ? OR (t.transaction_day = '' AND substr(t.first_seen_at, 1, 10) <= ?))")
		filterArgs = append(filterArgs, filter.To, filter.To)
	}
	if filter.Direction == "credit" {
		conditions = append(conditions, "t.credit > 0")
	} else if filter.Direction == "debit" {
		conditions = append(conditions, "t.debit > 0")
	}
	if filter.Query != "" {
		escaped := "%" + escapeLike(filter.Query) + "%"
		conditions = append(conditions, "(CAST(t.description_envelope AS TEXT) LIKE ? ESCAPE '\\' OR t.semantic_key LIKE ? ESCAPE '\\')")
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
		FROM transactions t` + whereClause

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
		itemConditions = append(itemConditions, "(t.first_seen_at < ? OR (t.first_seen_at = ? AND t.id < ?))")
		itemArgs = append(itemArgs, sortValue, sortValue, cursorID)
	}

	itemWhere := ""
	if len(itemConditions) > 0 {
		itemWhere = " WHERE " + strings.Join(itemConditions, " AND ")
	}

	query := `
		SELECT
			t.id,
			t.semantic_key,
			COALESCE(NULLIF(t.transaction_at_iso, ''), t.transaction_date),
			COALESCE(t.transaction_day, ''),
			COALESCE(t.date_precision, 'unknown'),
			t.effective_date,
			t.debit,
			t.credit,
			t.balance,
			COALESCE(CAST(t.description_envelope AS TEXT), ''),
			t.first_seen_at,
			COALESCE(t.ingest_source, 'REALTIME'),
			c.bank_code,
			CASE WHEN po.id IS NOT NULL THEN 'PAYOS' ELSE '' END,
			COALESCE(CAST(po.order_code AS TEXT), '')
		FROM transactions t
		JOIN connections c ON c.id = t.connection_id
		LEFT JOIN payment_orders po ON po.transaction_id = t.id` + itemWhere + ` ORDER BY t.first_seen_at DESC, t.id DESC LIMIT ?`
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
			&item.Bank,
			&item.Provider,
			&item.OrderCode,
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
			t.id,
			t.semantic_key,
			COALESCE(NULLIF(t.transaction_at_iso, ''), t.transaction_date),
			COALESCE(t.transaction_day, ''),
			COALESCE(t.date_precision, 'unknown'),
			t.effective_date,
			t.debit,
			t.credit,
			t.balance,
			COALESCE(CAST(t.description_envelope AS TEXT), ''),
			t.first_seen_at,
			COALESCE(t.ingest_source, 'REALTIME'),
			c.bank_code,
			CASE WHEN po.id IS NOT NULL THEN 'PAYOS' ELSE '' END,
			COALESCE(CAST(po.order_code AS TEXT), '')
		FROM transactions t
		JOIN connections c ON c.id = t.connection_id
		LEFT JOIN payment_orders po ON po.transaction_id = t.id
		WHERE t.id = ?`

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
		&item.Bank,
		&item.Provider,
		&item.OrderCode,
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

func (s *Store) ListAuditLogs(ctx context.Context, limit int) ([]AuditLogView, error) {
	page, err := s.ListAuditLogsPage(ctx, limit, "")
	return page.Items, err
}
