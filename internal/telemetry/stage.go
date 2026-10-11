package telemetry

import (
	"context"
	"time"
)

type paymentStageContextKey struct{}

// WithPaymentReceivedAt annotates the request context with the monotonic receive time.
func WithPaymentReceivedAt(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, paymentStageContextKey{}, t)
}

// PaymentReceivedAt retrieves the monotonic receive time if present.
func PaymentReceivedAt(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(paymentStageContextKey{}).(time.Time)
	return t, ok
}
