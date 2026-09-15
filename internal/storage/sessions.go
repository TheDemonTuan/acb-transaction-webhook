package storage

import (
	"context"
	"database/sql"
	"errors"
)

var ErrSessionNotRefreshable = errors.New("session is not refreshable")

type StoredSession struct {
	ConnectionID string
	Generation   int64
	Envelope     []byte
	KeyID        string
}

func (s *Store) Session(ctx context.Context, connectionID string, generation int64) (StoredSession, error) {
	var session StoredSession
	err := s.db.QueryRowContext(ctx, `
		SELECT connection_id, generation, envelope, key_id
		FROM sessions WHERE connection_id = ? AND generation = ?
	`, connectionID, generation).Scan(&session.ConnectionID, &session.Generation, &session.Envelope, &session.KeyID)
	if err != nil {
		return StoredSession{}, err
	}
	return session, nil
}

func (s *Store) RefreshSession(ctx context.Context, connectionID string, generation int64, envelope []byte, keyID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET envelope=?,key_id=?,verified_at=?,updated_at=? WHERE connection_id=? AND generation=? AND EXISTS (SELECT 1 FROM connections WHERE id=? AND generation=? AND state='MONITORING')`, envelope, keyID, now(), now(), connectionID, generation, connectionID, generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrSessionNotRefreshable
	}
	return nil
}

func (s *Store) DeleteSession(ctx context.Context, connectionID string, generation int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE connection_id=? AND generation=?`, connectionID, generation)
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}
