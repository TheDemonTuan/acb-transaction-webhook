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
	MaxAIRequests         = 3
	BrowserAttemptTTL     = 15 * time.Minute
	ReconcileInterval     = 5 * time.Second
	VerificationTimeout   = 60 * time.Second
	LoginCooldown         = 60 * time.Second
)

// SessionVerifier verifies an encrypted session envelope in the worker; it does
// not receive the account's login password.
type SessionVerifier interface {
	VerifySession(ctx context.Context, connectionID string, generation int64, encrypted []byte) error
}

// RecoveryScheduler records worker-owned catch-up scheduling intent.
type RecoveryScheduler interface {
	ScheduleRecovery(ctx context.Context, connectionID string, generation int64, eventKey string) error
}
