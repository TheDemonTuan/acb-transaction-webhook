package voicecopy

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const CatalogVersion = "v1"
const FormatterVersion = "money-vi-v1"
const CatalogMaxAmount int64 = 9007199254740991
const AmountCoreTemplate = "Đã nhận {amount}."

//go:embed catalog/catalog.v1.json catalog/source.v1.json catalog/amount-fixtures.v1.json catalog/review.v1.json
var catalogFiles embed.FS

type CatalogFragment struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	StyleID string `json:"styleId"`
	BandID  string `json:"bandId"`
}

type CatalogRecipe struct {
	ID        string `json:"id"`
	StyleID   string `json:"styleId"`
	BandID    string `json:"bandId"`
	OpeningID string `json:"openingId"`
	ClosingID string `json:"closingId"`
	Weight    int    `json:"weight"`
}

type ClassicPhrase struct {
	ID       string `json:"id"`
	Template string `json:"template"`
}

type VoiceCatalog struct {
	Version    string            `json:"version"`
	AmountCore string            `json:"amountCore"`
	Fragments  []CatalogFragment `json:"fragments"`
	Recipes    []CatalogRecipe   `json:"recipes"`
	Classic    []ClassicPhrase   `json:"classic"`
}

type CatalogSourceGroup struct {
	StyleID  string   `json:"styleId"`
	BandID   string   `json:"bandId"`
	Openings []string `json:"openings"`
	Closings []string `json:"closings"`
}

type CatalogSource struct {
	Version string               `json:"version"`
	Classic []ClassicPhrase      `json:"classic"`
	Groups  []CatalogSourceGroup `json:"groups"`
}

var classicPhrases = [...]ClassicPhrase{
	{"classic-001", "Đa tạ quý khách vì {amount}."},
	{"classic-002", "Ting ting, nhận {amount}, vui cả ngày!"},
	{"classic-003", "Đã nhận {amount}, ví cười tươi rồi!"},
	{"classic-004", "{amount} đã về, quá là xịn!"},
	{"classic-005", "Cảm ơn {amount}, lên tinh thần nào!"},
	{"classic-006", "Nhận {amount}, hôm nay có lộc!"},
}

// NormalizeCatalogText defines the uniqueness boundary: NFC, whitespace and final punctuation.
func NormalizeCatalogText(text string) string {
	text = strings.Join(strings.Fields(norm.NFC.String(text)), " ")
	text = strings.TrimSpace(strings.TrimRight(text, ".!?…"))
	if text == "" {
		return ""
	}
	return text + "."
}

var unsafeCatalogText = regexp.MustCompile(`(?i)(https?://|www\.|\{[^}]*\}|\p{N}|tài khoản|tham chiếu|chuyển thêm|chuyển tiếp|giàu|nghèo|nợ|giới tính|địt|đụ\b|cặc|lồn)`)

func validateFragment(text string) error {
	if text == "" || utf8.RuneCountInString(text) > 70 {
		return errors.New("fragment must contain 1..70 runes")
	}
	if unsafeCatalogText.MatchString(text) {
		return errors.New("fragment contains unsupported or unsafe text")
	}
	for _, r := range strings.TrimSuffix(text, ".") {
		if unicode.IsControl(r) || strings.ContainsRune(".!?;:…", r) {
			return errors.New("fragment must be one complete short sentence")
		}
	}
	return nil
}

// CompileCatalog is deterministic and validates every pair, not only the Cartesian count.
// Mechanical validation is not semantic approval; release review evidence is checked separately.
func CompileCatalog(sourceJSON []byte) (VoiceCatalog, error) {
	var source CatalogSource
	if err := decodeCatalogJSON(sourceJSON, &source); err != nil {
		return VoiceCatalog{}, err
	}
	if source.Version != CatalogVersion || len(source.Groups) != 8 {
		return VoiceCatalog{}, errors.New("source must contain v1 and exactly eight groups")
	}
	if len(source.Classic) != len(classicPhrases) {
		return VoiceCatalog{}, errors.New("six classic presets required")
	}
	for i, phrase := range classicPhrases {
		if source.Classic[i] != phrase {
			return VoiceCatalog{}, fmt.Errorf("classic preset %s changed", phrase.ID)
		}
	}
	catalog := VoiceCatalog{Version: CatalogVersion, AmountCore: AmountCoreTemplate, Classic: source.Classic}
	groups, fragments, tails := map[string]bool{}, map[string]bool{}, map[string]string{}
	for _, group := range source.Groups {
		if (group.StyleID != "cute" && group.StyleID != "funny") || !validCatalogBand(group.BandID) {
			return VoiceCatalog{}, errors.New("unknown style or band")
		}
		prefix := group.StyleID + "-" + group.BandID
		if groups[prefix] || len(group.Openings) != 50 || len(group.Closings) != 25 {
			return VoiceCatalog{}, fmt.Errorf("%s must have 50 openings and 25 closings exactly once", prefix)
		}
		groups[prefix] = true
		openingIDs, closingIDs := make([]string, 50), make([]string, 25)
		texts := make(map[string]string, 75)
		for part, values := range [][]string{group.Openings, group.Closings} {
			for i, raw := range values {
				text := NormalizeCatalogText(raw)
				if err := validateFragment(text); err != nil {
					return VoiceCatalog{}, fmt.Errorf("%s fragment %d/%d: %w", prefix, part, i+1, err)
				}
				if fragments[text] {
					return VoiceCatalog{}, fmt.Errorf("duplicate normalized fragment: %s", text)
				}
				fragments[text] = true
				letter := "o"
				if part == 1 {
					letter = "c"
				}
				id := fmt.Sprintf("%s-%s%03d", prefix, letter, i+1)
				texts[id] = text
				catalog.Fragments = append(catalog.Fragments, CatalogFragment{id, text, group.StyleID, group.BandID})
				if part == 0 {
					openingIDs[i] = id
				} else {
					closingIDs[i] = id
				}
			}
		}
		for oi, opening := range openingIDs {
			for ci, closing := range closingIDs {
				id := fmt.Sprintf("%s-o%03d-c%03d", prefix, oi+1, ci+1)
				tail := NormalizeCatalogText(texts[opening] + " " + texts[closing])
				if utf8.RuneCountInString(tail) > 120 {
					return VoiceCatalog{}, fmt.Errorf("tail %s exceeds 120 runes", id)
				}
				if old, ok := tails[tail]; ok {
					return VoiceCatalog{}, fmt.Errorf("duplicate tails %s and %s", old, id)
				}
				tails[tail] = id
				catalog.Recipes = append(catalog.Recipes, CatalogRecipe{id, group.StyleID, group.BandID, opening, closing, 1})
			}
		}
	}
	for _, phrase := range classicPhrases {
		catalog.Recipes = append(catalog.Recipes, CatalogRecipe{ID: phrase.ID, StyleID: "classic", BandID: "all", Weight: 1})
	}
	return catalog, nil
}

func validCatalogBand(id string) bool {
	return id == "small" || id == "medium" || id == "large" || id == "celebrate"
}

func decodeCatalogJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func MarshalCatalog(value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

var catalogOnce sync.Once
var catalogRaw []byte
var catalogTag string
var catalogRecipes map[string]CatalogRecipe
var catalogFragments map[string]string
var catalogLoadError error

func loadCatalog() {
	catalogRaw, catalogLoadError = catalogFiles.ReadFile("catalog/catalog.v1.json")
	if catalogLoadError != nil {
		return
	}
	var catalog VoiceCatalog
	catalogLoadError = decodeCatalogJSON(catalogRaw, &catalog)
	if catalogLoadError != nil {
		return
	}
	generated, err := CompileCatalog(CatalogSourceBytes())
	if err != nil {
		catalogLoadError = err
		return
	}
	expected, err := MarshalCatalog(generated)
	if err != nil || !bytes.Equal(expected, catalogRaw) {
		catalogLoadError = errors.New("embedded catalog does not match source")
		return
	}
	if err := CheckCatalogReview(catalog); err != nil {
		catalogLoadError = err
		return
	}
	hash := sha256.Sum256(catalogRaw)
	catalogTag = `"` + hex.EncodeToString(hash[:]) + `"`
	catalogRecipes = make(map[string]CatalogRecipe, len(catalog.Recipes))
	catalogFragments = make(map[string]string, len(catalog.Fragments))
	for _, recipe := range catalog.Recipes {
		catalogRecipes[recipe.ID] = recipe
	}
	for _, fragment := range catalog.Fragments {
		catalogFragments[fragment.ID] = fragment.Text
	}
}

// CatalogBytes returns a caller-owned copy of immutable generated JSON; nil means unavailable.
func CatalogBytes() []byte {
	catalogOnce.Do(loadCatalog)
	if catalogLoadError != nil {
		return nil
	}
	return bytes.Clone(catalogRaw)
}
func CatalogETag() string {
	catalogOnce.Do(loadCatalog)
	if catalogLoadError != nil {
		return ""
	}
	return catalogTag
}
func CatalogError() error { catalogOnce.Do(loadCatalog); return catalogLoadError }
func CatalogSourceBytes() []byte {
	raw, _ := catalogFiles.ReadFile("catalog/source.v1.json")
	return raw
}
func LookupRecipe(id string) (CatalogRecipe, bool) {
	if _, ok := ClassicTemplate(id); ok {
		return CatalogRecipe{ID: id, StyleID: "classic", BandID: "all", Weight: 1}, true
	}
	catalogOnce.Do(loadCatalog)
	r, ok := catalogRecipes[id]
	return r, ok && catalogLoadError == nil
}
func ValidateRecipe(id, styleID, bandID string) error {
	r, ok := LookupRecipe(id)
	if !ok || !validCatalogBand(bandID) || r.StyleID != styleID || (r.BandID != "all" && r.BandID != bandID) {
		return errors.New("VOICE_RECIPE_MISMATCH")
	}
	return nil
}
func RecipeTail(id string) (string, bool) {
	r, ok := LookupRecipe(id)
	if !ok || r.StyleID == "classic" {
		return "", false
	}
	return catalogFragments[r.OpeningID] + " " + catalogFragments[r.ClosingID], true
}
func ClassicTemplate(id string) (string, bool) {
	for _, p := range classicPhrases {
		if p.ID == id {
			return p.Template, true
		}
	}
	return "", false
}
func StandardAnnouncement(amount int64) string {
	return FormatAnnouncementTemplate(AmountCoreTemplate, amount, "", false)
}

type AmountFixture struct {
	Amount int64  `json:"amount"`
	Spoken string `json:"spoken"`
}

func CheckAmountFixtures() error {
	raw, err := catalogFiles.ReadFile("catalog/amount-fixtures.v1.json")
	if err != nil {
		return err
	}
	var fixtures []AmountFixture
	if err := decodeCatalogJSON(raw, &fixtures); err != nil {
		return err
	}
	required := [...]int64{1, 5, 10, 15, 21, 24, 25, 101, 105, 1001, 10005, 49999, 50000, 99999, 100001, 199999, 200000, 999999, 1000000, 1000001, 1010005, 1000000000, 1000000000000, 9007199254740991}
	if len(fixtures) != len(required) {
		return errors.New("amount fixture count mismatch")
	}
	for i, f := range fixtures {
		if f.Amount != required[i] || f.Spoken != SpeakVND(f.Amount) {
			return fmt.Errorf("amount fixture %d mismatch", f.Amount)
		}
	}
	return nil
}

type AudioInputProfile struct {
	VoiceID string  `json:"voiceId"`
	Rate    float64 `json:"rate"`
	PitchHz int     `json:"pitchHz"`
}

type AudioInputClip struct {
	ClipID     string `json:"clipId"`
	Kind       string `json:"kind"`
	Transcript string `json:"transcript"`
}

type AudioInputAmount struct {
	Standard string            `json:"standard"`
	Classic  map[string]string `json:"classic"`
}

type AudioInputOptions struct {
	Amounts               []int64
	PreloadAmounts        []int64
	IncludeNumberSegments bool
}

type AudioInputs struct {
	SchemaVersion    int                         `json:"schemaVersion"`
	SynthesisKey     string                      `json:"synthesisKey"`
	VoiceID          string                      `json:"voiceId"`
	Rate             float64                     `json:"rate"`
	PitchHz          int                         `json:"pitchHz"`
	FormatterVersion string                      `json:"formatterVersion"`
	CatalogVersion   string                      `json:"catalogVersion"`
	Clips            []AudioInputClip            `json:"clips"`
	Amounts          map[string]AudioInputAmount `json:"amounts"`
	PreloadClipIDs   []string                    `json:"preloadClipIds"`
	NumberSegments   map[string]string           `json:"numberSegments,omitempty"`
	NumberPrefix     *string                     `json:"numberPrefix,omitempty"`
	NumberSuffix     *string                     `json:"numberSuffix,omitempty"`
}

// ExportAudioInputs uses the production Go formatter; no Python money formatter is needed.
func ExportAudioInputs(profile AudioInputProfile, options AudioInputOptions) (AudioInputs, error) {
	provider := "edge"
	switch profile.VoiceID {
	case "vi-VN-HoaiMyNeural", "vi-VN-NamMinhNeural":
	case "vi-VN-vais1000-medium":
		provider = "piper"
		if profile.PitchHz != 0 {
			return AudioInputs{}, errors.New("Piper requires pitchHz 0")
		}
	default:
		return AudioInputs{}, errors.New("unsupported voiceId")
	}
	if profile.Rate < .75 || profile.Rate > 2 || profile.PitchHz < -50 || profile.PitchHz > 50 {
		return AudioInputs{}, errors.New("invalid profile rate or pitch")
	}
	rateText := strconv.FormatFloat(profile.Rate, 'f', 2, 64)
	rounded, _ := strconv.ParseFloat(rateText, 64)
	if rounded != profile.Rate {
		return AudioInputs{}, errors.New("rate must have at most two decimal places")
	}
	if err := CheckAmountFixtures(); err != nil {
		return AudioInputs{}, err
	}
	catalog, err := CompileCatalog(CatalogSourceBytes())
	if err != nil {
		return AudioInputs{}, err
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d|%s|pack-schema-2", provider, profile.VoiceID, rateText, profile.PitchHz, FormatterVersion)))
	result := AudioInputs{
		SchemaVersion:    2,
		SynthesisKey:     hex.EncodeToString(hash[:]),
		VoiceID:          profile.VoiceID,
		Rate:             profile.Rate,
		PitchHz:          profile.PitchHz,
		FormatterVersion: FormatterVersion,
		CatalogVersion:   CatalogVersion,
		Amounts:          make(map[string]AudioInputAmount),
		PreloadClipIDs:   make([]string, 0, 14),
	}

	baseAmounts := []int64{1000, 2000, 5000, 10000, 15000, 20000, 25000, 30000, 40000, 50000, 75000, 100000, 150000, 200000, 300000, 500000, 750000, 1000000, 2000000, 5000000}
	amountSet := make(map[int64]bool, len(baseAmounts)+len(options.Amounts))
	for _, a := range baseAmounts {
		amountSet[a] = true
	}
	for _, a := range options.Amounts {
		if a <= 0 || a > CatalogMaxAmount {
			return AudioInputs{}, errors.New("invalid audio input amount")
		}
		// Keep the required base set and retain extra amounts in caller ranking order.
		if !amountSet[a] && len(amountSet) < 128 {
			amountSet[a] = true
		}
	}
	selectedAmounts := make([]int64, 0, len(amountSet))
	for a := range amountSet {
		selectedAmounts = append(selectedAmounts, a)
	}
	slices.Sort(selectedAmounts)

	for _, amount := range selectedAmounts {
		key := strconv.FormatInt(amount, 10)
		entry := AudioInputAmount{Standard: "amount-" + key, Classic: make(map[string]string, 6)}
		result.Clips = append(result.Clips, AudioInputClip{entry.Standard, "amount", StandardAnnouncement(amount)})
		for _, phrase := range classicPhrases {
			id := phrase.ID + "-" + key
			entry.Classic[phrase.ID] = id
			result.Clips = append(result.Clips, AudioInputClip{id, "classic", FormatAnnouncementTemplate(phrase.Template, amount, "", false)})
		}
		result.Amounts[key] = entry
	}

	for _, fragment := range catalog.Fragments {
		result.Clips = append(result.Clips, AudioInputClip{fragment.ID, "tail-fragment", fragment.Text})
	}

	preloadAmounts := options.PreloadAmounts
	if len(preloadAmounts) == 0 {
		preloadAmounts = []int64{20000, 50000, 100000, 200000, 500000, 1000000}
	}
	if len(preloadAmounts) > 6 {
		preloadAmounts = preloadAmounts[:6]
	}
	seenPreloads := make(map[int64]bool, len(preloadAmounts))
	for _, a := range preloadAmounts {
		if !amountSet[a] {
			return AudioInputs{}, errors.New("preload amount is not in the retained pack")
		}
		if !seenPreloads[a] {
			result.PreloadClipIDs = append(result.PreloadClipIDs, "amount-"+strconv.FormatInt(a, 10))
			seenPreloads[a] = true
		}
	}
	seenGroup := make(map[string]bool)
	for _, fragment := range catalog.Fragments {
		grp := fragment.StyleID + ":" + fragment.BandID
		if !seenGroup[grp] {
			seenGroup[grp] = true
			result.PreloadClipIDs = append(result.PreloadClipIDs, fragment.ID)
			if len(seenGroup) == 8 {
				break
			}
		}
	}

	if options.IncludeNumberSegments {
		qaAmounts := []int64{
			1, 5, 10, 15, 21, 24, 25, 101, 105, 1001, 10005, 49999, 50000, 99999, 100001, 199999, 200000, 999999, 1000000, 1000001, 1010005, 1000000000, 1000000000000, 9007199254740991,
			1000, 2000, 5000, 10000, 15000, 20000, 25000, 30000, 40000, 75000, 100000, 105000, 127000, 150000, 201000, 215000, 251000, 300000, 368500, 500000, 750000, 1005000, 1015000, 2000000, 5000000, 1000000001,
		}
		partCounts := make(map[string]int)
		for _, a := range selectedAmounts {
			for _, p := range SpeakVNDParts(a) {
				partCounts[p]++
			}
		}
		for _, a := range qaAmounts {
			for _, p := range SpeakVNDParts(a) {
				if _, ok := partCounts[p]; !ok {
					partCounts[p] = 1
				}
			}
		}

		type partFreq struct {
			part  string
			count int
		}
		var rankedParts []partFreq
		for p, c := range partCounts {
			rankedParts = append(rankedParts, partFreq{p, c})
		}
		slices.SortFunc(rankedParts, func(a, b partFreq) int {
			if a.count != b.count {
				return b.count - a.count
			}
			return strings.Compare(a.part, b.part)
		})
		if len(rankedParts) > 128 {
			rankedParts = rankedParts[:128]
		}

		result.NumberSegments = make(map[string]string, len(rankedParts))
		for _, item := range rankedParts {
			h := sha256.Sum256([]byte(item.part))
			cid := "number-" + hex.EncodeToString(h[:])
			result.NumberSegments[item.part] = cid
			result.Clips = append(result.Clips, AudioInputClip{ClipID: cid, Kind: "amount-segment", Transcript: item.part})
		}

		prefixID := "amount-prefix"
		suffixID := "amount-suffix"
		result.NumberPrefix = &prefixID
		result.NumberSuffix = &suffixID
		result.Clips = append(result.Clips, AudioInputClip{ClipID: prefixID, Kind: "amount-prefix", Transcript: "Đã nhận"})
		result.Clips = append(result.Clips, AudioInputClip{ClipID: suffixID, Kind: "amount-suffix", Transcript: "đồng."})
	}

	return result, nil
}

type ReviewText struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type CatalogReviewBatch struct {
	ID      string       `json:"id"`
	StyleID string       `json:"styleId"`
	BandID  string       `json:"bandId"`
	Recipes []ReviewText `json:"recipes"`
}

type CatalogReviewDecision struct {
	ID                string   `json:"id"`
	SHA256            string   `json:"sha256"`
	RecipeCount       int      `json:"recipeCount"`
	Decision          string   `json:"decision"`
	Reviewer          string   `json:"reviewer"`
	RejectedRecipeIDs []string `json:"rejectedRecipeIds"`
	Notes             string   `json:"notes"`
}

type CatalogReviewEvidence struct {
	Version        string                  `json:"version"`
	ApprovedUnique int                     `json:"approvedUnique"`
	RejectedCount  int                     `json:"rejectedCount"`
	Batches        []CatalogReviewDecision `json:"batches"`
}

// BuildCatalogReviewBatches materializes every full tail for a separate semantic reviewer.
// Each group has ten batches of 125 texts; no fragment-only review can approve a batch.
func BuildCatalogReviewBatches(catalog VoiceCatalog) []CatalogReviewBatch {
	fragments := make(map[string]string, len(catalog.Fragments))
	for _, f := range catalog.Fragments {
		fragments[f.ID] = f.Text
	}
	batches := make([]CatalogReviewBatch, 0, 80)
	groupCounts := make(map[string]int, 8)
	for _, recipe := range catalog.Recipes {
		if recipe.StyleID == "classic" {
			continue
		}
		prefix := recipe.StyleID + "-" + recipe.BandID
		position := groupCounts[prefix]
		if position%125 == 0 {
			batches = append(batches, CatalogReviewBatch{ID: fmt.Sprintf("%s-b%02d", prefix, position/125+1), StyleID: recipe.StyleID, BandID: recipe.BandID, Recipes: make([]ReviewText, 0, 125)})
		}
		batch := &batches[len(batches)-1]
		batch.Recipes = append(batch.Recipes, ReviewText{recipe.ID, fragments[recipe.OpeningID] + " " + fragments[recipe.ClosingID]})
		groupCounts[prefix]++
	}
	return batches
}

func CatalogBatchDigest(batch CatalogReviewBatch) string {
	raw, _ := MarshalCatalog(batch)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func ValidateCatalogReview(catalog VoiceCatalog, evidence CatalogReviewEvidence, requireApproved bool) error {
	batches := BuildCatalogReviewBatches(catalog)
	if evidence.Version != CatalogVersion || len(evidence.Batches) != len(batches) {
		return errors.New("review evidence batch count/version mismatch")
	}
	approved, rejected := 0, 0
	for i, batch := range batches {
		decision := evidence.Batches[i]
		if decision.ID != batch.ID || decision.SHA256 != CatalogBatchDigest(batch) || decision.RecipeCount != len(batch.Recipes) {
			return fmt.Errorf("stale review evidence: %s", batch.ID)
		}
		seen := make(map[string]bool, len(decision.RejectedRecipeIDs))
		for _, id := range decision.RejectedRecipeIDs {
			found := false
			for _, recipe := range batch.Recipes {
				if recipe.ID == id {
					found = true
					break
				}
			}
			if !found || seen[id] {
				return fmt.Errorf("invalid rejected recipe ID: %s", id)
			}
			seen[id] = true
		}
		switch decision.Decision {
		case "approved":
			if strings.TrimSpace(decision.Reviewer) == "" || strings.TrimSpace(decision.Notes) == "" || len(seen) != 0 {
				return fmt.Errorf("incomplete approval: %s", batch.ID)
			}
			approved += len(batch.Recipes)
		case "rejected":
			if strings.TrimSpace(decision.Reviewer) == "" || strings.TrimSpace(decision.Notes) == "" || len(seen) == 0 {
				return fmt.Errorf("incomplete rejection: %s", batch.ID)
			}
			rejected += len(seen)
		case "pending":
			if len(seen) != 0 {
				return fmt.Errorf("pending batch has rejection IDs: %s", batch.ID)
			}
		default:
			return fmt.Errorf("invalid review decision: %s", batch.ID)
		}
	}
	if evidence.ApprovedUnique != approved || evidence.RejectedCount != rejected {
		return errors.New("review evidence totals do not match decisions")
	}
	if requireApproved && (approved != 10000 || rejected != 0) {
		return fmt.Errorf("semantic review incomplete: %d/10000 approved unique, %d rejected", approved, rejected)
	}
	return nil
}

func CheckCatalogReview(catalog VoiceCatalog) error {
	raw, err := catalogFiles.ReadFile("catalog/review.v1.json")
	if err != nil {
		return err
	}
	var evidence CatalogReviewEvidence
	if err := decodeCatalogJSON(raw, &evidence); err != nil {
		return err
	}
	return ValidateCatalogReview(catalog, evidence, true)
}
