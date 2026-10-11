package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPiperMigrationPreservesProfileAtomically(t *testing.T) {
	for _, mode := range []string{ProviderModeEdgeOnly, ProviderModeBrowserOnly} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, err := openInternal(ctx, filepath.Join(t.TempDir(), "pre21.db"), OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			err = store.withTx(ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
					return err
				}
				for _, migration := range migrations {
					if migration.version > 20 {
						break
					}
					if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, migration.version, migration.checksum, "2026-10-10T00:00:00Z"); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			bands := defaultAmountBands()
			cutoff := "45000"
			bands[0].ToVND = &cutoff
			bands[1].FromVND = cutoff
			bands[2].StyleID = "custom"
			raw, err := json.Marshal(bands)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.db.ExecContext(ctx, `INSERT INTO voice_settings(id,revision,provider_mode,edge_voice,rate,pitch_hz,amount_bands_json,chime_enabled,loading_sound_enabled,fun_enabled,include_description,custom_template,online_fallback,updated_at) VALUES('singleton',17,?,?,1.20,12,?,0,1,0,1,'Cảm ơn {amount}.',0,'2026-10-10T00:00:00Z')`, mode, VoiceIDNamMinh, string(raw))
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.GetVoiceProfile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == ProviderModeEdgeOnly {
				if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_voice_cutover BEFORE UPDATE ON voice_settings BEGIN SELECT RAISE(ABORT,'injected cutover failure'); END`); err != nil {
					t.Fatal(err)
				}
				if err := store.Migrate(ctx); err == nil {
					t.Fatal("expected migration rollback")
				}
				afterFailure, err := store.GetVoiceProfile(ctx)
				if err != nil || !reflect.DeepEqual(before, afterFailure) {
					t.Fatalf("partial profile mutation: %+v err=%v", afterFailure, err)
				}
				var registered int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=21`).Scan(&registered); err != nil || registered != 0 {
					t.Fatalf("failed cutover registered: %d %v", registered, err)
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
			if mode == ProviderModeEdgeOnly {
				want.ProviderMode = ProviderModePiperOnly
				want.VoiceID = VoiceIDPiperVais1000
				want.PitchHz = 0
				want.Revision++
				want.SynthesisKey = ComputeSynthesisKey(want.VoiceID, want.Rate, 0)
				want.UpdatedAt = after.UpdatedAt
			}
			if !reflect.DeepEqual(want, after) {
				t.Fatalf("profile not preserved: want=%+v got=%+v", want, after)
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			again, err := store.GetVoiceProfile(ctx)
			if err != nil || !reflect.DeepEqual(after, again) {
				t.Fatalf("migration reapplied: %+v %v", again, err)
			}
			var checksum string
			if err := store.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=21`).Scan(&checksum); err != nil || checksum != "2026-10-11-v21-piper-voice" {
				t.Fatalf("registration: %s %v", checksum, err)
			}
		})
	}
}

func TestExplicitProviderVoiceValidationAndSynthesisKeys(t *testing.T) {
	for _, fixture := range []struct {
		mode, voice string
		pitch       int
		valid       bool
	}{
		{ProviderModePiperOnly, VoiceIDPiperVais1000, 0, true},
		{ProviderModePiperOnly, VoiceIDPiperVais1000, 1, false},
		{ProviderModePiperOnly, VoiceIDHoaiMy, 0, false},
		{ProviderModeEdgeOnly, VoiceIDPiperVais1000, 0, false},
		{ProviderModeEdgeOnly, VoiceIDHoaiMy, -10, true},
		{ProviderModeEdgeOnly, VoiceIDNamMinh, 10, true},
		{ProviderModeBrowserOnly, VoiceIDPiperVais1000, 10, true},
		{"ONLINE_AUTO", VoiceIDHoaiMy, 0, false},
	} {
		in := VoiceSettingsInput{ProviderMode: fixture.mode, VoiceID: fixture.voice, Rate: 1.25, PitchHz: fixture.pitch, AmountBands: defaultAmountBands()}
		err := ValidateVoiceProfileInput(&in)
		if (err == nil) != fixture.valid {
			t.Errorf("%+v: %v", fixture, err)
		}
	}
	for _, fixture := range []struct{ voice, provider string }{{VoiceIDPiperVais1000, "piper"}, {VoiceIDHoaiMy, "edge"}, {VoiceIDNamMinh, "edge"}} {
		hash := sha256.Sum256([]byte(fixture.provider + "|" + fixture.voice + "|1.25|0|money-vi-v1|pack-schema-2"))
		if got := ComputeSynthesisKey(fixture.voice, 1.25, 0); got != hex.EncodeToString(hash[:]) {
			t.Errorf("wrong synthesis key for %s: %s", fixture.voice, got)
		}
	}
}
