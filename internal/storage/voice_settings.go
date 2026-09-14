package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrVoiceSettingsConflict = errors.New("voice settings revision conflict: modified by another operator")

type VoiceSettings struct {
	Revision       int64  `json:"revision"`
	ProviderMode   string `json:"providerMode"` // ONLINE_AUTO, EDGE_ONLY, BROWSER_ONLY
	EdgeVoice      string `json:"edgeVoice"`    // vi-VN-HoaiMyNeural, vi-VN-NamMinhNeural
	OnlineFallback bool   `json:"onlineFallback"`
	UpdatedAt      string `json:"updatedAt"`
}

var DefaultVoiceSettings = VoiceSettings{
	Revision:       1,
	ProviderMode:   "ONLINE_AUTO",
	EdgeVoice:      "vi-VN-HoaiMyNeural",
	OnlineFallback: true,
	UpdatedAt:      "2026-09-12T00:00:00Z",
}

func (s *Store) GetVoiceSettings(ctx context.Context) (VoiceSettings, error) {
	var (
		revision          int64
		providerMode      string
		edgeVoice         string
		onlineFallbackInt int
		updatedAt         string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT revision, provider_mode, edge_voice, online_fallback, updated_at
		FROM voice_settings
		WHERE id = 'singleton'
	`).Scan(&revision, &providerMode, &edgeVoice, &onlineFallbackInt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultVoiceSettings, nil
		}
		return VoiceSettings{}, fmt.Errorf("get voice settings: %w", err)
	}

	return VoiceSettings{
		Revision:       revision,
		ProviderMode:   providerMode,
		EdgeVoice:      edgeVoice,
		OnlineFallback: onlineFallbackInt == 1,
		UpdatedAt:      updatedAt,
	}, nil
}

func (s *Store) SaveVoiceSettings(ctx context.Context, settings VoiceSettings) (VoiceSettings, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return VoiceSettings{}, err
	}
	if settings.ProviderMode != "ONLINE_AUTO" && settings.ProviderMode != "EDGE_ONLY" && settings.ProviderMode != "BROWSER_ONLY" {
		return VoiceSettings{}, errors.New("invalid providerMode: must be ONLINE_AUTO, EDGE_ONLY, or BROWSER_ONLY")
	}
	if settings.EdgeVoice != "vi-VN-HoaiMyNeural" && settings.EdgeVoice != "vi-VN-NamMinhNeural" {
		return VoiceSettings{}, errors.New("invalid edgeVoice: must be vi-VN-HoaiMyNeural or vi-VN-NamMinhNeural")
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	onlineFallbackInt := 0
	if settings.OnlineFallback {
		onlineFallbackInt = 1
	}

	newRevision := settings.Revision + 1
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var currentRev int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM voice_settings WHERE id = 'singleton'`).Scan(&currentRev)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Insert initial record
				_, insertErr := tx.ExecContext(ctx, `
					INSERT INTO voice_settings(id, revision, provider_mode, edge_voice, online_fallback, updated_at)
					VALUES('singleton', ?, ?, ?, ?, ?)
				`, newRevision, settings.ProviderMode, settings.EdgeVoice, onlineFallbackInt, nowStr)
				return insertErr
			}
			return err
		}

		if currentRev != settings.Revision {
			return ErrVoiceSettingsConflict
		}

		_, updateErr := tx.ExecContext(ctx, `
			UPDATE voice_settings
			SET revision = ?, provider_mode = ?, edge_voice = ?, online_fallback = ?, updated_at = ?
			WHERE id = 'singleton' AND revision = ?
		`, newRevision, settings.ProviderMode, settings.EdgeVoice, onlineFallbackInt, nowStr, settings.Revision)
		return updateErr
	})

	if err != nil {
		return VoiceSettings{}, err
	}

	settings.Revision = newRevision
	settings.UpdatedAt = nowStr
	return settings, nil
}
