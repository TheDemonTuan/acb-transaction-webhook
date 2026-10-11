package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"
)

var ErrVoiceSettingsConflict = errors.New("voice settings revision conflict: modified by another operator")

const (
	CatalogVersionV1        = "v1"
	ColdRequestTimeoutMs    = 8000
	ProviderModeEdgeOnly    = "EDGE_ONLY"
	ProviderModePiperOnly   = "PIPER_ONLY"
	ProviderModeBrowserOnly = "BROWSER_ONLY"
	VoiceIDHoaiMy           = "vi-VN-HoaiMyNeural"
	VoiceIDNamMinh          = "vi-VN-NamMinhNeural"
	VoiceIDPiperVais1000    = "vi-VN-vais1000-medium"
)

type AmountBand struct {
	ID      string  `json:"id"`
	FromVND string  `json:"fromVnd"`
	ToVND   *string `json:"toVnd"`
	StyleID string  `json:"styleId"`
}

type VoiceProfile struct {
	Revision             int64        `json:"revision"`
	ProviderMode         string       `json:"providerMode"`
	VoiceID              string       `json:"voiceId"`
	Rate                 float64      `json:"rate"`
	PitchHz              int          `json:"pitchHz"`
	AmountBands          []AmountBand `json:"amountBands"`
	ChimeEnabled         bool         `json:"chimeEnabled"`
	LoadingSoundEnabled  bool         `json:"loadingSoundEnabled"`
	FunEnabled           bool         `json:"funEnabled"`
	IncludeDescription   bool         `json:"includeDescription"`
	CustomTemplate       string       `json:"customTemplate"`
	CatalogVersion       string       `json:"catalogVersion"`
	SynthesisKey         string       `json:"synthesisKey"`
	ColdRequestTimeoutMs int          `json:"coldRequestTimeoutMs"`
	UpdatedAt            string       `json:"updatedAt"`
}

type VoiceSettingsInput struct {
	Revision            int64        `json:"revision"`
	ProviderMode        string       `json:"providerMode"`
	VoiceID             string       `json:"voiceId"`
	Rate                float64      `json:"rate"`
	PitchHz             int          `json:"pitchHz"`
	AmountBands         []AmountBand `json:"amountBands"`
	ChimeEnabled        bool         `json:"chimeEnabled"`
	LoadingSoundEnabled bool         `json:"loadingSoundEnabled"`
	FunEnabled          bool         `json:"funEnabled"`
	IncludeDescription  bool         `json:"includeDescription"`
	CustomTemplate      string       `json:"customTemplate"`
}

func defaultAmountBands() []AmountBand {
	to50k := "50000"
	to200k := "200000"
	to1m := "1000000"
	return []AmountBand{
		{ID: "small", FromVND: "1", ToVND: &to50k, StyleID: "classic"},
		{ID: "medium", FromVND: "50000", ToVND: &to200k, StyleID: "classic"},
		{ID: "large", FromVND: "200000", ToVND: &to1m, StyleID: "classic"},
		{ID: "celebrate", FromVND: "1000000", ToVND: nil, StyleID: "classic"},
	}
}

func ComputeSynthesisKey(voiceID string, rate float64, pitchHz int) string {
	provider := "edge"
	if voiceID == VoiceIDPiperVais1000 {
		provider = "piper"
	}
	raw := fmt.Sprintf("%s|%s|%.2f|%d|money-vi-v1|pack-schema-2", provider, voiceID, rate, pitchHz)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// SynthesisProvider identifies the explicitly selected runtime, never a fallback.
func (p VoiceProfile) SynthesisProvider() string {
	switch p.ProviderMode {
	case ProviderModePiperOnly:
		return "piper"
	case ProviderModeEdgeOnly:
		return "edge"
	case ProviderModeBrowserOnly:
		return "browser"
	default:
		return ""
	}
}

var DefaultVoiceProfile = VoiceProfile{
	Revision:             1,
	ProviderMode:         ProviderModePiperOnly,
	VoiceID:              VoiceIDPiperVais1000,
	Rate:                 1.25,
	PitchHz:              0,
	AmountBands:          defaultAmountBands(),
	ChimeEnabled:         true,
	LoadingSoundEnabled:  false,
	FunEnabled:           true,
	IncludeDescription:   false,
	CustomTemplate:       "",
	CatalogVersion:       CatalogVersionV1,
	SynthesisKey:         ComputeSynthesisKey(VoiceIDPiperVais1000, 1.25, 0),
	ColdRequestTimeoutMs: ColdRequestTimeoutMs,
	UpdatedAt:            "2026-10-11T00:00:00Z",
}

var placeholderRegex = regexp.MustCompile(`\{([^}]+)\}`)

func ValidateVoiceProfileInput(in *VoiceSettingsInput) error {
	switch in.ProviderMode {
	case ProviderModePiperOnly:
		if in.VoiceID != VoiceIDPiperVais1000 || in.PitchHz != 0 {
			return errors.New("PIPER_ONLY requires vi-VN-vais1000-medium and pitchHz 0")
		}
	case ProviderModeEdgeOnly:
		if in.VoiceID != VoiceIDHoaiMy && in.VoiceID != VoiceIDNamMinh {
			return errors.New("EDGE_ONLY requires an approved Edge voiceId")
		}
	case ProviderModeBrowserOnly:
		if in.VoiceID != VoiceIDHoaiMy && in.VoiceID != VoiceIDNamMinh && in.VoiceID != VoiceIDPiperVais1000 {
			return errors.New("invalid voiceId")
		}
	default:
		return errors.New("invalid providerMode: must be PIPER_ONLY, EDGE_ONLY or BROWSER_ONLY")
	}

	if math.IsNaN(in.Rate) || math.IsInf(in.Rate, 0) || in.Rate < 0.75 || in.Rate > 2.00 {
		return errors.New("invalid rate: must be between 0.75 and 2.00")
	}
	if math.Abs(in.Rate-math.Round(in.Rate*100)/100) > 1e-6 {
		return errors.New("invalid rate: maximum two decimal places allowed")
	}

	if in.PitchHz < -50 || in.PitchHz > 50 {
		return errors.New("invalid pitchHz: must be between -50 and 50")
	}

	if in.CustomTemplate != "" {
		if utf8.RuneCountInString(in.CustomTemplate) > 120 {
			return errors.New("customTemplate exceeds 120 runes")
		}
		matches := placeholderRegex.FindAllStringSubmatch(in.CustomTemplate, -1)
		amountCount := 0
		for _, m := range matches {
			placeholder := m[1]
			switch placeholder {
			case "amount", "amount_raw":
				amountCount++
			case "description":
				// allowed
			default:
				return fmt.Errorf("unknown placeholder {%s} in customTemplate", placeholder)
			}
		}
		if amountCount != 1 {
			return errors.New("customTemplate must contain exactly one {amount} or {amount_raw}")
		}
	}

	if len(in.AmountBands) != 4 {
		return errors.New("amountBands must have exactly 4 bands")
	}

	expectedIDs := []string{"small", "medium", "large", "celebrate"}
	validStyles := map[string]bool{"classic": true, "cute": true, "funny": true, "custom": true}

	var prevToVal int64 = 0
	for i, band := range in.AmountBands {
		if band.ID != expectedIDs[i] {
			return fmt.Errorf("band %d must have id %q, got %q", i, expectedIDs[i], band.ID)
		}
		if !validStyles[band.StyleID] {
			return fmt.Errorf("invalid styleId %q for band %s", band.StyleID, band.ID)
		}

		fromVal, err := strconv.ParseInt(band.FromVND, 10, 64)
		if err != nil || fromVal <= 0 || fromVal > 9007199254740991 {
			return fmt.Errorf("invalid fromVnd %q for band %s", band.FromVND, band.ID)
		}

		if i == 0 {
			if fromVal != 1 {
				return errors.New("small band fromVnd must be '1'")
			}
		} else {
			if fromVal != prevToVal {
				return fmt.Errorf("band %s fromVnd %d does not match previous toVnd %d", band.ID, fromVal, prevToVal)
			}
		}

		if i == 3 {
			if band.ToVND != nil {
				return errors.New("celebrate band toVnd must be null")
			}
		} else {
			if band.ToVND == nil {
				return fmt.Errorf("band %s toVnd must not be null", band.ID)
			}
			toVal, err := strconv.ParseInt(*band.ToVND, 10, 64)
			if err != nil || toVal <= fromVal || toVal > 9007199254740991 {
				return fmt.Errorf("invalid toVnd %q for band %s (must be > %d)", *band.ToVND, band.ID, fromVal)
			}
			prevToVal = toVal
		}
	}

	return nil
}

func (s *Store) GetVoiceProfile(ctx context.Context) (VoiceProfile, error) {
	var (
		revision        int64
		providerMode    string
		voiceID         string
		rate            float64
		pitchHz         int
		amountBandsJSON string
		chimeEnabledInt int
		loadingSoundInt int
		funEnabledInt   int
		includeDescInt  int
		customTemplate  string
		updatedAt       string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT revision, provider_mode, edge_voice, rate, pitch_hz, amount_bands_json,
		       chime_enabled, loading_sound_enabled, fun_enabled, include_description,
		       custom_template, updated_at
		FROM voice_settings
		WHERE id = 'singleton'
	`).Scan(
		&revision, &providerMode, &voiceID, &rate, &pitchHz, &amountBandsJSON,
		&chimeEnabledInt, &loadingSoundInt, &funEnabledInt, &includeDescInt,
		&customTemplate, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			profile := DefaultVoiceProfile
			profile.AmountBands = defaultAmountBands()
			return profile, nil
		}
		return VoiceProfile{}, fmt.Errorf("get voice profile: %w", err)
	}

	var bands []AmountBand
	if err := json.Unmarshal([]byte(amountBandsJSON), &bands); err != nil || len(bands) != 4 {
		bands = defaultAmountBands()
	}

	return VoiceProfile{
		Revision:             revision,
		ProviderMode:         providerMode,
		VoiceID:              voiceID,
		Rate:                 rate,
		PitchHz:              pitchHz,
		AmountBands:          bands,
		ChimeEnabled:         chimeEnabledInt == 1,
		LoadingSoundEnabled:  loadingSoundInt == 1,
		FunEnabled:           funEnabledInt == 1,
		IncludeDescription:   includeDescInt == 1,
		CustomTemplate:       customTemplate,
		CatalogVersion:       CatalogVersionV1,
		SynthesisKey:         ComputeSynthesisKey(voiceID, rate, pitchHz),
		ColdRequestTimeoutMs: ColdRequestTimeoutMs,
		UpdatedAt:            updatedAt,
	}, nil
}

func (s *Store) SaveVoiceProfile(ctx context.Context, input VoiceSettingsInput) (VoiceProfile, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return VoiceProfile{}, err
	}
	if err := ValidateVoiceProfileInput(&input); err != nil {
		return VoiceProfile{}, fmt.Errorf("invalid voice settings: %w", err)
	}

	bandsBytes, err := json.Marshal(input.AmountBands)
	if err != nil {
		return VoiceProfile{}, fmt.Errorf("serialize amount bands: %w", err)
	}
	bandsJSON := string(bandsBytes)

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	chimeInt := 0
	if input.ChimeEnabled {
		chimeInt = 1
	}
	loadingInt := 0
	if input.LoadingSoundEnabled {
		loadingInt = 1
	}
	funInt := 0
	if input.FunEnabled {
		funInt = 1
	}
	includeDescInt := 0
	if input.IncludeDescription {
		includeDescInt = 1
	}

	newRevision := input.Revision + 1

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var currentRevision int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM voice_settings WHERE id = 'singleton'`).Scan(&currentRevision)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Insert default singleton if not present
				_, insertErr := tx.ExecContext(ctx, `
					INSERT INTO voice_settings (
						id, revision, provider_mode, edge_voice, rate, pitch_hz, amount_bands_json,
						chime_enabled, loading_sound_enabled, fun_enabled, include_description,
						custom_template, online_fallback, updated_at
					) VALUES ('singleton', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
				`, newRevision, input.ProviderMode, input.VoiceID, input.Rate, input.PitchHz, bandsJSON,
					chimeInt, loadingInt, funInt, includeDescInt, input.CustomTemplate, nowStr)
				return insertErr
			}
			return err
		}

		if currentRevision != input.Revision {
			return ErrVoiceSettingsConflict
		}

		res, updateErr := tx.ExecContext(ctx, `
			UPDATE voice_settings
			SET revision = ?, provider_mode = ?, edge_voice = ?, rate = ?, pitch_hz = ?,
			    amount_bands_json = ?, chime_enabled = ?, loading_sound_enabled = ?,
			    fun_enabled = ?, include_description = ?, custom_template = ?,
			    online_fallback = 0, updated_at = ?
			WHERE id = 'singleton' AND revision = ?
		`, newRevision, input.ProviderMode, input.VoiceID, input.Rate, input.PitchHz, bandsJSON,
			chimeInt, loadingInt, funInt, includeDescInt, input.CustomTemplate, nowStr, input.Revision)
		if updateErr != nil {
			return updateErr
		}
		rows, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if rows == 0 {
			return ErrVoiceSettingsConflict
		}
		return nil
	})

	if err != nil {
		return VoiceProfile{}, err
	}

	return VoiceProfile{
		Revision:             newRevision,
		ProviderMode:         input.ProviderMode,
		VoiceID:              input.VoiceID,
		Rate:                 input.Rate,
		PitchHz:              input.PitchHz,
		AmountBands:          input.AmountBands,
		ChimeEnabled:         input.ChimeEnabled,
		LoadingSoundEnabled:  input.LoadingSoundEnabled,
		FunEnabled:           input.FunEnabled,
		IncludeDescription:   input.IncludeDescription,
		CustomTemplate:       input.CustomTemplate,
		CatalogVersion:       CatalogVersionV1,
		SynthesisKey:         ComputeSynthesisKey(input.VoiceID, input.Rate, input.PitchHz),
		ColdRequestTimeoutMs: ColdRequestTimeoutMs,
		UpdatedAt:            nowStr,
	}, nil
}
