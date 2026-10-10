package sepay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

var (
	botTokenPattern           = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]{20,}$`)
	ErrTelegramTokenRequired  = errors.New("Telegram token required")
	ErrTelegramConfigRequired = errors.New("Telegram configuration required")
	ErrTelegramBotMismatch    = errors.New("Telegram bot ID mismatch")
	ErrTelegramForeignWebhook = errors.New("Telegram bot has another webhook")
	ErrTelegramRequest        = errors.New("Telegram request failed")
)

// TelegramClient never returns transport errors or Telegram descriptions: both
// can contain a token-bearing URL, attacker text, or receiver information.
type TelegramClient struct {
	client  *http.Client
	baseURL string
}

func NewTelegramClient() *TelegramClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 8 * time.Second
	return &TelegramClient{client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, baseURL: "https://api.telegram.org"}
}

func (c *TelegramClient) call(ctx context.Context, token, method string, request, result any) error {
	if !botTokenPattern.MatchString(token) {
		return ErrTelegramTokenRequired
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ErrTelegramRequest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return ErrTelegramRequest
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return ErrTelegramRequest
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrTelegramRequest
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return ErrTelegramRequest
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return ErrTelegramRequest
	}
	if json.Unmarshal(envelope.Result, result) != nil {
		return ErrTelegramRequest
	}
	return nil
}

type TelegramStatus struct {
	RegisteredURL      string     `json:"registeredUrl"`
	PendingUpdateCount int64      `json:"pendingUpdateCount"`
	LastErrorAt        *time.Time `json:"lastErrorAt"`
	LastErrorMessage   string     `json:"lastErrorMessage"`
	BotVerified        bool       `json:"botVerified"`
}

type webhookInfo struct {
	URL                string `json:"url"`
	PendingUpdateCount int64  `json:"pending_update_count"`
	LastErrorDate      int64  `json:"last_error_date"`
	LastErrorMessage   string `json:"last_error_message"`
}

func telegramCallback(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", ErrTelegramConfigRequired
	}
	return strings.TrimSuffix(origin, "/") + "/api/integrations/sepay/telegram", nil
}

func safeWebhookURL(raw string) string {
	if raw == "" {
		return ""
	}
	// An untrusted hostname can contain secrets too; reflect no foreign URL bytes.
	return "[redacted mismatched URL]"
}

func (s *Service) Telegram(ctx context.Context, origin string, register bool) (TelegramStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return TelegramStatus{}, err
	}
	if snap.token == "" {
		return TelegramStatus{}, ErrTelegramTokenRequired
	}
	cfg := snap.cfg
	if cfg.BotID <= 0 || cfg.ChatID >= 0 || cfg.SenderBotID <= 0 || cfg.WebhookSecret == "" {
		return TelegramStatus{}, ErrTelegramConfigRequired
	}
	callback, err := telegramCallback(origin)
	if err != nil {
		return TelegramStatus{}, err
	}
	client := snap.telegram
	if client == nil {
		client = NewTelegramClient()
	}
	var me struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	}
	if err := client.call(ctx, snap.token, "getMe", struct{}{}, &me); err != nil {
		return TelegramStatus{}, err
	}
	if !me.IsBot || me.ID != cfg.BotID {
		return TelegramStatus{}, ErrTelegramBotMismatch
	}
	var info webhookInfo
	if err := client.call(ctx, snap.token, "getWebhookInfo", struct{}{}, &info); err != nil {
		return TelegramStatus{}, err
	}
	if register {
		if info.URL != "" && info.URL != callback {
			return TelegramStatus{}, ErrTelegramForeignWebhook
		}
		// Refuse an already-obsolete request before changing external state.
		current, err := s.Snapshot(ctx)
		if err != nil {
			return TelegramStatus{}, err
		}
		if current.revision != snap.revision {
			return TelegramStatus{}, storage.ErrSePayRevisionConflict
		}
		if snap.store != nil {
			if err := snap.store.CheckMutationAllowed(ctx); err != nil {
				return TelegramStatus{}, err
			}
		}
		var accepted bool
		request := struct {
			URL            string   `json:"url"`
			Secret         string   `json:"secret_token"`
			Allowed        []string `json:"allowed_updates"`
			MaxConnections int      `json:"max_connections"`
			DropPending    bool     `json:"drop_pending_updates"`
		}{callback, cfg.WebhookSecret, []string{"message", "edited_message"}, 4, false}
		if err := client.call(ctx, snap.token, "setWebhook", request, &accepted); err != nil {
			return TelegramStatus{}, err
		}
		if !accepted {
			return TelegramStatus{}, ErrTelegramRequest
		}
		// setWebhook=true is Telegram's authoritative confirmation; diagnostics
		// retain the pending/error snapshot from getWebhookInfo, with no extra poll.
		info.URL = callback
	}
	current, err := s.Snapshot(ctx)
	if err != nil {
		return TelegramStatus{}, err
	}
	if current.revision != snap.revision {
		return TelegramStatus{}, storage.ErrSePayRevisionConflict
	}
	result := TelegramStatus{RegisteredURL: safeWebhookURL(info.URL), PendingUpdateCount: info.PendingUpdateCount, BotVerified: true}
	if info.URL == callback {
		result.RegisteredURL = callback
	}
	if info.PendingUpdateCount < 0 || info.PendingUpdateCount > 9007199254740991 || info.LastErrorDate < 0 || info.LastErrorDate > 253402300799 {
		return TelegramStatus{}, ErrTelegramRequest
	}
	if info.LastErrorDate > 0 {
		stamp := time.Unix(info.LastErrorDate, 0).UTC()
		result.LastErrorAt = &stamp
	}
	if info.LastErrorMessage != "" {
		result.LastErrorMessage = "Telegram could not deliver an update; check the callback, source configuration and pending updates."
	}
	if info.URL == "" {
		result.LastErrorMessage = "Telegram webhook is not registered; save configuration and register this bot."
	} else if info.URL != callback {
		result.LastErrorMessage = "Telegram bot uses another webhook; use a dedicated receiver bot instead of replacing it."
	}
	return result, nil
}
