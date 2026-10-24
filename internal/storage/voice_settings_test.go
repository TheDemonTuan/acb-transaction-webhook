package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVoiceProfileLifecycle(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_voice_settings.db")

	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// 1. Initial read should return default profile
	profile, err := store.GetVoiceProfile(ctx)
	if err != nil {
		t.Fatalf("GetVoiceProfile failed: %v", err)
	}
	if profile.Revision != 1 {
		t.Errorf("expected revision 1, got %d", profile.Revision)
	}
	if profile.ProviderMode != ProviderModeVieneuOnly {
		t.Errorf("expected %s, got %s", ProviderModeVieneuOnly, profile.ProviderMode)
	}
	if profile.VoiceID != VoiceIDVieneuMaiAnh {
		t.Errorf("expected %s, got %s", VoiceIDVieneuMaiAnh, profile.VoiceID)
	}
	if profile.Rate != 1.00 {
		t.Errorf("expected rate 1.00, got %f", profile.Rate)
	}
	if profile.PitchHz != 0 {
		t.Errorf("expected pitchHz 0, got %d", profile.PitchHz)
	}
	if len(profile.AmountBands) != 4 {
		t.Fatalf("expected 4 amount bands, got %d", len(profile.AmountBands))
	}
	if profile.SynthesisKey == "" {
		t.Error("expected non-empty synthesisKey")
	}

	// 2. Save modified settings
	input := VoiceSettingsInput{
		Revision:            profile.Revision,
		ProviderMode:        ProviderModeVieneuOnly,
		VoiceID:             VoiceIDVieneuMaiAnh,
		Rate:                1.00,
		PitchHz:             0,
		AmountBands:         profile.AmountBands,
		ChimeEnabled:        false,
		LoadingSoundEnabled: false,
		FunEnabled:          true,
		IncludeDescription:  false,
		CustomTemplate:      "",
	}
	saved, err := store.SaveVoiceProfile(ctx, input)
	if err != nil {
		t.Fatalf("SaveVoiceProfile failed: %v", err)
	}
	if saved.Revision != 2 {
		t.Errorf("expected revision 2, got %d", saved.Revision)
	}
	if saved.VoiceID != VoiceIDVieneuMaiAnh {
		t.Errorf("expected %s, got %s", VoiceIDVieneuMaiAnh, saved.VoiceID)
	}
	if saved.Rate != 1.00 || saved.PitchHz != 0 {
		t.Errorf("unexpected rate/pitch: %f / %d", saved.Rate, saved.PitchHz)
	}
	if saved.ChimeEnabled {
		t.Error("updated chime setting was not saved")
	}

	// 3. Stale revision save should conflict
	stale := input
	stale.Revision = 1
	_, err = store.SaveVoiceProfile(ctx, stale)
	if err != ErrVoiceSettingsConflict {
		t.Errorf("expected ErrVoiceSettingsConflict, got %v", err)
	}

	// 4. Invalid rate / pitch / bands should fail validation
	invalidRate := input
	invalidRate.Revision = saved.Revision
	invalidRate.Rate = 0.50
	if _, err = store.SaveVoiceProfile(ctx, invalidRate); err == nil {
		t.Error("expected error for rate < 0.75")
	}

	invalidPitch := input
	invalidPitch.Revision = saved.Revision
	invalidPitch.PitchHz = 100
	if _, err = store.SaveVoiceProfile(ctx, invalidPitch); err == nil {
		t.Error("expected error for pitch > 50")
	}

	invalidVoice := input
	invalidVoice.Revision = saved.Revision
	invalidVoice.VoiceID = "en-US-Jenny"
	if _, err = store.SaveVoiceProfile(ctx, invalidVoice); err == nil {
		t.Error("expected error for invalid voiceId")
	}

	invalidTemplate := input
	invalidTemplate.Revision = saved.Revision
	invalidTemplate.CustomTemplate = "Nhận {amount} từ {sender_name}" // unknown placeholder
	if _, err = store.SaveVoiceProfile(ctx, invalidTemplate); err == nil {
		t.Error("expected error for invalid customTemplate placeholder")
	}
}

func TestGetVoiceProfileDefaultSnapshotIsolation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "voice_profile.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Migrations seed the singleton; remove it to exercise the no-row default.
	if _, err := store.db.ExecContext(ctx, "DELETE FROM voice_settings"); err != nil {
		t.Fatalf("delete voice profile: %v", err)
	}
	first, err := store.GetVoiceProfile(ctx)
	if err != nil {
		t.Fatalf("get first default: %v", err)
	}
	second, err := store.GetVoiceProfile(ctx)
	if err != nil {
		t.Fatalf("get second default: %v", err)
	}
	if !reflect.DeepEqual(first, DefaultVoiceProfile) || !reflect.DeepEqual(second, DefaultVoiceProfile) {
		t.Fatalf("no-row profiles must match the default: first=%+v second=%+v", first, second)
	}
	if len(first.AmountBands) != 4 {
		t.Fatalf("expected 4 default amount bands, got %d", len(first.AmountBands))
	}
	for i := range first.AmountBands {
		first.AmountBands[i].ID = "changed"
		first.AmountBands[i].FromVND = "2"
		first.AmountBands[i].StyleID = "custom"
		if first.AmountBands[i].ToVND != nil {
			*first.AmountBands[i].ToVND = "3"
		}
	}
	wantBands := defaultAmountBands()
	if !reflect.DeepEqual(second.AmountBands, wantBands) {
		t.Errorf("mutating one snapshot changed another: %+v", second.AmountBands)
	}
	if !reflect.DeepEqual(DefaultVoiceProfile.AmountBands, wantBands) {
		t.Errorf("mutating one snapshot changed the default: %+v", DefaultVoiceProfile.AmountBands)
	}
	third, err := store.GetVoiceProfile(ctx)
	if err != nil {
		t.Fatalf("get default after mutation: %v", err)
	}
	if !reflect.DeepEqual(third.AmountBands, wantBands) {
		t.Errorf("mutating one snapshot changed a later read: %+v", third.AmountBands)
	}
}
