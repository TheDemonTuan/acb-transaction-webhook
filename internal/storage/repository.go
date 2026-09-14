package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNotFound = sql.ErrNoRows

type Connection struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	AccountMasked string `json:"accountMasked"`
	Generation    int64  `json:"generation"`
	StartedAt     string `json:"startedAt"`
	UpdatedAt     string `json:"updatedAt"`
}
type Endpoint struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Status    string `json:"status"`
	Revision  int    `json:"revision"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}
type DeliverySummary struct {
	Pending    int `json:"pending"`
	DeadLetter int `json:"deadLetter"`
}

type NotificationSummary struct {
	Total      DeliverySummary            `json:"total"`
	ByProvider map[string]DeliverySummary `json:"byProvider"`
}

func id(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func (s *Store) Connection(ctx context.Context) (Connection, error) {
	var c Connection
	err := s.db.QueryRowContext(ctx, `SELECT id,state,COALESCE(account_masked,''),generation,COALESCE(started_at,''),updated_at FROM connections ORDER BY created_at LIMIT 1`).Scan(&c.ID, &c.State, &c.AccountMasked, &c.Generation, &c.StartedAt, &c.UpdatedAt)
	return c, err
}
func (s *Store) ConfigureConnection(ctx context.Context, masked string) (Connection, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return Connection{}, err
	}
	masked = strings.TrimSpace(masked)
	if masked == "" {
		return Connection{}, errors.New("account masked is required")
	}
	t := now()
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM connections`).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("only one connection is supported")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO connections(id,account_masked,state,started_at,created_at,updated_at)VALUES(?,?, 'AUTH_REQUIRED',?,?,?)`, id("conn"), masked, t, t, t)
		return err
	})
	if err != nil {
		return Connection{}, err
	}
	return s.Connection(ctx)
}
func (s *Store) TransitionConnection(ctx context.Context, action string) (Connection, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return Connection{}, err
	}
	var next string
	switch action {
	case "pause":
		next = "PAUSED"
	case "resume":
		next = "AUTH_REQUIRED"
	default:
		return Connection{}, errors.New("unsupported action")
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE connections SET state=?, generation=generation+1, updated_at=?`, next, now())
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return Connection{}, err
	}
	return s.Connection(ctx)
}
func (s *Store) Endpoints(ctx context.Context) ([]Endpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.name,v.url,e.status,e.current_revision,e.created_at,e.updated_at FROM webhook_endpoints e JOIN endpoint_versions v ON v.endpoint_id=e.id AND v.revision=e.current_revision ORDER BY e.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	endpoints := make([]Endpoint, 0)
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.ID, &e.Name, &e.URL, &e.Status, &e.Revision, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		endpoints = append(endpoints, e)
	}
	return endpoints, rows.Err()
}
func (s *Store) CreateEndpoint(ctx context.Context, name, url string) (Endpoint, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return Endpoint{}, err
	}
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if name == "" || url == "" {
		return Endpoint{}, errors.New("name and URL are required")
	}
	e := Endpoint{ID: id("ep"), Name: name, URL: url, Status: "DISABLED", Revision: 1, CreatedAt: now(), UpdatedAt: now()}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at) VALUES(?,?,?,1,?,?)`, e.ID, e.Name, e.Status, e.CreatedAt, e.UpdatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO endpoint_versions(endpoint_id,revision,url,filters_json,created_at)VALUES(?,1,?,'[]',?)`, e.ID, e.URL, e.CreatedAt)
		return err
	})
	return e, err
}
func (s *Store) SetEndpointStatus(ctx context.Context, endpointID, status string) error {
	if status != "ACTIVE" && status != "DISABLED" {
		return errors.New("invalid endpoint status")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE webhook_endpoints SET status=?,updated_at=? WHERE id=?`, status, now(), endpointID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) DeliverySummary(ctx context.Context) (DeliverySummary, error) {
	var d DeliverySummary
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(CASE WHEN status IN('PENDING','IN_FLIGHT','RETRYING') THEN 1 ELSE 0 END),0),COALESCE(sum(CASE WHEN status='DEAD_LETTER' THEN 1 ELSE 0 END),0) FROM deliveries`).Scan(&d.Pending, &d.DeadLetter)
	if err != nil {
		return DeliverySummary{}, fmt.Errorf("delivery summary: %w", err)
	}
	return d, nil
}
func (s *Store) NotificationSummary(ctx context.Context) (NotificationSummary, error) {
	total, err := s.DeliverySummary(ctx)
	if err != nil {
		return NotificationSummary{}, err
	}
	result := NotificationSummary{Total: total, ByProvider: map[string]DeliverySummary{
		"WEBHOOK": {},
		"BARK":    {},
	}}
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(e.provider, 'WEBHOOK'),
		       COALESCE(sum(CASE WHEN d.status IN('PENDING','IN_FLIGHT','RETRYING') THEN 1 ELSE 0 END),0),
		       COALESCE(sum(CASE WHEN d.status='DEAD_LETTER' THEN 1 ELSE 0 END),0)
		FROM deliveries d JOIN webhook_endpoints e ON e.id=d.endpoint_id
		GROUP BY COALESCE(e.provider, 'WEBHOOK')`)
	if err != nil {
		return NotificationSummary{}, fmt.Errorf("notification summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var provider string
		var summary DeliverySummary
		if err := rows.Scan(&provider, &summary.Pending, &summary.DeadLetter); err != nil {
			return NotificationSummary{}, fmt.Errorf("notification summary: %w", err)
		}
		result.ByProvider[provider] = summary
	}
	return result, rows.Err()
}

func (s *Store) Audit(ctx context.Context, subject, role, action, target, requestID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_logs(id,actor_subject,actor_role,action,target,request_id,details_json,created_at)VALUES(?,?,?,?,?,?, '{}',?)`, id("audit"), subject, role, action, target, requestID, now())
	return err
}
