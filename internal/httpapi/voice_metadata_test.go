package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

func TestVoiceStreamRejectsDifferentVoiceBeforePlayback(t *testing.T) {
	for _, fixture := range []struct {
		provider, voice string
		fallback        bool
	}{
		{"edge", storage.EdgeVoiceNamMinh, false},
		{"gtts", storage.EdgeVoiceHoaiMy, false},
		{"edge", storage.EdgeVoiceHoaiMy, true},
	} {
		stream := &ttsclient.StreamResult{Reader: strings.NewReader("must not play"), Closer: io.NopCloser(strings.NewReader("")), Provider: fixture.provider, Voice: fixture.voice, Fallback: fixture.fallback}
		w := httptest.NewRecorder()
		writeAudioStream(w, stream, storage.DefaultVoiceProfile)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "must not play") {
			t.Fatalf("wrong-profile synthesis escaped: status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestVoiceRevisionCannotBypassConflictWithMalformedQuery(t *testing.T) {
	for _, revision := range []string{"", "abc", "0", "-1", "01", "9223372036854775808"} {
		req := httptest.NewRequest(http.MethodGet, "/voice/test?profileRevision="+revision, nil)
		if _, err := parseVoiceAudioRequest(req); err == nil {
			t.Fatalf("malformed revision accepted: %q", revision)
		}
	}
}
