package bark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type Sender struct {
	cfg          Config
	httpClient   *http.Client
	publicOrigin string
}

func NewSender(cfg Config, client *http.Client, publicOrigin string) *Sender {
	if client == nil {
		client = &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				TLSHandshakeTimeout:   3 * time.Second,
				ResponseHeaderTimeout: 3 * time.Second,
				IdleConnTimeout:       60 * time.Second,
				MaxIdleConns:          16,
				MaxIdleConnsPerHost:   8,
			},
		}
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Sender{
		cfg:          cfg,
		httpClient:   client,
		publicOrigin: publicOrigin,
	}
}

type pushPayload struct {
	DeviceKey string `json:"device_key"`
	Title     string `json:"title"`
	Subtitle  string `json:"subtitle,omitempty"`
	Body      string `json:"body"`
	Group     string `json:"group,omitempty"`
	Sound     string `json:"sound,omitempty"`
	Level     string `json:"level,omitempty"`
	Icon      string `json:"icon,omitempty"`
	URL       string `json:"url,omitempty"`
	IsArchive string `json:"isArchive,omitempty"`
}

type barkResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

func (s *Sender) Send(ctx context.Context, req notification.SendRequest) notification.SendResult {
	if !s.cfg.Configured() {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			ProviderErrorCode: "BARK_NOT_CONFIGURED",
			SanitizedError:    "Bark server URL is not configured",
		}
	}

	cfg := storage.DefaultBarkConfig()
	if req.Target.BarkConfig != nil {
		cfg = *req.Target.BarkConfig
	}

	var eventData map[string]any
	if err := json.Unmarshal(req.EventPayload, &eventData); err != nil {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			ProviderErrorCode: "MALFORMED_EVENT_PAYLOAD",
			SanitizedError:    "failed to parse event payload JSON",
		}
	}

	title, subtitle, body, group, sound, level, icon, linkURL := FormatTransactionNotification(eventData, cfg, s.publicOrigin)

	payload := pushPayload{
		DeviceKey: string(req.Target.Secret),
		Title:     title,
		Subtitle:  subtitle,
		Body:      body,
		Group:     group,
		Sound:     sound,
		Level:     level,
		Icon:      icon,
		URL:       linkURL,
		IsArchive: "1",
	}

	return s.doPush(ctx, payload)
}

func (s *Sender) SendTestNotification(ctx context.Context, target storage.DeliveryTarget) notification.SendResult {
	if !s.cfg.Configured() {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			ProviderErrorCode: "BARK_NOT_CONFIGURED",
			SanitizedError:    "Bark server URL is not configured",
		}
	}

	cfg := storage.DefaultBarkConfig()
	if target.BarkConfig != nil {
		cfg = *target.BarkConfig
	}

	title, subtitle, body, group, sound, level, icon := FormatTestNotification(cfg)

	payload := pushPayload{
		DeviceKey: string(target.Secret),
		Title:     title,
		Subtitle:  subtitle,
		Body:      body,
		Group:     group,
		Sound:     sound,
		Level:     level,
		Icon:      icon,
		IsArchive: "1",
	}

	return s.doPush(ctx, payload)
}

func (s *Sender) doPush(ctx context.Context, payload pushPayload) notification.SendResult {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			ProviderErrorCode: "MARSHAL_ERROR",
			SanitizedError:    "failed to marshal push payload",
		}
	}

	targetURL := strings.TrimRight(s.cfg.ServerURL, "/") + "/push"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			ProviderErrorCode: "INVALID_REQUEST",
			SanitizedError:    "failed to create HTTP request",
		}
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if s.cfg.BasicAuthUser != "" || s.cfg.BasicAuthPassword != "" {
		httpReq.SetBasicAuth(s.cfg.BasicAuthUser, s.cfg.BasicAuthPassword)
	}

	start := time.Now()
	resp, reqErr := s.httpClient.Do(httpReq)
	latencyMs := int(time.Since(start).Milliseconds())

	if reqErr != nil {
		code := "NETWORK_ERROR"
		message := "Bark network request failed"
		if errors.Is(reqErr, context.DeadlineExceeded) {
			code = "NETWORK_TIMEOUT"
			message = "Bark network request timed out"
		}
		return notification.SendResult{
			Outcome:           notification.OutcomeRetry,
			StatusCode:        0,
			LatencyMs:         latencyMs,
			ProviderErrorCode: code,
			SanitizedError:    message,
		}
	}
	defer resp.Body.Close()

	const maxResponseBody = 16 << 10
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if readErr != nil {
		return notification.SendResult{
			Outcome:           notification.OutcomeRetry,
			StatusCode:        resp.StatusCode,
			LatencyMs:         latencyMs,
			ProviderErrorCode: "READ_ERROR",
			SanitizedError:    "failed to read response body",
		}
	}
	if len(bodyBytes) > maxResponseBody {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			StatusCode:        resp.StatusCode,
			LatencyMs:         latencyMs,
			ProviderErrorCode: "BARK_RESPONSE_TOO_LARGE",
			SanitizedError:    "Bark response body exceeded the limit",
		}
	}

	// Basic Auth error check: Bark returns 418 or 401
	if resp.StatusCode == http.StatusTeapot || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			StatusCode:        resp.StatusCode,
			LatencyMs:         latencyMs,
			ProviderErrorCode: "BARK_AUTH_FAILED",
			SanitizedError:    "Bark server authentication failed (check basic auth credentials)",
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var barkResp barkResponse
		if err := json.Unmarshal(bodyBytes, &barkResp); err != nil {
			return notification.SendResult{
				Outcome:           notification.OutcomeRetry,
				StatusCode:        resp.StatusCode,
				LatencyMs:         latencyMs,
				ProviderErrorCode: "BARK_BAD_RESPONSE",
				SanitizedError:    "invalid JSON returned from Bark server",
			}
		}

		if barkResp.Code == 200 {
			return notification.SendResult{
				Outcome:    notification.OutcomeSuccess,
				StatusCode: resp.StatusCode,
				LatencyMs:  latencyMs,
			}
		}

		// Non-200 code in JSON response
		outcome := notification.OutcomeRetry
		if barkResp.Code == 400 || barkResp.Code == 404 || barkResp.Code == 413 || barkResp.Code == 422 {
			outcome = notification.OutcomeTerminalFailure
		}
		return notification.SendResult{
			Outcome:           outcome,
			StatusCode:        resp.StatusCode,
			LatencyMs:         latencyMs,
			ProviderErrorCode: fmt.Sprintf("BARK_APP_%d", barkResp.Code),
			SanitizedError:    fmt.Sprintf("Bark server error %d", barkResp.Code),
		}
	}

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return notification.SendResult{Outcome: notification.OutcomeTerminalFailure, StatusCode: resp.StatusCode, LatencyMs: latencyMs, ProviderErrorCode: "BARK_REDIRECT_REJECTED", SanitizedError: "Bark server redirect was rejected"}
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return notification.SendResult{Outcome: notification.OutcomeRetry, StatusCode: resp.StatusCode, LatencyMs: latencyMs, ProviderErrorCode: fmt.Sprintf("HTTP_%d", resp.StatusCode), SanitizedError: fmt.Sprintf("Bark server returned HTTP %d", resp.StatusCode)}
	}

	code := "BARK_CLIENT_ERROR"
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code = "BARK_BAD_REQUEST"
	case http.StatusNotFound:
		code = "BARK_NOT_FOUND"
	case http.StatusGone:
		code = "BARK_DEVICE_GONE"
	case http.StatusRequestEntityTooLarge:
		code = "BARK_PAYLOAD_TOO_LARGE"
	}
	return notification.SendResult{
		Outcome:           notification.OutcomeTerminalFailure,
		StatusCode:        resp.StatusCode,
		LatencyMs:         latencyMs,
		ProviderErrorCode: code,
		SanitizedError:    fmt.Sprintf("Bark request rejected with HTTP %d", resp.StatusCode),
	}
}
