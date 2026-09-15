package webhook

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

type Sender struct {
	httpClient        *http.Client
	skipURLValidation bool
}

func NewSender(client *http.Client, skipURLValidation bool) *Sender {
	if client == nil {
		client = NewHTTPClient()
	}
	return &Sender{
		httpClient:        client,
		skipURLValidation: skipURLValidation,
	}
}

func (s *Sender) Send(ctx context.Context, req notification.SendRequest) notification.SendResult {
	if !s.skipURLValidation {
		if _, err := security.ValidateWebhookURL(req.Target.URL); err != nil {
			return notification.SendResult{
				Outcome:           notification.OutcomeTerminalFailure,
				StatusCode:        0,
				ProviderErrorCode: "INVALID_URL",
				SanitizedError:    err.Error(),
			}
		}
	}

	nonce, err := NewNonce()
	if err != nil {
		return notification.SendResult{
			Outcome:           notification.OutcomeRetry,
			ProviderErrorCode: "NONCE_ERROR",
			SanitizedError:    "failed to generate nonce",
		}
	}

	signed := Sign(req.Target.Secret, req.EventPayload, time.Now().UTC(), nonce)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.Target.URL, bytes.NewReader(req.EventPayload))
	if err != nil {
		return notification.SendResult{
			Outcome:           notification.OutcomeTerminalFailure,
			ProviderErrorCode: "INVALID_REQUEST",
			SanitizedError:    "failed to create request",
		}
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Bank-Event-Id", req.EventID)
	httpReq.Header.Set("X-Bank-Delivery-Id", req.DeliveryID)
	httpReq.Header.Set("X-Idempotency-Key", req.DeliveryID)
	httpReq.Header.Set("X-Bank-Timestamp", signed.Timestamp)
	httpReq.Header.Set("X-Bank-Nonce", signed.Nonce)
	httpReq.Header.Set("X-Bank-Signature", signed.Signature)

	start := time.Now()
	resp, reqErr := s.httpClient.Do(httpReq)
	latencyMs := int(time.Since(start).Milliseconds())

	if reqErr != nil {
		errStr := reqErr.Error()
		if len(errStr) > 500 {
			errStr = errStr[:500]
		}
		return notification.SendResult{
			Outcome:           notification.OutcomeRetry,
			StatusCode:        0,
			LatencyMs:         latencyMs,
			ProviderErrorCode: "NETWORK_ERROR",
			SanitizedError:    errStr,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return notification.SendResult{
			Outcome:    notification.OutcomeSuccess,
			StatusCode: resp.StatusCode,
			LatencyMs:  latencyMs,
		}
	}

	outcome := notification.OutcomeRetry
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
		outcome = notification.OutcomeTerminalFailure
	}

	return notification.SendResult{
		Outcome:           outcome,
		StatusCode:        resp.StatusCode,
		LatencyMs:         latencyMs,
		ProviderErrorCode: fmt.Sprintf("HTTP_%d", resp.StatusCode),
		SanitizedError:    fmt.Sprintf("endpoint returned HTTP %d", resp.StatusCode),
	}
}
