package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

func (s *Server) sepayNativeWebhook(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now()
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeSePayWebhookError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeSePayWebhookError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
		return
	}
	snapshot, err := s.sepay.Snapshot(r.Context())
	if err != nil || !snapshot.NativeEnabled() {
		writeSePayWebhookError(w, http.StatusServiceUnavailable, "SEPAY_UNAVAILABLE")
		return
	}
	cfg := snapshot.Config()
	r.Body = http.MaxBytesReader(w, r.Body, maxSePayWebhookBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeSePayWebhookError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
		} else {
			writeSePayWebhookError(w, http.StatusBadRequest, "INVALID_SEPAY_WEBHOOK")
		}
		return
	}
	hook, err := sepay.VerifyNativeWebhook(raw, r.Header, cfg.NativeWebhookSecret, receivedAt)
	if err != nil {
		if errors.Is(err, sepay.ErrNativeAuthentication) {
			writeSePayWebhookError(w, http.StatusUnauthorized, "INVALID_AUTHENTICATION")
			return
		}
		writeSePayWebhookError(w, http.StatusBadRequest, "INVALID_SEPAY_WEBHOOK")
		return
	}
	verifiedAt := time.Now()
	telemetry.RecordPaymentStage(telemetry.PaymentProviderSePayNative, telemetry.PaymentStageVerify, verifiedAt.Sub(receivedAt))

	if err := snapshot.HandleNativeWebhook(r.Context(), hook); err != nil {
		writeSePayWebhookError(w, http.StatusServiceUnavailable, "SEPAY_UNAVAILABLE")
		return
	}
	committedAt := time.Now()
	telemetry.RecordPaymentStage(telemetry.PaymentProviderSePayNative, telemetry.PaymentStageCommit, committedAt.Sub(verifiedAt))

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
	ackAt := time.Now()
	telemetry.RecordPaymentStage(telemetry.PaymentProviderSePayNative, telemetry.PaymentStageAck, ackAt.Sub(committedAt))
}
