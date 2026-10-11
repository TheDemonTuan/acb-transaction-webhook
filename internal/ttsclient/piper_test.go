package ttsclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPiperBufferedAndStreamingRemainExplicit(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false:"buffered",true:"streaming"}[streaming], func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wantPath := "/synthesize"
				if streaming { wantPath += "/stream" }
				if r.URL.Path != wantPath { t.Errorf("unexpected retry or endpoint: %s",r.URL.Path) }
				var req SynthesizeRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil { t.Error(err) }
				if req.ProviderMode != "PIPER_ONLY" || req.Voice != "vi-VN-vais1000-medium" || req.Rate != "+25%" || req.Pitch != "+0Hz" || req.TimeoutMS < 1 || req.TimeoutMS > 8000 {
					t.Errorf("provider/rate must pass through exactly once: %+v",req)
				}
				w.Header().Set("Content-Type","audio/mpeg")
				w.Header().Set("X-TTS-Provider","piper")
				w.Header().Set("X-TTS-Voice",req.Voice)
				w.Header().Set("X-TTS-Fallback","false")
				_, _ = w.Write([]byte("ID3piper-audio"))
			}))
			defer server.Close()
			client := New(server.URL,"")
			req := SynthesizeRequest{Text:"Đã nhận năm trăm nghìn đồng.",Voice:"vi-VN-vais1000-medium",ProviderMode:"PIPER_ONLY",Rate:"+25%",Pitch:"+0Hz",Cacheable:true}
			if streaming {
				result, err := client.SynthesizeStream(context.Background(),req)
				if err != nil { t.Fatal(err) }
				defer result.Close()
				audio, err := io.ReadAll(result.Reader)
				if err != nil || string(audio) != "ID3piper-audio" || result.Provider != "piper" || result.Voice != req.Voice || result.Fallback { t.Fatalf("incorrect stream: %+v audio=%q err=%v",result,audio,err) }
			} else {
				result, err := client.Synthesize(context.Background(),req)
				if err != nil { t.Fatal(err) }
				if string(result.Audio) != "ID3piper-audio" || result.Provider != "piper" || result.Voice != req.Voice || result.Fallback { t.Fatalf("incorrect buffered audio: %+v",result) }
			}
			if calls != 1 { t.Fatalf("unexpected synthesis retry count: %d",calls) }
		})
	}
}

func TestMissingProviderMetadataIsNotInvented(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) { _, _ = w.Write([]byte("ID3audio")) }))
	defer server.Close()
	client := New(server.URL,"")
	buffered, err := client.Synthesize(context.Background(),SynthesizeRequest{Text:"test",ProviderMode:"PIPER_ONLY",Voice:"vi-VN-vais1000-medium"})
	if err != nil { t.Fatal(err) }
	if buffered.Provider != "" { t.Fatalf("invented provider: %q",buffered.Provider) }
	stream, err := client.SynthesizeStream(context.Background(),SynthesizeRequest{Text:"test",ProviderMode:"PIPER_ONLY",Voice:"vi-VN-vais1000-medium"})
	if err != nil { t.Fatal(err) }
	defer stream.Close()
	if stream.Provider != "" { t.Fatalf("invented streaming provider: %q",stream.Provider) }
}
