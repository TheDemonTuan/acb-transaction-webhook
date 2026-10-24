package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
)

func compileSource(t *testing.T) voicecopy.VoiceCatalog {
	t.Helper()
	catalog, err := voicecopy.CompileCatalog(voicecopy.CatalogSourceBytes())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestCatalogCountsAndWholeTailUniqueness(t *testing.T) {
	catalog := compileSource(t)
	if len(catalog.Fragments) != 600 || len(catalog.Recipes) != 10006 || len(catalog.Classic) != 6 {
		t.Fatalf("unexpected counts: %d fragments, %d recipes, %d classic", len(catalog.Fragments), len(catalog.Recipes), len(catalog.Classic))
	}
	seen, groups := map[string]bool{}, map[string]int{}
	for _, batch := range voicecopy.BuildCatalogReviewBatches(catalog) {
		if len(batch.Recipes) != 125 {
			t.Fatalf("batch %s count %d", batch.ID, len(batch.Recipes))
		}
		for _, recipe := range batch.Recipes {
			text := voicecopy.NormalizeCatalogText(recipe.Text)
			if seen[text] || utf8.RuneCountInString(text) > 120 {
				t.Fatalf("duplicate or oversized tail %s", recipe.ID)
			}
			if strings.Count(text, ".") != 2 {
				t.Fatalf("tail must have two complete sentences: %s", recipe.ID)
			}
			seen[text] = true
			groups[batch.StyleID+"-"+batch.BandID]++
		}
	}
	if len(seen) != 10000 || len(groups) != 8 {
		t.Fatalf("counts: unique=%d groups=%d", len(seen), len(groups))
	}
	for group, count := range groups {
		if count != 1250 {
			t.Fatalf("group %s count %d", group, count)
		}
	}
}

func TestCatalogNormalizationAndDuplicateRejection(t *testing.T) {
	if got := voicecopy.NormalizeCatalogText("  Ca\u0309m\tơn  bạn!  "); got != "Cảm ơn bạn." {
		t.Fatalf("normalization: %q", got)
	}
	var source voicecopy.CatalogSource
	if err := json.Unmarshal(voicecopy.CatalogSourceBytes(), &source); err != nil {
		t.Fatal(err)
	}
	source.Groups[0].Openings[1] = "  " + source.Groups[0].Openings[0] + "!  "
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := voicecopy.CompileCatalog(raw); err == nil {
		t.Fatal("normalized duplicate accepted")
	}
}

func TestCatalogRejectsUnsafeAndOversizedFragments(t *testing.T) {
	for _, text := range []string{"Hãy chuyển thêm nhé.", "Tài khoản vừa tới.", "Vui 123.", "Câu đầu. Câu sau.", strings.Repeat("a", 71)} {
		var source voicecopy.CatalogSource
		if err := json.Unmarshal(voicecopy.CatalogSourceBytes(), &source); err != nil {
			t.Fatal(err)
		}
		source.Groups[0].Openings[0] = text
		raw, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := voicecopy.CompileCatalog(raw); err == nil {
			t.Errorf("accepted invalid text %q", text)
		}
	}
}

func TestReviewGateRequiresEveryFullBatchAndExactDigest(t *testing.T) {
	catalog := compileSource(t)
	evidence := voicecopy.CatalogReviewEvidence{Version: "v1", Batches: []voicecopy.CatalogReviewDecision{}}
	for _, batch := range voicecopy.BuildCatalogReviewBatches(catalog) {
		evidence.Batches = append(evidence.Batches, voicecopy.CatalogReviewDecision{ID: batch.ID, SHA256: voicecopy.CatalogBatchDigest(batch), RecipeCount: len(batch.Recipes), Decision: "pending", RejectedRecipeIDs: []string{}})
	}
	if err := voicecopy.ValidateCatalogReview(catalog, evidence, false); err != nil {
		t.Fatal(err)
	}
	if err := voicecopy.ValidateCatalogReview(catalog, evidence, true); err == nil {
		t.Fatal("unreviewed catalog passed release gate")
	}
	// Synthetic approval exercises the validator only; it is not release evidence or semantic review.
	for i := range evidence.Batches {
		evidence.Batches[i].Decision = "approved"
		evidence.Batches[i].Reviewer = "test-only"
		evidence.Batches[i].Notes = "validator fixture, not semantic approval"
	}
	evidence.ApprovedUnique = 10000
	if err := voicecopy.ValidateCatalogReview(catalog, evidence, true); err != nil {
		t.Fatal(err)
	}
	evidence.Batches[0].SHA256 = strings.Repeat("0", 64)
	if err := voicecopy.ValidateCatalogReview(catalog, evidence, true); err == nil {
		t.Fatal("stale batch digest accepted")
	}
}

func TestAmountFixturesAndAudioInputContract(t *testing.T) {
	if err := voicecopy.CheckAmountFixtures(); err != nil {
		t.Fatal(err)
	}
	profile := voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDVieneuMaiAnh, Rate: 1.00, PitchHz: 0}
	inputs, err := voicecopy.ExportAudioInputs(profile, voicecopy.AudioInputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inputs.SchemaVersion != 2 || inputs.FormatterVersion != "money-vi-v1" || inputs.CatalogVersion != "v1" || len(inputs.Amounts) != 20 || inputs.SynthesisKey != storage.ComputeSynthesisKey(profile.VoiceID, profile.Rate, profile.PitchHz) {
		t.Fatalf("unexpected input metadata/counts")
	}
	seen := map[string]voicecopy.AudioInputClip{}
	kinds := map[string]int{}
	for _, clip := range inputs.Clips {
		if _, ok := seen[clip.ClipID]; ok {
			t.Fatalf("duplicate clip %s", clip.ClipID)
		}
		seen[clip.ClipID] = clip
		kinds[clip.Kind]++
	}
	if kinds["amount"] != 20 || kinds["classic"] != 120 || len(kinds) != 2 || len(inputs.Clips) != 140 {
		t.Fatalf("wrong kinds: %v", kinds)
	}
	entry := inputs.Amounts["500000"]
	if seen[entry.Standard].Transcript != voicecopy.StandardAnnouncement(500000) || len(entry.Classic) != 6 {
		t.Fatal("amount mapping mismatch")
	}
	catalog := compileSource(t)
	for _, phrase := range catalog.Classic {
		clip := seen[entry.Classic[phrase.ID]]
		if clip.Transcript != voicecopy.FormatSpokenAnnouncementTemplate(phrase.Template, 500000, "", false) {
			t.Fatalf("classic transcript changed: %s", phrase.ID)
		}
	}
	if len(inputs.PreloadClipIDs) != 6 {
		t.Fatalf("expected six whole-utterance preloads, got %d", len(inputs.PreloadClipIDs))
	}
	for _, id := range inputs.PreloadClipIDs {
		if seen[id].Kind != "amount" {
			t.Fatalf("preload %s is not a whole amount announcement", id)
		}
	}
	raw, err := voicecopy.MarshalCatalog(inputs)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"numberSegments", "numberPrefix", "numberSuffix"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("whole-utterance export contains %s", field)
		}
	}
}

func TestAudioInputAmountCapPreservesBaseAndRanking(t *testing.T) {
	profile := voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDVieneuMaiAnh, Rate: 1.00}
	base, err := voicecopy.ExportAudioInputs(profile, voicecopy.AudioInputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ranked := []int64{9007199254740991}
	for value := int64(1); value <= 150; value++ {
		ranked = append(ranked, value)
	}
	pack, err := voicecopy.ExportAudioInputs(profile, voicecopy.AudioInputOptions{Amounts: ranked, PreloadAmounts: []int64{9007199254740991, 1, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Amounts) != 128 || pack.Amounts["9007199254740991"].Standard == "" || pack.Amounts["150"].Standard != "" {
		t.Fatal("amount cap did not preserve highest-ranked extra amounts")
	}
	for amount := range base.Amounts {
		if pack.Amounts[amount].Standard == "" || len(pack.Amounts[amount].Classic) != 6 {
			t.Fatalf("required base amount %s lost at cap", amount)
		}
	}
	seen := map[string]bool{}
	for _, id := range pack.PreloadClipIDs {
		if seen[id] {
			t.Fatalf("duplicate preload %s", id)
		}
		seen[id] = true
	}
	if _, err := voicecopy.ExportAudioInputs(profile, voicecopy.AudioInputOptions{PreloadAmounts: []int64{123}}); err == nil {
		t.Fatal("unretained preload was accepted")
	}
}

func TestClassicValidationDoesNotDependOnNewCatalogApproval(t *testing.T) {
	if err := voicecopy.ValidateRecipe("classic-001", "classic", "small"); err != nil {
		t.Fatal(err)
	}
	if err := voicecopy.ValidateRecipe("classic-006", "classic", "celebrate"); err != nil {
		t.Fatal(err)
	}
	if err := voicecopy.ValidateRecipe("classic-001", "cute", "small"); err == nil {
		t.Fatal("classic recipe accepted for cute")
	}
	if err := voicecopy.ValidateRecipe("classic-001", "classic", "unknown"); err == nil {
		t.Fatal("unknown band accepted")
	}
}

func TestReadProfileRequiresFixedVieneuProfile(t *testing.T) {
	valid := map[string]any{
		"providerMode":         "VIENEU_ONLY",
		"voiceId":              storage.VoiceIDVieneuMaiAnh,
		"rate":                 1.00,
		"pitchHz":              0,
		"synthesisKey":         storage.ComputeSynthesisKey(storage.VoiceIDVieneuMaiAnh, 1.00, 0),
		"catalogVersion":       voicecopy.CatalogVersion,
		"revision":             3,
		"coldRequestTimeoutMs": 8000,
	}
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{name: "fixed profile"},
		{name: "missing provider", field: "providerMode", value: ""},
		{name: "retired Edge", field: "providerMode", value: "EDGE_ONLY"},
		{name: "retired Piper", field: "providerMode", value: "PIPER_ONLY"},
		{name: "retired browser", field: "providerMode", value: "BROWSER_ONLY"},
		{name: "retired voice", field: "voiceId", value: "vi-VN-vais1000-medium"},
		{name: "changed rate", field: "rate", value: 1.25},
		{name: "changed pitch", field: "pitchHz", value: 1},
		{name: "unsupported catalog", field: "catalogVersion", value: "v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := make(map[string]any, len(valid))
			for key, value := range valid {
				fixture[key] = value
			}
			if tc.field != "" {
				fixture[tc.field] = tc.value
			}
			raw, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "profile.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			profile, key, err := readProfile(path)
			if tc.field != "" {
				if err == nil {
					t.Fatal("unsupported profile accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if profile.VoiceID != storage.VoiceIDVieneuMaiAnh || profile.Rate != 1.00 || profile.PitchHz != 0 || key != valid["synthesisKey"] {
				t.Fatalf("resolved profile changed: %+v, key %s", profile, key)
			}
		})
	}
}
