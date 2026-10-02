package challenge

import (
	"context"
	"errors"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

var (
	ErrInvalidResponse = errors.New("challenge response format is invalid")
	ErrOutcomeUnknown  = errors.New("challenge submission outcome is unknown")
)

type Browser interface {
	Observe(context.Context, string) (authbrowser.AuthObservation, error)
}
type Sender interface {
	SendChallenge(context.Context, storage.AuthChallenge, []byte) (int64, error)
	DeleteMessage(context.Context, int64, int64) error
	SendText(context.Context, int64, string, any) (int64, error)
}
type Submitter interface {
	SubmitChallenge(context.Context, storage.AuthChallenge, string) (authbrowser.AuthObservation, error)
}
type Config struct {
	ChatID             int64
	CaptchaTTL, OTPTTL time.Duration
}
