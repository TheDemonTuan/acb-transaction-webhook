package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrSettingsConflict = errors.New("settings revision conflict: modified by another operator")

func (s *Store) GetMonitorSettings(ctx context.Context) (MonitorSettings, error) {
	var (
		revision           int64
		enabledInt         int
		timezone           string
		defaultProfileJSON string
		windowsJSON        string
		updatedAt          string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT revision, enabled, timezone, default_profile_json, windows_json, updated_at
		FROM monitor_settings
		WHERE id = 'singleton'
	`).Scan(&revision, &enabledInt, &timezone, &defaultProfileJSON, &windowsJSON, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultMonitorSettings, nil
		}
		return MonitorSettings{}, fmt.Errorf("get monitor settings: %w", err)
	}

	var defProfile Profile
	if err := json.Unmarshal([]byte(defaultProfileJSON), &defProfile); err != nil {
		defProfile = DefaultMonitorSettings.DefaultProfile
	}

	var windows []Window
	if err := json.Unmarshal([]byte(windowsJSON), &windows); err != nil {
		windows = DefaultMonitorSettings.Windows
	}

	return MonitorSettings{
		Revision:       revision,
		Enabled:        enabledInt == 1,
		Timezone:       timezone,
		DefaultProfile: defProfile,
		Windows:        windows,
		UpdatedAt:      updatedAt,
	}, nil
}

func (s *Store) SaveMonitorSettings(ctx context.Context, settings MonitorSettings) (MonitorSettings, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return MonitorSettings{}, err
	}
	if err := settings.Validate(); err != nil {
		return MonitorSettings{}, err
	}

	defProfileBytes, err := json.Marshal(settings.DefaultProfile)
	if err != nil {
		return MonitorSettings{}, err
	}

	windowsBytes, err := json.Marshal(settings.Windows)
	if err != nil {
		return MonitorSettings{}, err
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	enabledInt := 0
	if settings.Enabled {
		enabledInt = 1
	}

	newRevision := settings.Revision + 1
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var currentRev int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM monitor_settings WHERE id = 'singleton'`).Scan(&currentRev)
		if err == nil {
			if currentRev != settings.Revision {
				return ErrSettingsConflict
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE monitor_settings
				SET revision = ?, enabled = ?, timezone = ?, default_profile_json = ?, windows_json = ?, updated_at = ?
				WHERE id = 'singleton' AND revision = ?
			`, newRevision, enabledInt, settings.Timezone, string(defProfileBytes), string(windowsBytes), nowStr, settings.Revision)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// First-time insert
		newRevision = settings.Revision + 1
		_, err = tx.ExecContext(ctx, `
			INSERT INTO monitor_settings (id, revision, enabled, timezone, default_profile_json, windows_json, updated_at)
			VALUES ('singleton', ?, ?, ?, ?, ?, ?)
		`, newRevision, enabledInt, settings.Timezone, string(defProfileBytes), string(windowsBytes), nowStr)
		return err
	})

	if err != nil {
		return MonitorSettings{}, err
	}

	settings.Revision = newRevision
	settings.UpdatedAt = nowStr
	return settings, nil
}
