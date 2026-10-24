package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVieneuMigrationPreservesProfileAndCAS(t *testing.T) {
	for _, fixture := range []struct {
		name, mode, voice string
		rate              float64
		pitch, fallback   int
		changed           bool
	}{
		{"piper", "PIPER_ONLY", "vi-VN-vais1000-medium", 1.25, 0, 0, true},
		{"edge", "EDGE_ONLY", "vi-VN-HoaiMyNeural", 1.20, 12, 0, true},
		{"browser", "BROWSER_ONLY", "vi-VN-NamMinhNeural", 0.80, -10, 0, true},
		{"wrong-voice", ProviderModeVieneuOnly, "unknown", 1, 0, 0, true},
		{"wrong-rate", ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1.25, 0, 0, true},
		{"wrong-pitch", ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1, 1, 0, true},
		{"fallback", ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1, 0, 1, true},
		{"canonical", ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1, 0, 0, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := voiceStoreAtSchema22(t)
			bands := defaultAmountBands()
			cutoff := "45000"
			bands[0].ToVND = &cutoff
			bands[1].FromVND = cutoff
			bands[2].StyleID = "custom"
			raw, err := json.Marshal(bands)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.db.ExecContext(ctx, `INSERT INTO voice_settings(id,revision,provider_mode,edge_voice,rate,pitch_hz,amount_bands_json,chime_enabled,loading_sound_enabled,fun_enabled,include_description,custom_template,online_fallback,updated_at) VALUES('singleton',17,?,?,?,?,?,0,1,0,1,'Cảm ơn {amount}.',?,'2026-10-10T00:00:00Z')`, fixture.mode, fixture.voice, fixture.rate, fixture.pitch, string(raw), fixture.fallback)
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.GetVoiceProfile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.changed {
				if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_voice_cutover BEFORE UPDATE ON voice_settings BEGIN SELECT RAISE(ABORT,'injected cutover failure'); END`); err != nil {
					t.Fatal(err)
				}
				if err := store.Migrate(ctx); err == nil {
					t.Fatal("expected atomic migration failure")
				}
				after, err := store.GetVoiceProfile(ctx)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("partial profile mutation: %+v err=%v", after, err)
				}
				var count int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=23`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed migration registered: %d %v", count, err)
				}
				if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_voice_cutover`); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			after, err := store.GetVoiceProfile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			want.ProviderMode, want.VoiceID, want.Rate, want.PitchHz = ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1, 0
			want.SynthesisKey = ComputeSynthesisKey(VoiceIDVieneuMaiAnh, 1, 0)
			if fixture.changed {
				want.Revision++
				want.UpdatedAt = after.UpdatedAt
			}
			if !reflect.DeepEqual(want, after) {
				t.Fatalf("profile not preserved: want=%+v got=%+v", want, after)
			}
			var fallback int
			if err := store.db.QueryRowContext(ctx, `SELECT online_fallback FROM voice_settings`).Scan(&fallback); err != nil || fallback != 0 {
				t.Fatalf("fallback=%d err=%v", fallback, err)
			}
			var checksum string
			if err := store.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=23`).Scan(&checksum); err != nil || checksum != "2026-10-24-v23-vieneu-only" {
				t.Fatalf("registration: %s %v", checksum, err)
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			again, err := store.GetVoiceProfile(ctx)
			if err != nil || !reflect.DeepEqual(after, again) {
				t.Fatalf("migration reapplied: %+v %v", again, err)
			}
			input := VoiceSettingsInput{Revision: after.Revision, ProviderMode: after.ProviderMode, VoiceID: after.VoiceID, Rate: after.Rate, PitchHz: after.PitchHz, AmountBands: after.AmountBands, ChimeEnabled: after.ChimeEnabled, LoadingSoundEnabled: after.LoadingSoundEnabled, FunEnabled: after.FunEnabled, IncludeDescription: after.IncludeDescription, CustomTemplate: after.CustomTemplate}
			if fixture.changed {
				stale := input
				stale.Revision = before.Revision
				if _, err := store.SaveVoiceProfile(ctx, stale); !errors.Is(err, ErrVoiceSettingsConflict) {
					t.Fatalf("stale cutover save: %v", err)
				}
			}
			saved, err := store.SaveVoiceProfile(ctx, input)
			if err != nil || saved.Revision != after.Revision+1 {
				t.Fatalf("current revision save: %+v %v", saved, err)
			}
			if _, err := store.SaveVoiceProfile(ctx, input); !errors.Is(err, ErrVoiceSettingsConflict) {
				t.Fatalf("stale save: %v", err)
			}
		})
	}
}

func voiceStoreAtSchema22(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	store, err := openInternal(ctx, filepath.Join(t.TempDir(), "pre23.db"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	err = store.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
			return err
		}
		for _, m := range migrations {
			if m.version > 22 {
				break
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, m.version, m.checksum, "2026-10-22T00:00:00Z"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestVieneuMigrationDoesNotCreateSingleton(t *testing.T) {
	store := voiceStoreAtSchema22(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_settings`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected seeded profile: %d %v", count, err)
	}
	profile, err := store.GetVoiceProfile(ctx)
	if err != nil || !reflect.DeepEqual(profile, DefaultVoiceProfile) {
		t.Fatalf("missing singleton default: %+v %v", profile, err)
	}
}

func TestVieneuOnlyProfileValidationAndSynthesisKey(t *testing.T) {
	for _, fixture := range []struct {
		mode, voice string
		rate        float64
		pitch       int
		valid       bool
	}{
		{ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1, 0, true},
		{"PIPER_ONLY", "vi-VN-vais1000-medium", 1, 0, false},
		{"EDGE_ONLY", "vi-VN-HoaiMyNeural", 1, 0, false},
		{"BROWSER_ONLY", VoiceIDVieneuMaiAnh, 1, 0, false},
		{"ONLINE_AUTO", VoiceIDVieneuMaiAnh, 1, 0, false},
		{"unknown", VoiceIDVieneuMaiAnh, 1, 0, false},
		{ProviderModeVieneuOnly, "unknown", 1, 0, false},
		{ProviderModeVieneuOnly, "vi-VN-vais1000-medium", 1, 0, false},
		{ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1.25, 0, false},
		{ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, 1, 1, false},
		{ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, math.NaN(), 0, false},
		{ProviderModeVieneuOnly, VoiceIDVieneuMaiAnh, math.Inf(1), 0, false},
	} {
		input := VoiceSettingsInput{ProviderMode: fixture.mode, VoiceID: fixture.voice, Rate: fixture.rate, PitchHz: fixture.pitch, AmountBands: defaultAmountBands()}
		err := ValidateVoiceProfileInput(&input)
		if (err == nil) != fixture.valid {
			t.Errorf("%+v: %v", fixture, err)
		}
		if fixture.mode != ProviderModeVieneuOnly && (err == nil || err.Error() != "invalid providerMode: must be VIENEU_ONLY") {
			t.Errorf("invalid mode error: %v", err)
		}
		provider := (VoiceProfile{ProviderMode: fixture.mode}).SynthesisProvider()
		if fixture.mode == ProviderModeVieneuOnly && provider != "vieneu" || fixture.mode != ProviderModeVieneuOnly && provider != "" {
			t.Errorf("provider for %q: %q", fixture.mode, provider)
		}
	}
	hash := sha256.Sum256([]byte("vieneu|vi-VN-vieneu-v3-turbo-mai-anh-v1|1.00|0|money-vi-v1|pack-schema-2"))
	if got := ComputeSynthesisKey(VoiceIDVieneuMaiAnh, 1, 0); got != hex.EncodeToString(hash[:]) || got != "b5a2b7b8dec26c5ec21c3f8a7d91fa33ee75bcc587b51f444a6dbcb6bb34fcf0" {
		t.Fatalf("canonical synthesis key changed: %s", got)
	}
}
