package realtimestream_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/realtimestream"
)

func TestServerAuth(t *testing.T) {
	hub := eventhub.New()
	srv := realtimestream.NewServer(realtimestream.ServerConfig{
		Hub:   hub,
		Token: "correct-secret-token",
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "no token",
			headers:    nil,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "wrong token",
			headers: map[string]string{
				realtimestream.HeaderInternalToken: "wrong-token",
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "valid token header",
			headers: map[string]string{
				realtimestream.HeaderInternalToken: "correct-secret-token",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "valid bearer token fallback",
			headers: map[string]string{
				"Authorization": "Bearer correct-secret-token",
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+realtimestream.DefaultPath, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestServerMethodAndPath(t *testing.T) {
	hub := eventhub.New()
	srv := realtimestream.NewServer(realtimestream.ServerConfig{
		Hub:   hub,
		Token: "test-token",
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// POST not allowed
	req, _ := http.NewRequest(http.MethodPost, ts.URL+realtimestream.DefaultPath, nil)
	req.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}

	// Wrong path returns 404
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/wrong/path", nil)
	req.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wrong path request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong path status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestServerMaxSubscribers(t *testing.T) {
	hub := eventhub.New()
	srv := realtimestream.NewServer(realtimestream.ServerConfig{
		Hub:            hub,
		Token:          "test-token",
		MaxSubscribers: 2,
		Heartbeat:      1 * time.Second,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, ts.URL+realtimestream.DefaultPath, nil)
	req1.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("client 1 failed: resp=%v err=%v", resp1, err)
	}
	defer resp1.Body.Close()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, ts.URL+realtimestream.DefaultPath, nil)
	req2.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("client 2 failed: resp=%v err=%v", resp2, err)
	}
	defer resp2.Body.Close()

	if srv.ActiveSubscribers() != 2 {
		t.Fatalf("active subscribers = %d, want 2", srv.ActiveSubscribers())
	}

	// 3rd client exceeds limit -> 429
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+realtimestream.DefaultPath, nil)
	req3.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("client 3 request: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("client 3 status = %d, want %d", resp3.StatusCode, http.StatusTooManyRequests)
	}

	// Disconnect client 1
	cancel1()
	_ = resp1.Body.Close()
	time.Sleep(50 * time.Millisecond)

	// Now client 4 should succeed
	ctx4, cancel4 := context.WithCancel(context.Background())
	defer cancel4()
	req4, _ := http.NewRequestWithContext(ctx4, http.MethodGet, ts.URL+realtimestream.DefaultPath, nil)
	req4.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil || resp4.StatusCode != http.StatusOK {
		t.Fatalf("client 4 failed after disconnect: resp=%v err=%v", resp4, err)
	}
	defer resp4.Body.Close()
}

func TestServerPreservesJSONPayloadNotBase64(t *testing.T) {
	hub := eventhub.New()
	srv := realtimestream.NewServer(realtimestream.ServerConfig{
		Hub:          hub,
		Token:        "test-token",
		Heartbeat:    5 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+realtimestream.DefaultPath, nil)
	req.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	// Wait for connection to establish then publish
	time.Sleep(30 * time.Millisecond)
	rawJSON := `{"accountNumber":"123456","amount":500000,"currency":"VND"}`
	hub.Publish(eventhub.Event{
		Seq:         42,
		Epoch:       "ep1",
		EventType:   "bank.transaction.credit",
		AggregateID: "tx_abc123",
		Payload:     []byte(rawJSON),
		CreatedAt:   "2026-09-14T10:00:00Z",
	})

	buf := make([]byte, 2048)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read stream: %v", err)
	}
	streamOutput := string(buf[:n])

	if !strings.Contains(streamOutput, "id: ep1:42") {
		t.Fatalf("expected id: ep1:42 in output, got: %s", streamOutput)
	}
	if !strings.Contains(streamOutput, "event: bank.transaction.credit") {
		t.Fatalf("expected event: bank.transaction.credit in output, got: %s", streamOutput)
	}
	// Verify raw JSON is preserved, not base64 encoded
	if !strings.Contains(streamOutput, `"payload":{"accountNumber":"123456","amount":500000,"currency":"VND"}`) {
		t.Fatalf("expected unencoded JSON payload in SSE data, got: %s", streamOutput)
	}
	if strings.Contains(streamOutput, "eyJh") {
		t.Fatalf("detected base64 encoding of payload in output: %s", streamOutput)
	}
}

func TestServerHeartbeat(t *testing.T) {
	hub := eventhub.New()
	srv := realtimestream.NewServer(realtimestream.ServerConfig{
		Hub:          hub,
		Token:        "test-token",
		Heartbeat:    25 * time.Millisecond,
		WriteTimeout: 1 * time.Second,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+realtimestream.DefaultPath, nil)
	req.Header.Set(realtimestream.HeaderInternalToken, "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 1024)
	var accumulated strings.Builder
	var mu sync.Mutex

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			mu.Lock()
			accumulated.Write(buf[:n])
			mu.Unlock()
			if strings.Contains(accumulated.String(), ": heartbeat\n\n") {
				break
			}
		}
		if err != nil {
			break
		}
	}

	mu.Lock()
	content := accumulated.String()
	mu.Unlock()

	if !strings.Contains(content, ": heartbeat\n\n") {
		t.Fatalf("expected heartbeat comment in output, got: %q", content)
	}
}
