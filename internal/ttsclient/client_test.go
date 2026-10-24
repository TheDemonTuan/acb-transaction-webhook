package ttsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		w.Header().Set("X-TTS-Provider", "vieneu")
		w.Header().Set("X-TTS-Voice", "vi-VN-vieneu-v3-turbo-mai-anh-v1")
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-TTS-Cached", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake_mp3_audio_data"))
	}))
	defer ts.Close()

	client := New(ts.URL, "secret-token")
	res, err := client.Synthesize(context.Background(), SynthesizeRequest{
		Text:         "Xin chào",
		Voice:        "vi-VN-vieneu-v3-turbo-mai-anh-v1",
		ProviderMode: "VIENEU_ONLY",
		Rate:         "+0%", Pitch: "+0Hz",
		Cacheable: true,
	})
	if err != nil {
		t.Fatalf("Synthesize failed: %v", err)
	}

	if string(res.Audio) != "fake_mp3_audio_data" {
		t.Errorf("unexpected audio data: %q", string(res.Audio))
	}
	if res.Provider != "vieneu" {
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
		w.Header().Set("X-TTS-Provider", "vieneu")
		w.Header().Set("X-TTS-Voice", "vi-VN-vieneu-v3-turbo-mai-anh-v1")
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
		Text:         "Thử nghiệm stream âm thanh",
		Voice:        "vi-VN-vieneu-v3-turbo-mai-anh-v1",
		ProviderMode: "VIENEU_ONLY",
		Rate:         "+0%", Pitch: "+0Hz",
	})
	if err != nil {
		t.Fatalf("SynthesizeStream failed: %v", err)
	}
	defer stream.Close()

	if stream.Provider != "vieneu" {
		t.Errorf("expected provider vieneu, got %q", stream.Provider)
	}
	if stream.Voice != "vi-VN-vieneu-v3-turbo-mai-anh-v1" {
		t.Errorf("expected voice vi-VN-vieneu-v3-turbo-mai-anh-v1, got %q", stream.Voice)
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

func TestClientSynthesizeStreamSeparatedClientTimeouts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "vieneu")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		_, _ = w.Write([]byte("start_"))
		if ok {
			flusher.Flush()
		}
		// Sleep longer than bufferedClient timeout (60ms > 30ms)
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte("end"))
		if ok {
			flusher.Flush()
		}
	}))
	defer ts.Close()

	client := New(ts.URL, "")
	// bufferedClient has aggressive 30ms timeout, streamClient has no overall timeout
	client.WithHTTPClients(&http.Client{Timeout: 30 * time.Millisecond}, &http.Client{Timeout: 0})

	// Streaming request must succeed even though duration exceeds bufferedClient timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.SynthesizeStream(ctx, SynthesizeRequest{Text: "long stream"})
	if err != nil {
		t.Fatalf("SynthesizeStream failed: %v", err)
	}
	defer stream.Close()

	data, err := io.ReadAll(stream.Reader)
	if err != nil {
		t.Fatalf("read stream failed: %v", err)
	}
	if string(data) != "start_end" {
		t.Errorf("unexpected stream content: %q", string(data))
	}
}

type synthesisRoundTripper func(*http.Request) (*http.Response, error)

func (fn synthesisRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type trackedSynthesisBody struct {
	io.Reader
	reads  int
	closes int
}

func (b *trackedSynthesisBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.reads += n
	return n, err
}

func (b *trackedSynthesisBody) Close() error {
	b.closes++
	return nil
}

func TestSynthesisPropagatesRemainingBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, budget := range []time.Duration{10 * time.Millisecond, 500 * time.Millisecond, 20 * time.Second} {
			t.Run(budget.String()+map[bool]string{false: " buffered", true: " stream"}[stream], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				parentDeadline, _ := ctx.Deadline()
				transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
					var payload SynthesizeRequest
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload.TimeoutMS < 1 || payload.TimeoutMS > 8000 || time.Duration(payload.TimeoutMS)*time.Millisecond > budget {
						t.Fatalf("timeout_ms=%d exceeds remaining budget %s", payload.TimeoutMS, budget)
					}
					deadline, ok := req.Context().Deadline()
					if !ok || deadline.After(parentDeadline) || time.Until(deadline) > maxSynthesisDuration {
						t.Fatalf("request refreshed or lost deadline: %v", deadline)
					}
					if budget < maxSynthesisDuration && !deadline.Equal(parentDeadline) {
						t.Fatalf("parent deadline changed: got %v, want %v", deadline, parentDeadline)
					}
					wantPath := "/synthesize"
					if stream {
						wantPath += "/stream"
					}
					if req.URL.Path != wantPath {
						t.Fatalf("unexpected path: %s", req.URL.Path)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("audio"))}, nil
				})
				client := New("http://tts.invalid", "").WithHTTPClients(&http.Client{Transport: transport}, &http.Client{Transport: transport})
				request := SynthesizeRequest{Text: "test", TimeoutMS: 8000}
				if stream {
					result, err := client.SynthesizeStream(ctx, request)
					if err != nil {
						t.Fatal(err)
					}
					defer result.Close()
				} else if _, err := client.Synthesize(ctx, request); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSynthesisStatusErrorsAreSafeAndNeverRetry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			status int
			want   error
		}{
			{http.StatusUnauthorized, ErrUnauthorized},
			{http.StatusServiceUnavailable, ErrOverloaded},
			{http.StatusGatewayTimeout, ErrTimeout},
			{http.StatusBadGateway, ErrUnavailable},
			{http.StatusNotFound, ErrSynthesisFailed},
			{http.StatusInternalServerError, ErrSynthesisFailed},
		} {
			t.Run(http.StatusText(tc.status)+map[bool]string{false: " buffered", true: " stream"}[stream], func(t *testing.T) {
				body := &trackedSynthesisBody{Reader: strings.NewReader("secret upstream account and token")}
				calls := 0
				transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
				})
				client := New("http://tts.invalid", "").WithHTTPClients(&http.Client{Transport: transport}, &http.Client{Transport: transport})
				var err error
				if stream {
					_, err = client.SynthesizeStream(context.Background(), SynthesizeRequest{Text: "test"})
				} else {
					_, err = client.Synthesize(context.Background(), SynthesizeRequest{Text: "test"})
				}
				if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "secret") {
					t.Fatalf("unsafe or incorrectly classified error: %v", err)
				}
				if calls != 1 || body.closes != 1 || body.reads != 0 {
					t.Fatalf("error handling retried or exposed body: calls=%d closes=%d reads=%d", calls, body.closes, body.reads)
				}
			})
		}
	}
}

func TestSynthesisDeadlineDuringHeaders(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "stream"}[stream], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			})
			client := New("http://tts.invalid", "").WithHTTPClients(&http.Client{Transport: transport}, &http.Client{Transport: transport})
			var err error
			if stream {
				_, err = client.SynthesizeStream(ctx, SynthesizeRequest{Text: "test"})
			} else {
				_, err = client.Synthesize(ctx, SynthesizeRequest{Text: "test"})
			}
			if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "tts.invalid") {
				t.Fatalf("expected safe timeout with deadline identity, got %v", err)
			}
		})
	}
}

func TestSynthesisExpiredContextDoesNotSend(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
		t.Fatal("expired request reached upstream")
		return nil, nil
	})
	client := New("http://tts.invalid", "").WithHTTPClients(&http.Client{Transport: transport}, &http.Client{Transport: transport})
	if _, err := client.Synthesize(ctx, SynthesizeRequest{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("buffered expired context: %v", err)
	}
	if _, err := client.SynthesizeStream(ctx, SynthesizeRequest{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("stream expired context: %v", err)
	}
}

func TestSynthesisAudioSizeLimitForEveryConsumer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, size := range []int{maxAudioBytes, maxAudioBytes + 1} {
			t.Run(map[bool]string{false: "buffered", true: "stream"}[stream]+map[bool]string{false: " exact", true: " oversized"}[size > maxAudioBytes], func(t *testing.T) {
				body := &trackedSynthesisBody{Reader: bytes.NewReader(make([]byte, size))}
				transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
				})
				client := New("http://tts.invalid", "").WithHTTPClients(&http.Client{Transport: transport}, &http.Client{Transport: transport})
				var err error
				var audio []byte
				if stream {
					result, startErr := client.SynthesizeStream(context.Background(), SynthesizeRequest{})
					if startErr != nil {
						t.Fatal(startErr)
					}
					audio, err = io.ReadAll(result.Reader)
					_ = result.Close()
				} else {
					result, synthErr := client.Synthesize(context.Background(), SynthesizeRequest{})
					err = synthErr
					if result != nil {
						audio = result.Audio
					}
				}
				if size > maxAudioBytes {
					if !errors.Is(err, ErrSynthesisFailed) {
						t.Fatalf("oversized audio silently truncated: %v", err)
					}
				} else if err != nil || len(audio) != maxAudioBytes {
					t.Fatalf("exact-limit audio rejected: len=%d err=%v", len(audio), err)
				}
				if body.closes != 1 || body.reads > maxAudioBytes+1 {
					t.Fatalf("unbounded or unclosed body: reads=%d closes=%d", body.reads, body.closes)
				}
			})
		}
	}
}

type interruptedSynthesisBody struct {
	ctx     context.Context
	started bool
	closes  int
}

func (b *interruptedSynthesisBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		return copy(p, "first audio"), nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *interruptedSynthesisBody) Close() error {
	b.closes++
	return nil
}

func TestSynthesisBodyDeadlineDoesNotRenewOrRetry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "stream"}[stream], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			var body *interruptedSynthesisBody
			calls := 0
			transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				body = &interruptedSynthesisBody{ctx: req.Context()}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
			})
			client := New("http://tts.invalid", "").WithHTTPClients(&http.Client{Transport: transport}, &http.Client{Transport: transport})
			var err error
			if stream {
				result, startErr := client.SynthesizeStream(ctx, SynthesizeRequest{})
				if startErr != nil {
					t.Fatal(startErr)
				}
				_, err = io.ReadAll(result.Reader)
				_ = result.Close()
			} else {
				_, err = client.Synthesize(ctx, SynthesizeRequest{})
			}
			if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) || calls != 1 || body.closes != 1 {
				t.Fatalf("interrupted body contract: err=%v calls=%d closes=%d", err, calls, body.closes)
			}
		})
	}
}

func TestSynthesisStreamCloseCancelsOwnerOnce(t *testing.T) {
	var requestContext context.Context
	body := &trackedSynthesisBody{Reader: strings.NewReader("first audio")}
	transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
		requestContext = req.Context()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	client := New("http://tts.invalid", "").WithHTTPClients(nil, &http.Client{Transport: transport})
	result, err := client.SynthesizeStream(context.Background(), SynthesizeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Close()
	_ = result.Close()
	if requestContext.Err() != context.Canceled || body.closes != 1 {
		t.Fatalf("stream owner not released once: err=%v closes=%d", requestContext.Err(), body.closes)
	}
}

func TestSynthesisHeaderTimeoutIsOwnedByContext(t *testing.T) {
	client := New("http://tts.invalid", "")
	transport, ok := client.streamClient.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout != 0 {
		t.Fatal("stream transport imposes a separate premature header timeout")
	}
}

func TestSynthesisTransportErrorsAreSafe(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "stream"}[stream], func(t *testing.T) {
			transport := synthesisRoundTripper(func(req *http.Request) (*http.Response, error) {
				return nil, errors.New("secret upstream token and receiver")
			})
			client := New("http://tts.invalid", "").WithHTTPClients(&http.Client{Transport: transport}, &http.Client{Transport: transport})
			var err error
			if stream {
				_, err = client.SynthesizeStream(context.Background(), SynthesizeRequest{})
			} else {
				_, err = client.Synthesize(context.Background(), SynthesizeRequest{})
			}
			if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "tts.invalid") {
				t.Fatalf("transport error escaped redaction: %v", err)
			}
		})
	}
}

func TestSynthesisRejectsEmptySuccessfulHTTPBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	client := New(upstream.URL, "fixture")
	if _, err := client.Synthesize(context.Background(), SynthesizeRequest{}); !errors.Is(err, ErrSynthesisFailed) {
		t.Fatalf("empty buffered audio accepted: %v", err)
	}
	if _, err := client.SynthesizeStream(context.Background(), SynthesizeRequest{}); !errors.Is(err, ErrSynthesisFailed) {
		t.Fatalf("empty streamed audio accepted: %v", err)
	}
}
