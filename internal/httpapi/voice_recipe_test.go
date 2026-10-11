package httpapi

import (
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
)

func TestVoiceRecipeUsesResolvedBandAndAmount(t *testing.T) {
	profile := storage.DefaultVoiceProfile
	profile.AmountBands = append([]storage.AmountBand(nil), profile.AmountBands...)
	profile.AmountBands[0].StyleID = "custom"
	profile.CustomTemplate = "Đã nhận {amount}. Cảm ơn bạn."
	server := &Server{}
	small, err := server.resolveVoicePhrase(profile, 49999, "", "", "full")
	if err != nil || !strings.Contains(small, voicecopy.SpeakVND(49999)) || !strings.Contains(small, "Cảm ơn bạn") {
		t.Fatalf("custom band result=%q error=%v", small, err)
	}
	next, err := server.resolveVoicePhrase(profile, 50000, "", "classic-001", "full")
	if err != nil || !strings.Contains(next, voicecopy.SpeakVND(50000)) || strings.Contains(next, "Cảm ơn bạn") {
		t.Fatalf("upper-exclusive band result=%q error=%v", next, err)
	}
	for _, amount := range []int64{1, 49999, 50000, 200000, 1000000} {
		_, err := server.resolveVoicePhrase(profile, amount, "", "not-a-recipe", "full")
		if err == nil {
			t.Fatalf("unknown recipe accepted for %d", amount)
		}
	}
	if _, err := server.resolveVoicePhrase(profile, 49999, "", "classic-001", "full"); err == nil {
		t.Fatal("classic accepted in custom band")
	}
	profile.FunEnabled = false
	if _, err := server.resolveVoicePhrase(profile, 50000, "", "classic-001", "full"); err == nil {
		t.Fatal("recipe accepted when fun disabled")
	}
	onlyAmount, err := server.resolveVoicePhrase(profile, 50000, "secret description", "", "full")
	if err != nil || !strings.Contains(onlyAmount, voicecopy.SpeakVND(50000)) || strings.Contains(onlyAmount, "secret") {
		t.Fatalf("amount-only result=%q error=%v", onlyAmount, err)
	}
	tail, err := server.resolveVoicePhrase(profile, 50000, "", "", "tail")
	if err != nil || tail != "" {
		t.Fatalf("disabled tail result=%q error=%v", tail, err)
	}
}
