package authrecovery

import (
	"context"
	"time"
)

const (
	AutomaticOwner        = "system:acb-recovery"
	MaxAttempts           = 3
	MaxCaptchaSubmissions = 3
	MaxOTPSubmissions     = 1
	MaxAIRequests         = 1
	BrowserAttemptTTL     = 15 * time.Minute
	ReconcileInterval     = 5 * time.Second
	LoginCooldown         = 60 * time.Second
)

// Credentials live only for an attempt and must never be logged or persisted.
// AccountNumber is the exact account number, not a masked display value.
type Credentials struct {
	Username      string
	Password      string
	AccountNumber string
}

// SessionVerifier verifies an encrypted session envelope in the worker; it does
// not receive the account's login password.
type SessionVerifier interface {
	VerifySession(ctx context.Context, connectionID string, generation int64, encrypted []byte) error
}

// RecoveryScheduler records worker-owned catch-up scheduling intent.
type RecoveryScheduler interface {
	ScheduleRecovery(ctx context.Context, connectionID string, generation int64, eventKey string) error
}
