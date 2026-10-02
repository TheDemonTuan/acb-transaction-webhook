package captchasolver

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func requireDiagnostic(t *testing.T, err error, stage, code string, status int) {
	t.Helper()
	var diagnostic *DiagnosticError
	if !errors.Is(err, ErrUnavailable) || !errors.As(err, &diagnostic) {
		t.Fatalf("missing safe diagnostic: %v", err)
	}
	if diagnostic.Stage != stage || diagnostic.Code != code || diagnostic.HTTPStatus != status {
		t.Fatalf("diagnostic = %+v, want %s/%s/%d", diagnostic, stage, code, status)
	}
	for _, secret := range []string{"provider-private-payload", "synthetic-secret-key", "private-model-id", "private-host", "private-error"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("diagnostic exposed provider or config data")
		}
	}
	if errors.Unwrap(err) != ErrUnavailable {
		t.Fatal("diagnostic retained an unsafe underlying error")
	}
}

func TestNineRouterDiscoveryDiagnostics(t *testing.T) {
	const exact = `{"data":[{"id":"private-model-id","owned_by":"provider"}]}`
	for _, tc := range []struct {
		name, first, fallback, code                                string
		status, fallbackStatus, wantStatus, fallbacks, completions int
	}{
		{name: "unauthorized", status: 401, code: "HTTP_AUTH", wantStatus: 401},
		{name: "forbidden", status: 403, code: "HTTP_AUTH", wantStatus: 403},
		{name: "quota", status: 429, code: "HTTP_RATE_LIMIT", wantStatus: 429},
		{name: "upstream", status: 503, code: "HTTP_UPSTREAM", wantStatus: 503},
		{name: "redirect", status: 302, code: "HTTP_UPSTREAM", wantStatus: 302},
		{name: "404 fallback", status: 404, fallbackStatus: 200, fallback: exact, fallbacks: 1, completions: 1},
		{name: "405 fallback", status: 405, fallbackStatus: 200, fallback: exact, fallbacks: 1, completions: 1},
		{name: "empty fallback", status: 200, first: `{"data":[]}`, fallbackStatus: 200, fallback: exact, fallbacks: 1, completions: 1},
		{name: "schema fallback", status: 200, first: `{"data":"provider-private-payload"}`, fallbackStatus: 200, fallback: exact, fallbacks: 1, completions: 1},
		{name: "missing fallback", status: 200, first: `{"data":[{"id":"other-model"}]}`, fallbackStatus: 200, fallback: exact, fallbacks: 1, completions: 1},
		{name: "exact first", status: 200, first: exact, completions: 1},
		{name: "combo first", status: 200, first: `{"data":[{"id":"private-model-id","owned_by":"combo"}]}`, code: "MODEL_COMBO_UNSUPPORTED", wantStatus: 200},
		{name: "combo fallback", status: 404, fallbackStatus: 200, fallback: `{"data":[{"id":"private-model-id","owned_by":"combo"}]}`, fallbacks: 1, code: "MODEL_COMBO_UNSUPPORTED", wantStatus: 200},
		{name: "absent exact ID", status: 200, first: `{"data":[]}`, fallbackStatus: 200, fallback: `{"data":[{"id":"private-model-id-suffix"}]}`, fallbacks: 1, code: "MODEL_NOT_FOUND", wantStatus: 200},
		{name: "invalid fallback schema", status: 404, fallbackStatus: 200, fallback: `{"data":null,"error":"provider-private-payload"}`, fallbacks: 1, code: "RESPONSE_SCHEMA", wantStatus: 200},
		{name: "fallback not found", status: 404, fallbackStatus: 404, fallbacks: 1, code: "HTTP_NOT_FOUND", wantStatus: 404},
		{name: "fallback auth", status: 404, fallbackStatus: 401, fallbacks: 1, code: "HTTP_AUTH", wantStatus: 401},
		{name: "discovery oversized", status: 200, first: strings.Repeat("provider-private-payload", 4096), code: "RESPONSE_TOO_LARGE", wantStatus: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var discoveries, fallbacks, completions atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-secret-key" {
					t.Error("missing candidate key")
				}
				switch r.URL.Path {
				case "/v1/models/image-to-text":
					discoveries.Add(1)
					w.WriteHeader(tc.status)
					payload := tc.first
					if tc.status >= 300 {
						payload = "provider-private-payload"
					}
					_, _ = io.WriteString(w, payload+" ")
				case "/v1/models":
					fallbacks.Add(1)
					w.WriteHeader(tc.fallbackStatus)
					payload := tc.fallback
					if tc.fallbackStatus >= 300 {
						payload = "provider-private-payload"
					}
					_, _ = io.WriteString(w, payload+" ")
				case "/v1/chat/completions":
					completions.Add(1)
					var body struct {
						Model    string `json:"model"`
						Messages []struct {
							Content json.RawMessage `json:"content"`
						} `json:"messages"`
					}
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "private-model-id" || len(body.Messages) != 2 {
						t.Error("synthetic request did not use exact model")
						return
					}
					var content []struct {
						Type     string `json:"type"`
						ImageURL struct {
							URL string `json:"url"`
						} `json:"image_url"`
					}
					if json.Unmarshal(body.Messages[1].Content, &content) != nil || len(content) != 2 || content[1].ImageURL.URL != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(SyntheticPNG()) {
						t.Error("preflight image was not the synthetic PNG")
					}
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"text\":\"AB12CD\"}"}}]}`)
				default:
					t.Error("unexpected request")
				}
			}))
			defer api.Close()
			n, err := NewNineRouter(Config{BaseURL: api.URL, Model: "private-model-id", APIKey: "synthetic-secret-key"})
			if err != nil {
				t.Fatal(err)
			}
			err = n.CheckConfig(context.Background())
			if tc.code != "" {
				requireDiagnostic(t, err, "MODEL_DISCOVERY", tc.code, tc.wantStatus)
			} else if err != nil {
				t.Fatal(err)
			}
			if discoveries.Load() != 1 || int(fallbacks.Load()) != tc.fallbacks || int(completions.Load()) != tc.completions {
				t.Fatalf("unexpected requests: discovery=%d fallback=%d OCR=%d", discoveries.Load(), fallbacks.Load(), completions.Load())
			}
		})
	}
}

func TestNineRouterOCRDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"auth", "provider-private-payload", "HTTP_AUTH", 401},
		{"rate", "provider-private-payload", "HTTP_RATE_LIMIT", 429},
		{"upstream", "provider-private-payload", "HTTP_UPSTREAM", 500},
		{"not found", "provider-private-payload", "HTTP_NOT_FOUND", 404},
		{"refusal", `{"choices":[{"message":{"refusal":"provider-private-payload"}}]}`, "REFUSAL", 200},
		{"schema", `{"choices":[{"message":{"content":"provider-private-payload"}}]}`, "RESPONSE_SCHEMA", 200},
		{"oversized", strings.Repeat("provider-private-payload", 1024), "RESPONSE_TOO_LARGE", 200},
		{"mismatch", `{"choices":[{"message":{"content":"{\"text\":\"ZZ99XX\"}"}}]}`, "OCR_MISMATCH", 200},
		{"case mismatch", `{"choices":[{"message":{"content":"{\"text\":\"ab12cd\"}"}}]}`, "OCR_MISMATCH", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var completions atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models/image-to-text" {
					_, _ = io.WriteString(w, `{"data":[{"id":"private-model-id"}]}`)
					return
				}
				completions.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer api.Close()
			n, err := NewNineRouter(Config{BaseURL: api.URL, Model: "private-model-id", APIKey: "synthetic-secret-key"})
			if err != nil {
				t.Fatal(err)
			}
			requireDiagnostic(t, n.CheckConfig(context.Background()), "SYNTHETIC_OCR", tc.code, tc.status)
			if completions.Load() != 1 {
				t.Fatal("synthetic OCR was retried")
			}
			answer, err := n.Solve(context.Background(), SyntheticPNG())
			if tc.code == "OCR_MISMATCH" {
				if err != nil || (answer != "ZZ99XX" && answer != "ab12cd") {
					t.Fatal("runtime applied synthetic-answer policy")
				}
			} else {
				requireDiagnostic(t, err, "CAPTCHA_OCR", tc.code, tc.status)
				if answer != "" {
					t.Fatal("failure exposed an answer")
				}
			}
		})
	}
}

func TestNineRouterTransportDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, code     string
		transportError error
	}{
		{"DNS", "DNS", &net.DNSError{Err: "private-error", Name: "private-host", IsNotFound: true}},
		{"NETWORK", "NETWORK", errors.New("private-error")},
		{"TIMEOUT", "TIMEOUT", context.DeadlineExceeded},
		{"CANCELLED", "CANCELLED", context.Canceled},
		{"TLS record", "TLS", tls.RecordHeaderError{Msg: "private-error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				calls.Add(1)
				return nil, tc.transportError
			}}}
			n, err := NewNineRouter(Config{BaseURL: "http://private-host", Model: "private-model-id", APIKey: "synthetic-secret-key", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			requireDiagnostic(t, n.CheckConfig(context.Background()), "MODEL_DISCOVERY", tc.code, 0)
			if calls.Load() != 1 {
				t.Fatal("transport failure caused discovery fallback")
			}
		})
	}
	t.Run("real TLS verification", func(t *testing.T) {
		api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request crossed untrusted TLS") }))
		defer api.Close()
		n, err := NewNineRouter(Config{BaseURL: api.URL, Model: "private-model-id", APIKey: "synthetic-secret-key"})
		if err != nil {
			t.Fatal(err)
		}
		requireDiagnostic(t, n.CheckConfig(context.Background()), "MODEL_DISCOVERY", "TLS", 0)
	})
}

func TestNineRouterBoundedDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name, stage                                  string
		total, discovery, ocr                        time.Duration
		blockOCR, runtime, cancelled, parentDeadline bool
	}{
		{name: "discovery deadline", stage: "MODEL_DISCOVERY", total: time.Second, discovery: 25 * time.Millisecond, ocr: time.Second},
		{name: "synthetic deadline", stage: "SYNTHETIC_OCR", total: time.Second, discovery: time.Second, ocr: 25 * time.Millisecond, blockOCR: true},
		{name: "runtime deadline", stage: "CAPTCHA_OCR", total: time.Second, discovery: time.Second, ocr: 25 * time.Millisecond, runtime: true},
		{name: "total deadline", stage: "SYNTHETIC_OCR", total: 100 * time.Millisecond, discovery: time.Second, ocr: time.Second, blockOCR: true},
		{name: "parent deadline", stage: "MODEL_DISCOVERY", total: time.Second, discovery: time.Second, ocr: time.Second, parentDeadline: true},
		{name: "parent cancellation", stage: "MODEL_DISCOVERY", total: time.Second, discovery: time.Second, ocr: time.Second, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.parentDeadline {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer deadlineCancel()
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.blockOCR && r.URL.Path == "/v1/models/image-to-text" {
					_, _ = io.WriteString(w, `{"data":[{"id":"private-model-id"}]}`)
					return
				}
				if tc.cancelled {
					cancel()
				}
				<-r.Context().Done()
			}))
			defer api.Close()
			n, err := NewNineRouter(Config{BaseURL: api.URL, Model: "private-model-id", APIKey: "synthetic-secret-key"})
			if err != nil {
				t.Fatal(err)
			}
			n.timeouts = requestTimeouts{check: tc.total, discovery: tc.discovery, ocr: tc.ocr}
			start := time.Now()
			if tc.runtime {
				_, err = n.Solve(ctx, SyntheticPNG())
			} else {
				err = n.CheckConfig(ctx)
			}
			code := "TIMEOUT"
			if tc.cancelled {
				code = "CANCELLED"
			}
			requireDiagnostic(t, err, tc.stage, code, 0)
			if time.Since(start) > 500*time.Millisecond {
				t.Fatal("request escaped bounded deadline")
			}
			wantCalls := int32(1)
			if tc.blockOCR {
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Fatal("timeout retried or fell back")
			}
		})
	}
}

func TestDiagnosticErrorRejectsUnsafeFields(t *testing.T) {
	err := &DiagnosticError{Stage: "provider-private-payload", Code: "synthetic-secret-key", HTTPStatus: 999999}
	if strings.Contains(err.Error(), "provider-private-payload") || strings.Contains(err.Error(), "synthetic-secret-key") || strings.Contains(err.Error(), "999999") {
		t.Fatal("Error accepted values outside its allowlist")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("unsafe fields broke unavailable sentinel")
	}
}

func TestNineRouterInterruptedResponseBody(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "provider-private-payload")
	}))
	defer api.Close()
	n, err := NewNineRouter(Config{BaseURL: api.URL, Model: "private-model-id", APIKey: "synthetic-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	requireDiagnostic(t, n.CheckConfig(context.Background()), "MODEL_DISCOVERY", "NETWORK", 200)
	if calls.Load() != 1 {
		t.Fatal("interrupted discovery caused a fallback request")
	}
	answer, err := n.Solve(context.Background(), SyntheticPNG())
	requireDiagnostic(t, err, "CAPTCHA_OCR", "NETWORK", 200)
	if answer != "" {
		t.Fatal("interrupted OCR returned an answer")
	}
}

func TestNineRouterTLSProtocolDiagnostic(t *testing.T) {
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request crossed a failed TLS handshake")
	}))
	api.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	api.StartTLS()
	defer api.Close()
	client := api.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.MaxVersion = tls.VersionTLS12
	client.Transport = transport
	defer transport.CloseIdleConnections()
	n, err := NewNineRouter(Config{BaseURL: api.URL, Model: "private-model-id", APIKey: "synthetic-secret-key", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	requireDiagnostic(t, n.CheckConfig(context.Background()), "MODEL_DISCOVERY", "TLS", 0)
}
