package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
)

func (s *Server) voiceCatalog(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "version") != voicecopy.CatalogVersion {
		http.NotFound(w, r)
		return
	}
	data := voicecopy.CatalogBytes()
	if len(data) == 0 {
		writeError(w, http.StatusServiceUnavailable, "VOICE_CATALOG_UNAVAILABLE")
		return
	}
	etag := voicecopy.CatalogETag()
	w.Header().Set("Cache-Control", "public,max-age=31536000,immutable")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) voiceAssetManifest(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		http.NotFound(w, r)
		return
	}
	data, manifest, status, err := s.ttsClient.VoiceManifest(r.Context(), chi.URLParam(r, "key"))
	if err != nil || status != http.StatusOK {
		http.NotFound(w, r)
		return
	}
	etag := `"` + manifest.PackVersion + `"`
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) voiceAssetClip(w http.ResponseWriter, r *http.Request) {
	if s.ttsClient == nil {
		http.NotFound(w, r)
		return
	}
	data, status, err := s.ttsClient.VoiceClip(r.Context(), chi.URLParam(r, "hash"))
	if err != nil || status != http.StatusOK {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public,max-age=31536000,immutable")
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+chi.URLParam(r, "hash")+`"`)
	_, _ = w.Write(data)
}

// tryVoicePack returns only exact-profile, exact-transcript published audio.
// A missing or corrupt pack stays on the fixed-voice cold synthesis path.
func (s *Server) tryVoicePack(w http.ResponseWriter, r *http.Request, profile storage.VoiceProfile, phrase string) bool {
	if s.ttsClient == nil || phrase == "" || profile.IncludeDescription {
		return false
	}
	_, manifest, status, err := s.ttsClient.VoiceManifest(r.Context(), profile.SynthesisKey)
	if err != nil || status != http.StatusOK || manifest.VoiceID != profile.EdgeVoice || manifest.Rate != profile.Rate || manifest.PitchHz != profile.PitchHz {
		return false
	}
	for _, clip := range manifest.Clips {
		if clip.Transcript != phrase {
			continue
		}
		data, status, err := s.ttsClient.VoiceClip(r.Context(), clip.SHA256)
		if err != nil || status != http.StatusOK || int64(len(data)) != clip.Bytes {
			return false
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "private,no-store")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", profile.EdgeVoice)
		w.Header().Set("X-TTS-Cached", "true")
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-Voice-Revision", strconv.FormatInt(profile.Revision, 10))
		w.Header().Set("X-Voice-Synthesis-Key", profile.SynthesisKey)
		_, _ = w.Write(data)
		return true
	}
	return false
}
