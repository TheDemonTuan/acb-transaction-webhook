package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
)

type voiceAudioRequest struct {
	IncludeDescription bool   `json:"includeDescription"`
	Template           string `json:"template,omitempty"`
	VoiceID            string `json:"voiceId,omitempty"`
	Rate               any    `json:"rate,omitempty"`
	Pitch              any    `json:"pitch,omitempty"`
}

type voiceSummaryRequest struct {
	TransactionIDs     []string `json:"transactionIds"`
	IncludeDescription bool     `json:"includeDescription"`
	VoiceID            string   `json:"voiceId,omitempty"`
	Rate               any      `json:"rate,omitempty"`
	Pitch              any      `json:"pitch,omitempty"`
}

type voiceTestRequest struct {
	IncludeDescription bool   `json:"includeDescription,omitempty"`
	Template           string `json:"template,omitempty"`
	VoiceID            string `json:"voiceId,omitempty"`
	Rate               any    `json:"rate,omitempty"`
	Pitch              any    `json:"pitch,omitempty"`
}

func clampInt(val, minVal, maxVal int) int {
	if val < minVal {
		return minVal
	}
	if val > maxVal {
		return maxVal
	}
	return val
}

func sanitizeAnnouncementTemplate(raw string) string {
	tpl := strings.TrimSpace(raw)
	if tpl == "" {
		return voicecopy.DefaultAnnouncementTemplate
	}
	runes := []rune(tpl)
	if len(runes) > 120 {
		runes = runes[:120]
		tpl = strings.TrimSpace(string(runes))
	}
	if !strings.Contains(tpl, "{amount}") && !strings.Contains(tpl, "{amount_raw}") {
		return voicecopy.DefaultAnnouncementTemplate
	}
	return tpl
}

func formatTTSRate(val any) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return ""
		}
		if strings.HasSuffix(v, "%") {
			numStr := strings.TrimSuffix(v, "%")
			if p, err := strconv.Atoi(numStr); err == nil {
				p = clampInt(p, -50, 100)
				if p >= 0 {
					return fmt.Sprintf("+%d%%", p)
				}
				return fmt.Sprintf("%d%%", p)
			}
			return ""
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return formatFloatRate(f)
		}
		return ""
	case float64:
		return formatFloatRate(v)
	case int:
		return formatFloatRate(float64(v))
	case int64:
		return formatFloatRate(float64(v))
	default:
		return ""
	}
}

func formatFloatRate(rate float64) string {
	if rate <= 0 {
		return ""
	}
	diff := (rate - 1.0) * 100.0
	pct := clampInt(int(math.Round(diff)), -50, 100)
	if pct >= 0 {
		return fmt.Sprintf("+%d%%", pct)
	}
	return fmt.Sprintf("%d%%", pct)
}

func formatTTSPitch(val any) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return ""
		}
		if strings.HasSuffix(v, "Hz") {
			numStr := strings.TrimSuffix(v, "Hz")
			if p, err := strconv.Atoi(numStr); err == nil {
				p = clampInt(p, -50, 50)
				if p >= 0 {
					return fmt.Sprintf("+%dHz", p)
				}
				return fmt.Sprintf("%dHz", p)
			}
			return ""
		}
		if strings.HasSuffix(v, "%") {
			numStr := strings.TrimSuffix(v, "%")
			if p, err := strconv.Atoi(numStr); err == nil {
				p = clampInt(p, -50, 50)
				if p >= 0 {
					return fmt.Sprintf("+%d%%", p)
				}
				return fmt.Sprintf("%d%%", p)
			}
			return ""
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return formatFloatPitch(f)
		}
		return ""
	case float64:
		return formatFloatPitch(v)
	case int:
		return formatFloatPitch(float64(v))
	case int64:
		return formatFloatPitch(float64(v))
	default:
		return ""
	}
}

func formatFloatPitch(pitch float64) string {
	if pitch <= 0 {
		return ""
	}
	diff := (pitch - 1.0) * 50.0
	diffInt := clampInt(int(math.Round(diff)), -50, 50)
	if diffInt >= 0 {
		return fmt.Sprintf("+%dHz", diffInt)
	}
	return fmt.Sprintf("%dHz", diffInt)
}

func (s *Server) getVoiceSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetVoiceSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) updateVoiceSettings(w http.ResponseWriter, r *http.Request) {
	var input storage.VoiceSettings
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	saved, err := s.store.SaveVoiceSettings(r.Context(), input)
	if err != nil {
		if errors.Is(err, storage.ErrVoiceSettingsConflict) {
			writeError(w, http.StatusConflict, "voice settings modified by another operator")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	audit(s.store, r, "voice_settings.update", fmt.Sprintf("rev_%d", saved.Revision))
	s.publishStateEvent("voice.settings.changed", "singleton", saved)
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) voiceStatus(w http.ResponseWriter, r *http.Request) {
	settings, _ := s.store.GetVoiceSettings(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"available":      s.ttsClient != nil,
		"primary":        "edge",
		"fallback":       "gtts",
		"providerMode":   settings.ProviderMode,
		"edgeVoice":      settings.EdgeVoice,
		"onlineFallback": settings.OnlineFallback,
	})
}

func writeAudioStream(w http.ResponseWriter, stream *ttsclient.StreamResult) {
	defer stream.Close()

	firstByteMs := stream.FirstByteDuration.Milliseconds()

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-TTS-Provider", stream.Provider)
	w.Header().Set("X-TTS-Voice", stream.Voice)
	w.Header().Set("X-TTS-Fallback", fmt.Sprintf("%t", stream.Fallback))
	w.Header().Set("X-TTS-Cached", fmt.Sprintf("%t", stream.Cached))
	w.Header().Set("X-TTS-First-Byte-Ms", strconv.FormatInt(firstByteMs, 10))
	w.Header().Set("Server-Timing", fmt.Sprintf("tts_fb;dur=%d", firstByteMs))
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	buf := make([]byte, 4096)
	for {
		n, err := stream.Reader.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

func (s *Server) testVoiceAudio(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "tts_client_not_configured")
		return
	}

	var req voiceTestRequest
	if r.Method == http.MethodGet {
		req.VoiceID = r.URL.Query().Get("voiceId")
		req.Rate = r.URL.Query().Get("rate")
		req.Pitch = r.URL.Query().Get("pitch")
		req.Template = r.URL.Query().Get("template")
		req.IncludeDescription = r.URL.Query().Get("includeDescription") == "true"
	} else {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	settings, _ := s.store.GetVoiceSettings(r.Context())
	if settings.ProviderMode == "BROWSER_ONLY" {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}
	voice := settings.EdgeVoice
	if req.VoiceID == "vi-VN-HoaiMyNeural" || req.VoiceID == "vi-VN-NamMinhNeural" {
		voice = req.VoiceID
	}
	allowFallback := settings.OnlineFallback && settings.ProviderMode == "ONLINE_AUTO"

	template := sanitizeAnnouncementTemplate(req.Template)
	phrase := voicecopy.FormatAnnouncementTemplate(template, 500000, "Ung ho quy", req.IncludeDescription)
	ttsRate := formatTTSRate(req.Rate)
	ttsPitch := formatTTSPitch(req.Pitch)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:          phrase,
		Voice:         voice,
		Rate:          ttsRate,
		Pitch:         ttsPitch,
		Cacheable:     true,
		AllowFallback: &allowFallback,
		ProviderMode:  settings.ProviderMode,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("tts_synthesis_failed: %v", err))
		return
	}

	writeAudioStream(w, stream)
}

func (s *Server) publicTestVoiceAudio(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "tts_client_not_configured")
		return
	}

	var req voiceTestRequest
	if r.Method == http.MethodGet {
		req.VoiceID = r.URL.Query().Get("voiceId")
		req.Rate = r.URL.Query().Get("rate")
		req.Pitch = r.URL.Query().Get("pitch")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	settings, _ := s.store.GetVoiceSettings(r.Context())
	if settings.ProviderMode == "BROWSER_ONLY" {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}
	voice := settings.EdgeVoice
	if req.VoiceID == "vi-VN-HoaiMyNeural" || req.VoiceID == "vi-VN-NamMinhNeural" {
		voice = req.VoiceID
	}
	allowFallback := settings.OnlineFallback && settings.ProviderMode == "ONLINE_AUTO"

	// Strict fixed test phrase for public viewer - do not allow custom templates or descriptions
	phrase := voicecopy.FormatAnnouncementTemplate(voicecopy.DefaultAnnouncementTemplate, 500000, "", false)
	ttsRate := formatTTSRate(req.Rate)
	ttsPitch := formatTTSPitch(req.Pitch)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:          phrase,
		Voice:         voice,
		Rate:          ttsRate,
		Pitch:         ttsPitch,
		Cacheable:     true,
		AllowFallback: &allowFallback,
		ProviderMode:  settings.ProviderMode,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("tts_synthesis_failed: %v", err))
		return
	}

	writeAudioStream(w, stream)
}

func (s *Server) synthesizePublicTransactionAudio(w http.ResponseWriter, r *http.Request) {
	s.synthesizeTransactionAudioInternal(w, r, true)
}

func (s *Server) synthesizeTransactionAudio(w http.ResponseWriter, r *http.Request) {
	s.synthesizeTransactionAudioInternal(w, r, false)
}

func (s *Server) synthesizeTransactionAudioInternal(w http.ResponseWriter, r *http.Request, isPublic bool) {
	if s.ttsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "tts_client_not_configured")
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "transaction_id_required")
		return
	}

	var req voiceAudioRequest
	if r.Method == http.MethodGet {
		req.VoiceID = r.URL.Query().Get("voiceId")
		req.Rate = r.URL.Query().Get("rate")
		req.Pitch = r.URL.Query().Get("pitch")
		req.Template = r.URL.Query().Get("template")
		req.IncludeDescription = r.URL.Query().Get("includeDescription") == "true"
	} else {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	item, err := s.store.GetTransactionByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "transaction_not_found")
		return
	}

	if item.Credit <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "voice_credit_zero_or_negative")
		return
	}

	if item.Source != "REALTIME" {
		writeError(w, http.StatusUnprocessableEntity, "voice_source_not_realtime")
		return
	}

	// Freshness verification: transaction must have been observed within 5 minutes
	if item.FirstSeenAt != "" {
		seenTime, err := time.Parse(time.RFC3339Nano, item.FirstSeenAt)
		if err != nil {
			seenTime, err = time.Parse(time.RFC3339, item.FirstSeenAt)
		}
		if err == nil {
			age := time.Since(seenTime)
			if age > 5*time.Minute {
				writeError(w, http.StatusUnprocessableEntity, "voice_event_expired")
				return
			}
		}
	} else if isPublic {
		writeError(w, http.StatusUnprocessableEntity, "voice_event_expired")
		return
	}

	settings, _ := s.store.GetVoiceSettings(r.Context())
	if settings.ProviderMode == "BROWSER_ONLY" {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}
	voice := settings.EdgeVoice
	if req.VoiceID == "vi-VN-HoaiMyNeural" || req.VoiceID == "vi-VN-NamMinhNeural" {
		voice = req.VoiceID
	}
	allowFallback := settings.OnlineFallback && settings.ProviderMode == "ONLINE_AUTO"

	template := sanitizeAnnouncementTemplate(req.Template)
	phrase := voicecopy.FormatAnnouncementTemplate(template, item.Credit, item.Description, req.IncludeDescription)
	ttsRate := formatTTSRate(req.Rate)
	ttsPitch := formatTTSPitch(req.Pitch)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:          phrase,
		Voice:         voice,
		Rate:          ttsRate,
		Pitch:         ttsPitch,
		Cacheable:     !req.IncludeDescription,
		AllowFallback: &allowFallback,
		ProviderMode:  settings.ProviderMode,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("tts_synthesis_failed: %v", err))
		return
	}

	writeAudioStream(w, stream)
}

func (s *Server) synthesizeSummaryAudio(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "tts_client_not_configured")
		return
	}

	var req voiceSummaryRequest
	if r.Method == http.MethodGet {
		ids := r.URL.Query()["transactionIds"]
		if len(ids) == 0 {
			ids = r.URL.Query()["txIds"]
		}
		for _, raw := range ids {
			for _, id := range strings.Split(raw, ",") {
				id = strings.TrimSpace(id)
				if id != "" {
					req.TransactionIDs = append(req.TransactionIDs, id)
				}
			}
		}
		req.VoiceID = r.URL.Query().Get("voiceId")
		req.Rate = r.URL.Query().Get("rate")
		req.Pitch = r.URL.Query().Get("pitch")
		req.IncludeDescription = r.URL.Query().Get("includeDescription") == "true"
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	if len(req.TransactionIDs) < 2 || len(req.TransactionIDs) > 50 {
		writeError(w, http.StatusBadRequest, "transactionIds count must be between 2 and 50")
		return
	}

	var totalCredit int64
	validCount := 0

	for _, txID := range req.TransactionIDs {
		item, err := s.store.GetTransactionByID(r.Context(), txID)
		if err != nil || item == nil {
			continue
		}
		if item.Credit > 0 && item.Source == "REALTIME" {
			totalCredit += item.Credit
			validCount++
		}
	}

	if validCount == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_valid_realtime_credits_in_summary")
		return
	}

	settings, _ := s.store.GetVoiceSettings(r.Context())
	if settings.ProviderMode == "BROWSER_ONLY" {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}
	voice := settings.EdgeVoice
	if req.VoiceID == "vi-VN-HoaiMyNeural" || req.VoiceID == "vi-VN-NamMinhNeural" {
		voice = req.VoiceID
	}
	allowFallback := settings.OnlineFallback && settings.ProviderMode == "ONLINE_AUTO"

	phrase := voicecopy.BuildBurstAnnouncement(validCount, totalCredit)
	ttsRate := formatTTSRate(req.Rate)
	ttsPitch := formatTTSPitch(req.Pitch)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:          phrase,
		Voice:         voice,
		Rate:          ttsRate,
		Pitch:         ttsPitch,
		Cacheable:     true,
		AllowFallback: &allowFallback,
		ProviderMode:  settings.ProviderMode,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("tts_synthesis_failed: %v", err))
		return
	}

	writeAudioStream(w, stream)
}

func (s *Server) replayTransactionAudio(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "tts_client_not_configured")
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "transaction_id_required")
		return
	}

	var req voiceAudioRequest
	if r.Method == http.MethodGet {
		req.VoiceID = r.URL.Query().Get("voiceId")
		req.Rate = r.URL.Query().Get("rate")
		req.Pitch = r.URL.Query().Get("pitch")
		req.Template = r.URL.Query().Get("template")
		req.IncludeDescription = r.URL.Query().Get("includeDescription") == "true"
	} else {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	item, err := s.store.GetTransactionByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "transaction_not_found")
		return
	}

	if item.Credit <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "voice_credit_zero_or_negative")
		return
	}

	// Manual replay: does not enforce realtime source or freshness
	settings, _ := s.store.GetVoiceSettings(r.Context())
	if settings.ProviderMode == "BROWSER_ONLY" {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}
	voice := settings.EdgeVoice
	if req.VoiceID == "vi-VN-HoaiMyNeural" || req.VoiceID == "vi-VN-NamMinhNeural" {
		voice = req.VoiceID
	}
	allowFallback := settings.OnlineFallback && settings.ProviderMode == "ONLINE_AUTO"

	template := sanitizeAnnouncementTemplate(req.Template)
	phrase := voicecopy.FormatAnnouncementTemplate(template, item.Credit, item.Description, req.IncludeDescription)
	ttsRate := formatTTSRate(req.Rate)
	ttsPitch := formatTTSPitch(req.Pitch)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:          phrase,
		Voice:         voice,
		Rate:          ttsRate,
		Pitch:         ttsPitch,
		Cacheable:     !req.IncludeDescription,
		AllowFallback: &allowFallback,
		ProviderMode:  settings.ProviderMode,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("tts_synthesis_failed: %v", err))
		return
	}

	writeAudioStream(w, stream)
}
