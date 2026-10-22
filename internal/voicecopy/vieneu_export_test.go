package voicecopy_test

import (
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
)

func TestFormatSpokenAnnouncementTemplate(t *testing.T) {
	// Template with amount_raw and trailing currency "đồng"
	tpl := "Đã nhận {amount_raw} đồng."
	spoken := voicecopy.FormatSpokenAnnouncementTemplate(tpl, 105000, "", false)
	expected := "Đã nhận một trăm linh năm nghìn đồng."
	if spoken != expected {
		t.Fatalf("expected %q, got %q", expected, spoken)
	}

	// Boundary test: 1.005.000
	spokenMillion := voicecopy.FormatSpokenAnnouncementTemplate(tpl, 1005000, "", false)
	expectedMillion := "Đã nhận một triệu không trăm linh năm nghìn đồng."
	if spokenMillion != expectedMillion {
		t.Fatalf("expected %q, got %q", expectedMillion, spokenMillion)
	}
}

func TestVieneuAudioInputsExport(t *testing.T) {
	profile := voicecopy.AudioInputProfile{
		VoiceID: storage.VoiceIDVieneuMaiAnh,
		Rate:    1.00,
		PitchHz: 0,
	}

	inputs, err := voicecopy.ExportAudioInputs(profile, voicecopy.AudioInputOptions{})
	if err != nil {
		t.Fatalf("unexpected error exporting inputs: %v", err)
	}

	// 20 base amounts * (1 standard + 6 classic) = 140 clips
	if len(inputs.Clips) != 140 {
		t.Fatalf("expected 140 clips, got %d", len(inputs.Clips))
	}

	// Check synthesis key format
	expectedKey := storage.ComputeSynthesisKey(storage.VoiceIDVieneuMaiAnh, 1.00, 0)
	if inputs.SynthesisKey != expectedKey {
		t.Fatalf("synthesis key mismatch: expected %s, got %s", expectedKey, inputs.SynthesisKey)
	}

	// Number segments must be rejected for VieNeu
	_, err = voicecopy.ExportAudioInputs(profile, voicecopy.AudioInputOptions{IncludeNumberSegments: true})
	if err == nil {
		t.Fatal("expected error requesting number segments for VieNeu")
	}

	// Rate != 1.00 must be rejected
	invalidProfile := profile
	invalidProfile.Rate = 1.25
	_, err = voicecopy.ExportAudioInputs(invalidProfile, voicecopy.AudioInputOptions{})
	if err == nil {
		t.Fatal("expected error for rate != 1.00")
	}
}
