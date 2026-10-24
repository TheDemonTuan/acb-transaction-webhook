package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

// A real HTTP server is required: ResponseRecorder cannot detect truncated framing.
func TestAudioStreamUpstreamInterruptionIsNotSuccessfulEOF(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "complete"
		if interrupted {
			name = "interrupted"
		}
		t.Run(name, func(t *testing.T) {
			profile := storage.DefaultVoiceProfile
			firstRead := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-TTS-Provider", "vieneu")
				w.Header().Set("X-TTS-Voice", profile.VoiceID)
				_, _ = w.Write([]byte("first sentence"))
				w.(http.Flusher).Flush()
				<-firstRead
				if interrupted {
					panic(http.ErrAbortHandler)
				}
				_, _ = w.Write([]byte(" last sentence"))
			}))
			defer upstream.Close()
			client := ttsclient.New(upstream.URL, "fixture")
			proxy := httptest.NewServer(recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stream, err := client.SynthesizeStream(r.Context(), ttsclient.SynthesizeRequest{Text: "fixture", Voice: profile.VoiceID, ProviderMode: profile.ProviderMode})
				close(firstRead)
				if err != nil {
					writeError(w, http.StatusBadGateway, "TTS_UNAVAILABLE")
					return
				}
				writeAudioStream(w, stream, profile)
			})))
			defer proxy.Close()
			downstream := &http.Client{Timeout: 5 * time.Second}
			resp, err := downstream.Get(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%q", resp.StatusCode, body)
			}
			if interrupted {
				if readErr == nil || string(body) != "first sentence" {
					t.Fatalf("truncated stream became EOF: body=%q err=%v", body, readErr)
				}
			} else if readErr != nil || string(body) != "first sentence last sentence" {
				t.Fatalf("complete stream: body=%q err=%v", body, readErr)
			}
		})
	}
}
