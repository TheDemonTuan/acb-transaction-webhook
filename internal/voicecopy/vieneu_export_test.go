package voicecopy_test

import (
	"math"
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

func TestSpokenCustomTemplatePreservesSanitizedDescription(t *testing.T) {
	for _, tc := range []struct {
		name        string
		template    string
		description string
		include     bool
		want        string
	}{
		{
			name:        "spoken raw amount with description",
			template:    "Đã nhận {amount_raw} đồng từ {description}.",
			description: "Nguyen Van A https://example.com/pay",
			include:     true,
			want:        "Đã nhận một trăm linh năm nghìn đồng từ Nguyen Van A.",
		},
		{
			name:        "disabled description stays private",
			template:    "Đã nhận {amount_raw} đồng. Nội dung: {description}.",
			description: "Secret",
			want:        "Đã nhận một trăm linh năm nghìn đồng.",
		},
		{
			name:     "custom text remains intact",
			template: "Cửa hàng cảm ơn {amount}. Hẹn gặp lại!",
			want:     "Cửa hàng cảm ơn một trăm linh năm nghìn đồng. Hẹn gặp lại!",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := voicecopy.FormatSpokenAnnouncementTemplate(tc.template, 105000, tc.description, tc.include); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
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
}

func TestAudioInputsRejectNonVieneuProfilesAndSegments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile voicecopy.AudioInputProfile
		options voicecopy.AudioInputOptions
	}{
		{name: "retired Hoai My", profile: voicecopy.AudioInputProfile{VoiceID: "vi-VN-HoaiMyNeural", Rate: 1.00}},
		{name: "retired Nam Minh", profile: voicecopy.AudioInputProfile{VoiceID: "vi-VN-NamMinhNeural", Rate: 1.00}},
		{name: "retired Piper", profile: voicecopy.AudioInputProfile{VoiceID: "vi-VN-vais1000-medium", Rate: 1.00}},
		{name: "missing voice", profile: voicecopy.AudioInputProfile{Rate: 1.00}},
		{name: "changed rate", profile: voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDVieneuMaiAnh, Rate: 1.25}},
		{name: "non-finite rate", profile: voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDVieneuMaiAnh, Rate: math.NaN()}},
		{name: "changed pitch", profile: voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDVieneuMaiAnh, Rate: 1.00, PitchHz: 1}},
		{name: "number segments", profile: voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDVieneuMaiAnh, Rate: 1.00}, options: voicecopy.AudioInputOptions{IncludeNumberSegments: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := voicecopy.ExportAudioInputs(tc.profile, tc.options); err == nil {
				t.Fatal("unsupported export accepted")
			}
		})
	}
}
