package acb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

const (
	ErrConnectionReset = "ACB_CONNECTION_RESET"
	ErrRequestTimeout  = "ACB_REQUEST_TIMEOUT"
	ErrRequestCanceled = "ACB_REQUEST_CANCELED"
	ErrNetwork         = "ACB_NETWORK_ERROR"
)

// AuthFailure signals a confirmed session expiration or challenge
// following a clean verification probe.
type AuthFailure struct {
	Kind   PageKind
	Reason string
}

func (e *AuthFailure) Error() string {
	if e == nil {
		return "ACB authentication required"
	}
	return fmt.Sprintf("ACB authentication required: %s (%s)", e.Kind, e.Reason)
}

var (
	// ErrConversationReset indicates that conversational tokens were rejected,
	// but the underlying session remains valid after resync. Any active pagination
	// cursor must be discarded and restarted from page 1.
	ErrConversationReset = errors.New("ACB conversational state reset; pagination cursor invalidated")

	// ErrInconclusiveAuth indicates that a login-like response was received but a
	// subsequent probe could not confirm authentication loss (e.g. network/5xx error
	// or missing form state).
	ErrInconclusiveAuth = errors.New("ACB authentication probe inconclusive")
)

// SanitizeTransportError returns an operator-safe error code without URLs,
// form fields, cookies, or other request data.
func SanitizeTransportError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return ErrRequestCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrRequestTimeout
	case errors.Is(err, syscall.ECONNRESET):
		return ErrConnectionReset
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ErrNetwork
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrRequestTimeout
	}
	return ErrNetwork
}
