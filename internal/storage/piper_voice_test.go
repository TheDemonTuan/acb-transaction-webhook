package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPiperMigrationPreservesProfileAtomically(t *testing.T) {
	for _, mode := range []string{"EDGE_ONLY", "BROWSER_ONLY"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, err := openInternal(ctx, filepath.Join(t.TempDir(), "pre21.db"), OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			// This history regression intentionally stops at schema 21.
			migrate21 := func() error {
				return store.withTx(ctx, func(tx *sql.Tx) error {
					var registered int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=21`).Scan(&registered); err != nil {
						return err
					}
					if registered != 0 {
						return nil
					}
					m := migrations[20]
					if _, err := tx.ExecContext(ctx, m.sql); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, m.version, m.checksum, "2026-10-11T00:00:00Z")
					return err
				})
			}
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
			_, err = store.db.ExecContext(ctx, `INSERT INTO voice_settings(id,revision,provider_mode,edge_voice,rate,pitch_hz,amount_bands_json,chime_enabled,loading_sound_enabled,fun_enabled,include_description,custom_template,online_fallback,updated_at) VALUES('singleton',17,?,?,1.20,12,?,0,1,0,1,'Cảm ơn {amount}.',0,'2026-10-10T00:00:00Z')`, mode, "vi-VN-NamMinhNeural", string(raw))
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.GetVoiceProfile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "EDGE_ONLY" {
				if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_voice_cutover BEFORE UPDATE ON voice_settings BEGIN SELECT RAISE(ABORT,'injected cutover failure'); END`); err != nil {
					t.Fatal(err)
				}
				if err := migrate21(); err == nil {
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
			if err := migrate21(); err != nil {
				t.Fatal(err)
			}
			after, err := store.GetVoiceProfile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			if mode == "EDGE_ONLY" {
				want.ProviderMode = "PIPER_ONLY"
				want.VoiceID = "vi-VN-vais1000-medium"
				want.PitchHz = 0
				want.Revision++
				want.SynthesisKey = ComputeSynthesisKey(want.VoiceID, want.Rate, 0)
				want.UpdatedAt = after.UpdatedAt
			}
			if !reflect.DeepEqual(want, after) {
				t.Fatalf("profile not preserved: want=%+v got=%+v", want, after)
			}
			if err := migrate21(); err != nil {
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
