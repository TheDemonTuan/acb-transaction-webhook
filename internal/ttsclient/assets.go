package ttsclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

var assetHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var assetIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type AssetClip struct {
	ClipID        string   `json:"clipId"`
	Kind          string   `json:"kind"`
	Transcript    string   `json:"transcript"`
	SHA256        string   `json:"sha256"`
	Bytes         int64    `json:"bytes"`
	DurationMS    float64  `json:"durationMs"`
	SampleRate    int      `json:"sampleRate"`
	SpeechStartMS *float64 `json:"speechStartMs"`
	AmountStartMS *float64 `json:"amountStartMs"`
}

type CompositionModes struct {
	Tail    string `json:"tail"`
	Numbers string `json:"numbers"`
}

type AssetManifest struct {
	SchemaVersion         int               `json:"schemaVersion"`
	PackVersion           string            `json:"packVersion"`
	SynthesisKey          string            `json:"synthesisKey"`
	VoiceID               string            `json:"voiceId"`
	Rate                  float64           `json:"rate"`
	PitchHz               int               `json:"pitchHz"`
	FormatterVersion      string            `json:"formatterVersion"`
	CatalogVersion        string            `json:"catalogVersion"`
	QualityEvidenceSHA256 string            `json:"qualityEvidenceSha256"`
	Composition           CompositionModes  `json:"composition"`
	PreloadClipIDs        []string          `json:"preloadClipIds"`
	NumberSegments        map[string]string `json:"numberSegments,omitempty"`
	NumberPrefix          *string           `json:"numberPrefix,omitempty"`
	NumberSuffix          *string           `json:"numberSuffix,omitempty"`
	Clips                 []AssetClip       `json:"clips"`
	Amounts               map[string]any    `json:"amounts"`
}

// fetchAsset never accepts a caller-provided path: only a validated content key.
func (c *Client) fetchAsset(ctx context.Context, key string, manifest bool) ([]byte, int, error) {
	if !assetHashPattern.MatchString(key) {
		return nil, http.StatusNotFound, nil
	}
	path := "/assets/clips/" + key
	limit := int64(4 << 20)
	if manifest {
		path = "/assets/profiles/" + key + "/manifest"
		limit = 1 << 20
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Internal-TTS-Token", c.internalToken)
	resp, err := c.bufferedClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > limit {
		return nil, 0, fmt.Errorf("voice asset exceeds size limit")
	}
	if !manifest {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != key {
			return nil, 0, fmt.Errorf("voice asset checksum mismatch")
		}
	}
	return data, http.StatusOK, nil
}

func (c *Client) VoiceManifest(ctx context.Context, key string) ([]byte, *AssetManifest, int, error) {
	data, status, err := c.fetchAsset(ctx, key, true)
	if err != nil || status != http.StatusOK {
		return nil, nil, status, err
	}
	var manifest AssetManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, nil, 0, err
	}
	var canonical map[string]any
	if err := json.Unmarshal(data, &canonical); err != nil {
		return nil, nil, 0, err
	}
	for _, name := range []string{"schemaVersion", "packVersion", "synthesisKey", "voiceId", "rate", "pitchHz", "formatterVersion", "catalogVersion", "clips", "amounts", "preloadClipIds", "composition", "qualityEvidenceSha256"} {
		if _, exists := canonical[name]; !exists {
			return nil, nil, 0, fmt.Errorf("missing voice manifest field %s", name)
		}
	}
	rawClips, ok := canonical["clips"].([]any)
	if !ok {
		return nil, nil, 0, fmt.Errorf("invalid voice manifest clips")
	}
	for _, raw := range rawClips {
		clip, ok := raw.(map[string]any)
		if !ok || len(clip) != 9 {
			return nil, nil, 0, fmt.Errorf("missing voice clip metadata")
		}
		for _, name := range []string{"speechStartMs", "amountStartMs"} {
			if _, exists := clip[name]; !exists {
				return nil, nil, 0, fmt.Errorf("missing voice clip onset field")
			}
		}
	}
	delete(canonical, "packVersion")
	var canonicalBuffer bytes.Buffer
	encoder := json.NewEncoder(&canonicalBuffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(canonical); err != nil {
		return nil, nil, 0, err
	}
	canonicalBytes := bytes.TrimSuffix(canonicalBuffer.Bytes(), []byte("\n"))
	digest := sha256.Sum256(canonicalBytes)
	if hex.EncodeToString(digest[:]) != manifest.PackVersion {
		return nil, nil, 0, fmt.Errorf("voice manifest checksum mismatch")
	}
	if manifest.SchemaVersion != 2 || manifest.SynthesisKey != key || !assetHashPattern.MatchString(manifest.PackVersion) || !assetHashPattern.MatchString(manifest.QualityEvidenceSHA256) || manifest.FormatterVersion != "money-vi-v1" || manifest.CatalogVersion != "v1" {
		return nil, nil, 0, fmt.Errorf("invalid voice asset manifest")
	}
	if err := validateAssetManifest(manifest); err != nil {
		return nil, nil, 0, err
	}
	var total int64
	for _, clip := range manifest.Clips {
		if !assetHashPattern.MatchString(clip.SHA256) || clip.Bytes <= 0 || clip.Bytes > 4<<20 || clip.SampleRate != 24000 || clip.DurationMS <= 0 {
			return nil, nil, 0, fmt.Errorf("invalid voice asset clip")
		}
		total += clip.Bytes
		if total > 64<<20 {
			return nil, nil, 0, fmt.Errorf("voice pack exceeds size limit")
		}
	}
	return data, &manifest, status, nil
}

func validateAssetManifest(manifest AssetManifest) error {
	invalid := func() error { return fmt.Errorf("invalid voice asset manifest metadata") }
	if manifest.VoiceID != storage.VoiceIDVieneuMaiAnh {
		return invalid()
	}
	if manifest.Rate != 1.00 || manifest.PitchHz != 0 || storage.ComputeSynthesisKey(manifest.VoiceID, manifest.Rate, manifest.PitchHz) != manifest.SynthesisKey {
		return invalid()
	}
	if (manifest.Composition.Tail != "whole" && manifest.Composition.Tail != "fragments") || (manifest.Composition.Numbers != "whole" && manifest.Composition.Numbers != "segments") || len(manifest.Clips) == 0 || len(manifest.Clips) > 20000 {
		return invalid()
	}
	clips := make(map[string]AssetClip, len(manifest.Clips))
	for _, clip := range manifest.Clips {
		if !assetIDPattern.MatchString(clip.ClipID) || strings.TrimSpace(clip.Transcript) == "" || utf8.RuneCountInString(clip.Transcript) > 1000 || math.IsNaN(clip.DurationMS) || math.IsInf(clip.DurationMS, 0) || clip.DurationMS <= 0 || clip.DurationMS > 120000 {
			return invalid()
		}
		if _, exists := clips[clip.ClipID]; exists {
			return invalid()
		}
		switch clip.Kind {
		case "amount", "classic", "tail-fragment", "tail-full", "amount-segment", "amount-prefix", "amount-suffix":
		default:
			return invalid()
		}
		for _, onset := range []*float64{clip.SpeechStartMS, clip.AmountStartMS} {
			if onset != nil && (math.IsNaN(*onset) || math.IsInf(*onset, 0) || *onset < 0 || *onset >= clip.DurationMS) {
				return invalid()
			}
		}
		if (clip.Kind == "tail-fragment" || clip.Kind == "tail-full") && clip.AmountStartMS != nil {
			return invalid()
		}
		clips[clip.ClipID] = clip
	}
	if manifest.PreloadClipIDs == nil || len(manifest.PreloadClipIDs) > 14 || len(manifest.Amounts) == 0 || len(manifest.NumberSegments) > 128 {
		return invalid()
	}
	preloads := make(map[string]bool, len(manifest.PreloadClipIDs))
	for _, id := range manifest.PreloadClipIDs {
		if _, exists := clips[id]; !exists || preloads[id] {
			return invalid()
		}
		preloads[id] = true
	}
	for amount, raw := range manifest.Amounts {
		value, err := strconv.ParseInt(amount, 10, 64)
		if err != nil || value <= 0 || value > 9007199254740991 || strconv.FormatInt(value, 10) != amount {
			return invalid()
		}
		mapping, ok := raw.(map[string]any)
		if !ok || len(mapping) != 2 {
			return invalid()
		}
		id, ok := mapping["standard"].(string)
		if !ok || clips[id].Kind != "amount" {
			return invalid()
		}
		classic, ok := mapping["classic"].(map[string]any)
		if !ok {
			return invalid()
		}
		for phrase, rawID := range classic {
			id, ok := rawID.(string)
			if !assetIDPattern.MatchString(phrase) || !ok || clips[id].Kind != "classic" {
				return invalid()
			}
		}
	}
	for group, id := range manifest.NumberSegments {
		if strings.TrimSpace(group) == "" || clips[id].Kind != "amount-segment" || clips[id].Transcript != group {
			return invalid()
		}
	}
	if manifest.NumberPrefix != nil && clips[*manifest.NumberPrefix].Kind != "amount-prefix" {
		return invalid()
	}
	if manifest.NumberSuffix != nil && clips[*manifest.NumberSuffix].Kind != "amount-suffix" {
		return invalid()
	}
	if manifest.Composition.Numbers == "segments" && (len(manifest.NumberSegments) == 0 || manifest.NumberPrefix == nil || manifest.NumberSuffix == nil) {
		return invalid()
	}
	return nil
}

func (c *Client) VoiceClip(ctx context.Context, hash string) ([]byte, int, error) {
	return c.fetchAsset(ctx, hash, false)
}
