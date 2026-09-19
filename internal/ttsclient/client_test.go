package ttsclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientSynthesizeSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/synthesize" && r.URL.Path != "/synthesize/stream" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Internal-TTS-Token") != "secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", "vi-VN-HoaiMyNeural")
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-TTS-Cached", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake_mp3_audio_data"))
	}))
	defer ts.Close()

	client := New(ts.URL, "secret-token")
	res, err := client.Synthesize(context.Background(), SynthesizeRequest{
		Text:      "Xin chào",
		Voice:     "vi-VN-HoaiMyNeural",
		Cacheable: true,
	})
	if err != nil {
		t.Fatalf("Synthesize failed: %v", err)
	}

	if string(res.Audio) != "fake_mp3_audio_data" {
		t.Errorf("unexpected audio data: %q", string(res.Audio))
	}
	if res.Provider != "edge" {
		t.Errorf("unexpected provider: %q", res.Provider)
	}
	if res.Fallback != false {
		t.Errorf("expected Fallback=false, got true")
	}
	if res.Cached != true {
		t.Errorf("expected Cached=true, got false")
	}
}

func TestClientSynthesizeStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/synthesize/stream" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Internal-TTS-Token") != "secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", "vi-VN-NamMinhNeural")
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-TTS-Cached", "false")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		_, _ = w.Write([]byte("chunk_1_"))
		if ok {
			flusher.Flush()
		}
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("chunk_2"))
		if ok {
			flusher.Flush()
		}
	}))
	defer ts.Close()

	client := New(ts.URL, "secret-token")
	stream, err := client.SynthesizeStream(context.Background(), SynthesizeRequest{
		Text:  "Thử nghiệm stream âm thanh",
		Voice: "vi-VN-NamMinhNeural",
	})
	if err != nil {
		t.Fatalf("SynthesizeStream failed: %v", err)
	}
	defer stream.Close()

	if stream.Provider != "edge" {
		t.Errorf("expected provider edge, got %q", stream.Provider)
	}
	if stream.Voice != "vi-VN-NamMinhNeural" {
		t.Errorf("expected voice vi-VN-NamMinhNeural, got %q", stream.Voice)
	}
	if stream.FirstByteDuration <= 0 {
		t.Errorf("expected positive FirstByteDuration, got %v", stream.FirstByteDuration)
	}

	allBytes, err := io.ReadAll(stream.Reader)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(allBytes) != "chunk_1_chunk_2" {
		t.Errorf("unexpected content: %q", string(allBytes))
	}
}

func TestClientSynthesizeUnauthorized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()

	client := New(ts.URL, "wrong-token")
	_, err := client.Synthesize(context.Background(), SynthesizeRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err != ErrUnauthorized {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}
