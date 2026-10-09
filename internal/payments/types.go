package payments

import (
	"context"
	"errors"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
)

// Provider is the single payOS payment-link boundary. Business state belongs to Service.
type Provider interface {
	Create(context.Context, payos.CreatePaymentLinkRequest) (*payos.CreatePaymentLinkResponse, error)
	Get(context.Context, int64) (*payos.PaymentLink, error)
	Cancel(context.Context, int64) (*payos.PaymentLink, error)
	Verify(context.Context, map[string]any) (*payos.WebhookData, error)
}

// ProviderError contains only transport/envelope metadata, never provider descriptions,
// response bodies, credentials or raw SDK errors. Code is not an absence/duplicate contract.
type ProviderError struct {
	HTTPStatus    int
	Code          string
	RetryAfter    time.Duration
	Indeterminate bool
}

func (*ProviderError) Error() string { return "payment provider request failed" }

var (
	ErrInvalidWebhook   = errors.New("invalid payment webhook")
	ErrInvalidSignature = errors.New("invalid payment signature")
)
