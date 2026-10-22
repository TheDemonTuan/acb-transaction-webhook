package bark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestBarkSenderHappyPath(t *testing.T) {
	var receivedAuth string
	var receivedPayload pushPayload

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/push" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		receivedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedPayload)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(barkResponse{
			Code:      200,
			Message:   "success",
			Timestamp: time.Now().Unix(),
		})
	}))
	defer ts.Close()

	cfg := Config{
		ServerURL:         ts.URL,
		BasicAuthUser:     "admin",
		BasicAuthPassword: "password123",
		Timeout:           3 * time.Second,
	}

	sender := NewSender(cfg, ts.Client(), "https://bank.example.com")

	eventPayload, _ := json.Marshal(map[string]any{
		"transactionId":     "txn_001",
		"transactionNumber": "888999",
		"credit":            "500000",
		"source":            "REALTIME",
		"description":       "Chuyen tien",
	})

	req := notification.SendRequest{
		DeliveryID: "del_001",
		EventID:    "evt_001",
		Target: storage.DeliveryTarget{
			Provider: "BARK",
			Secret:   []byte("test_device_key_abc"),
			BarkConfig: &storage.BarkConfig{
				Group: "ACB",
				Level: "timeSensitive",
				Sound: "shake",
			},
		},
		EventPayload: eventPayload,
	}

	res := sender.Send(context.Background(), req)
	if res.Outcome != notification.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got: %v (code=%s, err=%s)", res.Outcome, res.ProviderErrorCode, res.SanitizedError)
	}

	if receivedPayload.DeviceKey != "test_device_key_abc" {
		t.Fatalf("expected device key received, got: %s", receivedPayload.DeviceKey)
	}
	if receivedPayload.IsArchive != "1" {
		t.Fatalf("expected isArchive to be 1, got: %s", receivedPayload.IsArchive)
	}
	if receivedAuth == "" {
		t.Fatalf("expected Basic Auth header to be present")
	}
}

func TestBarkSenderAuthFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // Bark returns 418 on bad auth
		_, _ = w.Write([]byte("I'm a teapot"))
	}))
	defer ts.Close()

	cfg := Config{
		ServerURL: ts.URL,
		Timeout:   3 * time.Second,
	}
	sender := NewSender(cfg, ts.Client(), "")

	res := sender.Send(context.Background(), notification.SendRequest{
		Target: storage.DeliveryTarget{
			Secret: []byte("key_xyz"),
		},
		EventPayload: []byte(`{"credit":"100"}`),
	})

	if res.Outcome != notification.OutcomeTerminalFailure || res.ProviderErrorCode != "BARK_AUTH_FAILED" {
		t.Fatalf("expected TerminalFailure with BARK_AUTH_FAILED, got: %+v", res)
	}
}

func TestBarkSenderHTTPClassification(t *testing.T) {
	tests := []struct {
		status  int
		outcome notification.Outcome
		code    string
	}{
		{http.StatusFound, notification.OutcomeTerminalFailure, "BARK_REDIRECT_REJECTED"},
		{http.StatusBadRequest, notification.OutcomeTerminalFailure, "BARK_BAD_REQUEST"},
		{http.StatusUnauthorized, notification.OutcomeTerminalFailure, "BARK_AUTH_FAILED"},
		{http.StatusTeapot, notification.OutcomeTerminalFailure, "BARK_AUTH_FAILED"},
		{http.StatusTooManyRequests, notification.OutcomeRetry, "HTTP_429"},
		{http.StatusInternalServerError, notification.OutcomeRetry, "HTTP_500"},
		{http.StatusServiceUnavailable, notification.OutcomeRetry, "HTTP_503"},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) }))
			defer ts.Close()
			res := NewSender(Config{ServerURL: ts.URL, Timeout: time.Second}, ts.Client(), "").SendTestNotification(context.Background(), storage.DeliveryTarget{Secret: []byte("device_key")})
			if res.Outcome != tc.outcome || res.ProviderErrorCode != tc.code {
				t.Fatalf("status %d: got outcome=%s code=%s", tc.status, res.Outcome, res.ProviderErrorCode)
			}
		})
	}
}

func TestBarkSenderRejectsOversizedResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, (16<<10)+1))
	}))
	defer ts.Close()
	res := NewSender(Config{ServerURL: ts.URL, Timeout: time.Second}, ts.Client(), "").SendTestNotification(context.Background(), storage.DeliveryTarget{Secret: []byte("device_key")})
	if res.Outcome != notification.OutcomeTerminalFailure || res.ProviderErrorCode != "BARK_RESPONSE_TOO_LARGE" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestBarkSenderServerErrorRetries(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	cfg := Config{
		ServerURL: ts.URL,
		Timeout:   3 * time.Second,
	}
	sender := NewSender(cfg, ts.Client(), "")

	res := sender.Send(context.Background(), notification.SendRequest{
		Target: storage.DeliveryTarget{
			Secret: []byte("key_xyz"),
		},
		EventPayload: []byte(`{"credit":"100"}`),
	})

	if res.Outcome != notification.OutcomeRetry {
		t.Fatalf("expected OutcomeRetry for 502, got: %+v", res)
	}
}

func TestBarkSenderSePaySnapshotOverrideIsRequestLocal(t *testing.T) {
	for _, scenario := range []string{"snapshot", "missing", "disabled", "other-provider"} {
		t.Run(scenario, func(t *testing.T) {
			var received pushPayload
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, `{"code":200,"message":"success"}`)
			}))
			defer ts.Close()
			cfg := storage.DefaultBarkConfig()
			cfg.IncludeBalance, cfg.IncludeDescription = scenario != "disabled", scenario != "disabled"
			provider := "SEPAY"
			if scenario == "other-provider" {
				provider = "PAYOS"
			}
			payload, _ := json.Marshal(map[string]any{"provider": provider, "credit": "150000", "balance": "9876543", "description": "stale public description"})
			original := string(payload)
			balance := int64(2350000)
			details := &storage.SePayNotificationDetails{BalanceVND: &balance, Description: "private snapshot memo"}
			if scenario == "missing" {
				details = nil
			}
			res := NewSender(Config{ServerURL: ts.URL, Timeout: time.Second}, ts.Client(), "").Send(context.Background(), notification.SendRequest{Target: storage.DeliveryTarget{Secret: []byte("fixture-device"), BarkConfig: &cfg}, EventPayload: payload, SePayDetails: details})
			if res.Outcome != notification.OutcomeSuccess {
				t.Fatalf("send=%+v", res)
			}
			if string(payload) != original {
				t.Fatal("sender mutated shared event bytes")
			}
			var expected map[string]any
			if err := json.Unmarshal(payload, &expected); err != nil {
				t.Fatal(err)
			}
			if provider == "SEPAY" {
				delete(expected, "balance")
				expected["description"] = ""
				if scenario == "snapshot" {
					expected["balance"], expected["description"] = "2350000", "private snapshot memo"
				}
			}
			title, subtitle, body, _, _, _, _, _ := FormatTransactionNotification(expected, cfg, "")
			if received.Title != title || received.Subtitle != subtitle || received.Body != body {
				t.Fatalf("snapshot mapping wrong: %+v", received)
			}
		})
	}
}
