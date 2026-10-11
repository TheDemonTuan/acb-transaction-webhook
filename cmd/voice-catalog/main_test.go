package main

import (
	"encoding/json"
	"reflect"
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
	profile := voicecopy.AudioInputProfile{VoiceID: "vi-VN-NamMinhNeural", Rate: 1.25, PitchHz: 0}
	inputs, err := voicecopy.ExportAudioInputs(profile)
	if err != nil {
		t.Fatal(err)
	}
	if inputs.SchemaVersion != 1 || inputs.FormatterVersion != "money-vi-v1" || inputs.CatalogVersion != "v1" || len(inputs.Clips) != 740 || len(inputs.Amounts) != 20 {
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
	if kinds["amount"] != 20 || kinds["classic"] != 120 || kinds["tail-fragment"] != 600 {
		t.Fatalf("wrong kinds: %v", kinds)
	}
	entry := inputs.Amounts["500000"]
	if seen[entry.Standard].Transcript != voicecopy.StandardAnnouncement(500000) || len(entry.Classic) != 6 {
		t.Fatal("amount mapping mismatch")
	}
	catalog := compileSource(t)
	for _, phrase := range catalog.Classic {
		clip := seen[entry.Classic[phrase.ID]]
		if clip.Transcript != voicecopy.FormatAnnouncementTemplate(phrase.Template, 500000, "", false) {
			t.Fatalf("classic transcript changed: %s", phrase.ID)
		}
	}
	other, err := voicecopy.ExportAudioInputs(voicecopy.AudioInputProfile{VoiceID: "vi-VN-HoaiMyNeural", Rate: 1.25, PitchHz: 0})
	if err != nil {
		t.Fatal(err)
	}
	if inputs.SynthesisKey == other.SynthesisKey {
		t.Fatal("different voices share synthesis key")
	}
	piper, err := voicecopy.ExportAudioInputs(voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDPiperVais1000, Rate: 1.25, PitchHz: 0})
	if err != nil {
		t.Fatal(err)
	}
	if piper.VoiceID != storage.VoiceIDPiperVais1000 || piper.SynthesisKey != storage.ComputeSynthesisKey(storage.VoiceIDPiperVais1000, 1.25, 0) ||
		piper.SchemaVersion != 1 || len(piper.Clips) != 740 || !reflect.DeepEqual(piper.Clips, inputs.Clips) || !reflect.DeepEqual(piper.Amounts, inputs.Amounts) {
		t.Fatal("Piper pack must export the same exact 740 Go transcripts under its own synthesis key")
	}
	if _, err := voicecopy.ExportAudioInputs(voicecopy.AudioInputProfile{VoiceID: storage.VoiceIDPiperVais1000, Rate: 1.25, PitchHz: 1}); err == nil {
		t.Fatal("Piper pitch override accepted")
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
