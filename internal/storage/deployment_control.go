package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	ErrMutationGateLocked   = errors.New("deployment mutation gate is locked")
	ErrActiveAuthInProgress = errors.New("active authentication session in progress")
	ErrInvalidLeaseToken    = errors.New("invalid or expired lease token")
	ErrMutationGateHeld     = errors.New("mutation gate held by another active lease")
)

type DeploymentGate struct {
	ID              string `json:"id"`
	GateState       string `json:"gateState"` // "OPEN", "LOCKED"
	Owner           string `json:"owner,omitempty"`
	LeaseToken      string `json:"leaseToken,omitempty"`
	LeaseExpiresAt  string `json:"leaseExpiresAt,omitempty"`
	Reason          string `json:"reason,omitempty"`
	FenceGeneration int64  `json:"fenceGeneration"`
	MetadataJSON    string `json:"metadataJson,omitempty"`
	ActiveAuthCount int    `json:"activeAuthCount"`
	TableExists     bool   `json:"tableExists"`
	Bootstrap       bool   `json:"bootstrap,omitempty"`
	IsStale         bool   `json:"isStale,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
}

func generateLeaseToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) deploymentControlTableExists(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) bool {
	var count int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='deployment_control'`).Scan(&count)
	return err == nil && count > 0
}

func (s *Store) countActiveAuthAttempts(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) int {
	var tableExists int
	_ = q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='auth_attempts'`).Scan(&tableExists)
	if tableExists == 0 {
		return 0
	}
	var count int
	_ = q.QueryRowContext(ctx, `
		SELECT count(*) FROM auth_attempts
		WHERE status IN ('STARTING', 'IN_PROGRESS', 'EXPORTING', 'VERIFYING')
	`).Scan(&count)
	return count
}

func (s *Store) GetDeploymentGate(ctx context.Context) (*DeploymentGate, error) {
	activeCount := s.countActiveAuthAttempts(ctx, s.db)
	if !s.deploymentControlTableExists(ctx, s.db) {
		return &DeploymentGate{
			ID:              "singleton",
			GateState:       "OPEN",
			ActiveAuthCount: activeCount,
			TableExists:     false,
			Bootstrap:       true,
		}, nil
	}

	gate := DeploymentGate{
		ActiveAuthCount: activeCount,
		TableExists:     true,
	}

	err := s.db.QueryRowContext(ctx, `
		SELECT id, gate_state, COALESCE(owner, ''), COALESCE(lease_token, ''), COALESCE(lease_expires_at, ''),
		       COALESCE(reason, ''), fence_generation, COALESCE(metadata_json, '{}'), created_at, updated_at
		FROM deployment_control WHERE id = 'singleton'
	`).Scan(&gate.ID, &gate.GateState, &gate.Owner, &gate.LeaseToken, &gate.LeaseExpiresAt,
		&gate.Reason, &gate.FenceGeneration, &gate.MetadataJSON, &gate.CreatedAt, &gate.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &DeploymentGate{
				ID:              "singleton",
				GateState:       "OPEN",
				ActiveAuthCount: activeCount,
				TableExists:     true,
			}, nil
		}
		return nil, fmt.Errorf("query deployment_control: %w", err)
	}

	if gate.GateState == "LOCKED" && gate.LeaseExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, gate.LeaseExpiresAt); err == nil {
			if time.Now().After(t) {
				gate.IsStale = true
			}
		}
	}

	return &gate, nil
}

func (s *Store) AcquireMutationGate(ctx context.Context, owner string, leaseDuration time.Duration, reason string) (*DeploymentGate, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if leaseDuration <= 0 {
		leaseDuration = 2 * time.Minute
	}
	nowTime := time.Now()
	nowStr := nowTime.Format(time.RFC3339)
	expiresAtStr := nowTime.Add(leaseDuration).Format(time.RFC3339)
	newToken := generateLeaseToken()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin acquire mutation gate tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Atomic active-auth check
	activeCount := s.countActiveAuthAttempts(ctx, tx)
	if activeCount > 0 {
		return nil, fmt.Errorf("%w: %d active attempt(s) in progress", ErrActiveAuthInProgress, activeCount)
	}

	// 2. Bootstrap transition check
	if !s.deploymentControlTableExists(ctx, tx) {
		_ = tx.Commit()
		return &DeploymentGate{
			ID:              "singleton",
			GateState:       "OPEN",
			Owner:           owner,
			LeaseToken:      newToken,
			LeaseExpiresAt:  expiresAtStr,
			Reason:          reason,
			ActiveAuthCount: 0,
			TableExists:     false,
			Bootstrap:       true,
		}, nil
	}

	// 3. Inspect current gate lease
	var gate DeploymentGate
	err = tx.QueryRowContext(ctx, `
		SELECT id, gate_state, COALESCE(owner, ''), COALESCE(lease_token, ''), COALESCE(lease_expires_at, ''),
		       fence_generation
		FROM deployment_control WHERE id = 'singleton'
	`).Scan(&gate.ID, &gate.GateState, &gate.Owner, &gate.LeaseToken, &gate.LeaseExpiresAt, &gate.FenceGeneration)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("query current gate: %w", err)
	}

	if err == nil && gate.GateState == "LOCKED" && gate.LeaseExpiresAt != "" {
		if expTime, parseErr := time.Parse(time.RFC3339, gate.LeaseExpiresAt); parseErr == nil {
			if nowTime.Before(expTime) && gate.Owner != owner {
				return nil, fmt.Errorf("%w: held by owner '%s' until %s", ErrMutationGateHeld, gate.Owner, gate.LeaseExpiresAt)
			}
		}
	}

	newFence := gate.FenceGeneration + 1
	_, err = tx.ExecContext(ctx, `
		INSERT INTO deployment_control (id, gate_state, owner, lease_token, lease_expires_at, reason, fence_generation, created_at, updated_at)
		VALUES ('singleton', 'LOCKED', ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			gate_state = 'LOCKED',
			owner = excluded.owner,
			lease_token = excluded.lease_token,
			lease_expires_at = excluded.lease_expires_at,
			reason = excluded.reason,
			fence_generation = excluded.fence_generation,
			updated_at = excluded.updated_at
	`, owner, newToken, expiresAtStr, reason, newFence, nowStr, nowStr)
	if err != nil {
		return nil, fmt.Errorf("update deployment_control: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit acquire mutation gate tx: %w", err)
	}

	return &DeploymentGate{
		ID:              "singleton",
		GateState:       "LOCKED",
		Owner:           owner,
		LeaseToken:      newToken,
		LeaseExpiresAt:  expiresAtStr,
		Reason:          reason,
		FenceGeneration: newFence,
		ActiveAuthCount: 0,
		TableExists:     true,
		CreatedAt:       nowStr,
		UpdatedAt:       nowStr,
	}, nil
}

func (s *Store) ReleaseMutationGate(ctx context.Context, owner, leaseToken string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin release mutation gate tx: %w", err)
	}
	defer tx.Rollback()

	if !s.deploymentControlTableExists(ctx, tx) {
		_ = tx.Commit()
		return nil
	}

	var curOwner, curToken, curExpiresAt, curState string
	var curFence int64
	err = tx.QueryRowContext(ctx, `
		SELECT gate_state, COALESCE(owner, ''), COALESCE(lease_token, ''), COALESCE(lease_expires_at, ''), fence_generation
		FROM deployment_control WHERE id = 'singleton'
	`).Scan(&curState, &curOwner, &curToken, &curExpiresAt, &curFence)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = tx.Commit()
			return nil
		}
		return fmt.Errorf("query gate for release: %w", err)
	}

	if curState != "LOCKED" {
		_ = tx.Commit()
		return nil
	}

	// Validate ownership
	isOwnerMatch := curOwner == owner && (leaseToken == "" || curToken == leaseToken)
	isExpired := false
	if curExpiresAt != "" {
		if expTime, parseErr := time.Parse(time.RFC3339, curExpiresAt); parseErr == nil {
			isExpired = time.Now().After(expTime)
		}
	}

	if !isOwnerMatch && !isExpired && owner != "force-unlock" {
		return fmt.Errorf("%w: gate held by '%s'", ErrInvalidLeaseToken, curOwner)
	}

	nowStr := time.Now().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
		UPDATE deployment_control SET
			gate_state = 'OPEN',
			owner = NULL,
			lease_token = NULL,
			lease_expires_at = NULL,
			reason = 'released',
			fence_generation = fence_generation + 1,
			updated_at = ?
		WHERE id = 'singleton'
	`, nowStr)
	if err != nil {
		return fmt.Errorf("release gate exec: %w", err)
	}

	return tx.Commit()
}

func (s *Store) RenewMutationGate(ctx context.Context, owner, leaseToken string, duration time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if duration <= 0 {
		duration = 2 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin renew mutation gate tx: %w", err)
	}
	defer tx.Rollback()

	if !s.deploymentControlTableExists(ctx, tx) {
		_ = tx.Commit()
		return nil
	}

	var curOwner, curToken, curState string
	err = tx.QueryRowContext(ctx, `
		SELECT gate_state, COALESCE(owner, ''), COALESCE(lease_token, '')
		FROM deployment_control WHERE id = 'singleton'
	`).Scan(&curState, &curOwner, &curToken)
	if err != nil {
		return fmt.Errorf("query gate for renewal: %w", err)
	}

	if curState != "LOCKED" || curOwner != owner || curToken != leaseToken {
		return fmt.Errorf("%w: unable to renew lease for '%s'", ErrInvalidLeaseToken, owner)
	}

	newExpiry := time.Now().Add(duration).Format(time.RFC3339)
	nowStr := time.Now().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
		UPDATE deployment_control SET
			lease_expires_at = ?,
			updated_at = ?
		WHERE id = 'singleton'
	`, newExpiry, nowStr)
	if err != nil {
		return fmt.Errorf("renew gate exec: %w", err)
	}

	return tx.Commit()
}

func (s *Store) ForceUnlockMutationGate(ctx context.Context, reason string) error {
	return s.ReleaseMutationGate(ctx, "force-unlock", "")
}

func (s *Store) CheckMutationAllowed(ctx context.Context) error {
	if !s.deploymentControlTableExists(ctx, s.db) {
		return nil
	}

	var gateState, expiresAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT gate_state, COALESCE(lease_expires_at, '')
		FROM deployment_control WHERE id = 'singleton'
	`).Scan(&gateState, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("check mutation gate: %w", err)
	}

	if gateState == "LOCKED" && expiresAt != "" {
		if expTime, parseErr := time.Parse(time.RFC3339, expiresAt); parseErr == nil {
			if time.Now().Before(expTime) {
				return ErrMutationGateLocked
			}
		}
	}
	return nil
}

func (s *Store) checkMutationAllowedTx(ctx context.Context, tx *sql.Tx) error {
	if !s.deploymentControlTableExists(ctx, tx) {
		return nil
	}

	var gateState, expiresAt string
	err := tx.QueryRowContext(ctx, `
		SELECT gate_state, COALESCE(lease_expires_at, '')
		FROM deployment_control WHERE id = 'singleton'
	`).Scan(&gateState, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("check mutation gate in tx: %w", err)
	}

	if gateState == "LOCKED" && expiresAt != "" {
		if expTime, parseErr := time.Parse(time.RFC3339, expiresAt); parseErr == nil {
			if time.Now().Before(expTime) {
				return ErrMutationGateLocked
			}
		}
	}
	return nil
}
