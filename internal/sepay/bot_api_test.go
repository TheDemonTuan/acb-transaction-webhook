package sepay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const testBotToken = "900001:TEST_token_abcdefghijklmnopqrstuvwxyz"
const testTelegramOrigin = "https://counter.example"

func telegramServiceFixture(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	provider := httptest.NewServer(handler)
	t.Cleanup(provider.Close)
	service := NewService(Config{Mode: ModeDisabled}, protocolStore(t), nil)
	fields := adminFixture(t)
	fields.Mode = ModeDisabled
	if _, err := service.SaveAdminConfig(context.Background(), 0, fields, testBotToken, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	client := NewTelegramClient()
	client.baseURL = provider.URL // constructor-owned test injection, never an env override
	service.telegram = client
	return service
}

func telegramResult(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": data})
}

func TestTelegramRegisterRealClientContractAndRedactedStatus(t *testing.T) {
	callback := testTelegramOrigin + "/api/integrations/sepay/telegram"
	var registered atomic.Bool
	service := telegramServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Error("Bot API request is not POST")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			telegramResult(w, map[string]any{"id": 900001, "is_bot": true})
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			target := ""
			if registered.Load() {
				target = callback
			}
			telegramResult(w, map[string]any{"url": target, "pending_update_count": 2, "last_error_date": time.Now().Unix(), "last_error_message": testBotToken + " https://evil.example/token?secret=private"})
		case strings.HasSuffix(r.URL.Path, "/setWebhook"):
			var body struct {
				URL     string   `json:"url"`
				Secret  string   `json:"secret_token"`
				Allowed []string `json:"allowed_updates"`
				Max     int      `json:"max_connections"`
				Drop    *bool    `json:"drop_pending_updates"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.URL != callback || len(body.Secret) != 43 || strings.Join(body.Allowed, ",") != "message,edited_message" || body.Max != 4 || body.Drop == nil || *body.Drop {
				t.Error("unsafe webhook registration contract")
			}
			registered.Store(true)
			telegramResult(w, true)
		default:
			t.Error("unexpected Bot API method")
			w.WriteHeader(404)
		}
	})
	result, err := service.Telegram(context.Background(), testTelegramOrigin, true)
	if err != nil || !registered.Load() || !result.BotVerified || result.RegisteredURL != callback || result.PendingUpdateCount != 2 || result.LastErrorAt == nil {
		t.Fatalf("registration failed: %v", err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), testBotToken) || strings.Contains(string(raw), "evil.example") || strings.Contains(string(raw), "secret=") {
		t.Fatal("Telegram error leaked untrusted secret text")
	}
}

func TestTelegramRefusesForeignWebhookAndWrongBot(t *testing.T) {
	for _, wrongBot := range []bool{false, true} {
		var sets atomic.Int32
		service := telegramServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/getMe"):
				bot := int64(900001)
				if wrongBot {
					bot++
				}
				telegramResult(w, map[string]any{"id": bot, "is_bot": true})
			case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
				telegramResult(w, map[string]any{"url": "https://user:password@other.example/private-token?token=secret", "pending_update_count": 0})
			case strings.HasSuffix(r.URL.Path, "/setWebhook"):
				sets.Add(1)
				telegramResult(w, true)
			}
		})
		_, err := service.Telegram(context.Background(), testTelegramOrigin, true)
		wanted := ErrTelegramForeignWebhook
		if wrongBot {
			wanted = ErrTelegramBotMismatch
		}
		if !errors.Is(err, wanted) || sets.Load() != 0 {
			t.Fatalf("foreign bot/webhook replaced: %v", err)
		}
		if !wrongBot {
			status, err := service.Telegram(context.Background(), testTelegramOrigin, false)
			if err != nil || strings.Contains(status.RegisteredURL, "password") || strings.Contains(status.RegisteredURL, "private-token") || strings.Contains(status.RegisteredURL, "token=") {
				t.Fatal("foreign webhook was not redacted")
			}
		}
	}
}

type failingTelegramTransport struct{}

func (failingTelegramTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("https://api.telegram.org/bot" + testBotToken + "/getMe: connection refused")
}

func TestTelegramClientNetworkBoundsAndSafeFailures(t *testing.T) {
	client := NewTelegramClient()
	client.client.Transport = failingTelegramTransport{}
	var me map[string]any
	if err := client.call(context.Background(), testBotToken, "getMe", struct{}{}, &me); !errors.Is(err, ErrTelegramRequest) || strings.Contains(err.Error(), testBotToken) {
		t.Fatal("transport failure exposed token")
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); telegramResult(w, true) }))
	defer target.Close()
	for _, body := range []string{"redirect", strings.Repeat("x", (64<<10)+1), `{"ok":false,"description":"` + testBotToken + `"}`, `{"ok":true,"result":null}`, `{"ok":true,"result":{}} {}`} {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if body == "redirect" {
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				return
			}
			_, _ = io.WriteString(w, body)
		}))
		client = NewTelegramClient()
		client.baseURL = provider.URL
		err := client.call(context.Background(), testBotToken, "getMe", struct{}{}, &me)
		provider.Close()
		if !errors.Is(err, ErrTelegramRequest) || strings.Contains(err.Error(), testBotToken) {
			t.Fatal("invalid Bot API response accepted or leaked")
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("client followed redirect with token")
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer provider.Close()
	client = NewTelegramClient()
	client.baseURL = provider.URL
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := client.call(ctx, testBotToken, "getMe", struct{}{}, &me); !errors.Is(err, ErrTelegramRequest) {
		t.Fatal("cancelled call was accepted")
	}
}

func TestTelegramRegistrationRejectsRevisionChangeDuringNetwork(t *testing.T) {
	var service *Service
	callback := testTelegramOrigin + "/api/integrations/sepay/telegram"
	var registered atomic.Bool
	service = telegramServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			telegramResult(w, map[string]any{"id": 900001, "is_bot": true})
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			target := ""
			if registered.Load() {
				target = callback
			}
			telegramResult(w, map[string]any{"url": target, "pending_update_count": 0})
		case strings.HasSuffix(r.URL.Path, "/setWebhook"):
			config, err := service.AdminConfig(context.Background())
			if err != nil {
				t.Error(err)
			}
			if _, err := service.SaveAdminConfig(context.Background(), config.Revision, config.Config, "900001:ROTATED_abcdefghijklmnopqrstuvwxyz", storage.PaymentProviderActor{}); err != nil {
				t.Error(err)
			}
			registered.Store(true)
			telegramResult(w, true)
		}
	})
	if _, err := service.Telegram(context.Background(), testTelegramOrigin, true); !errors.Is(err, storage.ErrSePayRevisionConflict) {
		t.Fatalf("old token verified new configuration: %v", err)
	}
}

func TestTelegramStatusNeverClaimsMissingOrForeignCallbackHealthy(t *testing.T) {
	for _, target := range []string{"", "https://another.example/secret-path?token=private"} {
		service := telegramServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/getMe") {
				telegramResult(w, map[string]any{"id": 900001, "is_bot": true})
				return
			}
			telegramResult(w, map[string]any{"url": target, "pending_update_count": 0})
		})
		status, err := service.Telegram(context.Background(), testTelegramOrigin, false)
		if err != nil || !status.BotVerified || status.LastErrorMessage == "" {
			t.Fatal("missing/foreign callback reported healthy")
		}
		if strings.Contains(status.RegisteredURL, "secret-path") || strings.Contains(status.RegisteredURL, "private") {
			t.Fatal("status leaked a foreign callback")
		}
	}
	for _, origin := range []string{"http://counter.example", "https://user:password@counter.example", "https://counter.example/pay", "https://counter.example?token=private", "https://counter.example/#fragment"} {
		if _, err := telegramCallback(origin); !errors.Is(err, ErrTelegramConfigRequired) {
			t.Fatal("unsafe public callback origin accepted")
		}
	}
}
