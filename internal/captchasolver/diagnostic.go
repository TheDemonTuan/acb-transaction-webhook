package captchasolver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strconv"
)

// DiagnosticError contains only finite, non-provider diagnostic values. It never
// retains a transport error, response body, endpoint, API key, or OCR output.
type DiagnosticError struct {
	Stage      string
	Code       string
	HTTPStatus int
}

func (e *DiagnosticError) Error() string {
	if e == nil {
		return ErrUnavailable.Error()
	}
	stage := e.Stage
	switch stage {
	case "MODEL_DISCOVERY", "SYNTHETIC_OCR", "CAPTCHA_OCR":
	default:
		stage = "CAPTCHA_OCR"
	}
	code := e.Code
	switch code {
	case "DNS", "TLS", "TIMEOUT", "CANCELLED", "NETWORK", "HTTP_AUTH", "HTTP_NOT_FOUND", "HTTP_RATE_LIMIT", "HTTP_UPSTREAM", "MODEL_NOT_FOUND", "MODEL_COMBO_UNSUPPORTED", "RESPONSE_TOO_LARGE", "RESPONSE_SCHEMA", "REFUSAL", "OCR_MISMATCH":
	default:
		code = "NETWORK"
	}
	message := ErrUnavailable.Error() + " stage=" + stage + " code=" + code
	if e.HTTPStatus >= 100 && e.HTTPStatus <= 599 {
		message += " http_status=" + strconv.Itoa(e.HTTPStatus)
	}
	return message
}

func (*DiagnosticError) Unwrap() error { return ErrUnavailable }

func diagnostic(stage, code string, status int) error {
	return &DiagnosticError{Stage: stage, Code: code, HTTPStatus: status}
}

func transportDiagnostic(ctx context.Context, stage string, status int, err error, tlsFailed bool) error {
	// Prefer the request's cancellation/deadline over a wrapped transport error.
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	code := "NETWORK"
	var dns *net.DNSError
	var network net.Error
	var certificate *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCertificate x509.CertificateInvalidError
	var hostname x509.HostnameError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code = "TIMEOUT"
	case errors.Is(err, context.Canceled):
		code = "CANCELLED"
	case errors.As(err, &network) && network.Timeout():
		code = "TIMEOUT"
	case tlsFailed:
		code = "TLS"
	case errors.As(err, &dns):
		code = "DNS"
	case errors.As(err, &certificate), errors.As(err, &unknownAuthority), errors.As(err, &invalidCertificate), errors.As(err, &hostname), errors.As(err, &record), errors.As(err, &alert):
		code = "TLS"
	}
	return diagnostic(stage, code, status)
}

func httpDiagnostic(stage string, status int) error {
	code := "HTTP_UPSTREAM"
	switch status {
	case 401, 403:
		code = "HTTP_AUTH"
	case 404, 405:
		code = "HTTP_NOT_FOUND"
	case 429:
		code = "HTTP_RATE_LIMIT"
	}
	return diagnostic(stage, code, status)
}
