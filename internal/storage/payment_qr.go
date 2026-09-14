package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type PaymentQR struct {
	ID               string `json:"id"`
	ConnectionID     string `json:"connectionId"`
	AccountNumber    string `json:"accountNumber"`
	AccountName      string `json:"accountName"`
	Bin              string `json:"bin"`
	BankName         string `json:"bankName"`
	ImagePath        string `json:"imagePath,omitempty"`
	ImageHash        string `json:"imageHash,omitempty"`
	ImageContentType string `json:"imageContentType,omitempty"`
	Provider         string `json:"provider"`
	Revision         int64  `json:"revision"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
}

func (s *Store) GetPaymentQR(ctx context.Context, connectionID string) (*PaymentQR, error) {
	if connectionID == "" {
		conn, err := s.Connection(ctx)
		if err != nil {
			return nil, err
		}
		connectionID = conn.ID
	}

	var qr PaymentQR
	var imgPath, imgHash, imgCT sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, account_number, account_name, bin, bank_name, image_path, image_hash, image_content_type, provider, revision, created_at, updated_at
		FROM payment_qr_settings
		WHERE connection_id = ?
	`, connectionID).Scan(
		&qr.ID, &qr.ConnectionID, &qr.AccountNumber, &qr.AccountName, &qr.Bin, &qr.BankName,
		&imgPath, &imgHash, &imgCT, &qr.Provider, &qr.Revision, &qr.CreatedAt, &qr.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // No QR configured yet
		}
		return nil, fmt.Errorf("get payment qr: %w", err)
	}

	qr.ImagePath = imgPath.String
	qr.ImageHash = imgHash.String
	qr.ImageContentType = imgCT.String
	return &qr, nil
}

func (s *Store) SavePaymentQR(ctx context.Context, qr PaymentQR) (*PaymentQR, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return nil, err
	}
	if qr.ConnectionID == "" {
		conn, err := s.Connection(ctx)
		if err != nil {
			return nil, err
		}
		qr.ConnectionID = conn.ID
	}
	if qr.AccountNumber == "" || qr.AccountName == "" {
		return nil, errors.New("account number and account name are required")
	}
	if qr.Bin == "" {
		qr.Bin = "970416"
	}
	if qr.BankName == "" {
		qr.BankName = "ACB"
	}
	if qr.Provider == "" {
		qr.Provider = "UPLOAD"
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	qrID := id("qr")

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var existingID string
		var existingRev int64
		err := tx.QueryRowContext(ctx, `SELECT id, revision FROM payment_qr_settings WHERE connection_id = ?`, qr.ConnectionID).Scan(&existingID, &existingRev)
		if err == nil {
			// Update
			qrID = existingID
			qr.Revision = existingRev + 1
			_, err = tx.ExecContext(ctx, `
				UPDATE payment_qr_settings
				SET account_number = ?, account_name = ?, bin = ?, bank_name = ?, image_path = ?, image_hash = ?, image_content_type = ?, provider = ?, revision = ?, updated_at = ?
				WHERE id = ?
			`, qr.AccountNumber, qr.AccountName, qr.Bin, qr.BankName, qr.ImagePath, qr.ImageHash, qr.ImageContentType, qr.Provider, qr.Revision, nowStr, qrID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// Insert
		qr.Revision = 1
		_, err = tx.ExecContext(ctx, `
			INSERT INTO payment_qr_settings(id, connection_id, account_number, account_name, bin, bank_name, image_path, image_hash, image_content_type, provider, revision, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		`, qrID, qr.ConnectionID, qr.AccountNumber, qr.AccountName, qr.Bin, qr.BankName, qr.ImagePath, qr.ImageHash, qr.ImageContentType, qr.Provider, nowStr, nowStr)
		return err
	})

	if err != nil {
		return nil, err
	}

	qr.ID = qrID
	qr.UpdatedAt = nowStr
	return &qr, nil
}

func (s *Store) DeletePaymentQR(ctx context.Context, connectionID string) error {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return err
	}
	if connectionID == "" {
		conn, err := s.Connection(ctx)
		if err != nil {
			return err
		}
		connectionID = conn.ID
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM payment_qr_settings WHERE connection_id = ?`, connectionID)
	return err
}

func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
