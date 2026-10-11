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
	"github.com/thedemontuan/acb-transaction-webhook/internal/auth"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
)

type voiceAudioRequest struct {
	ProfileRevision *int64   `json:"profileRevision"`
	PhraseID        string   `json:"phraseId"`
	Part            string   `json:"part"` // "full", "amount", "tail"
	AmountVND       string   `json:"amountVnd,omitempty"`
	TransactionIDs  []string `json:"transactionIds,omitempty"`

	// Legacy override fields that are explicitly rejected with 400 VOICE_OVERRIDE_NOT_ALLOWED
	IncludeDescription *bool  `json:"includeDescription,omitempty"`
	Template           string `json:"template,omitempty"`
	VoiceID            string `json:"voiceId,omitempty"`
	Rate               any    `json:"rate,omitempty"`
	Pitch              any    `json:"pitch,omitempty"`
	Amount             any    `json:"amount,omitempty"`
	Text               string `json:"text,omitempty"`
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

func formatFloatRate(rate float64) string {
	if rate <= 0 {
		return "+0%"
	}
	diff := (rate - 1.0) * 100.0
	pct := clampInt(int(math.Round(diff)), -50, 100)
	if pct >= 0 {
		return fmt.Sprintf("+%d%%", pct)
	}
	return fmt.Sprintf("%d%%", pct)
}

func formatPitchHz(pitchHz int) string {
	if pitchHz >= 0 {
		return fmt.Sprintf("+%dHz", pitchHz)
	}
	return fmt.Sprintf("%dHz", pitchHz)
}

func checkVoiceOverrides(r *http.Request, req voiceAudioRequest) error {
	q := r.URL.Query()
	if q.Get("voiceId") != "" || q.Get("rate") != "" || q.Get("pitch") != "" ||
		q.Get("template") != "" || q.Get("text") != "" || q.Get("amount") != "" ||
		q.Get("includeDescription") != "" {
		return errors.New("VOICE_OVERRIDE_NOT_ALLOWED")
	}
	if req.VoiceID != "" || req.Rate != nil || req.Pitch != nil ||
		req.Template != "" || req.Text != "" || req.Amount != nil ||
		req.IncludeDescription != nil {
		return errors.New("VOICE_OVERRIDE_NOT_ALLOWED")
	}
	return nil
}

func parseVoiceAudioRequest(r *http.Request) (voiceAudioRequest, error) {
	var req voiceAudioRequest
	if r.Method == http.MethodGet {
		if r.URL.Query().Has("profileRevision") {
			value := r.URL.Query().Get("profileRevision")
			revision, err := strconv.ParseInt(value, 10, 64)
			if err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != value {
				return req, errors.New("invalid profileRevision")
			}
			req.ProfileRevision = &revision
		}
		req.PhraseID = r.URL.Query().Get("phraseId")
		req.Part = r.URL.Query().Get("part")
		req.AmountVND = r.URL.Query().Get("amountVnd")
		txIDs := r.URL.Query().Get("transactionIds")
		if txIDs != "" {
			req.TransactionIDs = strings.Split(txIDs, ",")
		}
	} else if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			return req, err
		}
	}
	return req, nil
}

func (s *Server) resolveVoicePhrase(profile storage.VoiceProfile, amount int64, description string, phraseID string, part string) (string, error) {
	if part == "" {
		part = "full"
	}
	if part != "full" && part != "amount" && part != "tail" {
		return "", errors.New("VOICE_RECIPE_MISMATCH")
	}
	core := voicecopy.StandardAnnouncement(amount)
	var selected *storage.AmountBand
	for i := range profile.AmountBands {
		band := &profile.AmountBands[i]
		from, err := strconv.ParseInt(band.FromVND, 10, 64)
		if err != nil || amount < from {
			continue
		}
		if band.ToVND != nil {
			to, err := strconv.ParseInt(*band.ToVND, 10, 64)
			if err != nil || amount >= to {
				continue
			}
		}
		selected = band
		break
	}
	if phraseID != "" {
		if selected == nil || !profile.FunEnabled || selected.StyleID == "custom" || voicecopy.ValidateRecipe(phraseID, selected.StyleID, selected.ID) != nil {
			return "", errors.New("VOICE_RECIPE_MISMATCH")
		}
	}
	if part == "amount" {
		return core, nil
	}
	if !profile.FunEnabled || selected == nil {
		if part == "tail" {
			return "", nil
		}
		return core, nil
	}
	if selected.StyleID == "custom" {
		if part == "tail" {
			return "", errors.New("VOICE_RECIPE_MISMATCH")
		}
		return voicecopy.FormatAnnouncementTemplate(profile.CustomTemplate, amount, description, profile.IncludeDescription), nil
	}
	if template, ok := voicecopy.ClassicTemplate(phraseID); ok {
		if part == "tail" {
			return "", errors.New("VOICE_RECIPE_MISMATCH")
		}
		return voicecopy.FormatAnnouncementTemplate(template, amount, description, profile.IncludeDescription), nil
	}
	tail, _ := voicecopy.RecipeTail(phraseID)
	if part == "tail" {
		return tail, nil
	}
	if tail != "" {
		core += " " + tail
	}
	if profile.IncludeDescription {
		return voicecopy.FormatAnnouncementTemplate(core, amount, description, true), nil
	}
	return core, nil
}

func (s *Server) getVoiceSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	profile, err := s.store.GetVoiceProfile(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice settings")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) getVoiceProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	profile, err := s.store.GetVoiceProfile(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice profile")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) publicVoiceProfile(w http.ResponseWriter, r *http.Request) {
	s.getVoiceProfile(w, r)
}

func (s *Server) updateVoiceSettings(w http.ResponseWriter, r *http.Request) {
	var input storage.VoiceSettingsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	saved, err := s.store.SaveVoiceProfile(r.Context(), input)
	if err != nil {
		if errors.Is(err, storage.ErrVoiceSettingsConflict) {
			writeError(w, http.StatusConflict, "voice settings modified by another operator")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	audit(s.store, r, "voice_settings.update", fmt.Sprintf("rev_%d", saved.Revision))
	s.publishStateEvent("voice.settings.changed", "singleton", map[string]any{"revision": saved.Revision})
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) voiceStatus(w http.ResponseWriter, r *http.Request) {
	profile, _ := s.store.GetVoiceProfile(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"available":    s.ttsClient != nil,
		"primary":      "edge",
		"providerMode": profile.ProviderMode,
		"edgeVoice":    profile.EdgeVoice,
		"rate":         profile.Rate,
		"pitchHz":      profile.PitchHz,
		"revision":     profile.Revision,
	})
}

func writeAudioStream(w http.ResponseWriter, stream *ttsclient.StreamResult, profile storage.VoiceProfile) {
	defer stream.Close()
	if stream.Provider != "edge" || stream.Voice != profile.EdgeVoice || stream.Fallback {
		writeError(w, http.StatusBadGateway, "VOICE_SYNTHESIS_PROFILE_MISMATCH")
		return
	}

	firstByteMs := stream.FirstByteDuration.Milliseconds()

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-TTS-Provider", stream.Provider)
	w.Header().Set("X-TTS-Voice", stream.Voice)
	w.Header().Set("X-TTS-Fallback", fmt.Sprintf("%t", stream.Fallback))
	w.Header().Set("X-TTS-Cached", fmt.Sprintf("%t", stream.Cached))
	w.Header().Set("X-TTS-First-Byte-Ms", strconv.FormatInt(firstByteMs, 10))
	w.Header().Set("X-Voice-Revision", strconv.FormatInt(profile.Revision, 10))
	w.Header().Set("X-Voice-Synthesis-Key", profile.SynthesisKey)
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
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
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

	req, err := parseVoiceAudioRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := checkVoiceOverrides(r, req); err != nil {
		writeError(w, http.StatusBadRequest, "VOICE_OVERRIDE_NOT_ALLOWED")
		return
	}

	profile, err := s.store.GetVoiceProfile(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice profile")
		return
	}

	if req.ProfileRevision != nil && *req.ProfileRevision != profile.Revision {
		writeError(w, http.StatusConflict, "VOICE_PROFILE_CHANGED")
		return
	}

	if profile.ProviderMode == storage.ProviderModeBrowserOnly {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}

	testAmount := int64(500000)
	identity, _ := auth.FromContext(r.Context())
	if req.AmountVND != "" {
		if identity.Role != auth.Owner && identity.Role != auth.Operator {
			writeError(w, http.StatusForbidden, "amount_override_forbidden")
			return
		}
		parsed, err := strconv.ParseInt(req.AmountVND, 10, 64)
		if err != nil || parsed <= 0 || parsed > 9007199254740991 {
			writeError(w, http.StatusBadRequest, "invalid_amount_vnd")
			return
		}
		testAmount = parsed
	}

	phrase, err := s.resolveVoicePhrase(profile, testAmount, "Thanh toán thử nghiệm", req.PhraseID, req.Part)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VOICE_RECIPE_MISMATCH")
		return
	}
	if phrase == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(profile.ColdRequestTimeoutMs)*time.Millisecond)
	defer cancel()
	if s.tryVoicePack(w, r.WithContext(ctx), profile, phrase) {
		return
	}
	ttsRate := formatFloatRate(profile.Rate)
	ttsPitch := formatPitchHz(profile.PitchHz)

	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:         phrase,
		Voice:        profile.EdgeVoice,
		Rate:         ttsRate,
		Pitch:        ttsPitch,
		Cacheable:    true,
		ProviderMode: profile.ProviderMode,
	})
	if err != nil {
		writeVoiceSynthesisError(w, err)
		return
	}

	writeAudioStream(w, stream, profile)
}

func (s *Server) publicTestVoiceAudio(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "tts_client_not_configured")
		return
	}

	req, err := parseVoiceAudioRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := checkVoiceOverrides(r, req); err != nil {
		writeError(w, http.StatusBadRequest, "VOICE_OVERRIDE_NOT_ALLOWED")
		return
	}

	if req.AmountVND != "" && req.AmountVND != "500000" {
		writeError(w, http.StatusForbidden, "amount_override_forbidden")
		return
	}

	profile, err := s.store.GetVoiceProfile(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice profile")
		return
	}

	if req.ProfileRevision != nil && *req.ProfileRevision != profile.Revision {
		writeError(w, http.StatusConflict, "VOICE_PROFILE_CHANGED")
		return
	}

	if profile.ProviderMode == storage.ProviderModeBrowserOnly {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}

	phrase, err := s.resolveVoicePhrase(profile, 500000, "", req.PhraseID, req.Part)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VOICE_RECIPE_MISMATCH")
		return
	}
	if phrase == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(profile.ColdRequestTimeoutMs)*time.Millisecond)
	defer cancel()
	if s.tryVoicePack(w, r.WithContext(ctx), profile, phrase) {
		return
	}
	ttsRate := formatFloatRate(profile.Rate)
	ttsPitch := formatPitchHz(profile.PitchHz)

	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:         phrase,
		Voice:        profile.EdgeVoice,
		Rate:         ttsRate,
		Pitch:        ttsPitch,
		Cacheable:    true,
		ProviderMode: profile.ProviderMode,
	})
	if err != nil {
		writeVoiceSynthesisError(w, err)
		return
	}

	writeAudioStream(w, stream, profile)
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

	req, err := parseVoiceAudioRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := checkVoiceOverrides(r, req); err != nil {
		writeError(w, http.StatusBadRequest, "VOICE_OVERRIDE_NOT_ALLOWED")
		return
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

	// Freshness verification: transaction must have been observed within 5 minutes for public audio
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

	profile, err := s.store.GetVoiceProfile(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice profile")
		return
	}

	if req.ProfileRevision != nil && *req.ProfileRevision != profile.Revision {
		writeError(w, http.StatusConflict, "VOICE_PROFILE_CHANGED")
		return
	}

	if profile.ProviderMode == storage.ProviderModeBrowserOnly {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}

	phrase, err := s.resolveVoicePhrase(profile, item.Credit, item.Description, req.PhraseID, req.Part)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VOICE_RECIPE_MISMATCH")
		return
	}
	if phrase == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(profile.ColdRequestTimeoutMs)*time.Millisecond)
	defer cancel()
	if s.tryVoicePack(w, r.WithContext(ctx), profile, phrase) {
		return
	}
	ttsRate := formatFloatRate(profile.Rate)
	ttsPitch := formatPitchHz(profile.PitchHz)

	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:         phrase,
		Voice:        profile.EdgeVoice,
		Rate:         ttsRate,
		Pitch:        ttsPitch,
		Cacheable:    !profile.IncludeDescription,
		ProviderMode: profile.ProviderMode,
	})
	if err != nil {
		writeVoiceSynthesisError(w, err)
		return
	}

	writeAudioStream(w, stream, profile)
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

	req, err := parseVoiceAudioRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := checkVoiceOverrides(r, req); err != nil {
		writeError(w, http.StatusBadRequest, "VOICE_OVERRIDE_NOT_ALLOWED")
		return
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

	profile, err := s.store.GetVoiceProfile(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice profile")
		return
	}

	if req.ProfileRevision != nil && *req.ProfileRevision != profile.Revision {
		writeError(w, http.StatusConflict, "VOICE_PROFILE_CHANGED")
		return
	}

	if profile.ProviderMode == storage.ProviderModeBrowserOnly {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
		return
	}

	phrase, err := s.resolveVoicePhrase(profile, item.Credit, item.Description, req.PhraseID, req.Part)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VOICE_RECIPE_MISMATCH")
		return
	}
	if phrase == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(profile.ColdRequestTimeoutMs)*time.Millisecond)
	defer cancel()
	if s.tryVoicePack(w, r.WithContext(ctx), profile, phrase) {
		return
	}
	ttsRate := formatFloatRate(profile.Rate)
	ttsPitch := formatPitchHz(profile.PitchHz)

	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:         phrase,
		Voice:        profile.EdgeVoice,
		Rate:         ttsRate,
		Pitch:        ttsPitch,
		Cacheable:    !profile.IncludeDescription,
		ProviderMode: profile.ProviderMode,
	})
	if err != nil {
		writeVoiceSynthesisError(w, err)
		return
	}

	writeAudioStream(w, stream, profile)
}

func (s *Server) synthesizeSummaryAudio(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		writeError(w, http.StatusServiceUnavailable, "tts_client_not_configured")
		return
	}

	req, err := parseVoiceAudioRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := checkVoiceOverrides(r, req); err != nil {
		writeError(w, http.StatusBadRequest, "VOICE_OVERRIDE_NOT_ALLOWED")
		return
	}

	if len(req.TransactionIDs) < 2 || len(req.TransactionIDs) > 50 {
		writeError(w, http.StatusBadRequest, "transactionIds count must be between 2 and 50")
		return
	}

	profile, err := s.store.GetVoiceProfile(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get voice profile")
		return
	}

	if req.ProfileRevision != nil && *req.ProfileRevision != profile.Revision {
		writeError(w, http.StatusConflict, "VOICE_PROFILE_CHANGED")
		return
	}

	if profile.ProviderMode == storage.ProviderModeBrowserOnly {
		writeError(w, http.StatusUnprocessableEntity, "provider_mode_browser_only")
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
			// Check overflow on addition
			if totalCredit > math.MaxInt64-item.Credit {
				writeError(w, http.StatusBadRequest, "summary_amount_overflow")
				return
			}
			totalCredit += item.Credit
			validCount++
		}
	}

	if validCount == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_valid_realtime_credits_in_summary")
		return
	}

	phrase := voicecopy.BuildBurstAnnouncement(validCount, totalCredit)
	ttsRate := formatFloatRate(profile.Rate)
	ttsPitch := formatPitchHz(profile.PitchHz)

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(profile.ColdRequestTimeoutMs)*time.Millisecond)
	defer cancel()
	stream, err := s.ttsClient.SynthesizeStream(ctx, ttsclient.SynthesizeRequest{
		Text:         phrase,
		Voice:        profile.EdgeVoice,
		Rate:         ttsRate,
		Pitch:        ttsPitch,
		Cacheable:    true,
		ProviderMode: profile.ProviderMode,
	})
	if err != nil {
		writeVoiceSynthesisError(w, err)
		return
	}

	writeAudioStream(w, stream, profile)
}
