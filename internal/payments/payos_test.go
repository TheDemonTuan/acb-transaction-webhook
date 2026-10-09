package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
)

const fixtureChecksumKey = "fixture-only-checksum"

func fixtureHMAC(value string) string {
	mac := hmac.New(sha256.New, []byte(fixtureChecksumKey))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// Reproduce the documented sorted key=value signature, not a mock SDK verifier.
// Numbers use fixed-point formatting; nested values use canonical JSON.
func fixtureSignature(t *testing.T, data map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(normalized))
	for key := range normalized {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		var value string
		switch item := normalized[key].(type) {
		case nil:
		case string:
			value = item
		case float64:
			value = strconv.FormatFloat(item, 'f', -1, 64)
		default:
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			value = string(encoded)
		}
		parts = append(parts, key+"="+value)
	}
	return fixtureHMAC(strings.Join(parts, "&"))
}

func writeSignedFixture(t *testing.T, w http.ResponseWriter, data map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"code": "00", "desc": "success", "data": data,
		"signature": fixtureSignature(t, data),
	}); err != nil {
		t.Error(err)
	}
}

func fixtureAdapter(t *testing.T, server *httptest.Server) *PayOSAdapter {
	t.Helper()
	adapter, err := NewPayOS("fixture-client", "fixture-api-key", fixtureChecksumKey,
		WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func createFixture() map[string]any {
	return map[string]any{
		"bin": "970452", "accountNumber": "VA-ORDER-1", "accountName": "SHOP",
		"amount": 50000, "description": "DH100000000001", "orderCode": int64(100000000001),
		"currency": "VND", "paymentLinkId": "fixture-link", "status": "PENDING",
		"checkoutUrl": "https://pay.payos.vn/web/fixture-link", "qrCode": "original-provider-qr",
		"unknownSignedField": "retain for signature verification",
	}
}

func linkFixture(status string) map[string]any {
	return map[string]any{
		"id": "fixture-link", "orderCode": int64(100000000001), "amount": 50000,
		"amountPaid": 50000, "amountRemaining": 0, "status": status,
		"createdAt": "2026-10-08T10:00:00+07:00", "cancellationReason": nil, "canceledAt": nil,
		"transactions": []any{map[string]any{
			"reference": "REF-1", "amount": 50000, "accountNumber": "VA-ORDER-1",
			"description": "DH100000000001", "transactionDateTime": "2026-10-08 10:05:00",
			"virtualAccountNumber": "VA-ORDER-1",
		}},
	}
}

func webhookFixture() map[string]any {
	return map[string]any{
		"orderCode": int64(100000000001), "amount": 50000, "description": "DH100000000001",
		"accountNumber": "VA-ORDER-1", "reference": "REF-1",
		"transactionDateTime": "2026-10-08 10:05:00", "currency": "VND",
		"paymentLinkId": "fixture-link", "code": "00", "desc": "success",
		"virtualAccountNumber": "VA-ORDER-1",
		"unknownSignedField":   map[string]any{"safe": []any{1, 2, 3}},
	}
}

func TestPayOSAdapterSignedCreateGetCancel(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-client-id") != "fixture-client" || r.Header.Get("x-api-key") != "fixture-api-key" {
			t.Error("SDK authentication headers missing")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/payment-requests":
			var req payos.CreatePaymentLinkRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			expected := fixtureHMAC(fmt.Sprintf("amount=%d&cancelUrl=%s&description=%s&orderCode=%d&returnUrl=%s",
				req.Amount, req.CancelUrl, req.Description, req.OrderCode, req.ReturnUrl))
			if req.Signature == nil || *req.Signature != expected {
				t.Error("SDK did not sign create request")
			}
			writeSignedFixture(t, w, createFixture())
		case "GET /v2/payment-requests/100000000001":
			writeSignedFixture(t, w, linkFixture("PAID"))
		case "POST /v2/payment-requests/100000000001/cancel":
			var req payos.CancelPaymentLinkRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			if req.CancellationReason == nil || *req.CancellationReason != "Cancelled by operator" {
				t.Error("incorrect cancellation reason")
			}
			writeSignedFixture(t, w, linkFixture("CANCELLED"))
		default:
			t.Error("unexpected provider request")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	adapter := fixtureAdapter(t, server)
	created, err := adapter.Create(context.Background(), payos.CreatePaymentLinkRequest{
		OrderCode: 100000000001, Amount: 50000, Description: "DH100000000001",
		ReturnUrl: "https://example.test/pay/opaque", CancelUrl: "https://example.test/pay/opaque",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.QrCode != "original-provider-qr" || created.AccountNumber != "VA-ORDER-1" {
		t.Fatal("create data lost")
	}
	link, err := adapter.Get(context.Background(), 100000000001)
	if err != nil {
		t.Fatal(err)
	}
	if link.Status != payos.PaymentLinkStatusPaid || len(link.Transactions) != 1 || link.Transactions[0].Reference != "REF-1" {
		t.Fatal("signed Get data lost")
	}
	cancelled, err := adapter.Cancel(context.Background(), 100000000001)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != payos.PaymentLinkStatusCancelled || calls.Load() != 3 {
		t.Fatal("cancel result or request count incorrect")
	}
}

func TestPayOSVerifyFullDataAndPanicGuards(t *testing.T) {
	adapter, err := NewPayOS("fixture-client", "fixture-api-key", fixtureChecksumKey)
	if err != nil {
		t.Fatal(err)
	}
	data := webhookFixture()
	body := map[string]any{"code": "00", "success": true, "data": data, "signature": fixtureSignature(t, data)}
	verified, err := adapter.Verify(context.Background(), body)
	if err != nil || verified.Reference != "REF-1" {
		t.Fatalf("verify: %v", err)
	}
	body["data"].(map[string]any)["unknownSignedField"] = "tampered"
	if _, err := adapter.Verify(context.Background(), body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatal("unknown fields were discarded before verification")
	}
	for _, signature := range []any{nil, 123, map[string]any{}, []any{}, "", strings.Repeat("x", 64), strings.Repeat("0", 63)} {
		body := map[string]any{"data": webhookFixture(), "signature": signature}
		if _, err := adapter.Verify(context.Background(), body); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("signature guard: %T %v", signature, err)
		}
	}
	for _, data := range []any{nil, "string", []any{}, map[string]any(nil)} {
		body := map[string]any{"data": data, "signature": strings.Repeat("0", 64)}
		if _, err := adapter.Verify(context.Background(), body); !errors.Is(err, ErrInvalidWebhook) {
			t.Fatalf("data guard: %T %v", data, err)
		}
	}
}

func TestPayOSVerifyRejectsUnsafeNumbersRecursively(t *testing.T) {
	adapter, err := NewPayOS("fixture-client", "fixture-api-key", fixtureChecksumKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{float64(1.5), math.Inf(1), math.NaN(), int64(9007199254740992),
		uint64(math.MaxUint64), json.Number("9007199254740991.1"), json.Number("9007199254740992"), json.Number("-1.1")} {
		data := webhookFixture()
		data["unknownSignedField"] = map[string]any{"nested": []any{value}}
		if _, err := adapter.Verify(context.Background(), map[string]any{"data": data, "signature": strings.Repeat("0", 64)}); !errors.Is(err, ErrInvalidWebhook) {
			t.Fatalf("unsafe nested number %v accepted: %v", value, err)
		}
	}
	for _, field := range []string{"amount", "orderCode"} {
		data := webhookFixture()
		data[field] = "50000"
		if _, err := adapter.Verify(context.Background(), map[string]any{"data": data, "signature": strings.Repeat("0", 64)}); !errors.Is(err, ErrInvalidWebhook) {
			t.Fatalf("numeric string %s accepted", field)
		}
	}
	data := webhookFixture()
	data["orderCode"] = json.Number("9007199254740991")
	if _, err := adapter.Verify(context.Background(), map[string]any{"data": data, "signature": fixtureSignature(t, data)}); err != nil {
		t.Fatal(err)
	}
}

func TestPayOSAdapterRejectsTamperedSignatures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := linkFixture("PAID")
		signature := fixtureSignature(t, data)
		data["amount"] = 60000
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "data": data, "signature": signature})
	}))
	defer server.Close()
	link, err := fixtureAdapter(t, server).Get(context.Background(), 100000000001)
	var providerErr *ProviderError
	if link != nil || !errors.As(err, &providerErr) || providerErr.HTTPStatus != 200 || !providerErr.Indeterminate {
		t.Fatalf("tampered response accepted: %v %v", link, err)
	}
}

func TestPayOSProviderErrorMetadata(t *testing.T) {
	for _, tc := range []struct {
		status        int
		code          string
		indeterminate bool
	}{
		{404, "unverified_absence_code", false}, {400, "unverified_rejection_code", false},
		{200, "unverified_duplicate_code", true}, {408, "timeout", true},
		{429, "throttle", true}, {503, "outage", true},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": tc.code, "desc": "secret-raw-provider-detail", "data": nil})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := fixtureAdapter(t, server).Get(ctx, 100000000001)
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.HTTPStatus != tc.status || providerErr.Code != tc.code ||
				providerErr.Indeterminate != tc.indeterminate || providerErr.RetryAfter < time.Minute {
				t.Fatalf("metadata incorrect: %+v", providerErr)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("raw SDK error leaked")
			}
		})
	}
}

func TestPayOSRequestLocalConcurrentMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.URL.Path, "/v2/payment-requests/")
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code})
	}))
	defer server.Close()
	adapter := fixtureAdapter(t, server)
	var wg sync.WaitGroup
	for order := int64(100000000001); order < 100000000021; order++ {
		wg.Add(1)
		go func(order int64) {
			defer wg.Done()
			_, err := adapter.Get(context.Background(), order)
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.Code != strconv.FormatInt(order, 10) {
				t.Errorf("cross-request metadata: %+v", providerErr)
			}
		}(order)
	}
	wg.Wait()
}

func TestPayOSBoundedCapturePreservesExactBytes(t *testing.T) {
	for _, size := range []int{maxProviderResponseBytes, maxProviderResponseBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := append([]byte(`{"code":"00","data":{"amount":1}}`), bytes.Repeat([]byte(" "), size-len(`{"code":"00","data":{"amount":1}}`))...)
			body := &trackingBody{Reader: bytes.NewReader(payload)}
			ctx, cancel, capture := providerContext(context.Background())
			defer cancel()
			response, err := captureResponse(func(context.Context, *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
			})(ctx, &http.Request{})
			if !body.closed || body.read > maxProviderResponseBytes+1 {
				t.Fatal("unbounded/unclosed response")
			}
			if size > maxProviderResponseBytes {
				if err == nil || response != nil || !capture.transportFailure {
					t.Fatal("oversized body accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			restored, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(restored, payload) {
				t.Fatal("response bytes rewritten")
			}
		})
	}
}

type trackingBody struct {
	*bytes.Reader
	closed bool
	read   int
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *trackingBody) Close() error { b.closed = true; return nil }

func TestPayOSAdapterCapAndExactNumericGuard(t *testing.T) {
	for _, payload := range []string{
		strings.Repeat("x", maxProviderResponseBytes+1),
		`{"code":"00","data":{"orderCode":9007199254740991.1,"amount":50000},"signature":"` + strings.Repeat("0", 64) + `"}`,
		`{"code":"00","data":{"orderCode":100000000001,"amount":50000,"extension":{"nested":[9007199254740992]}},"signature":"` + strings.Repeat("0", 64) + `"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, payload) }))
		adapter := fixtureAdapter(t, server)
		_, err := adapter.Get(context.Background(), 100000000001)
		server.Close()
		var providerErr *ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Indeterminate || providerErr.HTTPStatus != 200 {
			t.Fatalf("invalid response accepted: %v", err)
		}
	}
}

type fixtureRoundTripper func(*http.Request) (*http.Response, error)

func (fn fixtureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestPayOSProductionURLAndRequestDeadlines(t *testing.T) {
	t.Setenv("PAYOS_BASE_URL", "http://untrusted.invalid")
	var calls int
	client := &http.Client{Timeout: time.Minute, Transport: fixtureRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != "api-merchant.payos.vn" {
			t.Error("production URL controlled by environment")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > providerHTTPTimeout {
			t.Error("HTTP request exceeds ten second timeout")
		}
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":"missing"}`))}, nil
	})}
	adapter, err := NewPayOS("fixture-client", "fixture-api-key", fixtureChecksumKey, WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = adapter.Get(context.Background(), 100000000001)
	if calls != 1 || client.Timeout != time.Minute {
		t.Fatal("unexpected retries or injected client mutated")
	}
	ctx, cancel, _ := providerContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 15*time.Second || time.Until(deadline) < 14*time.Second {
		t.Fatal("overall deadline incorrect")
	}
}

func TestPayOSSDKOwnsTwoRetries(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"code":"outage"}`)
	}))
	defer server.Close()
	_, err := fixtureAdapter(t, server).Get(context.Background(), 100000000001)
	if err == nil || calls.Load() != 3 {
		t.Fatalf("expected initial attempt plus two SDK retries; calls=%d err=%v", calls.Load(), err)
	}
}

func TestPayOSRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"120", 2 * time.Minute}, {now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0}, {"-1", 0}, {"NaN", 0}, {"1.5", 0},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("Retry-After %q: got %s want %s", tc.value, got, tc.want)
		}
	}
}

func TestPayOSCancelledTransportIsSanitized(t *testing.T) {
	client := &http.Client{Transport: fixtureRoundTripper(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("secret-provider-body-and-credentials")
	})}
	adapter, err := NewPayOS("fixture-client", "fixture-api-key", fixtureChecksumKey, WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = adapter.Get(ctx, 100000000001)
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.HTTPStatus != 0 || providerErr.Code != "" || !providerErr.Indeterminate {
		t.Fatalf("transport uncertainty lost: %+v", providerErr)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("transport error leaked")
	}
}

func TestPayOSRedirectDoesNotForwardCredentials(t *testing.T) {
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := fixtureAdapter(t, server).Get(context.Background(), 100000000001)
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.HTTPStatus != http.StatusTemporaryRedirect || targetCalls.Load() != 0 {
		t.Fatalf("provider redirect followed: calls=%d error=%+v", targetCalls.Load(), providerErr)
	}
}
