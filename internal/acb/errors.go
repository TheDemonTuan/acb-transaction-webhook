package acb

import (
	"context"
	"errors"
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
