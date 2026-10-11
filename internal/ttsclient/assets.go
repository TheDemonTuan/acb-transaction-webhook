package ttsclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

var assetHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type AssetClip struct {
	ClipID     string  `json:"clipId"`
	Kind       string  `json:"kind"`
	Transcript string  `json:"transcript"`
	SHA256     string  `json:"sha256"`
	Bytes      int64   `json:"bytes"`
	DurationMS float64 `json:"durationMs"`
	SampleRate int     `json:"sampleRate"`
}

type AssetManifest struct {
	SchemaVersion    int         `json:"schemaVersion"`
	PackVersion      string      `json:"packVersion"`
	SynthesisKey     string      `json:"synthesisKey"`
	VoiceID          string      `json:"voiceId"`
	Rate             float64     `json:"rate"`
	PitchHz          int         `json:"pitchHz"`
	FormatterVersion string      `json:"formatterVersion"`
	CatalogVersion   string      `json:"catalogVersion"`
	Clips            []AssetClip `json:"clips"`
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
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, 0, err
	}
	var canonical map[string]any
	if err := json.Unmarshal(data, &canonical); err != nil {
		return nil, nil, 0, err
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
	if manifest.SchemaVersion != 1 || manifest.SynthesisKey != key || !assetHashPattern.MatchString(manifest.PackVersion) || manifest.FormatterVersion != "money-vi-v1" || manifest.CatalogVersion != "v1" {
		return nil, nil, 0, fmt.Errorf("invalid voice asset manifest")
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

func (c *Client) VoiceClip(ctx context.Context, hash string) ([]byte, int, error) {
	return c.fetchAsset(ctx, hash, false)
}
